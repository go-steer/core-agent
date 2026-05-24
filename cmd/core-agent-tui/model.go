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

// mode dispatches the root model between three screens: the welcome
// landing (bare invocation; pick local-spawn vs remote-URL), the
// session picker (after attaching to a hub URL), and the chat view
// (active session). Esc moves chat → picker / picker → welcome
// (when reachable from welcome); /attach + /spawn + /welcome slash
// commands also drive transitions.
type mode int

const (
	modeWelcome mode = iota
	modePicker
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

	mode    mode
	welcome welcomeModel
	picker  pickerModel
	chat    chatModel
}

func newRootModel(client *attachclient.Client, theme, alias string) rootModel {
	// Welcome path: no client means the operator invoked
	// `core-agent-tui` with no URL. Land on the welcome screen
	// so they enter an attach URL inside the TUI.
	if client == nil {
		return rootModel{
			theme:   theme,
			alias:   alias,
			mode:    modeWelcome,
			welcome: newWelcomeModel(),
		}
	}
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
			client:  client,
			theme:   theme,
			alias:   alias,
			mode:    modeChat,
			welcome: newWelcomeModel(), // available for /welcome
			picker:  newPickerModel(client),
			chat:    newChatModel(client, entry, theme, alias),
		}
	}
	return rootModel{
		client:  client,
		theme:   theme,
		alias:   alias,
		mode:    modePicker,
		welcome: newWelcomeModel(),
		picker:  newPickerModel(client),
		chat:    chatModel{}, // populated when picker enters a session
	}
}

func (m rootModel) Init() tea.Cmd {
	switch m.mode {
	case modeWelcome:
		return m.welcome.Init()
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
		m.welcome.SetSize(msg.Width, msg.Height)
		m.picker.SetSize(msg.Width, msg.Height)
		m.chat.SetSize(msg.Width, msg.Height)
		var c1, c2, c3 tea.Cmd
		m.welcome, c1 = m.welcome.UpdateInner(msg)
		m.picker, c2 = m.picker.UpdateInner(msg)
		m.chat, c3 = m.chat.UpdateInner(msg)
		return m, tea.Batch(c1, c2, c3)
	case tea.KeyMsg:
		if msg.Type == tea.KeyCtrlC {
			return m, tea.Quit
		}
	}

	switch m.mode {
	case modeWelcome:
		var cmd tea.Cmd
		m.welcome, cmd = m.welcome.UpdateInner(msg)
		if m.welcome.chosen != nil {
			choice := *m.welcome.chosen
			m.welcome.chosen = nil
			// Remote URL: parse + build client + flip into picker.
			parsed, err := attachclient.ParseURL(choice.RemoteURL)
			if err != nil {
				m.welcome.error = err.Error()
				m.welcome.stage = welcomeInput
				return m, nil
			}
			m.client = attachclient.New(parsed, "", 0)
			m.picker = newPickerModel(m.client)
			m.mode = modePicker
			return m, tea.Batch(cmd, m.picker.Init())
		}
		return m, cmd
	case modePicker:
		var cmd tea.Cmd
		m.picker, cmd = m.picker.UpdateInner(msg)
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
		if m.chat.wantsPicker {
			m.chat.wantsPicker = false
			m.mode = modePicker
			return m, tea.Batch(cmd, m.picker.refreshCmd())
		}
		if m.chat.wantsWelcome {
			m.chat.wantsWelcome = false
			m.welcome = newWelcomeModel()
			m.welcome.SetSize(m.width, m.height)
			m.client = nil
			m.mode = modeWelcome
			return m, nil
		}
		if m.chat.wantsAttachURL != "" {
			target := m.chat.wantsAttachURL
			m.chat.wantsAttachURL = ""
			parsed, err := attachclient.ParseURL(target)
			if err != nil {
				// Bounce back to welcome with the error so the
				// operator sees what went wrong; chat's slash
				// handler validated grammar but we re-parse here
				// in case of edge cases.
				m.welcome = newWelcomeModel()
				m.welcome.SetSize(m.width, m.height)
				m.welcome.error = "attach: " + err.Error()
				m.client = nil
				m.mode = modeWelcome
				return m, nil
			}
			m.client = attachclient.New(parsed, "", 0)
			m.picker = newPickerModel(m.client)
			m.mode = modePicker
			return m, m.picker.Init()
		}
		return m, cmd
	}
}

func (m rootModel) View() string {
	switch m.mode {
	case modeWelcome:
		return m.welcome.View()
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
