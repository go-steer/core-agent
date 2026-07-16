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

package main

import (
	"context"
	"fmt"
	"os"

	"github.com/google/uuid"
	adkmodel "google.golang.org/adk/model"
	adktool "google.golang.org/adk/tool"

	"github.com/go-steer/core-agent/pkg/agent"
	"github.com/go-steer/core-agent/pkg/attach"
	"github.com/go-steer/core-agent/pkg/auth"
	"github.com/go-steer/core-agent/pkg/config"
	"github.com/go-steer/core-agent/pkg/eventlog"
	"github.com/go-steer/core-agent/pkg/instruction"
	"github.com/go-steer/core-agent/pkg/mcp"
	"github.com/go-steer/core-agent/pkg/permissions"
	"github.com/go-steer/core-agent/pkg/skills"
	"github.com/go-steer/core-agent/pkg/usage"
)

// buildMultiSessionAuthn translates the operator's
// attach.multi_session config block into the pkg/auth Authenticator
// that the attach listener consults per-request. Returns:
//
//   - authn: the resolved Authenticator (or nil for single-user mode)
//   - fallback: the Caller stamped on requests that don't authenticate
//     (used by callerMiddleware as the no-cred default)
//   - err: a fatal startup error if the config is internally
//     inconsistent OR a referenced file can't be loaded
//
// In single-user mode (multi_session.enabled = false), returns
// (nil, zero-Caller, nil) — the attach server defaults its own
// AnonymousAuth and the wiring is a no-op.
func buildMultiSessionAuthn(cfg config.MultiSessionConfig) (auth.Authenticator, auth.Caller, error) {
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

// sessionFactoryDeps bundles the daemon-wide configuration the
// per-session SessionFactory closure needs to capture. Constructed
// once at daemon startup; the resulting factory builds fresh
// *agent.Agent values for each POST /sessions request.
//
// v0 spike: substrate-essential options only (tools, eventlog,
// per-session sub-gate, per-caller instruction overlay, per-session
// prompter). Operator features that flow into the startup-time agent
// (BackgroundManager, Compactor, Watchdog, Checkpointer, CostCeiling,
// agentic tools, ask_user, MCP custom auth, etc.) are intentionally
// NOT wired into on-demand sessions in this iteration — every one of
// them needs scope-aware re-instantiation per session, which is a
// follow-up. Sessions created via POST /sessions are functional but
// see the substrate as it is, without the operator-feature
// extensions. Document the gap in the recipe.
type sessionFactoryDeps struct {
	// daemonCtx is the daemon's lifetime context — every per-session
	// wake loop spawned by the factory uses it as the cancellation
	// signal so SIGTERM / Ctrl-C ends them cleanly. Required.
	daemonCtx context.Context

	model          adkmodel.LLM
	template       *permissions.Gate
	builtinTools   []adktool.Tool
	toolsets       []adktool.Toolset
	eventlogHandle *eventlog.Handle
	pricingRate    usage.Pricing
	projectRoot    string
	userRoot       string
	agentsDir      string
	usersDir       string
	registry       *attach.SessionRegistry
	// cfg + mcpServers feed the read-only AttachXProvider closures
	// (memory / skills / mcp / pricing) so the per-session /memory,
	// /skills, /mcp, /pricing slash commands return real data
	// instead of "no servers configured" for on-demand sessions.
	cfg        *config.Config
	mcpServers []*mcp.Server
	// aclStore is the persistent ACL backing for session-resume
	// (Phase 2 of docs/session-resume-design.md). The factory
	// writes through it via RegisterOwned at session-creation time
	// (handled by the registry, not directly); the resumer reads
	// from it on Lookup miss to reconstruct evicted sessions. Nil
	// disables resume — the registry behaves as pre-v2.5.
	aclStore attach.SessionACLStore
}

// newSessionTracker constructs the *usage.Tracker each on-demand
// session-created agent gets. Var (not const-func) so
// multi_session_test.go can wrap it to capture the per-session
// instances and assert they're distinct — the regression gate for
// issue #275. Never nil; callers assume a working tracker.
var newSessionTracker = usage.NewTracker

// buildSessionFactory returns an attach.SessionFactory closure that
// constructs a fresh *agent.Agent per POST /sessions request. The
// closure captures the deps by value (slices + pointers); per-call
// it generates a unique sessionID, derives a per-session sub-gate +
// prompter, loads the per-caller instruction overlay, and assembles
// a minimal-but-functional agent.
//
// The handler is responsible for calling RegisterOwned on the
// returned Registrant with the originating Caller.Identity — this
// factory deliberately does NOT take a WithSessionRegistry option,
// because that would self-register via the legacy Register() (no
// Owner stamp), losing the ACL ownership that's the whole point.
func buildSessionFactory(deps sessionFactoryDeps) attach.SessionFactory {
	return func(_ context.Context, caller auth.Caller) (attach.Registrant, context.CancelFunc, error) {
		return reproduceAgent(deps, caller, newSessionID(), "created")
	}
}

// reproduceAgent constructs an *agent.Agent under (caller, sid) using
// the shared sessionFactoryDeps shape. Used both by the on-demand
// session factory (sid is freshly minted) and by the resumer (sid
// comes from the persisted ACL row — ADK's session.Service reattaches
// the prior conversation history when the same triple opens the
// eventlog).
//
// origin is "created" (factory path) or "resumed" (resumer path) and
// flows into the operator-visible stderr log line so the daemon log
// distinguishes the two.
//
// Returns the constructed agent + a CancelFunc that stops the
// per-session wake-loop goroutine. The caller hands the cancel to
// the registry (via RegisterOwnedWithCancel / registerResumed) so
// eviction terminates the loop cleanly instead of leaking it past
// the session's lifetime. The wake loop's ctx is derived from
// deps.daemonCtx — either source of cancellation (daemon shutdown
// or per-session evict) closes ctx.Done and the loop exits.
func reproduceAgent(deps sessionFactoryDeps, caller auth.Caller, sid string, origin string) (*agent.Agent, context.CancelFunc, error) {
	// Per-session HTTP prompt broker. Each new session gets its
	// own broker so prompts route to the right per-session
	// /perms/stream subscriber.
	broker := attach.NewPromptBroker()

	// Per-session sub-gate isolates sessionAllow / planRecorded
	// / mode / approvals from sibling sessions. Shares Policy /
	// PathScope / requirePlanArtifact via the template (the
	// documented limitation in docs/multi-session-design.md).
	sessionGate := deps.template.DeriveForSession(sid, broker)

	// Per-caller instruction overlay: the operator's
	// <usersDir>/<caller.Identity>/.agents/ tree layered on
	// top of project + user scopes. Empty usersDir or unknown
	// caller falls through to the daemon-wide instruction stack.
	instr, err := instruction.LoadForSession(deps.projectRoot, deps.userRoot, caller.Identity, deps.usersDir)
	if err != nil {
		broker.Close()
		return nil, nil, fmt.Errorf("load per-caller instructions: %w", err)
	}

	opts := []agent.Option{
		agent.WithTools(deps.builtinTools),
		agent.WithToolsets(deps.toolsets),
		agent.WithSystemInstructionPrefix(instr.Instruction),
		agent.WithGate(sessionGate),
		agent.WithSession(caller.Identity, sid),
		agent.WithAttachPromptBroker(broker),
	}
	if deps.eventlogHandle != nil {
		opts = append(opts, agent.WithEventLog(deps.eventlogHandle))
	}
	// Fresh tracker per session (issue #275). The Tracker's own godoc
	// says it "accumulates per-turn usage for one session"; sharing
	// one across every session-created agent made AttachUsage,
	// broadcaster's usage-update snapshot, and cost_ceiling all
	// return the union of every session's turns. Indirected through
	// a package var so multi_session_test.go can observe / capture
	// the per-session instances.
	sessionTracker := newSessionTracker()
	opts = append(opts, agent.WithUsageTracker(sessionTracker))
	// AttachXProvider closures power the operator-state slashes
	// (/memory, /skills, /mcp, /pricing). Without these the
	// per-session slashes report "no <thing> configured" even
	// though the underlying state is wired correctly into the
	// agent (toolsets include MCP, instructions are loaded,
	// etc.) — the slashes just have nothing to look at.
	opts = append(opts, attachProviderOpts(deps, sessionGate)...)

	ag, err := agent.New(deps.model, opts...)
	if err != nil {
		broker.Close()
		return nil, nil, fmt.Errorf("agent.New: %w", err)
	}
	// Operator-visible log line that mirrors the startup-time
	// "--no-repl: attach-only mode, session <sid>" message so the
	// daemon stderr reflects every long-lived agent it's hosting.
	fmt.Fprintf(os.Stderr, "core-agent: session %s (owner=%s, id=%s)\n", origin, caller.Identity, sid)
	// Derive the wake-loop ctx from daemonCtx so both daemon
	// shutdown AND per-session eviction terminate the loop
	// through the same <-ctx.Done() branch. cancelOnEvict is
	// handed to the registry, which invokes it when the eviction
	// sweep removes this session.
	loopCtx, cancelOnEvict := context.WithCancel(deps.daemonCtx)
	go runSessionWakeLoop(loopCtx, ag, sessionTracker, deps.model.Name(), deps.pricingRate)
	return ag, cancelOnEvict, nil
}

// buildSessionResumer wires the cmd-level SessionResumer for the
// attach server. Reads the persisted ACL row from deps.aclStore;
// materializes the original Caller from row.Owner; reconstructs the
// agent via reproduceAgent with the EXPLICIT sessionID so ADK's
// session.Service reattaches the prior conversation history from
// the eventlog.
//
// Returns nil when deps.aclStore is nil — session-resume is opt-in.
// The attach server's Options.Resumer being nil leaves the legacy
// "Lookup miss = 404" behavior in place, no behavior change for
// pre-v2.5 deployments.
//
// Resumer failures propagate to the registry; the registry's
// resumeAndRegister handles ErrSessionACLNotFound → ErrSessionNotFound
// translation. Other errors surface as 500 with the underlying
// cause (per docs/session-resume-design.md OQ #2).
func buildSessionResumer(deps sessionFactoryDeps) attach.SessionResumer {
	if deps.aclStore == nil {
		return nil
	}
	return &sessionResumer{deps: deps}
}

// sessionResumer implements attach.SessionResumer using the cmd-level
// factory deps. The same store the factory writes through (via the
// registry's RegisterOwned path) is the store this reads from on
// miss — guaranteed-consistent because they share the eventlog DB
// connection.
type sessionResumer struct {
	deps sessionFactoryDeps
}

func (r *sessionResumer) Resume(ctx context.Context, app, sid string) (attach.Registrant, auth.SessionACL, context.CancelFunc, error) {
	row, err := r.deps.aclStore.FindByAppSID(ctx, app, sid)
	if err != nil {
		// ErrSessionACLNotFound propagates as-is; the registry
		// translates it to ErrSessionNotFound. Any other store
		// error propagates to the 500 surface.
		return nil, auth.SessionACL{}, nil, err
	}
	caller := auth.Caller{Identity: row.Owner}
	ag, cancelOnEvict, err := reproduceAgent(r.deps, caller, sid, "resumed")
	if err != nil {
		return nil, auth.SessionACL{}, nil, fmt.Errorf("resume: %w", err)
	}
	return ag, row.ACL(), cancelOnEvict, nil
}

// runSessionWakeLoop is the per-session driver that the SessionFactory
// spawns for every on-demand session. Mirrors the inline --no-repl
// wake loop in main.go: select on context cancel + WakeRequested,
// then call ag.Run("") so the inbox drains into a real turn.
//
// Per-turn usage tap: every event's UsageMetadata is captured so
// tracker.Append fires once per turn — matches what the startup
// agent's --no-repl path does so /stats + status-banner cumulative
// totals reflect on-demand-session activity too.
//
// trackerName is the model name string that gets passed to
// tracker.Append; pricingRate may be zero (skipped Append in that
// case — same as the startup path).
func runSessionWakeLoop(ctx context.Context, ag *agent.Agent, tracker *usage.Tracker, trackerName string, pricingRate usage.Pricing) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-ag.WakeRequested():
			var lastUsage usage.TurnUsage
			for ev, runErr := range ag.Run(ctx, "") {
				if ev != nil && ev.UsageMetadata != nil {
					lastUsage = usage.TurnUsageFromGenaiMetadata(ev.UsageMetadata)
				}
				if runErr != nil {
					// Surface to stderr but keep the loop alive —
					// one bad turn shouldn't kill the session.
					fmt.Fprintf(os.Stderr, "core-agent: session %s turn: %v\n", ag.SessionID(), runErr)
				}
			}
			if tracker != nil && (lastUsage.InputTokens > 0 || lastUsage.OutputTokens > 0) {
				tracker.AppendUsage(trackerName, lastUsage, pricingRate)
			}
		}
	}
}

