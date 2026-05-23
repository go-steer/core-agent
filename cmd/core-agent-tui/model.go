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
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/go-steer/core-agent/internal/attachclient"
)

// mode dispatches the root model between two screens: the session
// picker (initial landing on a hub URL) and the chat view (active
// session). Esc moves chat → picker; Enter on a picker row moves
// picker → chat.
type mode int

const (
	modePicker mode = iota
	modeChat
)

// rootModel is the top-level tea.Model. It owns the active mode + the
// per-screen sub-models and forwards window-resize / key events to
// the active one.
type rootModel struct {
	client *attachclient.Client
	theme  string
	alias  string

	width  int
	height int

	mode   mode
	picker pickerModel
	chat   chatModel
}

func newRootModel(client *attachclient.Client, theme, alias string) rootModel {
	// Direct-jump path: if the URL targets a specific session,
	// skip the picker entirely.
	if !client.URL.IsHubURL() {
		entry := pickerEntry{
			App:       "core-agent", // unknown until we GET /sessions; placeholder OK
			SessionID: strings.TrimPrefix(client.URL.Session, "/sessions/"),
			Endpoint:  client.URL.BaseURL,
			Origin:    "local",
		}
		// Best-effort: split /sessions/<app>/<sid> if present.
		if parts := strings.Split(strings.TrimPrefix(client.URL.Session, "/sessions/"), "/"); len(parts) == 2 {
			entry.App = parts[0]
			entry.SessionID = parts[1]
		}
		return rootModel{
			client: client,
			theme:  theme,
			alias:  alias,
			mode:   modeChat,
			picker: newPickerModel(client),
			chat:   newChatModel(client, entry, theme, alias),
		}
	}
	return rootModel{
		client: client,
		theme:  theme,
		alias:  alias,
		mode:   modePicker,
		picker: newPickerModel(client),
		chat:   chatModel{}, // populated when picker enters a session
	}
}

func (m rootModel) Init() tea.Cmd {
	switch m.mode {
	case modeChat:
		return m.chat.Init()
	default:
		return m.picker.Init()
	}
}

func (m rootModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.picker.SetSize(msg.Width, msg.Height)
		m.chat.SetSize(msg.Width, msg.Height)
		// Pass through so sub-models can fan-out resize handling too.
		var c1, c2 tea.Cmd
		m.picker, c1 = m.picker.UpdateInner(msg)
		m.chat, c2 = m.chat.UpdateInner(msg)
		return m, tea.Batch(c1, c2)
	case tea.KeyMsg:
		// Global quit; sub-models can override by handling earlier in the chain.
		if msg.Type == tea.KeyCtrlC {
			return m, tea.Quit
		}
	}

	switch m.mode {
	case modePicker:
		var cmd tea.Cmd
		m.picker, cmd = m.picker.UpdateInner(msg)
		// Did the picker pick a session?
		if m.picker.selected != nil {
			entry := *m.picker.selected
			m.chat = newChatModel(m.client, entry, m.theme, m.alias)
			m.picker.selected = nil
			m.mode = modeChat
			return m, tea.Batch(cmd, m.chat.Init())
		}
		return m, cmd
	default:
		var cmd tea.Cmd
		m.chat, cmd = m.chat.UpdateInner(msg)
		// Did the chat want to go back to the picker?
		if m.chat.wantsPicker {
			m.chat.wantsPicker = false
			m.mode = modePicker
			return m, tea.Batch(cmd, m.picker.refreshCmd())
		}
		return m, cmd
	}
}

func (m rootModel) View() string {
	switch m.mode {
	case modePicker:
		return m.picker.View()
	default:
		return m.chat.View()
	}
}

// --- shared lipgloss style palette ---
//
// Centralized so tweaks ripple consistently and so unit tests can
// reason about the rendered output without re-deriving colors.

var (
	colorAccent     = lipgloss.Color("69")  // cyan-blue
	colorMuted      = lipgloss.Color("244") // grey
	colorWarn       = lipgloss.Color("214") // orange
	colorErr        = lipgloss.Color("196") // red
	colorOK         = lipgloss.Color("42")  // green
	colorBackground = lipgloss.Color("236") // panel bg
)

var (
	styleStatusBar = lipgloss.NewStyle().
			Foreground(lipgloss.Color("255")).
			Background(colorBackground).
			Padding(0, 1)
	styleFooter = lipgloss.NewStyle().
			Foreground(colorMuted).
			Padding(0, 1)
	styleBubbleUser = lipgloss.NewStyle().
			Foreground(colorAccent).
			Bold(true)
	styleBubbleAsst = lipgloss.NewStyle().
			Foreground(colorOK).
			Bold(true)
	styleBubbleTool = lipgloss.NewStyle().
			Foreground(colorMuted)
	styleBubbleErr = lipgloss.NewStyle().
			Foreground(colorErr).
			Bold(true)
	styleWarn = lipgloss.NewStyle().
			Foreground(colorWarn)
	styleHint = lipgloss.NewStyle().
			Foreground(colorMuted).
			Italic(true)
)
