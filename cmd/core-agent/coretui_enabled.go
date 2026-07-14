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

//go:build !no_tui

package main

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"os"
	"sort"
	"strings"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/genai"

	coretui "github.com/go-steer/core-tui/tui"

	"github.com/go-steer/core-agent/pkg/agent"
	"github.com/go-steer/core-agent/pkg/attach"
	"github.com/go-steer/core-agent/pkg/config"
	"github.com/go-steer/core-agent/pkg/instruction"
	"github.com/go-steer/core-agent/pkg/mcp"
	"github.com/go-steer/core-agent/pkg/permissions"
	"github.com/go-steer/core-agent/pkg/skills"
	"github.com/go-steer/core-agent/pkg/usage"
)

// pkgCoreElicitor is the core-tui elicitor handle shared between
// makeMCPElicitor (which constructs it before mcp.Build hands its
// elicit binding to each server) and launchTUIv2 (which threads
// the same handle into tui.Options so the bubble-tea program can
// attach it after construction). Set in makeMCPElicitor; consumed
// in launchTUIv2.
var pkgCoreElicitor coretui.Elicitor

// availableModelIDs is the hardcoded candidate list the /model
// picker surfaces — both Gemini and Anthropic families since
// core-agent supports both providers. Kept here rather than
// promoted to a public function on agent.Agent because it's pure
// UI policy. When the host grows a real model catalog this can
// move to a Provider-driven enumeration.
func availableModelIDs() []string {
	return []string{
		// Gemini 3.x — Google's flagship + supporting variants.
		// -customtools variant is the DefaultConfig pick; prefers
		// registered tools over raw bash. Same price/context as
		// the bare variant; better behavior for coding-assistant.
		"gemini-3.1-pro-preview-customtools",
		"gemini-3.1-pro-preview",
		"gemini-3.5-flash",
		"gemini-3-flash-preview",
		"gemini-3.1-flash-lite-preview",
		"gemini-3.1-flash-image-preview",
		// Gemini 2.5 — kept around for accounts still on prior
		// generation.
		"gemini-2.5-pro",
		"gemini-2.5-flash",
		// Anthropic Claude 4.x — opus / sonnet / haiku across the
		// 200K and 1M context tiers. Resolved through the
		// "anthropic" or "anthropic-vertex" provider in the host
		// config; the adapter routes the swap through the
		// configured provider.
		"claude-opus-4-7",
		"claude-opus-4-7-1m",
		"claude-sonnet-4-6",
		"claude-sonnet-4-6-1m",
		"claude-haiku-4-5",
	}
}

// launchTUIv2 is the core-tui-backed alternative to launchTUI. Same
// inputs, same return contract; differs only in which TUI library
// drives the operator surface. Picked at runtime by CORE_AGENT_TUI=
// core-tui (see main.go). While both code paths coexist (PRs 6-9 of
// docs/core-tui-adapter-design.md), this lets operators A/B the two
// and stick on either until the migration settles.
func launchTUIv2(ctx context.Context, deps tuiDeps) (didRun bool, exitCode int, err error) {
	a, err := agent.New(deps.Model, deps.AgentOpts...)
	if err != nil {
		return false, 0, fmt.Errorf("agent.New: %w", err)
	}

	prompter := coretui.NewPrompter()
	// Wrap so the gate sees a permissions.Prompter (its expected
	// interface) while the TUI drains a coretui.PermissionPrompter.
	deps.Gate.SetPrompter(&gatePrompterBridge{inner: prompter})

	// pkgCoreElicitor should have been set by makeMCPElicitor
	// (called earlier in main.go before mcp.Build). If it's nil
	// someone refactored the wiring — warn loudly and fall through
	// with a fresh elicitor so the TUI still starts; MCP-originated
	// elicits will be declined server-side rather than reach a
	// silent dead channel.
	elicitor := pkgCoreElicitor
	if elicitor == nil {
		fmt.Fprintln(os.Stderr, "core-agent: warning — pkgCoreElicitor was nil at launchTUIv2; MCP elicit requests will be declined (check makeMCPElicitor wiring)")
		elicitor = coretui.NewElicitor()
	}

	wrapped := &coreAgentAdapter{
		inner:    a,
		deps:     deps,
		ctxBuild: ctx,
	}

	// Notifier — host-side channel for framework-initiated chat rows
	// (MCP transport state changes, shutdown notices, etc — see
	// docs/site/content/docs/reference/notifications.md). Opt-in in
	// core-tui v0.8+: when Options.Notifier is non-nil, the TUI
	// drains the channel and renders each Notify(text) call as a
	// distinct RoleNotice row (◇ glyph, muted color). Constructed
	// here so launchTUIv2's local hook sites (MCP startup status
	// below; future producers) can push notices without needing a
	// package-level handle. Safe to call from any goroutine; no-op
	// after the TUI tears down (the Notifier silently drops sends
	// once its channel is closed).
	notifier := coretui.NewNotifier()

	opts := coretui.Options{
		Agent:        wrapped,
		Prompter:     prompter,
		Elicitor:     elicitor,
		Notifier:     notifier,
		UsageTracker: &coreUsageBridge{inner: deps.Tracker},
		AgentsDir:    deps.AgentsDir,
		Memory:       memoryToCoreTui(deps.Memory),
		MCPServers:   mcpServersToCoreTui(deps.MCPServers),
		Skills:       skillsToCoreTui(deps.LoadedSkills),
		PathScope:    pathScopeToCoreTui(deps.Cfg),
		// Branding.AgentIdentity surfaces cfg.Agent.DisplayName in
		// the status-line banner ("core-agent · scion · ◇ model")
		// so operators can tell which agent deployment they're
		// talking to across multiple windows. Matches the
		// internal/tui headerBrand affordance. Empty DisplayName
		// falls back to the bare wordmark per core-tui's dedup.
		Branding: coretui.Branding{
			AgentIdentity: agentDisplayName(deps.Cfg),
		},
		// UI overrides from cfg.UI (config.UIConfig). ForceTheme
		// short-circuits the OSC-11 query when the operator
		// explicitly picks dark/light; InitialThemeName seeds a
		// named theme (gopher, google, ...) previously chosen via
		// the /theme picker. Mouse threads the *bool pointer
		// through (nil = on, false = off) — see core-tui Options
		// docs for the semantics.
		ForceTheme:       uiThemeToCoreTui(deps.Cfg),
		InitialThemeName: uiInitialThemeName(deps.Cfg),
		Mouse:            uiMouseToCoreTui(deps.Cfg),
		PermissionMode: coretui.PermissionModeWiring{
			Initial: translateMode(deps.Gate.Mode()),
			Set: func(m coretui.PermissionMode) error {
				deps.Gate.SetMode(translateModeBack(m))
				return nil
			},
		},
		// AutoContinueFromInbox (core-tui v0.6, issue #9) — full PR-α
		// parity for the ADK-opaque-runner case. On turn-end, core-tui
		// calls InboxDrainer.DrainInbox to pull all queued operator
		// messages, formats them via AutoContinueFormatter (we wire
		// our PR-α framing), and submits the result as a synthetic
		// follow-up turn with a ↻ marker. Replaces the v0.5 stopgap
		// (QueueForNext) that fired one separate turn per queued
		// entry.
		MidTurnInjectionMode: coretui.AutoContinueFromInbox,
		// PR-α's "[Operator notes added during the previous task]"
		// system-prompt wrapper. Tells the model these notes arrived
		// mid-task so it can adapt the current step or capture them
		// via `todo`. agent.FormatAutoContinueInbox is exported for
		// exactly this use case.
		AutoContinueFormatter: agent.FormatAutoContinueInbox,
		// AllowAlways persists the entry to disk when the host's
		// AgentsDir is writable. Path-scope entries land in
		// .agents/config.json's path_scope.allow; everything else
		// becomes a permissions.allow pattern of the form
		// "<tool>:<key>" (matches Policy.Match's grammar) and is
		// added to both the live gate (so subsequent calls this
		// session don't re-prompt) and the on-disk config (so it
		// survives a restart). Without AgentsDir the callback is a
		// no-op and the TUI falls back to allow-session.
		AlwaysAllow: func(req coretui.PermissionRequest) error {
			if deps.AgentsDir == "" {
				return nil
			}
			if req.PersistTool == "path_scope" {
				return appendPathScope(deps.AgentsDir, req.PersistKey)
			}
			if req.PersistTool == "" || req.PersistKey == "" {
				return nil
			}
			pattern := req.PersistTool + ":" + req.PersistKey
			if err := deps.Gate.AddAllowPatterns([]string{pattern}); err != nil {
				return err
			}
			return appendPermissionsAllow(deps.AgentsDir, []string{pattern})
		},
		PersistModelChoice: func(id string) error {
			if deps.AgentsDir == "" {
				return nil
			}
			return persistModelChoice(deps.AgentsDir, id)
		},
		PersistThemeChoice: func(name string) error {
			if deps.AgentsDir == "" {
				return nil
			}
			return persistThemeChoice(deps.AgentsDir, name)
		},
	}

	// Wire the Reloader + PricingController bindings on the
	// wrapped adapter so they read the same callback closures
	// launchTUI uses.
	wrapped.reload = makeReloadCallback(ctx, deps, a)
	wrapped.refreshPricing = makeRefreshPricingCallback(ctx, deps)
	wrapped.setPricing = makeSetPricingCallback(deps)

	// Surface MCP startup failures in chat scroll. Without this,
	// failed MCP servers were only logged to stderr — invisible
	// once the TUI takes over the terminal. The notice is queued
	// via the buffered Notifier channel and drains as soon as the
	// listener spins up inside coretui.Run.
	if msg := mcpStartupFailureNotice(deps.MCPServers); msg != "" {
		notifier.Notify(msg)
	}

	if err := coretui.Run(ctx, opts); err != nil {
		return true, 1, err
	}
	return true, 0, nil
}

