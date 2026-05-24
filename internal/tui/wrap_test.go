// Copyright 2026 The Cogo Authors.
// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/go-steer/core-agent/config"
)

// TestWrapForChat covers the wrapping helper directly: width gates,
// indentation applied to continuation lines only, and the no-op path
// for pre-resize callers.
func TestWrapForChat(t *testing.T) {
	t.Parallel()
	long := "the quick brown fox jumps over the lazy dog every single morning"
	got := wrapForChat(long, 20, "  ")
	lines := strings.Split(got, "\n")
	if len(lines) < 2 {
		t.Fatalf("expected wrapping at width 20; got 1 line: %q", got)
	}
	for i, ln := range lines {
		visible := len(ln)
		if i > 0 {
			if !strings.HasPrefix(ln, "  ") {
				t.Errorf("continuation line %d missing indent: %q", i, ln)
			}
			// strip indent for width check
			visible -= 2
		}
		if visible > 20 {
			t.Errorf("line %d exceeds width 20 (%d chars): %q", i, visible, ln)
		}
	}

	// width <= 0 short-circuits to original text (pre-resize path).
	if got := wrapForChat("anything", 0, "  "); got != "anything" {
		t.Errorf("width=0 should return text untouched; got %q", got)
	}
	if got := wrapForChat("anything", -1, "  "); got != "anything" {
		t.Errorf("width<0 should return text untouched; got %q", got)
	}
}

// TestRenderMessage_LongUserPromptWraps pins the user-visible bug:
// long prompts must wrap inside the chat viewport instead of running
// off the right edge. Failure mode: user types a paragraph, sends it,
// and only the first ~viewport-width characters are visible while the
// rest disappears past the screen edge.
//
// DO NOT silence this test. The wrap is the whole point.
func TestRenderMessage_LongUserPromptWraps(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultConfig()
	m := NewModel(cfg, nil, "dark")
	m.Update(tea.WindowSizeMsg{Width: 60, Height: 24})

	long := "this is a moderately long prompt that absolutely must wrap inside the chat viewport otherwise users see only a slice of it"
	m.history.Append(Message{Role: RoleUser, Text: long})
	m.refreshViewport()
	body := stripANSI(m.viewport.View())
	lines := strings.Split(body, "\n")

	// At least two non-empty lines should contain prompt content.
	hits := 0
	for _, ln := range lines {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		if strings.Contains(ln, "wrap") || strings.Contains(ln, "viewport") || strings.Contains(ln, "moderately") {
			hits++
		}
	}
	if hits < 2 {
		t.Errorf("long prompt did not wrap across multiple chat rows; only %d row(s) contained prompt fragments.\nbody:\n%s", hits, body)
	}
}
