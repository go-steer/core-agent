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

// welcomeStage is which prompt the welcome model is currently in:
// either picking between local/remote, or (after choosing remote)
// entering the URL.
type welcomeStage int

const (
	welcomeChoice welcomeStage = iota
	welcomeURL
	welcomeSpawning
)

// welcomeModel is the landing screen the TUI shows when invoked
// with no URL and no --local. Two choices: spawn a local agent
// (equivalent to --local), or attach to a remote endpoint (URL
// input that flows into the session picker).
type welcomeModel struct {
	width, height int

	stage  welcomeStage
	cursor int // 0 = spawn local, 1 = attach remote
	url    textinput.Model
	error  string

	// chosen is set when the operator picks an option; the root
	// model reads it after each Update.
	chosen *welcomeChoiceMsg
}

// welcomeChoiceMsg carries the operator's decision out of the
// welcome screen back to the root model. Exactly one of LocalSpawn
// or RemoteURL is set per choice.
type welcomeChoiceMsg struct {
	LocalSpawn bool
	RemoteURL  string
}

func newWelcomeModel() welcomeModel {
	ti := textinput.New()
	ti.Placeholder = "http://localhost:7777 or unix:///tmp/sock"
	ti.CharLimit = 256
	ti.Width = 60
	return welcomeModel{
		stage: welcomeChoice,
		url:   ti,
	}
}

func (m *welcomeModel) SetSize(w, h int) {
	m.width = w
	m.height = h
	if w > 4 {
		m.url.Width = w - 4
	}
}

func (m welcomeModel) Init() tea.Cmd { return nil }

// UpdateInner handles welcome-screen key events. Returns the
// (possibly mutated) model + a command. Root model reads
// m.chosen after each Update to detect a decision.
func (m welcomeModel) UpdateInner(msg tea.Msg) (welcomeModel, tea.Cmd) {
	if key, ok := msg.(tea.KeyMsg); ok {
		switch m.stage {
		case welcomeChoice:
			switch key.String() {
			case "up", "k":
				if m.cursor > 0 {
					m.cursor--
				}
			case "down", "j":
				if m.cursor < 1 {
					m.cursor++
				}
			case "enter":
				if m.cursor == 0 {
					choice := welcomeChoiceMsg{LocalSpawn: true}
					m.chosen = &choice
					m.stage = welcomeSpawning
				} else {
					m.stage = welcomeURL
					m.url.Focus()
					return m, textinput.Blink
				}
			case "q", "esc":
				return m, tea.Quit
			}
			return m, nil
		case welcomeURL:
			switch key.String() {
			case "esc":
				// Back to choice screen.
				m.stage = welcomeChoice
				m.url.Blur()
				m.error = ""
				return m, nil
			case "enter":
				raw := strings.TrimSpace(m.url.Value())
				if raw == "" {
					m.error = "URL is required (e.g. http://localhost:7777)"
					return m, nil
				}
				// Basic sanity check; full parsing happens in
				// the root model when it constructs the client.
				if !strings.HasPrefix(raw, "http://") &&
					!strings.HasPrefix(raw, "https://") &&
					!strings.HasPrefix(raw, "unix://") {
					m.error = "URL must start with http://, https://, or unix://"
					return m, nil
				}
				choice := welcomeChoiceMsg{RemoteURL: raw}
				m.chosen = &choice
				return m, nil
			}
		}
	}

	// Forward to the URL textinput when it's the focused element.
	if m.stage == welcomeURL {
		var cmd tea.Cmd
		m.url, cmd = m.url.Update(msg)
		return m, cmd
	}
	return m, nil
}

// View renders the welcome screen.
func (m welcomeModel) View() string {
	header := styleStatusBar.Width(m.width).Render(
		"core-agent-tui  ●  no endpoint selected",
	)
	var body strings.Builder
	body.WriteString("\n\n  How would you like to start?\n\n")

	switch m.stage {
	case welcomeChoice:
		choices := []string{
			"Spawn a local agent          (--local equivalent)",
			"Attach to a remote endpoint  (enter URL)",
		}
		for i, c := range choices {
			marker := "    "
			line := c
			if i == m.cursor {
				marker = "  ▸ "
				line = styleBubbleUser.Render(c)
			}
			body.WriteString(marker + line + "\n")
		}
		body.WriteString("\n")
		body.WriteString(styleHint.Render("  ↑/↓ navigate · Enter select · q quit"))
		body.WriteString("\n")
	case welcomeURL:
		body.WriteString("  Endpoint URL:\n\n")
		body.WriteString("  " + m.url.View() + "\n\n")
		body.WriteString(styleHint.Render("  Enter to attach · Esc to go back"))
		body.WriteString("\n")
		if m.error != "" {
			body.WriteString("\n  " + styleBubbleErr.Render("✗ "+m.error) + "\n")
		}
	case welcomeSpawning:
		body.WriteString("  Spawning local agent…\n")
	}

	footer := styleFooter.Width(m.width).Render(
		"  /help in chat · q quit",
	)

	bodyStr := body.String()
	bodyLines := strings.Count(bodyStr, "\n")
	pad := m.height - bodyLines - 2 // header + footer
	if pad > 0 {
		bodyStr += strings.Repeat("\n", pad)
	}
	return fmt.Sprintf("%s\n%s%s", header, bodyStr, footer)
}