// mcpStartupFailureNotice returns the chat-row notice text for any
// MCP servers that failed to start. Returns "" when servers is
// empty or all are healthy (caller skips Notify). Each server's
// error message is included so operators can act without leaving
// the TUI to scan stderr. Pure / side-effect-free for unit testing;
// callers do the Notify themselves.
func mcpStartupFailureNotice(servers []*mcp.Server) string {
	if len(servers) == 0 {
		return ""
	}
	var failed []string
	for _, s := range servers {
		if s == nil || s.Status != mcp.StatusError {
			continue
		}
		msg := s.Name
		if s.Err != nil {
			msg += ": " + s.Err.Error()
		}
		failed = append(failed, msg)
	}
	if len(failed) == 0 {
		return ""
	}
	var b strings.Builder
	if len(failed) == 1 {
		b.WriteString("MCP server failed to start — ")
		b.WriteString(failed[0])
		return b.String()
	}
	fmt.Fprintf(&b, "%d MCP servers failed to start:\n", len(failed))
	for _, f := range failed {
		b.WriteString("  • ")
		b.WriteString(f)
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// Reload satisfies coretui.Reloader. Delegates to the closure
// constructed in launchTUIv2 (which holds the deps + ctx the host
// wired). On success the new agent + memory / MCP / skills are
// surfaced through coretui.ReloadResult so the TUI atomically
// swaps state.
func (a *coreAgentAdapter) Reload(_ context.Context) (coretui.ReloadResult, error) {
	if a.reload == nil {
		return coretui.ReloadResult{}, fmt.Errorf("reload not wired")
	}
	return a.reload()
}

// Refresh satisfies coretui.PricingController.
func (a *coreAgentAdapter) Refresh(ctx context.Context) (string, error) {
	if a.refreshPricing == nil {
		return "", fmt.Errorf("pricing refresh not wired")
	}
	return a.refreshPricing(ctx)
}

// Set satisfies coretui.PricingController.
func (a *coreAgentAdapter) Set(modelID string, in, out float64) (string, error) {
	if a.setPricing == nil {
		return "", fmt.Errorf("pricing set not wired")
	}
	return a.setPricing(modelID, in, out)
}

// makeReloadCallback returns the closure /reload dispatches
// through. Delegates to the agent's AttachReload (the same
// best-effort re-walks the remote POST /reload uses) and projects
// the result into coretui.ReloadResult — fresh display data from
// the live providers + a note line summarizing per-surface
// outcomes. Agent rebuild is out of scope; the system prompt and
// MCP servers retain whatever state they had at startup until a
// daemon restart.
func makeReloadCallback(ctx context.Context, deps tuiDeps, a *agent.Agent) func() (coretui.ReloadResult, error) {
	return func() (coretui.ReloadResult, error) {
		resp := a.AttachReload(ctx)
		freshMem, _ := instruction.Load(deps.ProjectRoot, deps.CoreHome)
		freshSkills, _ := skills.LoadAll(ctx, deps.AgentsDir, deps.CoreHome, deps.Gate)
		freshMCP := deps.MCPServers // not restarted; surfaces the same set as startup
		out := coretui.ReloadResult{
			Memory:     memoryToCoreTui(freshMem),
			Skills:     skillsToCoreTui(freshSkills),
			MCPServers: mcpServersToCoreTui(freshMCP),
			Note:       reloadNote(resp),
		}
		return out, nil
	}
}

// reloadNote turns an attach.ReloadResponse into the multi-line
// system-message confirmation surfaced via coretui.ReloadResult.Note.
// Mirrors the shape internal/coretuiremote/capabilities.go uses for
// the remote TUI's /reload output so both surfaces render identically.
func reloadNote(r attach.ReloadResponse) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Memory: %s\nSkills: %s\nMCP: %s",
		reloadOK(r.Memory), reloadOK(r.Skills), reloadOK(r.MCP))
	if len(r.Errors) > 0 {
		sb.WriteString("\nErrors:\n  - ")
		sb.WriteString(strings.Join(r.Errors, "\n  - "))
	}
	return sb.String()
}

