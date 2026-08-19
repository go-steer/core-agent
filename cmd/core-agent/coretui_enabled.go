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
	"strconv"
	"strings"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/genai"

	coretui "github.com/go-steer/core-tui/tui"

	"github.com/go-steer/core-agent/v2/internal/coretuievent"
	"github.com/go-steer/core-agent/v2/internal/subagentlog"
	"github.com/go-steer/core-agent/v2/pkg/agent"
	"github.com/go-steer/core-agent/v2/pkg/agent/background"
	"github.com/go-steer/core-agent/v2/pkg/attach"
	"github.com/go-steer/core-agent/v2/pkg/attachadapter"
	"github.com/go-steer/core-agent/v2/pkg/compose"
	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/instruction"
	"github.com/go-steer/core-agent/v2/pkg/mcp"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
	"github.com/go-steer/core-agent/v2/pkg/pricing"
	"github.com/go-steer/core-agent/v2/pkg/skills"
	"github.com/go-steer/core-agent/v2/pkg/usage"
)

// pkgCoreElicitor is the core-tui elicitor handle shared between
// makeMCPElicitor (which constructs it before mcp.Build hands its
// elicit binding to each server) and launchTUIv2 (which threads
// the same handle into tui.Options so the bubble-tea program can
// attach it after construction). Set in makeMCPElicitor; consumed
// in launchTUIv2.
var pkgCoreElicitor coretui.Elicitor

// pickerHead names the model IDs pinned to the top of the /model
// picker, in this order, ahead of the derived remainder.
//
// ORDER IS BEHAVIOR, not presentation: core-tui's picker opens on
// index 0 (newModelPickerDialog returns {idx: 0} — it does NOT
// preselect the active model), and `enter` both switches and persists
// via PersistModelChoice. So entry 0 is what a reflexive /model+enter
// durably lands on, and it therefore tracks the DefaultConfig pick
// (#571) rather than merely being "the newest thing we know about".
// gemini-3.7-flash took both slots when it was promoted off the
// deferral list; gemini-3.6-flash stays pinned one line down because
// it was the previous default and is the obvious fallback if 3.7
// misbehaves — reachable without leaving the dialog.
//
// A head entry that falls out of the pricing catalog is dropped rather
// than surfaced unpriced; the list then shifts and
// TestAvailableModelIDs_HeadOrder fails, so the slot gets re-pinned
// deliberately rather than silently inherited by whatever sorts first.
var pickerHead = []string{
	"gemini-3.7-flash",
	"gemini-3.6-flash",
}

// pickerMinMajor is the per-family generation cutoff for the picker.
// Older models stay fully usable via --model / config.model.name and
// stay priced in pkg/pricing — they just don't clutter a 19-line
// dialog. Gemini < 3 can't mix server-side built-ins with function
// declarations (see pkg/models/gemini's builtinsCompatible), so on the
// 2.5 line the research task class literally cannot search; Claude < 4
// is the pre-thinking-default generation.
var pickerMinMajor = map[string]int{
	"gemini": 3,
	"claude": 4,
}