// attachProviderOpts builds the daemon-wide read-only AttachXProvider
// closures (memory / skills / pricing snapshot / MCP) for on-demand
// sessions. Mirrors the startup-agent's closures in main.go so the
// per-session /memory, /skills, /pricing, /mcp slashes return real
// data instead of empty placeholders.
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
func attachProviderOpts(deps sessionFactoryDeps, _ *permissions.Gate) []agent.Option {
	var opts []agent.Option

	if deps.projectRoot != "" || deps.userRoot != "" {
		opts = append(opts, agent.WithAttachMemoryProvider(func() []attach.MemorySource {
			fresh, _ := instruction.Load(deps.projectRoot, deps.userRoot)
			out := make([]attach.MemorySource, 0, len(fresh.Sources))
			for _, s := range fresh.Sources {
				out = append(out, attach.MemorySource{Scope: s.Scope, Path: s.Path, Size: s.Bytes})
			}
			return out
		}))
	}

	if deps.agentsDir != "" || deps.userRoot != "" {
		opts = append(opts, agent.WithAttachSkillsProvider(func() []attach.SkillInfo {
			fresh, err := skills.LoadAll(deps.daemonCtx, deps.agentsDir, deps.userRoot, deps.template)
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

	if deps.cfg != nil {
		opts = append(opts, agent.WithAttachPricingProvider(func() attach.PricingInfo {
			info := attach.PricingInfo{CurrentModel: deps.cfg.Model.Name}
			if !deps.pricingRate.IsZero() {
				info.Current = &attach.ModelPricing{
					InputUSDPerMTok:  deps.pricingRate.InputPerMTok,
					OutputUSDPerMTok: deps.pricingRate.OutputPerMTok,
				}
			}
			return info
		}))
	}

	if len(deps.mcpServers) > 0 {
		opts = append(opts, agent.WithAttachMCPProvider(func() attach.MCPInfo {
			servers := make([]attach.MCPServerInfo, 0, len(deps.mcpServers))
			for _, s := range deps.mcpServers {
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
