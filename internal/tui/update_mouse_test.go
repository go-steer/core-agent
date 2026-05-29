// Copyright 2026 The Cogo Authors.
// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"fmt"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/go-steer/core-agent/pkg/config"
)

// TestUpdate_MouseWheelScrollsViewport_NotMouseClicks pins the routing
// contract for tea.MouseMsg in Model.Update:
//
//   - Wheel events MUST reach the viewport so the chat scrolls.
//   - Non-wheel mouse events (clicks, drags, motion) MUST be dropped so
//     stray clicks don't disturb scroll position or activate the input.
//
// DO NOT "fix" this test by relaxing the assertions if a future change
// breaks it. A failure here is the same bug users hit when they say
// "scrolling is broken" — the original incident that motivated mouse
// capture being wired in the first place. If new code legitimately
// changes how mouse routing works, replace these assertions with ones
// that prove the new code path still scrolls and still ignores clicks.
// Never delete the contract.
func TestUpdate_MouseWheelScrollsViewport_NotMouseClicks(t *testing.T) {
	t.Parallel()

	cfg := config.DefaultConfig()
	m := NewModel(cfg, nil, "dark")

	// Give the viewport a defined size, then load enough history that
	// the content overflows the visible window — a viewport with all
	// content already on-screen has no room to scroll.
	if _, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 24}); m.viewport.Height <= 0 {
		t.Fatalf("setup: viewport height not initialized after WindowSizeMsg")
	}
	for i := 0; i < 100; i++ {
		m.history.Append(Message{Role: RoleUser, Text: fmt.Sprintf("history line %03d", i)})
	}
	m.refreshViewport()
	m.viewport.GotoBottom()
	if !m.viewport.AtBottom() {
		t.Fatalf("setup: viewport should start at bottom; YOffset=%d", m.viewport.YOffset)
	}

	// 1) Wheel-up scrolls away from the bottom.
	m.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonWheelUp})
	if m.viewport.AtBottom() {
		t.Fatalf("wheel-up did not scroll the viewport (still at bottom)")
	}
	afterWheelUp := m.viewport.YOffset

	// 2) Non-wheel mouse events are ignored — viewport state must not move.
	m.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: 5, Y: 5})
	m.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonRight, X: 5, Y: 5})
	m.Update(tea.MouseMsg{Action: tea.MouseActionMotion, Button: tea.MouseButtonNone, X: 6, Y: 6})
	m.Update(tea.MouseMsg{Action: tea.MouseActionRelease, Button: tea.MouseButtonLeft, X: 5, Y: 5})
	if m.viewport.YOffset != afterWheelUp {
		t.Fatalf("non-wheel mouse events moved YOffset: was %d, now %d", afterWheelUp, m.viewport.YOffset)
	}

	// 3) Wheel-down brings us back toward the bottom. Repeat enough to
	// cover any line-stride the viewport applies per wheel event.
	for i := 0; i < 200; i++ {
		m.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonWheelDown})
	}
	if !m.viewport.AtBottom() {
		t.Fatalf("wheel-down did not scroll back to bottom; YOffset=%d", m.viewport.YOffset)
	}
}
