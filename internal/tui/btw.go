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

package tui

import (
	"context"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// btwState is the overlay shown while a /btw side question is in
// flight and after it completes. It is intentionally read-only —
// the operator dismisses it with Space/Enter/Escape and nothing
// about it ever lands in the main conversation history. See
// docs/operator-input-design.md layer C.
type btwState struct {
	// Question is the operator's question as typed (used as the
	// overlay header so they can confirm what they asked).
	Question string
	// Answer is the model's response. Empty while the side call is
	// still running; populated by btwResultMsg.
	Answer string
	// Err carries any error from the side call. Mutually exclusive
	// with Answer in practice — if both are set, render Err and
	// drop Answer so the operator sees the failure.
	Err error
	// pending is true while the goroutine is in flight. The
	// overlay shows a spinner-style "Asking…" indicator until the
	// btwResultMsg flips it to false.
	pending bool
}

// btwResultMsg carries the side call's outcome back from the
// goroutine into the main Update loop. Answer is empty when Err is
// set and vice versa.
type btwResultMsg struct {
	Answer string
	Err    error
}

// handleBTWCommand is wired from handleSlash for /btw <question>.
// Fires a goroutine that calls Agent.AskSideQuestion, shows a
// "pending" overlay immediately, and posts btwResultMsg when the
// model answers. Empty question prints a usage hint and is a no-op.
func (m *Model) handleBTWCommand(args string) (tea.Model, tea.Cmd) {
	question := strings.TrimSpace(args)
	if question == "" {
		m.history.Append(Message{Role: RoleSystem, Text: "Usage: /btw <question>   ask a quick side question — sees the full conversation, no tools, never enters history. Dismiss the overlay with Space, Enter, or Esc."})
		m.refreshViewport()
		return m, nil
	}
	if m.agent == nil {
		m.history.Append(Message{Role: RoleError, Text: "/btw unavailable: no agent constructed."})
		m.refreshViewport()
		return m, nil
	}
	// Show the overlay immediately so the operator sees the
	// question land. The goroutine fills in Answer/Err when the
	// model returns; meanwhile the operator can dismiss to cancel.
	m.btwOverlay = &btwState{Question: question, pending: true}
	go func(send programSender, q string) {
		// Side calls re-use the parent process context; cancelling
		// the TUI cancels in-flight side calls too. We deliberately
		// don't bind to m.cancelTurn — /btw is parallel to the
		// main turn and must not be cancelled by /interrupt.
		ans, err := m.agent.AskSideQuestion(context.Background(), q)
		send.Send(btwResultMsg{Answer: ans, Err: err})
	}(m.program, question)
	return m, nil
}

// handleBTWResult resolves the in-flight overlay with the
// goroutine's response. No-op when the overlay was already
// dismissed (the operator can hit Esc before the model returns).
func (m *Model) handleBTWResult(msg btwResultMsg) (tea.Model, tea.Cmd) {
	if m.btwOverlay == nil {
		// Operator dismissed before the answer landed; the response
		// is discarded. The /btw call was tool-less and never
		// touched the conversation, so there's no cleanup.
		return m, nil
	}
	m.btwOverlay.pending = false
	m.btwOverlay.Answer = msg.Answer
	m.btwOverlay.Err = msg.Err
	return m, nil
}

// handleBTWKey is dispatched from Update while a btw overlay is up.
// Space/Enter/Escape dismiss; everything else is intercepted so
// stray typing doesn't fall into the textarea (and so the operator
// can't accidentally send a prompt while the overlay covers the
// input).
func (m *Model) handleBTWKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case " ", "space", "enter", "esc":
		m.btwOverlay = nil
		return m, nil
	}
	// Swallow other keys. The overlay is modal — the operator must
	// dismiss before resuming typing.
	return m, nil
}

// renderBTWOverlay renders the overlay in place of the input area.
// Three states: pending (spinner-style "Asking…"), answer (model
// text), error (failure). All three include a footer hint for the
// dismiss keys.
func (m *Model) renderBTWOverlay() string {
	if m.btwOverlay == nil {
		return ""
	}
	st := m.btwOverlay
	headerStyle := lipgloss.NewStyle().Foreground(brandPink).Bold(true)
	footerStyle := m.styles.Footer
	body := headerStyle.Render("/btw  "+st.Question) + "\n\n"
	switch {
	case st.Err != nil:
		body += m.styles.Error.Render("⚠ " + st.Err.Error())
	case st.pending:
		body += m.styles.System.Render(m.spinner.View() + " Asking… (Esc to dismiss)")
	default:
		// Render through Glamour so markdown in the answer (lists,
		// code, emphasis) shows up properly. Trim trailing newlines
		// so the panel doesn't gain a phantom blank row.
		body += strings.TrimRight(m.md.Render(st.Answer), "\n")
	}
	body += "\n" + footerStyle.Render("[space / enter / esc] dismiss")
	return m.styles.InputBorder.Render(body)
}
