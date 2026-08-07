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

package compose

import (
	"context"
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/google/uuid"
	adkmodel "google.golang.org/adk/model"
	"google.golang.org/adk/session"
	adktool "google.golang.org/adk/tool"

	"github.com/go-steer/core-agent/v2/pkg/agent"
	"github.com/go-steer/core-agent/v2/pkg/attach"
	"github.com/go-steer/core-agent/v2/pkg/attachadapter"
	"github.com/go-steer/core-agent/v2/pkg/auth"
	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/eventlog"
	"github.com/go-steer/core-agent/v2/pkg/instruction"
	"github.com/go-steer/core-agent/v2/pkg/mcp"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
	"github.com/go-steer/core-agent/v2/pkg/runner"
	"github.com/go-steer/core-agent/v2/pkg/skills"
	"github.com/go-steer/core-agent/v2/pkg/usage"
)

// BuildMultiSessionAuthn translates the operator's
// attach.multi_session config block into the pkg/auth Authenticator
// that the attach listener consults per-request. Returns:
//
//   - authn: the resolved Authenticator (or nil for single-user mode)
//   - fallback: the Caller stamped on requests that don't authenticate
//     (used by the host's caller middleware as the no-cred default)
//   - err: a fatal startup error if the config is internally
//     inconsistent OR a referenced file can't be loaded
//
// In single-user mode (multi_session.enabled = false), returns
// (nil, zero-Caller, nil) — the attach server defaults its own
// AnonymousAuth and the wiring is a no-op.
func BuildMultiSessionAuthn(cfg config.MultiSessionConfig) (auth.Authenticator, auth.Caller, error) {
	// Default Caller comes from the config knob (resolved to "anon"
	// when unset to match the design doc's documented default). Used
	// for the legacy / single-user path AND as the AllowAnonymous
	// fallback when multi-session is on.
	defaultCaller := auth.Caller{Identity: cfg.DefaultIdentity}
	if defaultCaller.Identity == "" {
		defaultCaller = auth.Anonymous
	}

	if !cfg.Enabled {
		return nil, defaultCaller, nil
	}

	switch cfg.Auth.Kind {
	case "", config.MultiSessionAuthKindBearerTable:
		users, err := auth.LoadUsersFile(cfg.Auth.TableFile)
		if err != nil {
			return nil, defaultCaller, fmt.Errorf("load users file: %w", err)
		}
		authn := auth.NewBearerTokenAuth(users.Users, cfg.AdminIdentities, cfg.ProxyIdentities)
		return authn, defaultCaller, nil
	default:
		// Validation in config.Validate() should catch this earlier;
		// guard anyway so a corrupted call path produces a clear error
		// instead of a silent fallback.
		return nil, defaultCaller, fmt.Errorf("unsupported auth.kind %q (only %q is shipped in this version)", cfg.Auth.Kind, config.MultiSessionAuthKindBearerTable)
	}
}

