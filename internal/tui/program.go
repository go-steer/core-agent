// Copyright 2026 The Cogo Authors.
// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"context"
	"fmt"
	"os"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/go-steer/core-agent/pkg/agent"
	"github.com/go-steer/core-agent/pkg/config"
	"github.com/go-steer/core-agent/pkg/instruction"
	"github.com/go-steer/core-agent/pkg/mcp"
	"github.com/go-steer/core-agent/pkg/permissions"
	"github.com/go-steer/core-agent/pkg/skills"
	"github.com/go-steer/core-agent/pkg/usage"
)

// Exit codes used by Run. Distinct so callers can disambiguate
// config-time failures (bad model / bad gate) from runtime ones
// (bubble-tea panic).
const (
	ExitOK          = 0
	ExitRunError    = 1
	ExitConfigError = 2
)

// Options bundles the dependencies tui.Run needs. core-agent's main.go
// builds everything (agent, gate, mcp, skills, instruction, tracker) at
// the same point it would otherwise call runner.REPL — this struct
// just hands the lot to the TUI rather than reconstructing.
//
// Required: Agent, Cfg, Gate, Tracker.
// Optional: Memory, MCPServers, Skills (display-only), AgentsDir
// (for transcript-on-exit and slash-command persistence callbacks).
//
// The callback fields (RebuildAgent, ReloadFromDisk, PersistModelChoice,
// AlwaysAllow, AddAllowPatterns, AddDenyPatterns, AddBuiltinAllowExtra,
// SessionApprovals) plug runtime features into the TUI without the TUI
// having to know about provider, gate, tools, or config layout. All are
// nil-safe — slash commands that need them print "not available" when
// the host didn't wire one. Typically populated by cmd/core-agent's
// run() since that's where the construction context lives.
type Options struct {
	Agent      *agent.Agent
	Cfg        *config.Config
	Gate       *permissions.Gate
	Tracker    *usage.Tracker
	Memory     instruction.Loaded
	MCPServers []*mcp.Server
	Skills     skills.Skills
	AgentsDir  string

	// Elicitor is the MCP elicitation bridge the host built BEFORE
	// constructing MCP servers (so each server can hold the .Elicit
	// method as its ElicitorFn during connect). tui.Run attaches the
	// running bubble-tea program to the elicitor post-NewProgram so
	// elicit requests can render modals. Nil = build a fresh one
	// internally; MCP servers that arrived without a connected
	// elicitor will then have a non-functional Elicit and decline.
	Elicitor *Elicitor

	// RebuildAgent constructs a fresh agent bound to modelID while
	// preserving tools/toolsets/instruction. Lets /model swap the
	// model mid-session without the TUI knowing about provider /
	// gate / tools wiring.
	RebuildAgent func(modelID string) (*agent.Agent, error)

	// ReloadFromDisk re-reads .agents/ (mcp.json, skills/, AGENTS.md,
	// config.json) and rebuilds the agent in place. Only wired when
	// AgentsDir is non-empty.
	ReloadFromDisk func() (ReloadResult, error)

	// PersistModelChoice writes modelID to .agents/config.json so the
	// switch survives across runs. Only wired when AgentsDir is
	// non-empty; in-session-only switches when nil.
	PersistModelChoice func(modelID string) error

	// AlwaysAllow is invoked when the user picks "always allow" in the
	// permission modal. The host persists the pattern to
	// .agents/config.json. Nil-safe.
	AlwaysAllow func(req permissions.PromptRequest) error

	// SessionApprovals returns the gate's chronological approval log
	// for the current session. Used by /permissions to drive the
	// recommendation picker. Nil-safe.
	SessionApprovals func() []permissions.ApprovalLog

	// AddAllowPatterns appends one or more allowlist patterns to
	// .agents/config.json's permissions.allow block AND patches the
	// live gate so the additions take effect for the rest of the
	// session (no /reload needed). Driven by /permissions and /allow.
	AddAllowPatterns func(patterns []string) error

	// AddDenyPatterns is the symmetric extension for permissions.deny
	// driven by /deny.
	AddDenyPatterns func(patterns []string) error

	// AddBuiltinAllowExtra appends a bundle name to
	// permissions.builtin_allow_extras AND injects that bundle's
	// patterns into the live gate. Used by /allow bundle:<name>.
	AddBuiltinAllowExtra func(name string) error

	// RefreshPricing forces an out-of-cycle pricing-catalog refresh
	// from upstream (typically LiteLLM via internal/pricing.Refresh
	// with MinInterval: -1s) AND reinstalls the freshly-built
	// catalog into usage.SetCatalog so cost lookups for the rest
	// of the session see the new rates. Called by /pricing refresh.
	// Returns a human-readable summary line for the chat scrollback.
	// Nil-safe.
	RefreshPricing func(ctx context.Context) (string, error)

	// SetPricing writes a per-model rate to the manual section of
	// the user pricing file AND rebuilds the live catalog so the
	// new rate takes effect immediately. Called by
	// /pricing set <model> <in> <out>. modelID is the model name
	// (case-insensitive); rates are USD per million tokens.
	// Returns a human-readable summary line. Nil-safe.
	SetPricing func(modelID string, inputPerMTok, outputPerMTok float64) (string, error)
}