// reloadOK renders a per-surface success bool as the ✓ / ✗ glyph
// the remote TUI's renderReloadResp uses.
func reloadOK(ok bool) string {
	if ok {
		return "✓"
	}
	return "✗"
}

func makeRefreshPricingCallback(_ context.Context, deps tuiDeps) func(context.Context) (string, error) {
	if deps.CoreHome == "" {
		return nil
	}
	return func(ctx context.Context) (string, error) {
		return refreshPricingForTUI(ctx, deps.Cfg, deps.AgentsDir, deps.CoreHome)
	}
}

func makeSetPricingCallback(deps tuiDeps) func(string, float64, float64) (string, error) {
	if deps.CoreHome == "" {
		return nil
	}
	return func(model string, in, out float64) (string, error) {
		return setPricingForTUI(deps.Cfg, deps.AgentsDir, deps.CoreHome, model, in, out)
	}
}

// memoryToCoreTui / mcpServersToCoreTui / skillsToCoreTui /
// pathScopeToCoreTui translate the host's native shapes into the
// neutral coretui Info structs. Each adapter loses some
// host-specific detail (e.g. MCP server credentials) — that's the
// design: the TUI only needs display data.

// memoryToCoreTui / mcpServersToCoreTui / skillsToCoreTui /
// pathScopeToCoreTui are stubbed for the first wiring slice — the
// host types (instruction.Loaded, []*mcp.Server, skills.Skills,
// config.Config) don't expose the field-by-field accessors the
// coretui Info structs want yet. The /memory, /mcp, /skills slash
// commands will render an empty list with a hint until these
// translators are filled in by a follow-up commit (or until the
// host types grow the accessors).

// memoryToCoreTui maps the instruction loader's Sources slice into
// the TUI's MemoryFile rows. Sources carry scope + path + size +
// truncated bit; we propagate all four so /memory can render the
// rich annotation (bytes + truncated marker) that internal/tui
// surfaces.
func memoryToCoreTui(m instruction.Loaded) []coretui.MemoryFile {
	if m.Empty() {
		return nil
	}
	out := make([]coretui.MemoryFile, 0, len(m.Sources))
	for _, s := range m.Sources {
		out = append(out, coretui.MemoryFile{
			Path:      s.Path,
			Bytes:     int64(s.Bytes),
			Truncated: s.Truncated,
		})
	}
	return out
}

// mcpServersToCoreTui maps each *mcp.Server into a flat
// MCPServerInfo. Transport / URL aren't surfaced on mcp.Server
// directly (the connection state lives behind the scenes), so we
// leave Transport empty and rely on Connected (Status == "ready")
// + ToolCount for the /mcp display. ToolInfos (name + description
// per tool) propagate through Tools so /mcp can render the nested
// catalog instead of just a per-server count.
func mcpServersToCoreTui(servers []*mcp.Server) []coretui.MCPServerInfo {
	out := make([]coretui.MCPServerInfo, 0, len(servers))
	for _, s := range servers {
		entry := coretui.MCPServerInfo{
			Name:      s.Name,
			Connected: s.Status == mcp.StatusOK,
			ToolCount: len(s.Tools),
		}
		// Prefer the rich ToolInfos (name+description) when the MCP
		// shim populated them; fall back to raw tool names so the
		// /mcp render still nests something instead of degrading to
		// a bare count.
		switch {
		case len(s.ToolInfos) > 0:
			entry.Tools = make([]coretui.MCPToolInfo, 0, len(s.ToolInfos))
			for _, ti := range s.ToolInfos {
				entry.Tools = append(entry.Tools, coretui.MCPToolInfo{
					Name:        ti.Name,
					Description: ti.Description,
				})
			}
		case len(s.Tools) > 0:
			entry.Tools = make([]coretui.MCPToolInfo, 0, len(s.Tools))
			for _, t := range s.Tools {
				entry.Tools = append(entry.Tools, coretui.MCPToolInfo{Name: t})
			}
		}
		out = append(out, entry)
	}
	return out
}

// skillsToCoreTui maps the skills loader's Infos slice into
// SkillInfo rows. Source stays "local" — skills only load from
// ~/.core-agent/skills today.
func skillsToCoreTui(s skills.Skills) []coretui.SkillInfo {
	if s.Empty() {
		return nil
	}
	out := make([]coretui.SkillInfo, 0, len(s.Infos))
	for _, info := range s.Infos {
		out = append(out, coretui.SkillInfo{
			Name:        info.Name,
			Description: info.Description,
			Source:      "local",
		})
	}
	return out
}

// pathScopeToCoreTui maps Config.PathScope.Allow into the TUI's
// PathScope roots. Empty when the host hasn't configured any
// extras (the TUI then treats every path as in-scope).
func pathScopeToCoreTui(cfg *config.Config) coretui.PathScope {
	if cfg == nil {
		return coretui.PathScope{}
	}
	return coretui.PathScope{Roots: cfg.PathScope.Allow}
}

// coreAgentAdapter wraps *agent.Agent so it satisfies core-tui's
// tui.Agent plus every optional capability interface core-agent can
// support. Built incrementally — capability methods are listed
// below in spec order.
type coreAgentAdapter struct {
	inner    *agent.Agent
	deps     tuiDeps
	ctxBuild context.Context

	// Closures populated by launchTUIv2 so the capability methods
	// below can dispatch to the host's existing /reload + /pricing
	// implementations without each method needing the full deps.
	reload         func() (coretui.ReloadResult, error)
	refreshPricing func(context.Context) (string, error)
	setPricing     func(modelID string, in, out float64) (string, error)
}

