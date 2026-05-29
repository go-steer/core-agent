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
	"errors"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/go-steer/core-agent/pkg/agent"
)

// compactResultMsg carries the outcome of an asynchronous
// /compact invocation back to the Update loop. Goroutine →
// program.Send → handleCompactResult.
type compactResultMsg struct {
	res agent.CompactionResult
	err error
}

// handleCompactCommand dispatches /compact [focus]. Spawns a
// goroutine that calls Agent.Compact and posts a compactResultMsg
// when done — the model call takes 1–5+ seconds for a meaningful
// summary, so a synchronous call would freeze the TUI. (Side-step
// of core-tui#10 for our internal/tui path.) See
// docs/context-management-design.md §Mechanism A.
func (m *Model) handleCompactCommand(args string) (tea.Model, tea.Cmd) {
	if m.agent == nil {
		m.history.Append(Message{Role: RoleError, Text: "/compact unavailable: no agent constructed."})
		m.refreshViewport()
		return m, nil
	}
	focus := strings.TrimSpace(args)
	notice := "Compacting conversation… (summarizer is running; new turns are queued until it returns)."
	if focus != "" {
		notice = "Compacting (focus: " + focus + ")… (summarizer is running)."
	}
	m.history.Append(Message{Role: RoleSystem, Text: notice})
	m.refreshViewport()

	go func(send programSender, focus string) {
		// /compact is an operator-driven action; bind to background
		// context so a turn-cancel doesn't kill the summarizer.
		res, err := m.agent.Compact(context.Background(), focus)
		send.Send(compactResultMsg{res: res, err: err})
	}(m.program, focus)

	return m, nil
}

// handleCompactResult appends a one-line status message describing
// the outcome. Compaction itself already wrote the summary event to
// the session (visible in the audit log); this is purely operator
// feedback so they know it landed.
func (m *Model) handleCompactResult(msg compactResultMsg) (tea.Model, tea.Cmd) {
	switch {
	case errors.Is(msg.err, agent.ErrNoCompactor):
		m.history.Append(Message{Role: RoleError, Text: "/compact unavailable: this agent was constructed without WithCompactor. Wire `agent.WithCompactor(agent.NewDefaultCompactor())` when constructing the agent."})
	case msg.err != nil:
		m.history.Append(Message{Role: RoleError, Text: "/compact failed: " + msg.err.Error()})
	case msg.res.Skipped:
		m.history.Append(Message{Role: RoleSystem, Text: "/compact: nothing to summarize yet (empty session). Run at least one turn first."})
	default:
		summaryLen := len(msg.res.SummaryText)
		m.history.Append(Message{Role: RoleSystem, Text: fmt.Sprintf(
			"Compacted. Summary written (%d chars, %s). Prior events will be sliced from the next turn's context; the full audit log is preserved in the session.",
			summaryLen, msg.res.Duration.Round(0).String())})
	}
	m.refreshViewport()
	return m, nil
}
