// Copyright 2026 The Cogo Authors.
// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/go-steer/core-agent/config"
	"github.com/go-steer/core-agent/permissions"
)

// TestPermissionsPicker_EmptyApprovalsShowsMessage covers the early
// return path: with no interactive approvals this session, /permissions
// must NOT open an empty modal — it should write a system message into
// chat and stay closed.
func TestPermissionsPicker_EmptyApprovalsShowsMessage(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultConfig()
	m := NewModel(cfg, nil, "dark")
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	m.SessionApprovals = func() []permissions.ApprovalLog { return nil }

	m.handlePermissionsCommand("")
	if m.permissionsPicker != nil {
		t.Fatalf("picker should NOT open with zero approvals; got %+v", m.permissionsPicker)
	}
	last := m.history.Snapshot()[m.history.Len()-1]
	if last.Role != RoleSystem || !strings.Contains(last.Text, "nothing to review") {
		t.Errorf("expected system message about empty approval log; got %+v", last)
	}
}

// TestPermissionsPicker_TogglesAndPersistsChosen pins the full flow:
// recommendations land in the picker, space toggles them, enter
// invokes AddAllowPatterns with the toggled-on patterns ONLY.
//
// DO NOT silence this test. Both halves matter: if toggle gets
// inverted, users persist patterns they didn't pick; if persist
// doesn't fire, the user's confirm has no effect.
func TestPermissionsPicker_TogglesAndPersistsChosen(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultConfig()
	m := NewModel(cfg, nil, "dark")
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})

	now := time.Now()
	m.SessionApprovals = func() []permissions.ApprovalLog {
		return []permissions.ApprovalLog{
			{Tool: "bash", Key: "git status", Decision: permissions.DecisionAllowOnce, At: now},
			{Tool: "bash", Key: "git log", Decision: permissions.DecisionAllowOnce, At: now},
			{Tool: "read_file", Key: "go.mod", Decision: permissions.DecisionAllowOnce, At: now},
		}
	}

	var persisted []string
	m.AddAllowPatterns = func(patterns []string) error {
		persisted = append(persisted, patterns...)
		return nil
	}

	m.handlePermissionsCommand("")
	if m.permissionsPicker == nil {
		t.Fatalf("picker should open with non-empty approval log")
	}
	p := m.permissionsPicker
	if len(p.recs) < 2 {
		t.Fatalf("expected >= 2 recommendations from the test approval set; got %d:\n%+v", len(p.recs), p.recs)
	}

	// Cursor starts at 0; toggle the first row, skip the rest.
	m.handlePermissionsPickerKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{' '}})
	if !p.selected[0] {
		t.Errorf("space did not toggle row 0 on")
	}
	// Press enter; the picker closes and AddAllowPatterns gets
	// called with exactly the chosen pattern.
	m.handlePermissionsPickerKey(tea.KeyMsg{Type: tea.KeyEnter})
	if m.permissionsPicker != nil {
		t.Errorf("picker should close on enter; got %+v", m.permissionsPicker)
	}
	if len(persisted) != 1 {
		t.Fatalf("AddAllowPatterns called with %d patterns; want exactly 1\ngot: %v", len(persisted), persisted)
	}
	if persisted[0] != p.recs[0].Pattern {
		t.Errorf("persisted pattern = %q; want toggled-on row %q", persisted[0], p.recs[0].Pattern)
	}
}

// TestPermissionsPicker_EnterWithNothingSelectedNoOps covers the
// "user opened the picker, looked, decided nothing to add" path:
// AddAllowPatterns must NOT be called with an empty list.
func TestPermissionsPicker_EnterWithNothingSelectedNoOps(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultConfig()
	m := NewModel(cfg, nil, "dark")
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	now := time.Now()
	m.SessionApprovals = func() []permissions.ApprovalLog {
		return []permissions.ApprovalLog{
			{Tool: "bash", Key: "ls -la", Decision: permissions.DecisionAllowOnce, At: now},
		}
	}
	called := false
	m.AddAllowPatterns = func(patterns []string) error {
		called = true
		return nil
	}
	m.handlePermissionsCommand("")
	m.handlePermissionsPickerKey(tea.KeyMsg{Type: tea.KeyEnter}) // no toggles
	if called {
		t.Errorf("AddAllowPatterns should not fire when nothing was toggled on")
	}
}
