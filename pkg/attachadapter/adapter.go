// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package attachadapter bridges a *agent.Agent onto the attach-mode
// HTTP/SSE surface (pkg/attach). It is phase 4 of the pkg/agent
// decomposition (docs/agent-package-split-design.md): the 22 Attach*
// capability methods and the WithAttach* provider options that used
// to live on the core Agent now live here, so the frozen agent
// surface stays narrow and hosts that never serve attach-mode never
// carry the wiring.
//
// Usage:
//
//	a := agent.New(llm, ...)                 // core options only
//	ad := attachadapter.New(a,
//	    attachadapter.WithMemoryProvider(f), // formerly agent.WithAttachMemoryProvider
//	    attachadapter.WithPromptBroker(b),
//	)
//	reg.Register(ad)                          // ad satisfies attach.Registrant
//
// The adapter satisfies attach.Registrant plus every optional attach
// capability interface (ToolsProvider, UsageProvider, PermsController,
// ...); registering it with a *attach.SessionRegistry makes the agent
// reachable over HTTP/SSE via attach.NewServer.
package attachadapter

import (
	"context"

	"github.com/go-steer/core-agent/v2/pkg/agent"
	"github.com/go-steer/core-agent/v2/pkg/attach"
	"github.com/go-steer/core-agent/v2/pkg/auth"
	"github.com/go-steer/core-agent/v2/pkg/eventlog"
)

// Adapter wraps a *agent.Agent with the attach-facing capability
// surface. Construct with New; the zero value is not useful.
//
// The capability methods (Attach*) are safe on a nil receiver — they
// degrade to the same "capability not registered" responses an
// unwired closure produces, matching the nil-safety convention of
// the agent package. The plain Registrant forwards (AppName, Inject,
// ...) assume a real wrapped agent, same as registering a bare
// *agent.Agent did before the split.
type Adapter struct {
	a *agent.Agent

	// Snapshot / controller closures wired via the With* options.
	// Deliberately closures, not interfaces: each one snapshots the
	// current state at call time so the attach handlers see fresh
	// state without the host rebuilding the adapter.
	memoryFn     func() []attach.MemorySource
	skillsFn     func() []attach.SkillInfo
	mcpFn        func() attach.MCPInfo
	pricingFn    func() attach.PricingInfo
	refreshFn    func(ctx context.Context) (attach.PricingRefreshResponse, error)
	setPricingFn func(req attach.PricingSetRequest) error
	reloadFn     func(ctx context.Context) attach.ReloadResponse
	replanFn     func(ctx context.Context, req attach.ReplanRequest) (attach.ReplanResponse, error)
	promptBroker *attach.PromptBroker
}

// Option configures an Adapter under construction.
type Option func(*Adapter)

// New wraps a with the attach capability surface.
//
// Contract (one rule, not two): the CAPABILITY methods (Attach*) are
// nil-safe — on a nil adapter or nil wrapped agent they degrade to
// the same "capability not registered" / zero-value responses an
// unwired closure produces. The plain attach.Registrant forwards are
// NOT nil-safe: they require a real wrapped agent and misbehave
// otherwise (the identity accessors — AppName, SessionID, EventLog —
// panic; Inject/InjectAs error; RequestWake no-ops), exactly as
// registering a bare *agent.Agent did before the #443 split. Passing a nil agent
// is therefore only useful for constructing a capability-only value
// in tests — never register one (attach.SessionRegistry would reject
// its empty identity anyway).
func New(a *agent.Agent, opts ...Option) *Adapter {
	ad := &Adapter{a: a}
	for _, opt := range opts {
		if opt != nil {
			opt(ad)
		}
	}
	return ad
}

// Agent returns the wrapped *agent.Agent. Hosts that thread the
// adapter through construction seams (session factories, TUI deps)
// use this to recover the agent for Run/Inject/etc without carrying
// both values.
func (ad *Adapter) Agent() *agent.Agent {
	if ad == nil {
		return nil
	}
	return ad.a
}

// WithMemoryProvider wires a snapshot func that returns the agent's
// loaded instruction sources for the remote-attach
// /sessions/<sid>/memory endpoint (backs the remote TUI's /memory
// slash). The caller usually projects an `instruction.Loaded`'s
// Sources list into []attach.MemorySource; nil = endpoint returns
// empty. Formerly agent.WithAttachMemoryProvider.
func WithMemoryProvider(fn func() []attach.MemorySource) Option {
	return func(ad *Adapter) { ad.memoryFn = fn }
}

// WithSkillsProvider wires a snapshot func for
// /sessions/<sid>/skills (backs /skills). Formerly
// agent.WithAttachSkillsProvider.
func WithSkillsProvider(fn func() []attach.SkillInfo) Option {
	return func(ad *Adapter) { ad.skillsFn = fn }
}

// WithMCPProvider wires a snapshot func for /sessions/<sid>/mcp
// (backs /mcp). Formerly agent.WithAttachMCPProvider.
func WithMCPProvider(fn func() attach.MCPInfo) Option {
	return func(ad *Adapter) { ad.mcpFn = fn }
}