// SessionFactoryDeps bundles the daemon-wide configuration the
// per-session SessionFactory closure needs to capture. Constructed
// once at daemon startup; the resulting factory builds fresh
// *agent.Agent values for each POST /sessions request.
//
// Substrate + config-only operator features are wired per-session:
// tools, eventlog, per-session sub-gate, per-caller instruction
// overlay, per-session prompter, plus Compactor / Checkpointer /
// CostCeiling (these are pure config, so per-session reconstruction
// is trivial and the alternative was /compact and /done erroring on
// every session-created agent). Features that need per-session
// scoping decisions the daemon doesn't yet make (BackgroundManager
// sharing, Watchdog alert-sink routing, agentic tool wrappers, MCP
// custom auth) remain deferred — sessions created via POST /sessions
// see the substrate without them.
type SessionFactoryDeps struct {
	// DaemonCtx is the daemon's lifetime context — every per-session
	// wake loop spawned by the factory uses it as the cancellation
	// signal so SIGTERM / Ctrl-C ends them cleanly. Required.
	DaemonCtx context.Context

	Model          adkmodel.LLM
	Template       *permissions.Gate
	BuiltinTools   []adktool.Tool
	Toolsets       []adktool.Toolset
	EventlogHandle *eventlog.Handle
	PricingRate    usage.Pricing
	ProjectRoot    string
	UserRoot       string
	HomeAgentsDir  string
	AgentsDir      string
	UsersDir       string
	// ContentRoots are operator-declared external directories trusted as
	// additional instruction/skill scopes (config content_roots +
	// --agents-content-dir), already resolved to absolute paths. Threaded
	// into every per-session and attach-provider loader call so multi-
	// session sessions see the same external content the daemon-wide
	// startup load did. Empty = no external scopes (default). See
	// docs/external-content-root-design.md.
	ContentRoots []string
	// EnvInterp is the ${env:VAR} interpolator wired from the daemon's
	// env manifest (see pkg/agentenv, #322). May be nil when the
	// bundle doesn't ship an env.yaml / env.json — loaders treat nil
	// as "no interpolation."
	EnvInterp func(string) string
	Registry  *attach.SessionRegistry
	// Cfg + MCPServers feed the read-only AttachXProvider closures
	// (memory / skills / mcp / pricing) so the per-session /memory,
	// /skills, /mcp, /pricing slash commands return real data
	// instead of "no servers configured" for on-demand sessions.
	Cfg        *config.Config
	MCPServers []*mcp.Server
	// ACLStore is the persistent ACL backing for session-resume
	// (Phase 2 of docs/session-resume-design.md). The factory
	// writes through it via RegisterOwned at session-creation time
	// (handled by the registry, not directly); the resumer reads
	// from it on Lookup miss to reconstruct evicted sessions. Nil
	// disables resume — the registry behaves as pre-v2.5.
	ACLStore attach.SessionACLStore
	// AutoContinueEnabled + AutoContinueFreshness switch on opt-in
	// continuation of restart-interrupted turns on the lazy-resume
	// path (#539, docs/auto-continue-design.md). Freshness 0 means
	// "no window" (always continue); the daemon parses and validates
	// the config strings so these are ready-to-use values here.
	// Requires EventlogHandle — with no durable eventlog there is
	// nothing to detect against.
	AutoContinueEnabled   bool
	AutoContinueFreshness time.Duration
	// AutoContinueDeferInject suppresses ReproduceAgent's inline
	// continuation inject on the resumed path. Set per-call by the
	// resumer from a context marker (withDeferAutoContinueInject) when
	// the boot scan drives the resume: the scan then owns the
	// classify+inject step itself so it can observe the outcome
	// (injected vs. run-lock-held-elsewhere) and keep the boot-log
	// attempt accounting honest (#575). The lazy-touch resume path
	// leaves this false and injects inline as before.
	AutoContinueDeferInject bool
	// NoCompact / NoCheckpoint mirror the --no-compact /
	// --no-checkpoint CLI flags. When false (the default),
	// ReproduceAgent wires WithCompactor / WithCheckpointer so
	// /compact and /done work against session-created agents; when
	// true, the corresponding option is skipped so the disable flag
	// applies uniformly to the main agent AND every session-created
	// agent under it.
	NoCompact    bool
	NoCheckpoint bool

	// Customize, when non-nil, runs at the top of every session
	// construction — POST /sessions creations AND lazy resumes — with
	// the caller the session belongs to (#505). It receives a
	// SessionCustomization pre-filled from the daemon-wide deps and
	// may vary the per-tenant knobs: the model, the tools, and the
	// toolsets (skills ride as toolsets — load a per-tenant skills
	// tree into a toolset here). Per-tenant PERMISSIONS need no hook:
	// every session already runs a sub-gate derived from Template,
	// and per-caller instructions layer via UsersDir.
	//
	// An error aborts the construction (the client sees the 500 with
	// this error's text). The hook must be safe for concurrent calls.
	//
	// On RESUME, Identity is the only Caller field populated (it is
	// materialized from the persisted ACL owner — Labels and Admin
	// are not stored). Key customization decisions on Identity
	// alone, or re-derive tenant metadata from your own store;
	// a hook keyed on Labels would silently build a different
	// session shape on lazy resume than it did at creation.
	Customize SessionCustomizer
}

