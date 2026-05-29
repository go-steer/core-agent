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

// doneResultMsg carries the outcome of an asynchronous /done
// invocation back to the Update loop. Same shape as
// compactResultMsg — both are slicing-boundary operations.
type doneResultMsg struct {
	res agent.CheckpointResult
	err error
}

// handleDoneCommand dispatches /done [note] (alias /checkpoint).
// Spawns a goroutine that calls Agent.Checkpoint and posts a
// doneResultMsg when complete — same sidestep-the-freeze pattern
// as /compact. See docs/context-management-design.md §Mechanism C.
func (m *Model) handleDoneCommand(args string) (tea.Model, tea.Cmd) {
	if m.agent == nil {
		m.history.Append(Message{Role: RoleError, Text: "/done unavailable: no agent constructed."})
		m.refreshViewport()
		return m, nil
	}
	note := strings.TrimSpace(args)
	notice := "Writing checkpoint… (summarizer is running; the next turn will start with a clean context window)."
	if note != "" {
		notice = "Writing checkpoint (note: " + note + ")… (summarizer is running)."
	}
	m.history.Append(Message{Role: RoleSystem, Text: notice})
	m.refreshViewport()

	go func(send programSender, note string) {
		// Operator-driven action; bind to background so a turn-
		// cancel doesn't kill the summarizer.
		res, err := m.agent.Checkpoint(context.Background(), note)
		send.Send(doneResultMsg{res: res, err: err})
	}(m.program, note)

	return m, nil
}

// handleDoneResult appends a one-line status message describing
// the outcome. The checkpoint event landed in the session
// already (visible in the audit log); this is purely operator
// feedback.
func (m *Model) handleDoneResult(msg doneResultMsg) (tea.Model, tea.Cmd) {
	switch {
	case errors.Is(msg.err, agent.ErrNoCheckpointer):
		m.history.Append(Message{Role: RoleError, Text: "/done unavailable: this agent was constructed without WithCheckpointer. Wire `agent.WithCheckpointer(agent.NewDefaultCheckpointer())` when constructing the agent."})
	case msg.err != nil:
		m.history.Append(Message{Role: RoleError, Text: "/done failed: " + msg.err.Error()})
	case msg.res.Skipped:
		m.history.Append(Message{Role: RoleSystem, Text: "/done: nothing to checkpoint yet (empty session). Run at least one turn first."})
	default:
		noteFragment := ""
		if msg.res.TaskNote != "" {
			noteFragment = " (note: " + msg.res.TaskNote + ")"
		}
		m.history.Append(Message{Role: RoleSystem, Text: fmt.Sprintf(
			"Checkpoint written%s. Summary captured (%d chars, %s). Prior task events will be sliced from the next turn's context; the full audit log is preserved in the session.",
			noteFragment, len(msg.res.SummaryText), msg.res.Duration.Round(0).String())})
	}
	m.refreshViewport()
	return m, nil
}