// availableModelIDs is the candidate list the /model picker surfaces.
//
// DERIVED, not hand-listed: the set is pricing.Builtin() — itself
// generated from LiteLLM's catalog by dev/regen-builtin-pricing —
// narrowed by the rules below. The hand-maintained list this replaced
// had drifted into surfacing six models with no rate in the catalog,
// which showed as `$—` in the TUI and, worse, made max_turn_cost_usd /
// max_session_cost_usd silently inert for anyone who picked one: an
// unpriced turn costs 0, and a budget cap on 0 never trips.
//
// Deriving it means the picker cannot outrun the pricing table again,
// and a Monday pricing regen that adds a model makes it selectable
// with no second edit. Exclusions:
//
//   - Below the per-family generation cutoff (see pickerMinMajor).
//   - Date-pinned aliases (claude-opus-4-7-20260416) — same model as
//     the bare id, twice the picker rows.
//   - The Mythos-class tier's duplicate ids (claude-mythos-5,
//     claude-mythos-preview): LiteLLM publishes that tier three times
//     at identical rates. claude-fable-5 is the one we surface.
//
// Note this drops the "-1m" long-context variants the old list carried:
// Opus 4.6+ and Sonnet 4.6+ ship a 1M window with no suffix (see
// usage.ContextWindowSizeFor), so those rows offered nothing the bare
// id doesn't. Still typeable for the earlier 4.x models that need them.
//
// Kept here rather than promoted to a public function on agent.Agent
// because the narrowing is UI policy; the catalog underneath is not.
func availableModelIDs() []string {
	priced := pricing.Builtin()

	head := make([]string, 0, len(pickerHead))
	pinned := make(map[string]bool, len(pickerHead))
	for _, id := range pickerHead {
		if _, ok := priced[id]; !ok {
			continue
		}
		head = append(head, id)
		pinned[id] = true
	}

	var gemini, claude []string
	for id := range priced {
		if pinned[id] || !pickerEligible(id) {
			continue
		}
		if strings.HasPrefix(id, "gemini-") {
			gemini = append(gemini, id)
		} else {
			claude = append(claude, id)
		}
	}
	// Alphabetical within family, deliberately not "newest first": a
	// lexical sort is only a recency proxy until the first two-digit
	// minor ships (gemini-3.10 sorts below gemini-3.9), and a picker
	// whose order silently inverts is worse than one that never
	// claimed to be ordered by recency. The recommended models are
	// pinned above; below that it's a lookup list.
	sort.Strings(gemini)
	sort.Strings(claude)

	out := make([]string, 0, len(head)+len(gemini)+len(claude))
	out = append(out, head...)
	out = append(out, gemini...)
	return append(out, claude...)
}

// pickerEligible reports whether a priced model ID belongs in the
// picker's derived remainder. See availableModelIDs for the rationale
// behind each rule.
func pickerEligible(id string) bool {
	if strings.Contains(id, "claude-mythos") {
		return false
	}
	fields := strings.Split(id, "-")
	major := -1
	for _, f := range fields {
		// A date pin is the only 8-digit field these ids carry.
		if len(f) == 8 && isAllDigits(f) {
			return false
		}
		if major < 0 && len(f) > 0 && f[0] >= '0' && f[0] <= '9' {
			// "3.6" → 3, "4" → 4. ParseInt on the pre-dot prefix
			// rather than the whole field so minors don't matter.
			n, err := strconv.Atoi(strings.SplitN(f, ".", 2)[0])
			if err != nil {
				return false
			}
			major = n
		}
	}
	cutoff, ok := pickerMinMajor[fields[0]]
	if !ok {
		// A family the cutoff table doesn't know about. Surface it —
		// it is priced, so it is at least budget-safe, and a silent
		// drop would hide a newly supported provider.
		return true
	}
	return major >= cutoff
}

func isAllDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return len(s) > 0
}