// ReloadResult is what ReloadFromDisk returns on success. Mirrors the
// state the program builds at startup; the model swaps these into
// place atomically after the callback returns.
type ReloadResult = reloadResult

// Run launches the in-process TUI bound to opts.Agent. Blocks until
// the operator quits. Config failures (no auth, bad gate) return
// ExitConfigError; bubble-tea runtime failures return ExitRunError.
//
// The TUI takes over stdin/stdout in raw + alt-screen mode for its
// lifetime; the caller should ensure no other goroutine writes to
// the terminal while Run is in flight.
func Run(ctx context.Context, opts Options) (int, error) {
	if opts.Agent == nil {
		return ExitConfigError, fmt.Errorf("tui: Options.Agent is required")
	}
	if opts.Cfg == nil {
		return ExitConfigError, fmt.Errorf("tui: Options.Cfg is required")
	}
	if opts.Gate == nil {
		return ExitConfigError, fmt.Errorf("tui: Options.Gate is required")
	}
	if opts.Tracker == nil {
		opts.Tracker = usage.NewTracker()
	}

	startedAt := time.Now()

	// Resolve the Glamour style BEFORE tea.NewProgram takes over
	// stdin. Glamour's WithAutoStyle sends an OSC-11 query whose
	// response would otherwise race into the textarea as input.
	// Resolving once up front and threading the result through
	// NewModel keeps every Glamour rebuild (resize, etc.) silent.
	//
	// Operator override via cfg.UI.Theme ("dark"/"light") wins —
	// some terminals (SSH stacks, tmux passthrough) answer the
	// OSC-11 query unreliably and forcing a theme is the escape
	// hatch. "auto" or empty falls through to detection.
	mdStyle := "dark"
	switch opts.Cfg.UI.Theme {
	case config.ThemeDark:
		mdStyle = "dark"
	case config.ThemeLight:
		mdStyle = "light"
	default: // "", ThemeAuto
		if !lipgloss.HasDarkBackground() {
			mdStyle = "light"
		}
	}

	m := NewModel(opts.Cfg, nil, mdStyle)

	// Install the TUI prompter on the caller's gate. The prompter
	// holds a stub `send` for now; it's wired to the bubble-tea
	// program after tea.NewProgram returns (chicken-and-egg).
	prompter := NewPrompter(nil)
	opts.Gate.SetPrompter(prompter)

	// Use the caller's elicitor if they constructed one (typical:
	// main.go built it pre-MCP so each server's connect could hold
	// the .Elicit closure). Otherwise build a fresh one — useful
	// for tests that don't exercise MCP elicitation.
	elicitor := opts.Elicitor
	if elicitor == nil {
		elicitor = NewElicitor()
	}

	m.agent = opts.Agent
	m.scope = opts.Gate.Scope()
	m.memory = opts.Memory
	m.usage = opts.Tracker
	m.mcpServers = opts.MCPServers
	m.skills = opts.Skills

	// Plumb runtime callbacks from the host. All nil-safe; slash
	// commands that need them surface "not available" when the
	// host didn't wire one (e.g. /model with no rebuilder).
	m.rebuildAgent = opts.RebuildAgent
	m.reloadFromDisk = opts.ReloadFromDisk
	m.persistModelChoice = opts.PersistModelChoice
	m.AlwaysAllow = opts.AlwaysAllow
	m.SessionApprovals = opts.SessionApprovals
	m.AddAllowPatterns = opts.AddAllowPatterns
	m.AddDenyPatterns = opts.AddDenyPatterns
	m.AddBuiltinAllowExtra = opts.AddBuiltinAllowExtra
	m.refreshPricing = opts.RefreshPricing
	m.setPricing = opts.SetPricing

	// Mouse capture wires the wheel to viewport scrolling. Capturing
	// mouse events also takes plain click-drag away from the terminal's
	// native text selection, so users hold Shift to select. Default
	// on; override via cfg.UI.Mouse = false (or runtime /mouse off).
	m.mouseEnabled = opts.Cfg.UI.MouseEnabled()
	teaOpts := []tea.ProgramOption{tea.WithAltScreen()}
	if m.mouseEnabled {
		teaOpts = append(teaOpts, tea.WithMouseCellMotion())
	}
	p := tea.NewProgram(m, teaOpts...)
	m.SetProgram(p)
	prompter.(*tuiPrompter).send = p
	elicitor.attach(p)

	finalModel, err := p.Run()
	if err != nil {
		return ExitRunError, fmt.Errorf("tui: %w", err)
	}

	if fm, ok := finalModel.(*Model); ok {
		// Reap stdio MCP children before we exit. They'd be reaped
		// by init eventually anyway, but doing it here keeps the
		// process-leak window small and makes `ps` cleaner.
		for _, srv := range fm.mcpServers {
			srv.Close()
		}
		// Persist transcript on exit when we have a project root.
		// Failures are non-fatal; we report to stderr so they're
		// visible after the alt-screen is torn down.
		if opts.AgentsDir != "" {
			path, err := saveTranscript(opts.AgentsDir, startedAt, fm)
			if err != nil {
				fmt.Fprintf(os.Stderr, "core-agent: transcript save: %v\n", err)
			} else if path != "" {
				fmt.Fprintf(os.Stderr, "core-agent: transcript saved to %s\n", path)
			}
		}
	}
	return ExitOK, nil
}