// WithPricingProvider wires a snapshot func for
// /sessions/<sid>/pricing (backs the remote TUI's /pricing read).
// Formerly agent.WithAttachPricingProvider.
func WithPricingProvider(fn func() attach.PricingInfo) Option {
	return func(ad *Adapter) { ad.pricingFn = fn }
}

// WithRefreshPricer wires a func that runs on
// POST /sessions/<sid>/pricing/refresh — typically calls into
// `pkg/pricing.Refresh` and rebuilds the catalog. Returns
// the outcome the operator sees. Formerly agent.WithAttachRefreshPricer.
func WithRefreshPricer(fn func(ctx context.Context) (attach.PricingRefreshResponse, error)) Option {
	return func(ad *Adapter) { ad.refreshFn = fn }
}

// WithPricingSetter wires a func that runs on
// POST /sessions/<sid>/pricing/set — writes a manual per-model
// rate and rebuilds the catalog. Formerly agent.WithAttachPricingSetter.
func WithPricingSetter(fn func(req attach.PricingSetRequest) error) Option {
	return func(ad *Adapter) { ad.setPricingFn = fn }
}

// WithReloader wires a func that runs on POST
// /sessions/<sid>/reload. The closure is expected to re-walk
// project deps (instruction sources, skills bundles, MCP config)
// and return per-surface success in the response so the operator
// sees which parts succeeded and which failed. The adapter doesn't
// inspect the response shape; what "reload" means is the host's
// concern. Without this option the operator sees 501 / capability
// not registered. Formerly agent.WithAttachReloader.
func WithReloader(fn func(ctx context.Context) attach.ReloadResponse) Option {
	return func(ad *Adapter) { ad.reloadFn = fn }
}

// WithReplanner wires a func that runs on POST
// /sessions/<sid>/slash/replan and on the in-process TUI's
// /replan slash dispatch. The closure is expected to clear the
// gate's planRecorded flag and archive the latest plan artifact
// (typically `tools.RevokeLatestPlan(gate, agentsDir)`). Without
// this option the slash returns 501 / "capability not registered".
//
// Wiring it under `plan_mode: "advisory"` or `"off"` is harmless —
// there is no gate flag set, so the closure archives whatever
// artifact exists (or reports none) and blocks nothing. The CLI
// wires it unconditionally for that reason; the closure's own
// response text is what distinguishes the modes, since telling an
// operator "the next mutating call will be denied" when advisory
// mode will deny nothing is the same unenforced-claim bug the mode
// exists to avoid. Formerly agent.WithAttachReplanner.
func WithReplanner(fn func(ctx context.Context, req attach.ReplanRequest) (attach.ReplanResponse, error)) Option {
	return func(ad *Adapter) { ad.replanFn = fn }
}

// WithPromptBroker wires the broker that bridges the agent's
// permissions.Gate prompts to remote operators over
// GET /sessions/<sid>/perms/stream and POST /perms/respond. The
// caller is also responsible for wiring this broker into the gate
// (typically via Gate.SetPrompter(broker)) so prompts the gate
// generates actually flow through it. Without this option the
// /perms/stream + /perms/respond routes return 501. Formerly
// agent.WithAttachPromptBroker.
func WithPromptBroker(b *attach.PromptBroker) Option {
	return func(ad *Adapter) { ad.promptBroker = b }
}

// --- attach.Registrant ---------------------------------------------
//
// Plain forwards onto the wrapped agent. The registry keys entries by
// the (AppName, UserID, SessionID) triple; Inject/InjectAs/RequestWake
// are the control surface POST /inject and /wake dispatch through.

// AppName implements attach.Registrant.
func (ad *Adapter) AppName() string { return ad.Agent().AppName() }

// UserID implements attach.Registrant.
func (ad *Adapter) UserID() string { return ad.Agent().UserID() }

// SessionID implements attach.Registrant.
func (ad *Adapter) SessionID() string { return ad.Agent().SessionID() }

// EventLog implements attach.Registrant.
func (ad *Adapter) EventLog() *eventlog.Handle { return ad.Agent().EventLog() }

// Inject implements attach.Registrant.
func (ad *Adapter) Inject(message string) error { return ad.Agent().Inject(message) }

// InjectAs implements attach.Registrant.
func (ad *Adapter) InjectAs(message string, caller auth.Caller) error {
	return ad.Agent().InjectAs(message, caller)
}

// RequestWake implements attach.Registrant.
func (ad *Adapter) RequestWake() { ad.Agent().RequestWake() }

// Description implements attach.DescriptionProvider — the
// /.well-known/agent-card.json handler falls back to this when no
// explicit AgentCardConfig.Description override is supplied.
func (ad *Adapter) Description() string { return ad.Agent().Description() }

