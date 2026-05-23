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
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// welcomeStage tracks whether the welcome screen is awaiting input
// (the default) or showing a spawn-in-progress indicator. There's
// no separate "URL stage" — operators type `/attach <url>` directly
// into the single input box, mirroring how slash commands work in
// the chat view.
type welcomeStage int

const (
	welcomeInput welcomeStage = iota
	welcomeSpawning
)

// welcomeModel is the landing screen the TUI shows when invoked
// with no URL and no --local. Input-driven: one textinput at the
// bottom accepts `/spawn`, `/attach <url>`, `/help`, `/quit`. A
// hint table above shows the available commands so first-time
// operators don't have to know them upfront.
type welcomeModel struct {
	width, height int

	stage welcomeStage
	input textinput.Model
	error string
	hint  string // transient hint (e.g. "/help — type a command above")

	// chosen is set when the operator picks an option; the root
	// model reads it after each Update.
	chosen *welcomeChoiceMsg
}

// welcomeChoiceMsg carries the operator's decision out of the
// welcome screen back to the root model. Exactly one of LocalSpawn
// or RemoteURL is set per choice; SpawnArgs forwards trailing args
// from `/spawn -- ...` to the spawned agent.
type welcomeChoiceMsg struct {
	LocalSpawn bool
	SpawnArgs  []string
	RemoteURL  string
}

func newWelcomeModel() welcomeModel {
	ti := textinput.New()
	ti.Placeholder = "type a command, e.g. /spawn or /attach <url>"
	ti.CharLimit = 512
	ti.Width = 60
	ti.Prompt = "> "
	ti.Focus()
	return welcomeModel{
		stage: welcomeInput,
		input: ti,
	}
}

func (m *welcomeModel) SetSize(w, h int) {
	m.width = w
	m.height = h
	if w > 4 {
		m.input.Width = w - 4
	}
}

func (m welcomeModel) Init() tea.Cmd { return textinput.Blink }