// saveTranscript serializes the TUI's chat history + usage totals to
// .agents/sessions/<timestamp>.json. Uses the package-local Transcript
// types (see transcript.go), lifted from cogo's internal/session.
func saveTranscript(agentsDir string, started time.Time, m *Model) (string, error) {
	msgs := m.history.Snapshot()
	out := make([]TranscriptMsg, 0, len(msgs))
	for _, msg := range msgs {
		out = append(out, TranscriptMsg{Role: roleString(msg.Role), Text: msg.Text})
	}
	tot := TranscriptUsage{}
	if m.usage != nil {
		t := m.usage.Totals()
		tot = TranscriptUsage{Turns: t.Turns, InputTokens: t.InputTokens, OutputTokens: t.OutputTokens, CostUSD: t.CostUSD}
	}
	return saveTranscriptFile(agentsDir, Transcript{
		StartedAt: started,
		Model:     m.cfg.Model.Name,
		Messages:  out,
		Usage:     tot,
	})
}

// roleString maps the TUI's Role enum to the human-readable strings
// the transcript schema uses.
func roleString(r Role) string {
	switch r {
	case RoleUser:
		return "user"
	case RoleAssistant:
		return "assistant"
	case RoleSystem:
		return "system"
	case RoleError:
		return "error"
	}
	return "unknown"
}
