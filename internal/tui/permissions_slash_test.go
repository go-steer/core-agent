// Copyright 2026 The Cogo Authors.
// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/go-steer/core-agent/config"
	"github.com/go-steer/core-agent/permissions"
)

// TestHandleAllow_Pattern pins the happy path for `/allow <pattern>`:
// the configured hook runs with the verbatim pattern, the chat shows
// a confirmation, and the system message hints that no /reload is
// needed. If this regresses, every user who runs /allow has to either
// /reload or restart for the new permission to actually fire — the
// original UX bug we just fixed. DO NOT delete this test to silence a
// compile failure.
func TestHandleAllow_Pattern(t *testing.T) {
	t.Parallel()
	m := newSlashTestModel(t)
	var got []string
	m.AddAllowPatterns = func(patterns []string) error {
		got = append(got, patterns...)
		return nil
	}

	m.handleAllowCommand("bash:git *")

	if len(got) != 1 || got[0] != "bash:git *" {
		t.Fatalf("AddAllowPatterns called with %v, want [bash:git *]", got)
	}
	last := lastSystemMessage(t, m)
	if !strings.Contains(last, "bash:git *") {
		t.Errorf("system message %q should echo the pattern", last)
	}
	if !strings.Contains(last, "applies immediately") {
		t.Errorf("system message %q should mention immediate application", last)
	}
}

// TestHandleAllow_Bundle pins the bundle-toggle UX: typing
// `/allow bundle:dev_tools` must hit the bundle-specific hook (which
// persists the BUNDLE NAME in builtin_allow_extras, not the expanded
// patterns) rather than the pattern hook. If these get swapped, the
// user's cogo.json grows by 16 dev_tools entries instead of one bundle
// name, defeating the "enable a curated set" intent.
func TestHandleAllow_Bundle(t *testing.T) {
	t.Parallel()
	m := newSlashTestModel(t)
	var pat, bundle string
	m.AddAllowPatterns = func(patterns []string) error {
		pat = strings.Join(patterns, ",")
		return nil
	}
	m.AddBuiltinAllowExtra = func(name string) error {
		bundle = name
		return nil
	}

	m.handleAllowCommand("bundle:dev_tools")

	if bundle != "dev_tools" {
		t.Errorf("AddBuiltinAllowExtra got %q, want \"dev_tools\"", bundle)
	}
	if pat != "" {
		t.Errorf("AddAllowPatterns must not fire for bundle form; got %q", pat)
	}
}

func TestHandleAllow_UsageWhenEmpty(t *testing.T) {
	t.Parallel()
	m := newSlashTestModel(t)
	called := false
	m.AddAllowPatterns = func([]string) error { called = true; return nil }

	m.handleAllowCommand("")

	if called {
		t.Error("empty arg must not hit the persistence hook")
	}
	if !strings.Contains(lastSystemMessage(t, m), "Usage:") {
		t.Errorf("expected usage hint, got %q", lastSystemMessage(t, m))
	}
}

func TestHandleAllow_NoProjectRoot(t *testing.T) {
	t.Parallel()
	m := newSlashTestModel(t)
	// Hooks left nil to simulate running without an .agents/ dir.
	m.handleAllowCommand("bash:git *")
	if !strings.Contains(lastSystemMessage(t, m), "no project root") {
		t.Errorf("expected helpful no-project-root message, got %q", lastSystemMessage(t, m))
	}
}

func TestHandleAllow_PersistError(t *testing.T) {
	t.Parallel()
	m := newSlashTestModel(t)
	m.AddAllowPatterns = func([]string) error {
		return permissions.ErrNoPrompter // any non-nil error
	}
	m.handleAllowCommand("bash:foo *")
	last := lastSystemMessage(t, m)
	if !strings.Contains(last, "Couldn't add allow pattern") {
		t.Errorf("expected error surface in chat, got %q", last)
	}
}