// Run satisfies coretui.Agent. Translates each *session.Event from
// the ADK iterator into a coretui.Event, and feeds the host's
// usage.Tracker so /stats + the status sidebar see per-turn data.
// The model name is stamped onto every event so the TUI's per-turn
// footer and live status reflect the current model from the first
// chunk onward.
func (a *coreAgentAdapter) Run(ctx context.Context, prompt string) iter.Seq2[coretui.Event, error] {
	return func(yield func(coretui.Event, error) bool) {
		modelName := a.inner.ModelName()
		// Per-turn cumulative usage tracking. Gemini's UsageMetadata is
		// cumulative across streaming chunks within one model turn — the
		// last chunk carries the final count, earlier chunks carry
		// running totals. Appending on every UsageMetadata-bearing event
		// both inflates the tracker's turn count and double-counts
		// tokens (issue surfaced as "totals exactly 2x last turn").
		// Mirror pkg/runner/headless.go tapUsage and
		// pkg/agent/subtask.go:315-374: overwrite per event, commit
		// once on TurnComplete, reset so multi-turn Run loops account
		// each model turn separately.
		var lastTurnUsage usage.TurnUsage
		for ev, err := range a.inner.Run(ctx, prompt) {
			if err != nil {
				yield(coretui.Event{}, err)
				return
			}
			te := coretui.Event{Partial: ev.Partial, Model: modelName}
			if ev.UsageMetadata != nil {
				lastTurnUsage = usage.TurnUsageFromGenaiMetadata(ev.UsageMetadata)
				te.Usage = &coretui.Usage{
					InputTokens:  lastTurnUsage.InputTokens,
					OutputTokens: lastTurnUsage.OutputTokens,
				}
			}
			if ev.Content != nil {
				for _, p := range ev.Content.Parts {
					if p.FunctionCall != nil {
						te.ToolCalls = append(te.ToolCalls, coretui.ToolCall{
							ID:   p.FunctionCall.ID,
							Name: p.FunctionCall.Name,
							Args: p.FunctionCall.Args,
						})
					}
					if p.FunctionResponse != nil {
						response, errStr := splitFunctionResponse(p.FunctionResponse)
						te.ToolResults = append(te.ToolResults, coretui.ToolResult{
							ID:       p.FunctionResponse.ID,
							Name:     p.FunctionResponse.Name,
							Response: response,
							Error:    errStr,
						})
					}
					if p.Text != "" {
						te.Text += p.Text
					}
				}
			}
			// Commit usage exactly once per completed model turn.
			// TurnComplete fires after the model's tool-call loops
			// settle for that turn; lastTurnUsage captured from the
			// stream's UsageMetadata events is the final per-turn
			// breakdown at this point.
			if ev.TurnComplete && (lastTurnUsage.InputTokens > 0 || lastTurnUsage.OutputTokens > 0) {
				if a.deps.Tracker != nil && a.deps.Cfg != nil {
					pricing := usage.PriceFor(modelName, a.deps.Cfg)
					turn := a.deps.Tracker.AppendUsage(modelName, lastTurnUsage, pricing)
					te.CostUSD = turn.CostUSD
				}
				lastTurnUsage = usage.TurnUsage{}
			}
			if !yield(te, nil) {
				return
			}
		}
	}
}

// Interrupt satisfies coretui.Interruptible.
func (a *coreAgentAdapter) Interrupt() bool { return a.inner.Interrupt() }

// Inject satisfies coretui.InjectableAgent (R-CHAT-11).
func (a *coreAgentAdapter) Inject(message string) error { return a.inner.Inject(message) }

// DrainInbox + PendingInboxCount satisfy coretui.InboxDrainer
// (core-tui v0.6, issue #9). Combined with InjectableAgent (above)
// and MidTurnInjectionMode: AutoContinueFromInbox (in Options),
// core-tui drives the auto-continue loop end-to-end against our
// opaque ADK runner: operator types during streaming → Inject,
// turn ends → DrainInbox returns everything queued → core-tui
// formats it via AutoContinueFormatter and fires a synthetic
// follow-up turn with the ↻ marker.
func (a *coreAgentAdapter) DrainInbox() []string   { return a.inner.DrainInbox() }
func (a *coreAgentAdapter) PendingInboxCount() int { return a.inner.PendingInboxCount() }

// WakeRequested satisfies coretui.WakeRequester (R-WAKE-1).
func (a *coreAgentAdapter) WakeRequested() <-chan struct{} { return a.inner.WakeRequested() }

// AvailableModels satisfies coretui.ModelSwapper. Returns the
// hardcoded Gemini 3.x catalog (see availableModelIDs comment).
func (a *coreAgentAdapter) AvailableModels() []coretui.ModelInfo {
	ids := availableModelIDs()
	out := make([]coretui.ModelInfo, 0, len(ids))
	for _, id := range ids {
		out = append(out, coretui.ModelInfo{ID: id, Display: id})
	}
	return out
}

// SwitchModel satisfies coretui.ModelSwapper. Resolves the new
// model through the host's provider and rebuilds the agent with
// the same agent opts.
//
// Uses context.Background() for the Provider.Model call so an
// in-flight shutdown of the launch context doesn't poison the
// operator-initiated model swap. The new agent gets the same
// ctxBuild as the old one (used only by future SwitchModel calls
// — same lifetime semantics).
//
// Also propagates the reload / pricing closures so /reload + /
// pricing keep working after the swap (without this, every
// /model swap silently downgrades those slash commands to "not
// wired").
func (a *coreAgentAdapter) SwitchModel(modelID string) (coretui.Agent, error) {
	newLLM, err := a.deps.Provider.Model(context.Background(), modelID)
	if err != nil {
		return nil, err
	}
	newAgent, err := agent.New(newLLM, a.deps.AgentOpts...)
	if err != nil {
		return nil, err
	}
	return &coreAgentAdapter{
		inner:          newAgent,
		deps:           a.deps,
		ctxBuild:       a.ctxBuild,
		reload:         a.reload,
		refreshPricing: a.refreshPricing,
		setPricing:     a.setPricing,
	}, nil
}

// SessionApprovals satisfies coretui.PermissionController. Maps the
// gate's ApprovalLog slice 1:1 into the core-tui shape.
func (a *coreAgentAdapter) SessionApprovals() []coretui.ApprovalLog {
	src := a.deps.Gate.Approvals()
	out := make([]coretui.ApprovalLog, 0, len(src))
	for _, ap := range src {
		out = append(out, coretui.ApprovalLog{
			Tool:     ap.Tool,
			Key:      ap.Key,
			Decision: ap.Decision.String(),
		})
	}
	return out
}

// AddAllowPatterns satisfies coretui.PermissionController.
// Updates the live gate AND (when AgentsDir is writable) persists
// the entries to .agents/config.json so they survive restart —
// mirrors launchTUI's existing behavior.
func (a *coreAgentAdapter) AddAllowPatterns(patterns []string) error {
	if err := a.deps.Gate.AddAllowPatterns(patterns); err != nil {
		return err
	}
	if a.deps.AgentsDir == "" {
		return nil
	}
	return appendPermissionsAllow(a.deps.AgentsDir, patterns)
}

// AddDenyPatterns satisfies coretui.PermissionController.
// Symmetric persistence to AddAllowPatterns.
func (a *coreAgentAdapter) AddDenyPatterns(patterns []string) error {
	if err := a.deps.Gate.AddDenyPatterns(patterns); err != nil {
		return err
	}
	if a.deps.AgentsDir == "" {
		return nil
	}
	return appendPermissionsDeny(a.deps.AgentsDir, patterns)
}

