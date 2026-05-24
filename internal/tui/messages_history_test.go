// Copyright 2026 The Cogo Authors.
// SPDX-License-Identifier: Apache-2.0

package tui

import "testing"

func TestHistory_AppendAndSnapshot(t *testing.T) {
	t.Parallel()
	var h History
	if h.Len() != 0 {
		t.Fatalf("fresh history len = %d, want 0", h.Len())
	}
	i := h.Append(Message{Role: RoleUser, Text: "hi"})
	if i != 0 {
		t.Errorf("first append index = %d, want 0", i)
	}
	if h.Len() != 1 {
		t.Errorf("len after append = %d, want 1", h.Len())
	}
	snap := h.Snapshot()
	if len(snap) != 1 || snap[0].Text != "hi" {
		t.Fatalf("snapshot = %#v", snap)
	}
	// Mutating the snapshot must not affect the history.
	snap[0].Text = "mutated"
	if h.Snapshot()[0].Text != "hi" {
		t.Errorf("snapshot mutation leaked back into history")
	}
}

func TestHistory_AppendText_Accumulates(t *testing.T) {
	t.Parallel()
	var h History
	i := h.Append(Message{Role: RoleAssistant})
	h.AppendText(i, "Hello, ")
	h.AppendText(i, "world!")
	if got := h.Snapshot()[i].Text; got != "Hello, world!" {
		t.Errorf("text after appends = %q, want %q", got, "Hello, world!")
	}
}

func TestHistory_SetRendered_DoesNotAlterText(t *testing.T) {
	t.Parallel()
	var h History
	i := h.Append(Message{Role: RoleAssistant, Text: "**bold**"})
	h.SetRendered(i, "BOLD")
	got := h.Snapshot()[i]
	if got.Text != "**bold**" {
		t.Errorf("Text changed: %q", got.Text)
	}
	if got.Rendered != "BOLD" {
		t.Errorf("Rendered = %q, want %q", got.Rendered, "BOLD")
	}
	if got.Display() != "BOLD" {
		t.Errorf("Display() = %q, want rendered form", got.Display())
	}
}

func TestMessage_DisplayPrefersRendered(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		m    Message
		want string
	}{
		{"only text", Message{Text: "hi"}, "hi"},
		{"rendered set", Message{Text: "**hi**", Rendered: "HI"}, "HI"},
		{"empty", Message{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.m.Display(); got != tc.want {
				t.Errorf("Display() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestHistory_Reset_ClearsAndPreservesCapacity(t *testing.T) {
	t.Parallel()
	var h History
	for i := 0; i < 5; i++ {
		h.Append(Message{Role: RoleUser, Text: "msg"})
	}
	h.Reset()
	if h.Len() != 0 {
		t.Errorf("len after reset = %d, want 0", h.Len())
	}
	// New appends should still work after reset.
	h.Append(Message{Role: RoleUser, Text: "new"})
	if h.Snapshot()[0].Text != "new" {
		t.Errorf("post-reset append failed")
	}
}