// launchTUIv2 is the core-tui-backed alternative to launchTUI. Same
// inputs, same return contract; differs only in which TUI library
// drives the operator surface. Picked at runtime by CORE_AGENT_TUI=
// core-tui (see main.go). While both code paths coexist (PRs 6-9 of
// docs/core-tui-adapter-design.md), this lets operators A/B the two
// and stick on either until the migration settles.
func launchTUIv2(ctx context.Context, deps tuiDeps) (didRun bool, exitCode int, err error) {
	a, ad, err := buildAttachedAgent(deps.Model, deps.AgentOpts, deps.AdapterOpts, deps.AttachReg)
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
		attachAd: ad,
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
		// -i / --interactive-prompt (issue #291). core-tui v0.13+
		// consumes this via a one-shot msg from Init() that flows
		// through the same submitTurn path an operator-typed
		// submission uses.
		InitialPrompt: deps.InitialPrompt,
		Memory:        memoryToCoreTui(deps.Memory),
		MCPServers:    mcpServersToCoreTui(deps.MCPServers),
		Skills:        skillsToCoreTui(deps.LoadedSkills),
		PathScope:     pathScopeToCoreTui(deps.Cfg),
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
		// AllowAlways must be non-nil so the modal offers the
		// "always" choice (a nil callback makes core-tui downgrade
		// to allow-session), but since #386 PR 3 the persistence
		// itself is the GATE's job: DecisionAllowAlways flows back
		// through gatePrompterBridge and the gate installs the
		// in-memory pattern AND persists the fully-expanded grant
		// via the permissions.ConfigGrantStore wired in main. Doing it
		// here too would double-write — and with a DIVERGENT shape
		// (this callback only saw the raw PersistKey, not the
		// subtree-expanded pattern the gate installs).
		AlwaysAllow: func(coretui.PermissionRequest) error { return nil },
		PersistModelChoice: func(id string) error {
			if deps.AgentsDir == "" {
				return nil
			}
			return config.PersistModelChoice(deps.AgentsDir, id)
		},
		PersistThemeChoice: func(name string) error {
			if deps.AgentsDir == "" {
				return nil
			}
			return config.PersistThemeChoice(deps.AgentsDir, name)
		},
		// ClipboardWriter is the host half of a transcript copy
		// (`y` / `c`), ADDITIVE to the OSC 52 escape core-tui already
		// emits — never a replacement. The escape targets the machine
		// the operator is sitting at; the writer targets the machine
		// this process runs on, and only the writer can report whether
		// anything landed, which is what turns the footer's "copied N
		// lines" from a claim into an observation.
		//
		// SystemClipboardWriter resolves a helper (pbcopy / wl-copy /
		// xclip / xsel / clip.exe) once here and returns nil when the
		// box has none — a headless server, typically. A nil hook is
		// indistinguishable from leaving the field unset, so this needs
		// no build tag and adds no clipboard dependency to our module
		// graph (it is os/exec underneath).
		ClipboardWriter: coretui.SystemClipboardWriter(),
	}

	// Wire the Reloader + PricingController bindings on the
	// wrapped adapter so they read the same callback closures
	// launchTUI uses.
	wrapped.reload = makeReloadCallback(ctx, deps, ad)
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
func makeReloadCallback(ctx context.Context, deps tuiDeps, ad *attachadapter.Adapter) func() (coretui.ReloadResult, error) {
	return func() (coretui.ReloadResult, error) {
		resp := ad.AttachReload(ctx)
		freshMem, _ := instruction.Load(deps.ProjectRoot, deps.CoreHome,
			instruction.WithHomeAgentsRoot(deps.HomeAgentsDir),
			instruction.WithContentRoots(deps.ContentRoots),
			instruction.WithInterpolator(deps.EnvInterp))
		freshSkills, _ := skills.LoadAll(ctx, deps.AgentsDir, deps.CoreHome, deps.Gate,
			skills.WithHomeAgentsSkillsDir(deps.HomeAgentsDir),
			skills.WithContentRoots(deps.ContentRoots),
			skills.WithInterpolator(deps.EnvInterp))
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
		return compose.RefreshPricing(ctx, deps.Cfg, deps.AgentsDir, deps.CoreHome)
	}
}