func TestHandleDeny_Pattern(t *testing.T) {
	t.Parallel()
	m := newSlashTestModel(t)
	var got []string
	m.AddDenyPatterns = func(patterns []string) error {
		got = append(got, patterns...)
		return nil
	}

	m.handleDenyCommand("bash:curl *")

	if len(got) != 1 || got[0] != "bash:curl *" {
		t.Fatalf("AddDenyPatterns called with %v, want [bash:curl *]", got)
	}
	if !strings.Contains(lastSystemMessage(t, m), "deny wins over any allow rule") {
		t.Errorf("system message should remind users about deny precedence: %q", lastSystemMessage(t, m))
	}
}

// TestHandlePermissionsList prints a read-only snapshot. The output
// shape (labels and the "(empty)" placeholder) is exercised so a
// future refactor doesn't accidentally swallow categories.
func TestHandlePermissionsList(t *testing.T) {
	t.Parallel()
	m := newSlashTestModel(t)
	m.cfg.Permissions.Allow = []string{"bash:make build", "read_file:internal/**"}
	m.cfg.Permissions.Deny = []string{"bash:curl *"}
	off := false
	m.cfg.Permissions.UseBuiltinAllow = &off
	m.cfg.Permissions.BuiltinAllowExtras = []string{"dev_tools"}
	m.cfg.Permissions.Mode = "ask"

	m.handlePermissionsCommand("list")
	out := lastSystemMessage(t, m)

	wantSubs := []string{
		"Permission mode: ask",
		"Built-in allow: disabled",
		"extra bundles: dev_tools",
		"permissions.allow (2):",
		"bash:make build",
		"read_file:internal/**",
		"permissions.deny (1):",
		"bash:curl *",
	}
	for _, want := range wantSubs {
		if !strings.Contains(out, want) {
			t.Errorf("/permissions list output missing %q in:\n%s", want, out)
		}
	}
}

func TestHandlePermissionsList_EmptyLists(t *testing.T) {
	t.Parallel()
	m := newSlashTestModel(t)
	m.handlePermissionsCommand("list")
	out := lastSystemMessage(t, m)
	if !strings.Contains(out, "permissions.allow: (empty)") {
		t.Errorf("expected (empty) placeholder for allow; got:\n%s", out)
	}
	if !strings.Contains(out, "permissions.deny: (empty)") {
		t.Errorf("expected (empty) placeholder for deny; got:\n%s", out)
	}
}

// TestParseSlash_AllowDeny confirms the new command names route to
// the right SlashAction. The aliases map drives palette + handler
// dispatch, so a typo here silently dead-ends user input.
func TestParseSlash_AllowDeny(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in        string
		wantAct   SlashAction
		wantArgs  string
		wantIsCmd bool
	}{
		{"/allow bash:git *", SlashAllow, "bash:git *", true},
		{"/allow bundle:dev_tools", SlashAllow, "bundle:dev_tools", true},
		{"/deny bash:curl *", SlashDeny, "bash:curl *", true},
		{"/permissions list", SlashPermissions, "list", true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			act, _, args, ok := ParseSlash(tc.in)
			if !ok || act != tc.wantAct {
				t.Errorf("ParseSlash(%q) = (%v, ok=%v), want (%v, true)", tc.in, act, ok, tc.wantAct)
			}
			if args != tc.wantArgs {
				t.Errorf("ParseSlash(%q) args = %q, want %q", tc.in, args, tc.wantArgs)
			}
		})
	}
}

// newSlashTestModel builds a bare Model without a running Bubble Tea
// program. The slash-handler methods we exercise here mutate model
// state synchronously from the test goroutine, so wrapping in teatest
// would race with its event loop. Mirrors the pattern in
// permissions_picker_test.go.
func newSlashTestModel(t *testing.T) *Model {
	t.Helper()
	cfg := config.DefaultConfig()
	m := NewModel(cfg, nil, "dark")
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	return m
}

func lastSystemMessage(t *testing.T, m *Model) string {
	t.Helper()
	msgs := m.history.Snapshot()
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == RoleSystem || msgs[i].Role == RoleError {
			return msgs[i].Text
		}
	}
	t.Fatal("no system/error message recorded in history")
	return ""
}