// AddBuiltinAllowExtra satisfies coretui.PermissionController.
// Resolves the bundle to its allow entries, extends the live gate,
// and persists the bundle name (not the resolved entries) to the
// config's builtin_allow_extras list — matches launchTUI's pattern
// so the same bundle re-resolves correctly on next startup.
func (a *coreAgentAdapter) AddBuiltinAllowExtra(bundleName string) error {
	entries, ok := permissions.Bundles[bundleName]
	if !ok {
		return fmt.Errorf("unknown bundle %q (want one of %v)", bundleName, permissions.KnownBundles())
	}
	if err := a.deps.Gate.AddAllowPatterns(entries); err != nil {
		return err
	}
	if a.deps.AgentsDir == "" {
		return nil
	}
	return appendBuiltinAllowExtra(a.deps.AgentsDir, bundleName)
}

// Tools satisfies coretui.ToolLister. Routes through the agent's
// AttachTools accessor so the Source field reflects the agent's
// own classification (builtin vs other — MCP/skill differentiation
// lands in attach when the agent grows per-tool provenance). The
// GateState field is computed by AttachTools using the same gate
// the live calls consult, so /tools and the actual approval
// behavior stay consistent.
func (a *coreAgentAdapter) Tools() []coretui.ToolInfo {
	raw := a.inner.AttachTools()
	out := make([]coretui.ToolInfo, 0, len(raw))
	for _, t := range raw {
		out = append(out, coretui.ToolInfo{
			Name:        t.Name,
			Description: t.Description,
			Source:      t.Source,
			GateState:   t.GateState,
		})
	}
	return out
}

// Subagents satisfies coretui.SubagentLister (R-SUB-1). Reads the
// BackgroundAgentManager's live handles and reports each one's
// real status (running / completed / failed / stopped) via
// BackgroundHandle.Status — the manager keeps terminal handles in
// the list until reaped, so the /subagents display reflects
// post-completion state instead of always reading "running."
func (a *coreAgentAdapter) Subagents() []coretui.SubagentInfo {
	mgr := a.inner.BackgroundManager()
	if mgr == nil {
		return nil
	}
	handles := mgr.List()
	out := make([]coretui.SubagentInfo, 0, len(handles))
	for _, h := range handles {
		entry := coretui.SubagentInfo{
			Name:      h.Name,
			Status:    h.Status().String(),
			StartedAt: h.StartedAt,
		}
		if errVal := h.Err(); errVal != nil {
			entry.LastReport = errVal.Error()
		}
		out = append(out, entry)
	}
	return out
}

// Status satisfies coretui.StatusReporter. Wraps the agent's
// AttachStatus snapshot so the status surface reflects deferred /
// waiting / etc. state. Provider is sourced from the host config
// when known (auto-detect via the resolver leaves it as the empty
// string, in which case the chip is suppressed rather than showing
// a bogus tag).
func (a *coreAgentAdapter) Status() coretui.Status {
	s := a.inner.AttachStatus()
	provider := ""
	if a.deps.Cfg != nil {
		provider = a.deps.Cfg.Model.Provider
	}
	return coretui.Status{
		ModelName: a.inner.ModelName(),
		State:     s.State,
		Provider:  provider,
	}
}

// coreUsageBridge wraps *usage.Tracker so it satisfies
// coretui.UsageTracker. Per-turn + session totals + context-window
// fill (R-USE-1 / R-USE-2 / R-USE-3). ContextWindowSize/Used stay
// zero — core-agent's Tracker doesn't surface them today; a follow-
// up exposes ModelConfig context limits.
type coreUsageBridge struct{ inner *coreUsageInner }

// coreUsageInner is just usage.Tracker (avoids importing the
// usage package into the coretui_enabled file twice when other
// adapters grow).
type coreUsageInner = usage.Tracker

func (b *coreUsageBridge) SessionTotals() coretui.Usage {
	t := b.inner.Totals()
	return coretui.Usage{InputTokens: t.InputTokens, OutputTokens: t.OutputTokens}
}
func (b *coreUsageBridge) SessionCostUSD() float64 { return b.inner.Totals().CostUSD }
func (b *coreUsageBridge) LastTurn() (coretui.Usage, float64) {
	turn, ok := b.inner.Last()
	if !ok {
		return coretui.Usage{}, 0
	}
	return coretui.Usage{InputTokens: turn.InputTokens, OutputTokens: turn.OutputTokens}, turn.CostUSD
}

// ContextWindowSize / ContextWindowUsed delegate to usage.Tracker —
// the lookup table + per-turn approximation moved to substrate
// (usage/context_window.go) so agent-level callers (compaction
// trigger, micro-subagents) share the same accessor instead of
// re-deriving from the bridge.
func (b *coreUsageBridge) ContextWindowSize() int { return b.inner.ContextWindowSize() }
func (b *coreUsageBridge) ContextWindowUsed() int { return b.inner.ContextWindowUsed() }

func (b *coreUsageBridge) SessionTurns() int              { return b.inner.Totals().Turns }
func (b *coreUsageBridge) SessionDuration() time.Duration { return b.inner.Duration() }

// SlashCommands satisfies coretui.SlashProvider. Surfaces /btw,
// /subagent, /compact, and /done to the palette + /help. The
// context-management commands (/compact, /done) are gated on
// whether their respective machinery was wired — relaunching with
// --no-compact / --no-checkpoint removes them from /help and the
// palette so operators don't see commands that would only error
// out. Same gate the InvokeSlash handlers below use; the gate is
// surface-level only, the agent's HasCompactor / HasCheckpointer
// is the single source of truth.
func (a *coreAgentAdapter) SlashCommands() []coretui.SlashCommandSpec {
	cmds := []coretui.SlashCommandSpec{
		{
			Name:        "btw",
			Aliases:     []string{"by-the-way"},
			Description: "ask a side question (modal, no tool, doesn't land in history)",
		},
		{
			Name:        "subagent",
			Aliases:     []string{"sub"},
			Description: "spawn a background sub-agent: /subagent <goal> [--name=X --tools=Y --max-turns=N]",
		},
	}
	if a.inner.HasCompactor() {
		cmds = append(cmds, coretui.SlashCommandSpec{
			Name:        "compact",
			Aliases:     []string{"summarize"},
			Description: "summarize the conversation so far and slice prior events from future turns: /compact [focus]",
		})
	}
	if a.inner.HasCheckpointer() {
		cmds = append(cmds, coretui.SlashCommandSpec{
			Name:        "done",
			Aliases:     []string{"checkpoint"},
			Description: "write a task-boundary checkpoint and slice prior events from future turns: /done [note]",
		})
	}
	cmds = append(cmds, coretui.SlashCommandSpec{
		Name:        "context",
		Aliases:     []string{"boundaries"},
		Description: "show context-management activity for this session (compactions, checkpoints, subtask usage)",
	})
	// /replan is registered unconditionally; the InvokeSlash case
	// returns a friendly "plan-first gating isn't enabled" message
	// when WithAttachReplanner wasn't wired (operator's config has
	// require_plan_artifact: false). That's a clearer operator
	// experience than hiding the command and surfacing "unknown
	// command" when they expect it from the recipe docs.
	cmds = append(cmds, coretui.SlashCommandSpec{
		Name:        "replan",
		Description: "revoke the current plan; archive plan-N.md to plan-N-revoked.md; force the agent to record_plan again (plan-first mode only)",
	})
	return cmds
}