// SetOperatorEventEmitter implements attach.OperatorEventTarget.
// The attach broadcaster calls this on first SSE subscriber (wiring
// its Emit method) and again with nil when the last subscriber
// disconnects. Forwards to the agent, which owns the emit machinery
// — the core run loop is the thing that emits status/turn/usage
// events.
func (ad *Adapter) SetOperatorEventEmitter(f func(eventType string, payload any)) {
	ad.Agent().SetOperatorEventEmitter(f)
}

// SetAttachEmitter is the pre-#506 name.
//
// Deprecated: use SetOperatorEventEmitter.
func (ad *Adapter) SetAttachEmitter(f func(eventType string, payload any)) {
	ad.SetOperatorEventEmitter(f)
}

// AttachCapabilities implements attach.CapabilityReporter (#490).
// The adapter satisfies every optional capability interface
// unconditionally (see the conformance block below), so interface
// presence stopped signaling wiredness the moment the adapter became
// the universal registration path — every session advertised
// mcp/perms_stream/specialists and all five slash commands, and
// remote UIs rendered dead affordances backed by empty payloads or
// 501s. This report states what is actually wired:
//
//   - perms_stream ⇔ a prompt broker was supplied (WithPromptBroker);
//   - mcp ⇔ an MCP snapshot fn was supplied (WithMCPProvider);
//   - specialists / "subagent" ⇔ the agent carries a background
//     manager (agent.WithBackgroundManager);
//   - interrupt, guardrails, "btw" ⇔ a live agent is wrapped (all
//     three are core agent capabilities — Interrupt, the guardrail
//     read/reset pair and AskSideQuestion need no extra wiring);
//   - cost_ceiling ⇔ a per-turn or per-session spend cap is armed
//     (#666 — a live-state read, not interface presence);
//   - "compact" ⇔ agent.HasCompactor(); "done" ⇔ HasCheckpointer();
//   - "replan" ⇔ a replanner fn was supplied (WithReplanner).
func (ad *Adapter) AttachCapabilities() attach.CapabilityReport {
	var rep attach.CapabilityReport
	if ad == nil {
		return rep
	}
	rep.PermsStream = ad.promptBroker != nil
	rep.MCP = ad.mcpFn != nil
	if ad.replanFn != nil {
		rep.SlashCommands = append(rep.SlashCommands, "replan")
	}
	a := ad.a
	if a == nil {
		return rep
	}
	rep.Interrupt = true
	rep.Pause = true
	rep.Guardrails = true
	rep.CostCeiling = guardrailCostCeilingArmed(a)
	rep.SlashCommands = append(rep.SlashCommands, "btw")
	if a.BackgroundManager() != nil {
		rep.Specialists = true
		rep.SlashCommands = append(rep.SlashCommands, "subagent")
	}
	if a.HasCompactor() {
		rep.SlashCommands = append(rep.SlashCommands, "compact")
	}
	if a.HasCheckpointer() {
		rep.SlashCommands = append(rep.SlashCommands, "done")
	}
	return rep
}

// Compile-time interface conformance. If one of these ever fails,
// the corresponding attach endpoint silently degrades to
// "capability not registered" in production — keep the full list.
var (
	_ attach.Registrant          = (*Adapter)(nil)
	_ attach.DescriptionProvider = (*Adapter)(nil)
	_ attach.OperatorEventTarget = (*Adapter)(nil)
	_ attach.EmitTarget          = (*Adapter)(nil) //nolint:staticcheck // deprecation-cycle conformance
	_ attach.CapabilityReporter  = (*Adapter)(nil)

	_ attach.ToolsProvider           = (*Adapter)(nil)
	_ attach.AgentsProvider          = (*Adapter)(nil)
	_ attach.SubagentCatalogProvider = (*Adapter)(nil)
	_ attach.StatusProvider          = (*Adapter)(nil)
	_ attach.InterruptProvider       = (*Adapter)(nil)
	_ attach.InterruptSelfAuditor    = (*Adapter)(nil)
	_ attach.UsageProvider           = (*Adapter)(nil)
	_ attach.ContextProvider         = (*Adapter)(nil)
	_ attach.MemoryProvider          = (*Adapter)(nil)
	_ attach.SkillsProvider          = (*Adapter)(nil)
	_ attach.MCPProvider             = (*Adapter)(nil)
	_ attach.PricingProvider         = (*Adapter)(nil)
	_ attach.PermsProvider           = (*Adapter)(nil)
	_ attach.PermsController         = (*Adapter)(nil)
	_ attach.PricingController       = (*Adapter)(nil)
	_ attach.Reloader                = (*Adapter)(nil)
	_ attach.ReplanProvider          = (*Adapter)(nil)
	_ attach.PromptBrokerProvider    = (*Adapter)(nil)
	_ attach.CompactSlashProvider    = (*Adapter)(nil)
	_ attach.CheckpointSlashProvider = (*Adapter)(nil)
	_ attach.SideQueryProvider       = (*Adapter)(nil)
	_ attach.SubagentSpawner         = (*Adapter)(nil)
	_ attach.GuardrailProvider       = (*Adapter)(nil)
	_ attach.GuardrailResetter       = (*Adapter)(nil)
)