func makeSetPricingCallback(deps tuiDeps) func(string, float64, float64) (string, error) {
	if deps.CoreHome == "" {
		return nil
	}
	return func(model string, in, out float64) (string, error) {
		return compose.SetPricing(deps.Cfg, deps.AgentsDir, deps.CoreHome, model, in, out)
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
	inner *agent.Agent
	// attachAd carries the attach capability surface (AttachTools /
	// AttachStatus / AttachUsage / AttachReplan / AttachReload) that
	// moved off *agent.Agent with the pkg/agent split (#388 phase 4).
	// The in-process TUI reads the same projections remote operators
	// get over HTTP.
	attachAd *attachadapter.Adapter
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
		// Per-turn usage bookkeeping via the shared TurnTap discipline
		// (overwrite-per-event, commit-once-per-TurnComplete, reset
		// between turns). Without this, cumulative-within-turn
		// UsageMetadata from Gemini/Vertex double-counts — the
		// regression #156 fixed and #157 extracted to pkg/usage.TurnTap
		// so future adapters get it right by default.
		var tap usage.TurnTap
		for ev, err := range a.inner.Run(ctx, prompt) {
			if err != nil {
				yield(coretui.Event{}, err)
				return
			}
			te := coretui.Event{Partial: ev.Partial, Model: modelName}
			tap.Observe(ev)
			// Live per-event usage snapshot for the TUI's status
			// sidebar — running cumulative during the turn, final at
			// TurnComplete. Observe has already updated tap.last;
			// Peek reads it without resetting. Must precede Commit
			// (which resets state on TurnComplete).
			if ev.UsageMetadata != nil {
				peek := tap.Peek()
				te.Usage = &coretui.Usage{
					InputTokens:  peek.InputTokens,
					OutputTokens: peek.OutputTokens,
				}
			}
			if u, ok := tap.Commit(ev); ok {
				if a.deps.Tracker != nil && a.deps.Cfg != nil {
					pricing := usage.PriceFor(modelName, a.deps.Cfg)
					turn := a.deps.Tracker.AppendUsage(modelName, u, pricing)
					te.CostUSD = turn.CostUSD
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

// AvailableModels satisfies coretui.ModelSwapper. Returns the priced
// current-generation catalog (see availableModelIDs comment).
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
	newAgent, newAd, err := buildAttachedAgent(newLLM, a.deps.AgentOpts, a.deps.AdapterOpts, a.deps.AttachReg)
	if err != nil {
		return nil, err
	}
	return &coreAgentAdapter{
		inner:          newAgent,
		attachAd:       newAd,
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
	return config.AppendPermissionsAllow(a.deps.AgentsDir, patterns)
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
	return config.AppendPermissionsDeny(a.deps.AgentsDir, patterns)
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
	return config.AppendBuiltinAllowExtra(a.deps.AgentsDir, bundleName)
}

// Tools satisfies coretui.ToolLister. Routes through the agent's
// AttachTools accessor so the Source field reflects the agent's
// own classification (builtin vs other — MCP/skill differentiation
// lands in attach when the agent grows per-tool provenance). The
// GateState field is computed by AttachTools using the same gate
// the live calls consult, so /tools and the actual approval
// behavior stay consistent.
func (a *coreAgentAdapter) Tools() []coretui.ToolInfo {
	raw := a.attachAd.AttachTools()
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

// Subagents is the roster half of coretui.SubagentReporter (R-SUB-1). Reads the
// BackgroundAgentManager's live handles and reports each one's
// real status (running / completed / failed / stopped) via
// BackgroundHandle.Status — the manager keeps terminal handles in
// the list until reaped, so the /subagents display reflects
// post-completion state instead of always reading "running."
func (a *coreAgentAdapter) Subagents() []coretui.SubagentInfo {
	mgr := background.ManagerOf(a.inner)
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

// SubagentEvents is the turn-log half of coretui.SubagentReporter
// (core-tui v0.18.0) — the `/subagents <name>` overlay and the live
// tail under a running sync subagent's tool row.
//
// The in-process TUI reads the event log directly; the remote adapter
// gets the same answer over
// GET /sessions/<sid>/agents/<name>/events. Both go through
// internal/subagentlog, because "which branch spellings does this name
// resolve to" is exactly the question the two must not answer
// differently (#694).
//
// A session started without --session-db has no log to read, so the
// capability reports every name as unknown rather than painting an
// empty turn list that looks like a subagent doing nothing.
func (a *coreAgentAdapter) SubagentEvents(ctx context.Context, name string, since int64) (coretui.SubagentEventPage, error) {
	if err := subagentlog.ValidateName(name); err != nil {
		return coretui.SubagentEventPage{}, err
	}
	elog := a.inner.EventLog()
	if elog == nil || elog.Stream == nil {
		return coretui.SubagentEventPage{}, fmt.Errorf(
			"this session has no event log; subagent turn history requires --session-db")
	}
	tree := subagentlog.Tree{
		AppName:   a.inner.AppName(),
		UserID:    a.inner.UserID(),
		SessionID: a.inner.SessionID(),
	}
	q := subagentlog.Resolve(ctx, elog.Stream, tree, name, a.subagentRoster())
	if !q.Known {
		return coretui.SubagentEventPage{}, &coretui.SubagentNotFoundError{
			Name:      name,
			Available: q.Available,
		}
	}
	page := subagentlog.Read(ctx, elog.Stream, tree, q, since, 0)
	out := coretui.SubagentEventPage{
		NextSince: page.NextSince,
		Truncated: page.Truncated,
		Events:    make([]coretui.SubagentEvent, 0, len(page.Events)),
	}
	for _, e := range page.Events {
		ev, ok := coretuievent.Subagent(e.Seq, e.Event)
		if !ok {
			continue
		}
		out.Events = append(out.Events, ev)
	}
	return out, nil
}

// subagentRoster collects the names this session knows about outside
// the log — live spawned instances, plus whatever the config declares
// as spawnable. Both only widen an answer: a name either list carries
// is real even before it has written a turn.
//
// Deliberately routed through the same two attach providers the HTTP
// handler consults rather than the background manager directly, so the
// two surfaces can't drift on which names count as real.
func (a *coreAgentAdapter) subagentRoster() subagentlog.Roster {
	var r subagentlog.Roster
	if a.attachAd == nil {
		return r
	}
	for _, ai := range a.attachAd.AttachAgents() {
		r.Instances = append(r.Instances, ai.Name)
	}
	for _, s := range a.attachAd.AttachSubagentCatalog() {
		r.Declared = append(r.Declared, s.Name)
	}
	return r
}

// invokeGuardrail implements /guardrail (#666).
//
//	/guardrail                             — state
//	/guardrail reset                       — clear whatever tripped
//	/guardrail reset cost_ceiling          — clear one
//	/guardrail reset +5                    — clear + add $5 of session budget
//	/guardrail reset cost_ceiling +5       — both
//
// Routed through the same attachadapter methods POST
// /guardrails/reset uses, so the local slash and the remote endpoint
// can't disagree about what a reset does — including the refusal when
// the reset would immediately re-trip.
func (a *coreAgentAdapter) invokeGuardrail(args string) coretui.SlashResult {
	if args == "" {
		return coretui.SlashResult{SystemMessage: attach.RenderGuardrails(a.attachAd.AttachGuardrails())}
	}
	fields := strings.Fields(args)
	if !strings.EqualFold(fields[0], "reset") {
		return coretui.SlashResult{SystemMessage: fmt.Sprintf(
			"/guardrail: unknown subcommand %q — usage: /guardrail [reset [watchdog|cost_ceiling|all] [+<usd>]]", fields[0])}
	}
	req := attach.GuardrailResetRequest{}
	for _, f := range fields[1:] {
		switch {
		case strings.HasPrefix(f, "+"):
			usd, err := strconv.ParseFloat(strings.TrimPrefix(f, "+"), 64)
			if err != nil || usd <= 0 {
				return coretui.SlashResult{SystemMessage: fmt.Sprintf(
					"/guardrail: %q is not a positive dollar amount — write it as +5 or +2.50", f)}
			}
			req.AdditionalBudgetUSD = usd
		case f == attach.GuardrailWatchdog, f == attach.GuardrailCostCeiling, f == attach.GuardrailAll:
			req.Guardrail = f
		default:
			return coretui.SlashResult{SystemMessage: fmt.Sprintf(
				"/guardrail reset: unknown argument %q — expected watchdog, cost_ceiling, all, or +<usd>", f)}
		}
	}
	resp, err := a.attachAd.AttachResetGuardrail(req)
	if err != nil {
		if errors.Is(err, attach.ErrGuardrailRetrip) {
			return coretui.SlashResult{SystemMessage: "/guardrail reset refused: " + err.Error()}
		}
		return coretui.SlashResult{SystemMessage: "/guardrail reset failed: " + err.Error()}
	}
	return coretui.SlashResult{SystemMessage: resp.Message + "\n" + attach.RenderGuardrails(resp.Guardrails)}
}

// invokeSubagentSpawn implements the singular /subagent command: spawn a
// configured subagent (declarative template or catalog spec) by name with a
// goal, fire-and-continue. With no args it lists the configured reference
// names; the plural /subagents lists live instances (#627).
func (a *coreAgentAdapter) invokeSubagentSpawn(ctx context.Context, args string) coretui.SlashResult {
	mgr := background.ManagerOf(a.inner)
	if mgr == nil {
		return coretui.SlashResult{SystemMessage: "/subagent unavailable: background sub-agents are disabled (relaunch without --no-background-agents)."}
	}
	names := mgr.ReferenceNames()
	trimmed := strings.TrimSpace(args)
	if trimmed == "" {
		if len(names) == 0 {
			return coretui.SlashResult{SystemMessage: "/subagent: no configured sub-agents. Add a subagents[] entry to your config to spawn one by name."}
		}
		return coretui.SlashResult{SystemMessage: formatSubagentCatalog(mgr.Catalog())}
	}
	name, goal, _ := strings.Cut(trimmed, " ")
	goal = strings.TrimSpace(goal)
	if goal == "" {
		return coretui.SlashResult{SystemMessage: fmt.Sprintf("/subagent %s: provide a goal — usage: /subagent <name> <goal>", name)}
	}
	h, err := mgr.SpawnRef(ctx, "", name, goal, background.RefOverrides{}, "")
	switch {
	case errors.Is(err, background.ErrUnknownSubagent):
		avail := "none configured"
		if len(names) > 0 {
			avail = strings.Join(names, ", ")
		}
		return coretui.SlashResult{SystemMessage: fmt.Sprintf("/subagent: no configured sub-agent named %q. Available: %s", name, avail)}
	case err != nil:
		return coretui.SlashResult{SystemMessage: "/subagent failed: " + err.Error()}
	default:
		return coretui.SlashResult{SystemMessage: fmt.Sprintf("Spawned sub-agent %q (branch %s), running in the background — its report will appear on a later turn; use /subagents to check status.", h.Name, h.Branch)}
	}
}

// formatSubagentCatalog renders the configured-subagent roster (#627) for
// the /subagent no-args listing: one line per subagent with its model,
// content root, and invocation modes, so operators see what the daemon
// loaded (and can spawn) without grepping the boot log. Distinct from
// /subagents, which reports live instances.
func formatSubagentCatalog(cat []attach.SubagentCatalogInfo) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d configured sub-agent(s) — spawn one with /subagent <name> <goal>:", len(cat))
	for _, e := range cat {
		attrs := make([]string, 0, 3)
		model := e.Model
		if model == "" {
			model = "inherit"
		}
		attrs = append(attrs, "model="+model)
		if e.Root != "" {
			attrs = append(attrs, "root="+e.Root)
		}
		if len(e.Modes) > 0 {
			attrs = append(attrs, strings.Join(e.Modes, "+"))
		}
		line := fmt.Sprintf("\n  • %s [%s]", e.Name, strings.Join(attrs, ", "))
		if e.Description != "" {
			line += " — " + e.Description
		}
		b.WriteString(line)
	}
	return b.String()
}

// Status satisfies coretui.StatusReporter. Wraps the agent's
// AttachStatus snapshot so the status surface reflects deferred /
// waiting / etc. state. Provider is sourced from the host config
// when known (auto-detect via the resolver leaves it as the empty
// string, in which case the chip is suppressed rather than showing
// a bogus tag).
func (a *coreAgentAdapter) Status() coretui.Status {
	s := a.attachAd.AttachStatus()
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
			Description: "spawn a configured background sub-agent by name: /subagent <name> <goal> (run /subagent with no args to list the configured roster — model, root, modes)",
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
	cmds = append(cmds, coretui.SlashCommandSpec{
		Name:        "usage",
		Description: "show cache-hit attribution + per-turn cost breakdown (companion to /stats)",
	})
	// /guardrail is registered unconditionally — the whole point of
	// #666 is that an operator staring at "agent refuses new turns"
	// can find the recovery command without knowing in advance which
	// backstop is armed. With nothing armed it prints "running".
	cmds = append(cmds, coretui.SlashCommandSpec{
		Name:        "guardrail",
		Aliases:     []string{"guardrails"},
		Description: "show watchdog + cost-ceiling state; /guardrail reset [watchdog|cost_ceiling|all] [+<usd>] to clear a halt",
	})
	// /replan is registered unconditionally, and so is the replanner
	// behind it — the closure reports "no active plan to revoke" under
	// plan_mode off/advisory rather than 501-ing, since advisory still
	// produces artifacts worth archiving. The InvokeSlash case keeps a
	// friendly message for a host that wired no replanner at all,
	// which is a clearer operator experience than hiding the command
	// and surfacing "unknown command" when the recipe docs promise it.
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
			// "The model said nothing" is an answer, not a failure —
			// same rule the attach handler applies for remote clients
			// (protocol 1.5.0). Rendering it as Err would put an error
			// modal in front of an operator whose question simply came
			// back empty, with no hint as to why.
			var empty *agent.SideQuestionEmptyError
			if errors.As(err, &empty) {
				text := attach.SideQueryResponse{Empty: true, Detail: empty.Detail}.AnswerText()
				return coretui.SlashResult{
					ModalAnswer: &coretui.SideAnswer{Question: args, Answer: text},
				}, nil
			}
			return coretui.SlashResult{
				ModalAnswer: &coretui.SideAnswer{Question: args, Err: err},
			}, nil
		}
		return coretui.SlashResult{
			ModalAnswer: &coretui.SideAnswer{Question: args, Answer: answer},
		}, nil
	case "subagent", "sub":
		// Singular /subagent = spawn a CONFIGURED subagent by name onto
		// the unified async surface (#626). Plural /subagents = list
		// live instances (Subagents()/#627). Operator-driven ad-hoc
		// personas are intentionally not offered here — the operator
		// curates specs in config; the command only references them.
		//
		// The spawn is fire-and-continue: SpawnRef launches the goroutine
		// and returns immediately, so this stays non-blocking in core-tui's
		// synchronous Update loop. The subagent's result arrives on a later
		// turn as a [Background reports] line.
		return a.invokeSubagentSpawn(ctx, args), nil
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
		//
		// Parent input rate is passed through so the digest-savings
		// block can compute "saved ~$X" dollar figures at display
		// time (pricing catalog is layered — resolving here ensures
		// operator overrides + fresh refreshes both take effect
		// without recomputing per-record at accumulation time).
		var parentInputRate float64
		if a.deps.Cfg != nil {
			parentInputRate = usage.PriceFor(a.deps.Cfg.Model.Name, a.deps.Cfg).InputPerMTok
		}
		return coretui.SlashResult{
			SystemMessage: compose.RenderContextStats(a.inner.ContextStats(), parentInputRate),
		}, nil
	case "usage":
		// /usage projects the local agent's AttachUsage() through the
		// same formatter the remote adapter uses so operators see the
		// same block regardless of whether they're driving the TUI in
		// process or over a socket. /stats keeps the terse aggregate;
		// /usage carries the cache-hit attribution + per-turn history.
		return coretui.SlashResult{
			SystemMessage: attach.RenderUsage(a.attachAd.AttachUsage()),
		}, nil
	case "guardrail", "guardrails":
		// Bare /guardrail reads state; /guardrail reset [what] [+usd]
		// clears a halt. Same adapter methods the REST endpoints call,
		// so the local and remote surfaces can't drift.
		return a.invokeGuardrail(strings.TrimSpace(args)), nil
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
		resp, err := a.attachAd.AttachReplan(ctx, attach.ReplanRequest{Reason: strings.TrimSpace(args)})
		if err != nil {
			if errors.Is(err, attach.ErrCapabilityNotRegistered) {
				return coretui.SlashResult{
					SystemMessage: "/replan unavailable: this session has no replan capability registered.",
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

// InvokeSlashAsync satisfies coretui.AsyncSlashProvider (core-tui
// v0.6.3, issue #16 / our #55; v0.21.0 folded the former
// AsyncSlashProviderWithPreamble into the bare interface, so the
// preamble return is no longer an opt-in variant). The synchronous
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
	case permissions.PromptKindFileWrite, permissions.PromptKindPathScope, permissions.PromptKindControlPlaneWrite:
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
		// core-tui v0.21.0 split "I could not draw this" out of the
		// Action enum and into the error return: an unrenderable
		// request now comes back as ErrElicitUnsupported paired with
		// a placeholder ElicitActionCancel, where v0.20.0 reported a
		// plain ElicitActionDecline and a nil error.
		//
		// Forwarding that as an error would turn what used to be a
		// clean protocol answer into a JSON-RPC failure on the
		// calling server's goroutine. MCP already has a word for
		// "nobody is going to fill this in" — decline — and it is
		// what DeclineHandler (pkg/mcp/elicitation.go) returns for
		// the neighbouring case of no interactive UI at all. A form
		// this TUI cannot draw is the same answer arrived at later,
		// so it gets the same wire value and the operator gets
		// core-tui's transcript row explaining which part was
		// undrawable.
		//
		// Every other error — a cancelled context above all — still
		// propagates, because those really are failures to carry the
		// request out rather than answers to it.
		if errors.Is(err, coretui.ErrElicitUnsupported) {
			return &mcpsdk.ElicitResult{Action: "decline"}, nil
		}
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