// UpdateInner handles welcome-screen key events. Returns the
// (possibly mutated) model + a command. Root model reads
// m.chosen after each Update to detect a decision.
func (m welcomeModel) UpdateInner(msg tea.Msg) (welcomeModel, tea.Cmd) {
	if key, ok := msg.(tea.KeyMsg); ok {
		switch key.Type {
		case tea.KeyEnter:
			return m.submit()
		case tea.KeyEsc:
			// Esc clears the input or, if already empty, quits.
			// (Bare welcome → nothing to "go back" to; quitting
			// matches operator muscle memory from the picker.)
			if m.input.Value() == "" {
				return m, tea.Quit
			}
			m.input.Reset()
			m.error = ""
			return m, nil
		}
		// Any keystroke clears stale errors so they don't shout
		// at the operator forever.
		if m.error != "" {
			m.error = ""
		}
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

// submit parses the input box as a slash command. Returns a model
// with `chosen` set on success (root drives the transition), or
// with `error` set on bad input.
func (m welcomeModel) submit() (welcomeModel, tea.Cmd) {
	raw := strings.TrimSpace(m.input.Value())
	if raw == "" {
		m.error = "type a command — e.g. /spawn or /attach <url> (or /help)"
		return m, nil
	}
	// Bare URL with no /attach prefix is a common slip — accept it
	// rather than scolding the operator about the missing slash.
	if !strings.HasPrefix(raw, "/") {
		if isURLish(raw) {
			raw = "/attach " + raw
		} else {
			m.error = fmt.Sprintf("not a slash command: %q — try /spawn or /attach <url>", raw)
			return m, nil
		}
	}

	parts := strings.Fields(raw)
	cmd := strings.ToLower(parts[0])
	args := parts[1:]
	switch cmd {
	case "/help":
		m.hint = welcomeHelpText
		m.input.Reset()
		return m, nil
	case "/quit", "/exit":
		return m, tea.Quit
	case "/spawn":
		// /spawn [-- args...] — trailing args forward verbatim to
		// the spawned agent (e.g. /spawn -- --model=mock).
		spawnArgs := stripDoubleDash(args)
		choice := welcomeChoiceMsg{LocalSpawn: true, SpawnArgs: spawnArgs}
		m.chosen = &choice
		m.stage = welcomeSpawning
		m.input.Reset()
		return m, nil
	case "/attach":
		if len(args) == 0 {
			m.error = "/attach: usage: /attach <url>  (http://, https://, or unix://)"
			return m, nil
		}
		url := args[0]
		if !strings.HasPrefix(url, "http://") &&
			!strings.HasPrefix(url, "https://") &&
			!strings.HasPrefix(url, "unix://") {
			m.error = "/attach: URL must start with http://, https://, or unix://"
			return m, nil
		}
		choice := welcomeChoiceMsg{RemoteURL: url}
		m.chosen = &choice
		m.input.Reset()
		return m, nil
	default:
		m.error = fmt.Sprintf("unknown command: %s — try /help", cmd)
		return m, nil
	}
}

// View renders the welcome screen. Layout matches chat: status bar
// on top, output area (cheat sheet + errors + hints + spinner) in
// the middle, input box pinned just above the footer. Output flows
// upward; the operator's eye always lands on the input at the same
// position regardless of how much output is above it.
func (m welcomeModel) View() string {
	header := styleStatusBar.Width(m.width).Render(
		"core-agent-tui  ●  no endpoint selected",
	)
	footer := styleFooter.Width(m.width).Render(
		"  Enter run · Esc clear/quit · Ctrl+C quit",
	)

	// Output region (above the input). Everything the operator
	// might want to read goes here so the input stays anchored.
	var out strings.Builder
	out.WriteString("\n  Type a command to get started:\n\n")
	out.WriteString(welcomeCheatSheet)
	if m.stage == welcomeSpawning {
		out.WriteString("\n  " + styleHint.Render("⏳ spawning local agent…") + "\n")
	}
	if m.error != "" {
		out.WriteString("\n" + renderMultilineError(m.error) + "\n")
	}
	if m.hint != "" {
		out.WriteString("\n" + m.hint + "\n")
	}

	// Input region (anchored just above the footer). Always
	// rendered so the operator's eye lands on it consistently —
	// during spawn we just don't expect them to type, but the
	// box stays put.
	inputBlock := "  " + m.input.View() + "\n"

	// Pad between output and input so the input box sits at the
	// bottom regardless of how short the output is.
	outStr := out.String()
	outLines := strings.Count(outStr, "\n")
	inputLines := strings.Count(inputBlock, "\n")
	used := 1 + outLines + inputLines + 1 // header + output + input + footer
	pad := m.height - used
	if pad > 0 {
		outStr += strings.Repeat("\n", pad)
	}
	return fmt.Sprintf("%s%s%s%s", header+"\n", outStr, inputBlock, footer)
}

// welcomeCheatSheet is the static command list shown above the
// input. Two columns: command form + one-line description. Kept
// short — `/help` (handled inline) prints the full list.
const welcomeCheatSheet = `    /spawn [-- args...]      spawn a local agent (forward args to it)
    /attach <url>            attach to a remote endpoint
    /help                    show all commands
    /quit                    exit
`

// welcomeHelpText is what /help dumps below the input box. Includes
// commands not on the cheat sheet to avoid bloating the default view.
const welcomeHelpText = `  /help text:

    URL forms accepted by /attach:
      http(s)://host:port                              — hub form (picker)
      http(s)://host:port/sessions/<sid>               — direct-jump
      http(s)://host:port/sessions/<app>/<sid>         — qualified
      unix:///path/to/socket                           — unix-socket hub

    Bare URL also works:
      http://localhost:7777                            — same as /attach http://...

    From inside a chat session you can also use:
      /welcome                 return to this screen
      /interrupt               cancel the in-flight model turn
      /sessions                pop to the session picker`

// stripDoubleDash drops the leading "--" separator from spawn args
// so `/spawn -- --model=mock` forwards as ["--model=mock"] (the
// `--` is just there to mark "what follows is for the agent").
func stripDoubleDash(args []string) []string {
	if len(args) > 0 && args[0] == "--" {
		return args[1:]
	}
	return args
}

// isURLish is a permissive check — anything that looks like one of
// our accepted schemes counts. Lets operators paste a URL without
// remembering the `/attach` prefix.
func isURLish(s string) bool {
	return strings.HasPrefix(s, "http://") ||
		strings.HasPrefix(s, "https://") ||
		strings.HasPrefix(s, "unix://")
}

// renderMultilineError prefixes the first line of s with "  ✗ " and
// continuation lines with "    " so a multi-line error (e.g. one
// decorated by decorateSpawnErr with binary/args/stderr-tail) stays
// visually grouped under the ✗ marker. The error style is applied
// per-line so lipgloss doesn't strip newlines.
func renderMultilineError(s string) string {
	lines := strings.Split(s, "\n")
	var b strings.Builder
	for i, line := range lines {
		if i == 0 {
			b.WriteString("  " + styleBubbleErr.Render("✗ "+line))
		} else {
			b.WriteString("\n    " + styleBubbleErr.Render(line))
		}
	}
	return b.String()
}