// SessionCustomizer is SessionFactoryDeps.Customize — the per-caller
// hook that varies session construction without forking
// ReproduceAgent. ctx is the daemon lifetime context (construction
// may outlive the triggering request; a resume's work certainly
// does).
type SessionCustomizer func(ctx context.Context, caller auth.Caller, c *SessionCustomization) error

// SessionCustomization is the per-caller slice of the session recipe
// a SessionCustomizer may change. Every field arrives pre-filled with
// the daemon-wide default (the slices are copies — append extends,
// reassign replaces, and neither corrupts the shared deps).
type SessionCustomization struct {
	// Model drives the session's turns. Defaults to deps.Model.
	// When changed, per-turn cost attribution follows the new
	// model's name and its pricing is re-resolved from the layered
	// catalog (deps.Cfg overrides, pricing files, builtin).
	Model adkmodel.LLM
	// Tools is the flat tool list (defaults to deps.BuiltinTools).
	Tools []adktool.Tool
	// Toolsets is the toolset list (defaults to deps.Toolsets) —
	// MCP servers and skills bundles live here.
	Toolsets []adktool.Toolset
}

// newSessionTracker constructs the *usage.Tracker each on-demand
// session-created agent gets. Var (not const-func) so
// multi_session_test.go can wrap it to capture the per-session
// instances and assert they're distinct — the regression gate for
// issue #275. Never nil; callers assume a working tracker.
var newSessionTracker = usage.NewTracker

// BuildSessionFactory returns an attach.SessionFactory closure that
// constructs a fresh *agent.Agent per POST /sessions request. The
// closure captures the deps by value (slices + pointers); per-call
// it generates a unique sessionID, derives a per-session sub-gate +
// prompter, loads the per-caller instruction overlay, and assembles
// a minimal-but-functional agent.
//
// The handler is responsible for calling RegisterOwned on the
// returned Registrant with the originating Caller.Identity — this
// factory deliberately does NOT register with the session registry
// itself, because that would self-register via the legacy Register()
// (no Owner stamp), losing the ACL ownership that's the whole point.
func BuildSessionFactory(deps SessionFactoryDeps) attach.SessionFactory {
	return func(_ context.Context, caller auth.Caller) (attach.Registrant, context.CancelFunc, error) {
		return ReproduceAgent(deps, caller, newSessionID(), "created")
	}
}

