// Copyright 2026 The Cogo Authors.
// SPDX-License-Identifier: Apache-2.0

// Package tui implements Cogo's interactive Bubble Tea program.
//
// This file defines the in-memory chat history used by the Model. History
// is intentionally simple: a slice of role-tagged Messages. Persistence
// across sessions is Slice 5 (FR-11.1 transcript writes).
package tui

// Role tags a chat message.
type Role int

const (
	RoleUser      Role = iota // input typed by the human
	RoleAssistant             // model output (raw markdown)
	RoleSystem                // notices like "/clear", "/help" output
	RoleError                 // unrecoverable error from a turn
	RoleTool                  // a tool invocation made by the agent (display-only)
)

// Message is one entry in the chat history.
//
// Text holds the original payload — for assistant messages this is raw
// markdown streamed token-by-token; for everything else it is plain
// text. Rendered is optional and populated for assistant messages once
// the turn completes (see internal/tui/markdown.go).
type Message struct {
	Role     Role
	Text     string
	Rendered string // optional pre-rendered (Glamour) form for assistants
}

// Display returns the form of the message that should be shown in the
// viewport: the rendered form when populated, otherwise the raw Text.
func (m Message) Display() string {
	if m.Rendered != "" {
		return m.Rendered
	}
	return m.Text
}

// History is the append-only chat log for one session.
//
// All operations take O(1) amortized time. Snapshot returns a defensive
// copy; mutators take an index returned by Append (no bounds-checking
// helpers are exposed since callers always use freshly returned indices).
type History struct {
	msgs []Message
}

// Append adds m and returns its index for later in-place mutation
// (AppendText, SetRendered).
func (h *History) Append(m Message) int {
	h.msgs = append(h.msgs, m)
	return len(h.msgs) - 1
}

// AppendText appends chunk to the Text of the message at i. Used to
// accumulate streaming partial events into one in-progress assistant
// message.
func (h *History) AppendText(i int, chunk string) {
	h.msgs[i].Text += chunk
}

// SetRendered replaces the rendered form of the message at i. Called
// once per assistant turn after TurnComplete.
func (h *History) SetRendered(i int, rendered string) {
	h.msgs[i].Rendered = rendered
}

// Reset empties the history. Used by the /clear slash command.
func (h *History) Reset() {
	h.msgs = h.msgs[:0]
}

// Len reports the number of messages.
func (h *History) Len() int { return len(h.msgs) }

// Snapshot returns a copy of the message slice safe to read without
// further mutation by the History.
func (h *History) Snapshot() []Message {
	out := make([]Message, len(h.msgs))
	copy(out, h.msgs)
	return out
}
