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

	"github.com/go-steer/core-agent/agent"
	"github.com/go-steer/core-agent/config"
	"github.com/go-steer/core-agent/instruction"
	"github.com/go-steer/core-agent/mcp"
	"github.com/go-steer/core-agent/permissions"
	"github.com/go-steer/core-agent/skills"
	"github.com/go-steer/core-agent/usage"
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
// (for transcript-on-exit).
//
// What's deliberately NOT in this struct (cogo had them; we drop for v2):
//   - RebuildAgent / ReloadFromDisk / PersistModelChoice — runtime
//     /model + /reload features. Re-add in PR 2 if needed.
//   - AddAllowPatterns / AddDenyPatterns / AddBuiltinAllowExtra —
//     /allow + /deny + bundle slash commands. Re-add in PR 2.
//   - AlwaysAllow path-scope persistence — same. Always-allow
//     decisions are still respected for the session; just not
//     persisted to disk in PR 1.
type Options struct {
	Agent      *agent.Agent
	Cfg        *config.Config
	Gate       *permissions.Gate
	Tracker    *usage.Tracker
	Memory     instruction.Loaded
	MCPServers []*mcp.Server
	Skills     skills.Skills
	AgentsDir  string
}

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

	// Detect terminal background BEFORE tea.NewProgram takes over
	// stdin. Glamour's WithAutoStyle sends an OSC-11 query whose
	// response would otherwise race into the textarea as input.
	// Resolving the style name once up front and threading it
	// through NewModel keeps every Glamour rebuild (resize, etc.)
	// silent.
	mdStyle := "dark"
	if !lipgloss.HasDarkBackground() {
		mdStyle = "light"
	}

	m := NewModel(opts.Cfg, nil, mdStyle)

	// Install the TUI prompter on the caller's gate. The prompter
	// holds a stub `send` for now; it's wired to the bubble-tea
	// program after tea.NewProgram returns (chicken-and-egg).
	prompter := NewPrompter(nil)
	opts.Gate.SetPrompter(prompter)

	elicitor := newTUIElicitor()

	m.agent = opts.Agent
	m.scope = opts.Gate.Scope()
	m.memory = opts.Memory
	m.usage = opts.Tracker
	m.mcpServers = opts.MCPServers
	m.skills = opts.Skills

	// Mouse capture wires the wheel to viewport scrolling. Capturing
	// mouse events also takes plain click-drag away from the terminal's
	// native text selection, so users hold Shift to select. Default
	// on for v2; a config option follows in PR 2 if needed.
	m.mouseEnabled = true
	teaOpts := []tea.ProgramOption{tea.WithAltScreen(), tea.WithMouseCellMotion()}
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