// ReproduceAgent constructs an *agent.Agent under (caller, sid) using
// the shared SessionFactoryDeps shape. Used both by the on-demand
// session factory (sid is freshly minted) and by the resumer (sid
// comes from the persisted ACL row — ADK's session.Service reattaches
// the prior conversation history when the same triple opens the
// eventlog).
//
// origin is "created" (factory path) or "resumed" (resumer path) and
// flows into the operator-visible stderr log line so the daemon log
// distinguishes the two.
//
// Returns the constructed agent wrapped in its attach adapter (the
// registry entry the handler registers via RegisterOwnedWithCancel /
// registerResumed — recover the agent with Adapter.Agent()) + a
// CancelFunc that stops the per-session wake-loop goroutine. The
// caller hands the cancel to the registry so eviction terminates the
// loop cleanly instead of leaking it past the session's lifetime.
// The wake loop's ctx is derived from deps.DaemonCtx — either source
// of cancellation (daemon shutdown or per-session evict) closes
// ctx.Done and the loop exits.
func ReproduceAgent(deps SessionFactoryDeps, caller auth.Caller, sid string, origin string) (*attachadapter.Adapter, context.CancelFunc, error) {
	// Per-caller customization first (#505): fail-fast before any
	// per-session resource (broker, gate, instruction I/O) exists,
	// so an aborting hook needs no cleanup. The slices are cloned so
	// a hook that appends can never write through into the shared
	// deps backing arrays and leak one tenant's tools to another.
	cust := SessionCustomization{
		Model:    deps.Model,
		Tools:    slices.Clone(deps.BuiltinTools),
		Toolsets: slices.Clone(deps.Toolsets),
	}
	if deps.Customize != nil {
		if err := deps.Customize(deps.DaemonCtx, caller, &cust); err != nil {
			return nil, nil, fmt.Errorf("customize session for %q: %w", caller.Identity, err)
		}
		if cust.Model == nil {
			// Defensive: a hook that nils the model gets the default
			// back rather than a construction panic downstream.
			cust.Model = deps.Model
		}
	}
	// Cost attribution follows the effective model. When the hook
	// swapped it, the daemon-wide PricingRate (resolved for
	// deps.Model at startup) would misprice every turn — re-resolve
	// from the same layered catalog the startup path used.
	pricingRate := deps.PricingRate
	if cust.Model.Name() != deps.Model.Name() {
		// Name comparison, not interface identity: pricing depends
		// only on the model name, and comparing interface values
		// panics on non-comparable host-supplied LLM types.
		pricingRate = usage.PriceFor(cust.Model.Name(), deps.Cfg)
	}

	// Per-session HTTP prompt broker. Each new session gets its
	// own broker so prompts route to the right per-session
	// /perms/stream subscriber.
	broker := attach.NewPromptBroker()

	// Per-session sub-gate isolates sessionAllow / planRecorded
	// / mode / approvals from sibling sessions. Shares Policy /
	// PathScope / requirePlanArtifact via the template (the
	// documented limitation in docs/multi-session-design.md).
	sessionGate := deps.Template.DeriveForSession(sid, broker)

	// Per-caller instruction overlay: the operator's
	// <UsersDir>/<caller.Identity>/.agents/ tree layered on
	// top of project + user scopes. Empty UsersDir or unknown
	// caller falls through to the daemon-wide instruction stack.
	instr, err := instruction.LoadForSession(deps.ProjectRoot, deps.UserRoot, caller.Identity, deps.UsersDir,
		instruction.WithHomeAgentsRoot(deps.HomeAgentsDir),
		instruction.WithContentRoots(deps.ContentRoots),
		instruction.WithInterpolator(deps.EnvInterp))
	if err != nil {
		broker.Close()
		return nil, nil, fmt.Errorf("load per-caller instructions: %w", err)
	}

	opts := []agent.Option{
		agent.WithTools(cust.Tools),
		agent.WithToolsets(cust.Toolsets),
		agent.WithUserInstruction(instr.Instruction), // layer 4 since #459 — memory AFTER the core, the intended precedence flip
		agent.WithGate(sessionGate),
		agent.WithSession(caller.Identity, sid),
	}
	// Per-session adapter options: the prompt broker plus the
	// AttachXProvider closures that power the operator-state slashes
	// (/memory, /skills, /mcp, /pricing). Without the providers the
	// per-session slashes report "no <thing> configured" even though
	// the underlying state is wired correctly into the agent
	// (toolsets include MCP, instructions are loaded, etc.) — the
	// slashes just have nothing to look at.
	adOpts := append(attachProviderOpts(deps, sessionGate, cust.Model.Name(), pricingRate),
		attachadapter.WithPromptBroker(broker))
	if deps.EventlogHandle != nil {
		opts = append(opts, agent.WithEventLog(deps.EventlogHandle))
	}
	// Fresh tracker per session (issue #275). The Tracker's own godoc
	// says it "accumulates per-turn usage for one session"; sharing
	// one across every session-created agent made AttachUsage,
	// broadcaster's usage-update snapshot, and cost_ceiling all
	// return the union of every session's turns. Indirected through
	// a package var so multi_session_test.go can observe / capture
	// the per-session instances.
	sessionTracker := newSessionTracker()

	// On resume (session evicted from memory, brought back by
	// SessionResumer), replay persisted eventlog into the fresh
	// tracker so /stats + AttachUsage return real historical totals
	// instead of "0 in / 0 out / $0.00". Fixes the aggregate-totals
	// regression that shipped alongside #275 — per-turn footers kept
	// working because they replay from live SSE events, but the
	// aggregate started at zero on every resume. Only runs when
	// origin=="resumed"; freshly-created sessions have no history
	// to rebuild.
	if origin == "resumed" && deps.EventlogHandle != nil && deps.EventlogHandle.Stream != nil {
		events := deps.EventlogHandle.Stream.Since(
			deps.DaemonCtx, 0,
			eventlog.ForSession("core-agent", caller.Identity, sid),
		)
		// Adapter: eventlog yields (Entry, error); tracker rebuild
		// wants (*session.Event, error). Peel the event field.
		eventsSeq := func(yield func(*session.Event, error) bool) {
			for entry, err := range events {
				if !yield(entry.Event, err) {
					return
				}
			}
		}
		if err := usage.RebuildTrackerFromEvents(
			deps.DaemonCtx, sessionTracker, eventsSeq,
			cust.Model.Name(),
			func(model string) usage.Pricing { return pricingRate },
		); err != nil {
			// Non-fatal — the session still functions, just with
			// zero baseline aggregate. Log so operators can spot
			// silent rebuild failures.
			fmt.Fprintf(os.Stderr, "core-agent: rebuild tracker for resumed session %s: %v\n", sid, err)
		}
	}

	opts = append(opts, agent.WithUsageTracker(sessionTracker))
	// Context-window compaction (Mechanism A). Default-on unless
	// --no-compact was passed. Without this wiring, /compact against
	// session-created agents errored with agent.ErrNoCompactor even
	// though the CLI defaults advertise the feature.
	if !deps.NoCompact {
		var compactionCfg config.CompactionConfig
		if deps.Cfg != nil {
			compactionCfg = deps.Cfg.Compaction
		}
		opts = append(opts, agent.WithCompactor(BuildCompactor(compactionCfg)))
	}
	// Task-boundary checkpoints (Mechanism C). Default-on unless
	// --no-checkpoint was passed. Without this wiring, /done and
	// the model-facing mark_task_done tool were unavailable on
	// session-created agents.
	if !deps.NoCheckpoint {
		opts = append(opts, agent.WithCheckpointer(agent.NewDefaultCheckpointer()))
	}
	// Cost-ceiling kill switch (#145). The zero-value CostCeiling is
	// a no-op, so this is safe to always append; enforcement runs
	// only when either bound in Cfg.Agent is > 0. Config-driven
	// only — the operator's --max-turn-cost-usd /
	// --max-session-cost-usd CLI flags are already merged into
	// Cfg.Agent before deps is built.
	if deps.Cfg != nil {
		ceiling := agent.CostCeiling{}
		if deps.Cfg.Agent.MaxTurnCostUSD != nil {
			ceiling.MaxTurnUSD = *deps.Cfg.Agent.MaxTurnCostUSD
		}
		if deps.Cfg.Agent.MaxSessionCostUSD != nil {
			ceiling.MaxSessionUSD = *deps.Cfg.Agent.MaxSessionCostUSD
		}
		if ceiling.MaxTurnUSD > 0 || ceiling.MaxSessionUSD > 0 {
			opts = append(opts, agent.WithCostCeiling(ceiling))
		}
	}

	ag, err := agent.New(cust.Model, opts...)
	if err != nil {
		broker.Close()
		return nil, nil, fmt.Errorf("agent.New: %w", err)
	}
	ad := attachadapter.New(ag, adOpts...)
	// Operator-visible log line that mirrors the startup-time
	// "--no-repl: attach-only mode, session <sid>" message so the
	// daemon stderr reflects every long-lived agent it's hosting.
	fmt.Fprintf(os.Stderr, "core-agent: session %s (owner=%s, id=%s)\n", origin, caller.Identity, sid)
	// Derive the wake-loop ctx from DaemonCtx so both daemon
	// shutdown AND per-session eviction terminate the loop
	// through the same <-ctx.Done() branch. cancelOnEvict is
	// handed to the registry, which invokes it when the eviction
	// sweep removes this session.
	//
	// runner.WakeLoop drains the inbox into a real turn on every
	// WakeRequested and commits per-turn usage into the session
	// tracker via usage.TurnTap; its default OnTurnError writes
	// "core-agent: session <sid> turn: ..." to stderr and keeps the
	// loop alive — one bad turn must not kill the session.
	// Auto-continue (#539 PR 1): if this resume finds a fresh
	// interrupted turn in the committed tail, queue a synthesized
	// continuation before the wake loop starts — the injected note
	// latches the wake signal, so the loop's first drain runs it.
	// Created sessions have no history to be interrupted; only the
	// resumed path checks. AutoContinueDeferInject hands the
	// classify+inject to the boot scan (see the field's doc) so it can
	// account the outcome; the lazy-touch path leaves it false.
	if origin == "resumed" && deps.AutoContinueEnabled && !deps.AutoContinueDeferInject &&
		deps.EventlogHandle != nil && deps.EventlogHandle.Service != nil {
		maybeAutoContinue(deps, caller, sid, ag)
	}
	loopCtx, cancelOnEvict := context.WithCancel(deps.DaemonCtx)
	go runner.WakeLoop(loopCtx, ag, runner.WakeLoopOptions{
		Tracker: sessionTracker,
		Model:   cust.Model.Name(),
		Pricing: pricingRate,
	})
	return ad, cancelOnEvict, nil
}