// InvokeSlash satisfies coretui.SlashProvider. /btw calls
// AskSideQuestion + surfaces the answer through a SideAnswer modal;
// /subagent parses flags and spawns through BackgroundManager;
// /compact runs Agent.Compact and reports the outcome inline.
func (a *coreAgentAdapter) InvokeSlash(ctx context.Context, name, args string) (coretui.SlashResult, error) {
	switch name {
	case "btw", "by-the-way":
		answer, err := a.inner.AskSideQuestion(ctx, args)
		if err != nil {
			return coretui.SlashResult{
				ModalAnswer: &coretui.SideAnswer{Question: args, Err: err},
			}, nil
		}
		return coretui.SlashResult{
			ModalAnswer: &coretui.SideAnswer{Question: args, Answer: answer},
		}, nil
	case "subagent", "sub":
		// Full flag parsing not yet wired into the core-tui adapter.
		// Library callers can drive subagent spawn directly via
		// BackgroundAgentManager.Spawn while we lift the slash flag
		// parser.
		return coretui.SlashResult{
			SystemMessage: "/subagent requires a flag parser that isn't wired into the core-tui adapter yet.",
		}, nil
	case "done", "checkpoint":
		// Mirrors /compact's structure — Agent.Checkpoint runs the
		// same summarizer machinery; differences are the tag value
		// ("checkpoint" vs "summary") and the prompt's completion-
		// record framing.
		note := strings.TrimSpace(args)
		res, err := a.inner.Checkpoint(ctx, note)
		switch {
		case errors.Is(err, agent.ErrNoCheckpointer):
			return coretui.SlashResult{
				SystemMessage: "/done unavailable: this agent was constructed without WithCheckpointer. Relaunch without --no-checkpoint, or wire agent.WithCheckpointer(agent.NewDefaultCheckpointer()) on the agent.",
			}, nil
		case err != nil:
			return coretui.SlashResult{
				SystemMessage: "/done failed: " + err.Error(),
			}, nil
		case res.Skipped:
			return coretui.SlashResult{
				SystemMessage: "/done: nothing to checkpoint yet (empty session). Run at least one turn first.",
			}, nil
		default:
			noteFragment := ""
			if res.TaskNote != "" {
				noteFragment = " (note: " + res.TaskNote + ")"
			}
			return coretui.SlashResult{
				SystemMessage: fmt.Sprintf(
					"Checkpoint written%s. Summary captured (%d chars, %s). Prior task events will be sliced from the next turn's context; the full audit log is preserved in the session.",
					noteFragment, len(res.SummaryText), res.Duration.Round(0).String()),
			}, nil
		}
	case "context", "boundaries":
		// /context renders Agent.ContextStats — boundary counts,
		// total summary chars, subtask cost rollup. Companion to
		// /stats: /stats shows token totals + cost, /context shows
		// the SHAPE of the conversation (what's been compressed,
		// what came from subtasks).
		return coretui.SlashResult{
			SystemMessage: renderContextStats(a.inner.ContextStats()),
		}, nil
	case "compact", "summarize":
		// NOTE: core-tui v0.5 calls InvokeSlash synchronously from
		// its Update loop (see core-tui#10). The compactor's LLM call
		// will freeze the TUI for its duration — consistent with how
		// /btw behaves today; both get unfrozen when core-tui#10
		// ships an async dispatch path.
		focus := strings.TrimSpace(args)
		res, err := a.inner.Compact(ctx, focus)
		switch {
		case errors.Is(err, agent.ErrNoCompactor):
			return coretui.SlashResult{
				SystemMessage: "/compact unavailable: this agent was constructed without WithCompactor. Relaunch without --no-compact, or wire agent.WithCompactor(agent.NewDefaultCompactor()) on the agent.",
			}, nil
		case err != nil:
			return coretui.SlashResult{
				SystemMessage: "/compact failed: " + err.Error(),
			}, nil
		case res.Skipped:
			return coretui.SlashResult{
				SystemMessage: "/compact: nothing to summarize yet (empty session). Run at least one turn first.",
			}, nil
		default:
			return coretui.SlashResult{
				SystemMessage: fmt.Sprintf(
					"Compacted. Summary written (%d chars, %s). Prior events will be sliced from the next turn's context; the full audit log is preserved in the session.",
					len(res.SummaryText), res.Duration.Round(0).String()),
			}, nil
		}
	case "replan":
		// /replan revokes the latest plan + clears the gate's
		// planRecorded flag, forcing the model to call record_plan
		// again before any mutating tool succeeds. Available only
		// when plan-first gating is wired (the agent's
		// AttachReplan returns 501 / "capability not registered"
		// otherwise).
		resp, err := a.inner.AttachReplan(ctx, attach.ReplanRequest{Reason: strings.TrimSpace(args)})
		if err != nil {
			if errors.Is(err, attach.ErrCapabilityNotRegistered) {
				return coretui.SlashResult{
					SystemMessage: "/replan unavailable: plan-first gating isn't enabled (set permissions.require_plan_artifact: true in .agents/config.json).",
				}, nil
			}
			return coretui.SlashResult{SystemMessage: "/replan failed: " + err.Error()}, nil
		}
		msg := resp.Message
		if msg == "" {
			if resp.PlanWasActive {
				msg = "Plan revoked. The model must call record_plan again before any mutating tool will be allowed."
			} else {
				msg = "/replan: no active plan to revoke."
			}
		}
		return coretui.SlashResult{SystemMessage: msg}, nil
	}
	return coretui.SlashResult{}, fmt.Errorf("unknown slash: %s", name)
}

