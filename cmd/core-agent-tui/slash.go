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

	tea "github.com/charmbracelet/bubbletea"

	"github.com/go-steer/core-agent/attach"
)

// helpText is what /help dumps into the chat scrollback. Kept short
// and scannable — the design doc has the full keymap.
const helpText = `slash commands:
  /help                    show this list
  /quit, /exit             leave the TUI
  /clear                   clear the local scrollback (server log unchanged)
  /sessions                back to the session picker
  /welcome                 back to the welcome landing screen
  /attach <url>            disconnect current, attach to a new endpoint
  /spawn [args...]         spawn a new local agent; args forward to it
  /reconnect               force-reconnect the SSE stream
  /interrupt               POST /interrupt — cancel the in-flight turn
  /wake                    POST /wake — pierce a scheduler sleep
  /inject <msg>            same as Enter; useful for paste-via-/inject
  /theme auto|dark|light   switch glamour theme (re-renders existing log)
  /tools                   list tools available to this agent (with gate state)
  /subagents               list background subagents
  /status                  show model + run state
  /peers                   list peers when connected to a hub
  /transcript [path]       save scrollback to a file (default /tmp/<sid>.md)

keybindings:
  Enter         submit input (or run slash command)
  Shift+Enter   newline in input
  Ctrl+E        open $EDITOR with the current input as a buffer
  PgUp/PgDn     scroll scrollback
  Esc           clear input if typing; otherwise /interrupt the turn
  Ctrl+C        quit`

// handleSubmit reacts to Enter in the input box. Branches on whether
// the text starts with "/" — slash → command dispatch; otherwise →
// inject the line as a user message via /inject.
func (m chatModel) handleSubmit(text string) (chatModel, tea.Cmd) {
	if !strings.HasPrefix(text, "/") {
		return m, m.injectCmd(text)
	}
	parts := strings.Fields(text)
	cmd := strings.ToLower(parts[0])
	args := parts[1:]
	switch cmd {
	case "/help":
		m.appendSystem(helpText)
		m.refreshViewport()
		return m, nil
	case "/quit", "/exit":
		return m, tea.Quit
	case "/clear":
		m.events = nil
		m.refreshViewport()
		return m, nil
	case "/sessions":
		m.wantsPicker = true
		return m, nil
	case "/welcome":
		m.wantsWelcome = true
		return m, nil
	case "/attach":
		if len(args) == 0 {
			m.appendErr("/attach: usage: /attach <url>")
			m.refreshViewport()
			return m, nil
		}
		raw := args[0]
		if !strings.HasPrefix(raw, "http://") &&
			!strings.HasPrefix(raw, "https://") &&
			!strings.HasPrefix(raw, "unix://") {
			m.appendErr("/attach: URL must start with http://, https://, or unix://")
			m.refreshViewport()
			return m, nil
		}
		m.wantsAttachURL = raw
		return m, nil
	case "/spawn":
		// Pass through any trailing args verbatim to the spawned
		// agent. A bare /spawn is fine — spawns with defaults.
		m.wantsSpawn = args
		if m.wantsSpawn == nil {
			m.wantsSpawn = []string{}
		}
		return m, nil
	case "/interrupt":
		return m, m.interruptCmd()
	case "/reconnect":
		m.appendSystem(fmt.Sprintf("reconnecting from seq=%d…", m.lastSeq))
		m.refreshViewport()
		return m, m.subscribeCmd(m.lastSeq)
	case "/wake":
		return m, m.wakeCmd()
	case "/inject":
		if len(args) == 0 {
			m.appendErr("/inject: usage: /inject <message>")
			m.refreshViewport()
			return m, nil
		}
		return m, m.injectCmd(strings.Join(args, " "))
	case "/theme":
		if len(args) == 0 {
			m.appendErr("/theme: usage: /theme auto|dark|light|notty")
			m.refreshViewport()
			return m, nil
		}
		m.theme = args[0]
		// Re-render all asst messages so the theme change takes effect.
		for i := range m.events {
			if m.events[i].kind == "asst" && !m.events[i].partial {
				m.events[i].rendered = m.renderMarkdown(m.events[i].body)
			}
		}
		m.refreshViewport()
		return m, nil
	case "/tools":
		return m, m.fetchToolsCmd()
	case "/subagents", "/agents":
		return m, m.fetchAgentsCmd()
	case "/status":
		return m, m.fetchStatusCmd()
	case "/peers":
		return m, m.fetchPeersCmd()
	case "/transcript":
		path := "/tmp/" + m.entry.SessionID + ".md"
		if len(args) > 0 {
			path = args[0]
		}
		err := writeTranscript(path, m.events)
		if err != nil {
			m.appendErr(fmt.Sprintf("/transcript: %v", err))
		} else {
			m.appendSystem("transcript saved to " + path)
		}
		m.refreshViewport()
		return m, nil
	default:
		m.appendErr(fmt.Sprintf("unknown command: %s — type /help for the list", cmd))
		m.refreshViewport()
		return m, nil
	}
}

// --- modal renderers ---

func renderToolsList(tools []attach.ToolInfo) string {
	if len(tools) == 0 {
		return "/tools: no tools registered"
	}
	var sb strings.Builder
	sb.WriteString("/tools — agent's tool catalog\n\n")
	fmt.Fprintf(&sb, "  %-22s %-10s %-12s %s\n", "NAME", "SOURCE", "GATE", "DESCRIPTION")
	sb.WriteString("  " + strings.Repeat("─", 80) + "\n")
	for _, t := range tools {
		desc := t.Description
		if len(desc) > 60 {
			desc = desc[:60] + "…"
		}
		gate := t.GateState
		if gate == "" {
			gate = "-"
		}
		fmt.Fprintf(&sb, "  %-22s %-10s %-12s %s\n", t.Name, t.Source, gate, desc)
	}
	return sb.String()
}

func renderAgentsList(agents []attach.AgentInfo) string {
	if len(agents) == 0 {
		return "/subagents: no background subagents running"
	}
	var sb strings.Builder
	sb.WriteString("/subagents — background subagents\n\n")
	fmt.Fprintf(&sb, "  %-30s %-10s %s\n", "NAME", "STATUS", "STARTED")
	sb.WriteString("  " + strings.Repeat("─", 80) + "\n")
	for _, a := range agents {
		fmt.Fprintf(&sb, "  %-30s %-10s %s\n", a.Name, a.Status, a.StartedAt.Format("15:04:05"))
	}
	return sb.String()
}

func renderPeersList(peers []peerEntry) string {
	if len(peers) == 0 {
		return "/peers: no peers"
	}
	var sb strings.Builder
	sb.WriteString("/peers — registered peers\n\n")
	fmt.Fprintf(&sb, "  %-25s %-40s %s\n", "NAME", "ENDPOINT", "LABELS")
	sb.WriteString("  " + strings.Repeat("─", 80) + "\n")
	for _, p := range peers {
		labels := ""
		for k, v := range p.Labels {
			if labels != "" {
				labels += ","
			}
			labels += k + "=" + v
		}
		fmt.Fprintf(&sb, "  %-25s %-40s %s\n", p.Name, p.Endpoint, labels)
	}
	return sb.String()
}