// BuildSessionResumer wires the attach server's SessionResumer.
// Reads the persisted ACL row from deps.ACLStore; materializes the
// original Caller from row.Owner; reconstructs the agent via
// ReproduceAgent with the EXPLICIT sessionID so ADK's
// session.Service reattaches the prior conversation history from
// the eventlog.
//
// Returns nil when deps.ACLStore is nil — session-resume is opt-in.
// The attach server's Options.Resumer being nil leaves the legacy
// "Lookup miss = 404" behavior in place, no behavior change for
// pre-v2.5 deployments.
//
// Resumer failures propagate to the registry; the registry's
// resumeAndRegister handles ErrSessionACLNotFound → ErrSessionNotFound
// translation. Other errors surface as 500 with the underlying
// cause (per docs/session-resume-design.md OQ #2).
func BuildSessionResumer(deps SessionFactoryDeps) attach.SessionResumer {
	if deps.ACLStore == nil {
		return nil
	}
	return &sessionResumer{deps: deps}
}

// sessionResumer implements attach.SessionResumer using the shared
// SessionFactoryDeps. The same store the factory writes through (via
// the registry's RegisterOwned path) is the store this reads from on
// miss — guaranteed-consistent because they share the eventlog DB
// connection.
type sessionResumer struct {
	deps SessionFactoryDeps
}