// InvokeSlashAsync satisfies coretui.AsyncSlashProviderWithPreamble
// (core-tui v0.6.3, issue #16 / our #55). The synchronous
// InvokeSlash above runs inside core-tui's Update loop and freezes
// the TUI for the duration of any slash that does network I/O
// (/btw, /compact, /subagent all take 1-10s on a real model). The
// async variant runs the same work in a goroutine and posts the
// result on a channel core-tui selects on — TUI stays responsive
// throughout.
//
// The preamble (first return value) is appended to chat as a
// RoleSystem row at dispatch time, BEFORE the result channel is
// drained. Empty preamble = no row (back to bare-async behavior).
// The bottom-bar toast (▸ /<name> running…) fires regardless;
// the preamble is the in-chat reinforcement for slashes whose
// wall-clock is long enough that the toast alone is easy to miss
// (~5s+). Per-command wording in preambleFor below.
//
// Buffered channel of size 1 so the goroutine can send-and-exit
// cleanly even if core-tui's receiver hasn't started yet (it does
// start promptly, but defense against future scheduling changes).
func (a *coreAgentAdapter) InvokeSlashAsync(ctx context.Context, name, args string) (string, <-chan coretui.SlashResultOrErr) {
	preamble := preambleFor(name, args)
	ch := make(chan coretui.SlashResultOrErr, 1)
	go func() {
		defer close(ch)
		res, err := a.InvokeSlash(ctx, name, args)
		ch <- coretui.SlashResultOrErr{Res: res, Err: err}
	}()
	return preamble, ch
}

// preambleFor returns the chat-visible "this is running" row for
// async slashes whose wall-clock makes the bottom toast easy to
// miss. Returning "" skips the row entirely — that's the right
// answer for fast slashes (/context, /stats — though those go
// through the sync path) and for slashes we haven't classified
// yet. New long-running slashes should add a case here when they
// land.
//
// Wording rule: present tense ("Capturing…", "Summarizing…"),
// echo the operator's arg when it would be useful to confirm the
// command was parsed correctly (the /done note, the /compact
// focus). The completion message — the SystemMessage in the slash
// handler's return — lands BELOW this row when the work finishes,
// so the two together read as "started X / finished X with Y."
func preambleFor(name, args string) string {
	args = strings.TrimSpace(args)
	switch name {
	case "done", "checkpoint":
		if args == "" {
			return "Capturing checkpoint summary…"
		}
		return "Capturing checkpoint summary (note: " + args + ")…"
	case "compact", "summarize":
		if args == "" {
			return "Summarizing session for context compaction…"
		}
		return "Summarizing session for context compaction (focus: " + args + ")…"
	case "btw", "by-the-way":
		// /btw runs AskSideQuestion — one tool-less LLM call, 1-5s
		// on a real model. The result lands in a modal so the
		// SystemMessage path isn't used; the preamble is the only
		// in-chat feedback the operator gets that the side
		// question is in flight.
		if args == "" {
			return "Asking the model a side question…"
		}
		return "Asking the model: " + args
	default:
		return ""
	}
}

// gatePrompterBridge adapts a core-tui PermissionPrompter so it
// satisfies permissions.Prompter (the gate's expected interface).
// Translates PromptKind / Decision values across the two enum
// vocabularies.
type gatePrompterBridge struct {
	inner coretui.PermissionPrompter
}

// AskApproval implements permissions.Prompter by delegating to the
// core-tui prompter after translating the request shape.
func (g *gatePrompterBridge) AskApproval(ctx context.Context, req permissions.PromptRequest) (permissions.Decision, error) {
	cReq := coretui.PermissionRequest{
		Kind:        translateKind(req.Kind),
		ToolName:    req.ToolName,
		Detail:      req.Detail,
		DetailKind:  translateDetailKind(req.Kind),
		Verb:        req.Verb,
		Source:      req.Source,
		PersistTool: req.PersistTool,
		PersistKey:  req.PersistKey,
	}
	cDec, err := g.inner.AskApproval(ctx, cReq)
	if err != nil {
		return permissions.DecisionDeny, err
	}
	return translateDecision(cDec), nil
}

// translateKind maps permissions.PromptKind → coretui.PermissionKind.
// Four-to-four mapping with PathScope folded into Edit (both are
// file-access events from the operator's perspective).
func translateKind(k permissions.PromptKind) coretui.PermissionKind {
	switch k {
	case permissions.PromptKindBash:
		return coretui.PermissionKindBash
	case permissions.PromptKindFileWrite, permissions.PromptKindPathScope:
		return coretui.PermissionKindEdit
	default:
		return coretui.PermissionKindOther
	}
}

// translateDetailKind picks the right Glamour code-fence language
// tag for the modal body based on the request Kind. The host has
// already rendered req.Detail; this is just the styling hint.
func translateDetailKind(k permissions.PromptKind) coretui.DetailKind {
	switch k {
	case permissions.PromptKindBash:
		return coretui.DetailShell
	case permissions.PromptKindFileWrite:
		return coretui.DetailDiff
	default:
		return coretui.DetailPlain
	}
}

// translateDecision maps coretui.PermissionDecision → permissions.Decision.
// One-to-one because the spec for both adopted the same R-PERM-2
// vocabulary.
func translateDecision(d coretui.PermissionDecision) permissions.Decision {
	switch d {
	case coretui.DecisionAllowOnce:
		return permissions.DecisionAllowOnce
	case coretui.DecisionAllowSession:
		return permissions.DecisionAllowSession
	case coretui.DecisionAllowSessionVerb:
		return permissions.DecisionAllowSessionVerb
	case coretui.DecisionAllowSessionTool:
		return permissions.DecisionAllowSessionTool
	case coretui.DecisionAllowAlways:
		return permissions.DecisionAllowAlways
	default:
		return permissions.DecisionDeny
	}
}

// translateMode / translateModeBack bridge the gate's Mode values
// and core-tui's PermissionMode enum. Both sides now carry the
// same four modes (default / acceptEdits / plan / bypass) since
// the gate grew ModePlan + ModeAcceptEdits — see
// permissions/gate.go.
//
// permissions.ModeAllow (config-side "auto-allow if in allowlist
// else fail") has no chip equivalent and is intentionally collapsed
// to default-on-the-chip; cycling out of default lands on
// acceptEdits / plan / bypass rather than re-entering ModeAllow.
// Operators who want ModeAllow set it via .agents/config.json.
func translateMode(m permissions.Mode) coretui.PermissionMode {
	switch m {
	case permissions.ModeAcceptEdits:
		return coretui.PermissionModeAcceptEdits
	case permissions.ModePlan:
		return coretui.PermissionModePlan
	case permissions.ModeYolo:
		return coretui.PermissionModeBypass
	default:
		return coretui.PermissionModeDefault
	}
}

func translateModeBack(m coretui.PermissionMode) permissions.Mode {
	switch m {
	case coretui.PermissionModeAcceptEdits:
		return permissions.ModeAcceptEdits
	case coretui.PermissionModePlan:
		return permissions.ModePlan
	case coretui.PermissionModeBypass:
		return permissions.ModeYolo
	default:
		return permissions.ModeAsk
	}
}

// coreMCPElicitor wraps a coretui.Elicitor as an mcp.ElicitorFn so
// the MCP servers can route their elicit requests through the
// shared core-tui modal. Translates between the MCP SDK's JSON-
// schema-shaped request and core-tui's flat field list.
type coreMCPElicitor struct {
	inner coretui.Elicitor
}