func (r *sessionResumer) Resume(ctx context.Context, app, sid string) (attach.Registrant, auth.SessionACL, context.CancelFunc, error) {
	row, err := r.deps.ACLStore.FindByAppSID(ctx, app, sid)
	if err != nil {
		// ErrSessionACLNotFound propagates as-is; the registry
		// translates it to ErrSessionNotFound. Any other store
		// error propagates to the 500 surface.
		return nil, auth.SessionACL{}, nil, err
	}
	caller := auth.Caller{Identity: row.Owner}
	// A boot-scan-driven resume marks the ctx so we defer the inline
	// continuation inject to the scan (which then accounts its outcome).
	// Mutate a per-call copy, never the shared r.deps.
	deps := r.deps
	if deferAutoContinueInject(ctx) {
		deps.AutoContinueDeferInject = true
	}
	ag, cancelOnEvict, err := ReproduceAgent(deps, caller, sid, "resumed")
	if err != nil {
		return nil, auth.SessionACL{}, nil, fmt.Errorf("resume: %w", err)
	}
	return ag, row.ACL(), cancelOnEvict, nil
}

// attachProviderOpts builds the daemon-wide read-only AttachXProvider
// closures (memory / skills / pricing snapshot / MCP) for on-demand
// sessions. Mirrors the startup-agent's closures in cmd/core-agent's
// main.go so the per-session /memory, /skills, /pricing, /mcp slashes
// return real data instead of empty placeholders.
//
// Mutating closures (RefreshPricer, PricingSetter, Reloader,
// Replanner) are deferred — they need careful per-session threading
// (Replanner uses the per-session gate; PricingSetter writes the
// user's config file daemon-wide; Reloader's MCP-restart story is
// itself unresolved upstream). Sessions can observe state via the
// providers; mutation slashes 501 until wired in a follow-up.
//
// sessionGate is the derived sub-gate; threaded here so the
// soon-to-arrive Replanner closure picks it up without expanding the
// deps signature when it lands.
func attachProviderOpts(deps SessionFactoryDeps, _ *permissions.Gate, modelName string, rate usage.Pricing) []attachadapter.Option {
	var opts []attachadapter.Option

	if deps.ProjectRoot != "" || deps.UserRoot != "" {
		opts = append(opts, attachadapter.WithMemoryProvider(func() []attach.MemorySource {
			fresh, _ := instruction.Load(deps.ProjectRoot, deps.UserRoot,
				instruction.WithHomeAgentsRoot(deps.HomeAgentsDir),
				instruction.WithContentRoots(deps.ContentRoots),
				instruction.WithInterpolator(deps.EnvInterp))
			out := make([]attach.MemorySource, 0, len(fresh.Sources))
			for _, s := range fresh.Sources {
				out = append(out, attach.MemorySource{Scope: s.Scope, Path: s.Path, Size: s.Bytes})
			}
			return out
		}))
	}

	if deps.AgentsDir != "" || deps.UserRoot != "" {
		opts = append(opts, attachadapter.WithSkillsProvider(func() []attach.SkillInfo {
			fresh, err := skills.LoadAll(deps.DaemonCtx, deps.AgentsDir, deps.UserRoot, deps.Template,
				skills.WithHomeAgentsSkillsDir(deps.HomeAgentsDir),
				skills.WithContentRoots(deps.ContentRoots),
				skills.WithInterpolator(deps.EnvInterp))
			if err != nil {
				return nil
			}
			out := make([]attach.SkillInfo, 0, len(fresh.Infos))
			for _, s := range fresh.Infos {
				out = append(out, attach.SkillInfo{Name: s.Name, Description: s.Description})
			}
			return out
		}))
	}

	if deps.Cfg != nil {
		// modelName/rate are the SESSION's effective values — after a
		// Customize model swap they differ from deps.Cfg.Model.Name /
		// deps.PricingRate, and /pricing must agree with what /status
		// reports and what the tracker bills (#505).
		opts = append(opts, attachadapter.WithPricingProvider(func() attach.PricingInfo {
			info := attach.PricingInfo{CurrentModel: modelName}
			if !rate.IsZero() {
				info.Current = &attach.ModelPricing{
					InputUSDPerMTok:  rate.InputPerMTok,
					OutputUSDPerMTok: rate.OutputPerMTok,
				}
			}
			return info
		}))
	}

	if len(deps.MCPServers) > 0 {
		opts = append(opts, attachadapter.WithMCPProvider(func() attach.MCPInfo {
			servers := make([]attach.MCPServerInfo, 0, len(deps.MCPServers))
			for _, s := range deps.MCPServers {
				tools := make([]attach.MCPToolInfo, 0, len(s.ToolInfos))
				for _, t := range s.ToolInfos {
					tools = append(tools, attach.MCPToolInfo{Name: t.Name, Description: t.Description})
				}
				// Mirror the startup-agent's status mapping
				// (pkg/mcp internal "ok"/"error" → wire-format
				// "running"/"failed") so the remote TUI's
				// Connected detection works the same way.
				status := "running"
				if s.Status == mcp.StatusError {
					status = "failed"
				}
				servers = append(servers, attach.MCPServerInfo{
					Name:      s.Name,
					Status:    status,
					Transport: "",
					Tools:     tools,
				})
			}
			return attach.MCPInfo{Servers: servers}
		}))
	}

	return opts
}

// newSessionID returns a unique session identifier suitable for the
// (app, user, sid) triple. UUID v7 is sortable by creation time so
// "newest session" queries are free; V4 fallback only fires on a
// genuinely broken OS clock.
func newSessionID() string {
	id, err := uuid.NewV7()
	if err != nil {
		// Fallback to V4 — V7 only fails when the OS clock is
		// unrecoverably broken. A V4 still uniquely identifies the
		// session; we just lose the time-sortable property.
		return uuid.NewString()
	}
	return id.String()
}