// elicit implements mcp.ElicitorFn.
func (c *coreMCPElicitor) elicit(ctx context.Context, serverName string, req *mcpsdk.ElicitRequest) (*mcpsdk.ElicitResult, error) {
	params := req.Params
	if params == nil {
		return &mcpsdk.ElicitResult{Action: "decline"}, nil
	}
	cReq, ok := translateMCPSchemaToElicitRequest(params)
	if !ok {
		return &mcpsdk.ElicitResult{Action: "decline"}, nil
	}
	result, err := c.inner.Elicit(ctx, serverName, cReq)
	if err != nil {
		return &mcpsdk.ElicitResult{Action: "cancel"}, err
	}
	out := &mcpsdk.ElicitResult{
		Action: translateElicitAction(result.Action),
	}
	if result.Action == coretui.ElicitActionSubmit {
		out.Content = result.Values
	}
	return out, nil
}

// translateMCPSchemaToElicitRequest flattens the SDK's JSON schema
// into core-tui's []ElicitField. Supports primitive types
// (string/number/integer/boolean) + enums; nested objects are
// declined (R-ELIC-3 — the second-return-false path drops the
// request server-side instead of opening a broken modal).
func translateMCPSchemaToElicitRequest(p *mcpsdk.ElicitParams) (coretui.ElicitRequest, bool) {
	out := coretui.ElicitRequest{
		Mode:        coretui.ElicitFormMode,
		Title:       p.Message,
		Description: p.Message,
	}
	schema, ok := p.RequestedSchema.(map[string]any)
	if !ok {
		return out, false
	}
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		return out, false
	}
	requiredSet := map[string]bool{}
	// MCP SDK may unmarshal `required` as either []any (when the
	// schema came in as raw JSON) or []string (when it was decoded
	// through a typed struct). Accept both so a SDK-shape change
	// can't silently drop the required-field annotations.
	switch req := schema["required"].(type) {
	case []any:
		for _, r := range req {
			if s, ok := r.(string); ok {
				requiredSet[s] = true
			}
		}
	case []string:
		for _, s := range req {
			requiredSet[s] = true
		}
	}
	// Sort the property names so the rendered form has a stable
	// field order across calls — iterating `props` directly would
	// shuffle the modal between runs of the same elicit (Go map
	// iteration is randomized).
	names := make([]string, 0, len(props))
	for name := range props {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		propMap, ok := props[name].(map[string]any)
		if !ok {
			return out, false
		}
		typeName, _ := propMap["type"].(string)
		field := coretui.ElicitField{
			Name:        name,
			Description: stringOf(propMap, "description"),
			Required:    requiredSet[name],
		}
		switch typeName {
		case "string":
			if enum, ok := propMap["enum"].([]any); ok {
				field.Type = coretui.ElicitFieldEnum
				for _, e := range enum {
					if s, ok := e.(string); ok {
						field.EnumChoices = append(field.EnumChoices, s)
					}
				}
			} else {
				field.Type = coretui.ElicitFieldString
			}
		case "number":
			field.Type = coretui.ElicitFieldNumber
		case "integer":
			field.Type = coretui.ElicitFieldInteger
		case "boolean":
			field.Type = coretui.ElicitFieldBoolean
		default:
			return out, false // unsupported type
		}
		if d, ok := propMap["default"]; ok {
			field.Default = d
		}
		out.Fields = append(out.Fields, field)
	}
	return out, true
}

// stringOf is a tiny helper for pulling optional string fields out
// of a schema map — returns "" when missing or non-string.
func stringOf(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return v
}

// translateElicitAction maps core-tui's ElicitAction back to the
// MCP SDK's stringy action vocabulary.
func translateElicitAction(a coretui.ElicitAction) string {
	switch a {
	case coretui.ElicitActionSubmit:
		return "accept"
	case coretui.ElicitActionDecline:
		return "decline"
	default:
		return "cancel"
	}
}

// splitFunctionResponse separates the structured success response
// from a possible error string carried in a genai.FunctionResponse.
// The ADK convention (per google.golang.org/adk base_flow.go) is to
// place tool errors under the reserved "error" key inside Response;
// successful calls put whatever shape the tool returned into the
// same map. Splitting at the adapter boundary keeps the TUI's
// rendering path uniform — it only ever needs to check Error.
//
// Returns the response map unchanged plus the extracted error
// string when "error" is present and string-typed. Nil resp /
// nil Response yields (nil, "").
func splitFunctionResponse(resp *genai.FunctionResponse) (map[string]any, string) {
	if resp == nil || resp.Response == nil {
		return nil, ""
	}
	if v, ok := resp.Response["error"]; ok {
		switch e := v.(type) {
		case string:
			return resp.Response, e
		case error:
			return resp.Response, e.Error()
		}
	}
	return resp.Response, ""
}

// uiThemeToCoreTui maps cfg.UI.Theme to coretui.Options.ForceTheme.
// ForceTheme is the OSC-11 override knob — it only accepts the
// reserved buckets ("", "dark", "light"). Named themes (gopher,
// google, ...) flow through uiInitialThemeName → InitialThemeName
// instead, NOT through this field.
func uiThemeToCoreTui(cfg *config.Config) string {
	if cfg == nil {
		return coretui.ThemeAuto
	}
	switch cfg.UI.Theme {
	case config.ThemeDark:
		return coretui.ThemeDark
	case config.ThemeLight:
		return coretui.ThemeLight
	default:
		return coretui.ThemeAuto
	}
}

// uiInitialThemeName returns the named-theme seed for
// coretui.Options.InitialThemeName so a previously-persisted
// /theme pick survives across launches. Empty for the reserved
// buckets ("", "auto", "dark", "light") — those go through
// ForceTheme. Empty for nil cfg.
func uiInitialThemeName(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}
	switch cfg.UI.Theme {
	case "", config.ThemeAuto, config.ThemeDark, config.ThemeLight:
		return ""
	default:
		return cfg.UI.Theme
	}
}

// uiMouseToCoreTui surfaces cfg.UI.Mouse (a *bool — unset means
// "default on") as the *bool coretui.Options expects. Returning
// nil when the operator hasn't set the field preserves core-tui's
// default-enabled behavior; an explicit false threads the
// opt-out through to View()'s MouseMode selection.
func uiMouseToCoreTui(cfg *config.Config) *bool {
	if cfg == nil || cfg.UI.Mouse == nil {
		return nil
	}
	v := *cfg.UI.Mouse
	return &v
}

// agentDisplayName returns cfg.Agent.DisplayName as the operator
// label for the status-line banner. Nil cfg / empty DisplayName
// yields "" — core-tui's Branding.AgentIdentity treats empty as
// "skip the identity segment" so the banner stays as the bare
// wordmark + model. Defensive against nil cfg so headless test
// paths don't panic.
func agentDisplayName(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}
	return cfg.Agent.DisplayName
}
