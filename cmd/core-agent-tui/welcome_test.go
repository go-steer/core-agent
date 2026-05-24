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
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// keyMsg builds a tea.KeyMsg from a single rune so tests stay
// readable (avoids tea.KeyMsg{Runes: []rune{'k'}} repetition).
func keyMsg(r rune) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}
}

// keyType builds a tea.KeyMsg for a non-rune key.
func keyType(t tea.KeyType) tea.KeyMsg {
	return tea.KeyMsg{Type: t}
}

// typeString sends each rune of s through UpdateInner so the
// textinput accumulates the value.
func typeString(m welcomeModel, s string) welcomeModel {
	for _, r := range s {
		m, _ = m.UpdateInner(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	return m
}

// TestWelcome_SpawnCommandHelpfullyRejects pins the v2 transition
// behavior: typing /spawn should no longer trigger a spawn (the
// machinery was removed; use `core-agent` directly). The TUI
// surfaces a "removed in v2" hint rather than just an "unknown
// command" so operators with muscle memory know where to go.
func TestWelcome_SpawnCommandHelpfullyRejects(t *testing.T) {
	t.Parallel()
	m := newWelcomeModel()
	m = typeString(m, "/spawn")
	m, _ = m.UpdateInner(keyType(tea.KeyEnter))
	if m.chosen != nil {
		t.Fatalf("/spawn should NOT set chosen in v2; got %+v", m.chosen)
	}
	if !strings.Contains(m.error, "removed") || !strings.Contains(m.error, "core-agent") {
		t.Errorf("expected migration hint mentioning 'removed' + 'core-agent'; got %q", m.error)
	}
}

func TestWelcome_AttachCommandSubmitsURL(t *testing.T) {
	t.Parallel()
	m := newWelcomeModel()
	m = typeString(m, "/attach http://localhost:7777")
	m, _ = m.UpdateInner(keyType(tea.KeyEnter))
	if m.chosen == nil {
		t.Fatal("chosen not set after /attach + Enter")
	}
	if m.chosen.RemoteURL != "http://localhost:7777" {
		t.Errorf("RemoteURL = %q", m.chosen.RemoteURL)
	}
}

func TestWelcome_AttachRejectsBadScheme(t *testing.T) {
	t.Parallel()
	m := newWelcomeModel()
	m = typeString(m, "/attach ftp://nope")
	m, _ = m.UpdateInner(keyType(tea.KeyEnter))
	if m.chosen != nil {
		t.Errorf("chosen set despite bad scheme: %+v", m.chosen)
	}
	if !strings.Contains(m.error, "http://, https://, or unix://") {
		t.Errorf("error doesn't mention valid schemes: %q", m.error)
	}
}

func TestWelcome_BareURLAcceptedAsAttach(t *testing.T) {
	t.Parallel()
	m := newWelcomeModel()
	// No /attach prefix — bare URL should be coerced.
	m = typeString(m, "http://localhost:8080")
	m, _ = m.UpdateInner(keyType(tea.KeyEnter))
	if m.chosen == nil {
		t.Fatal("chosen not set for bare URL")
	}
	if m.chosen.RemoteURL != "http://localhost:8080" {
		t.Errorf("RemoteURL = %q", m.chosen.RemoteURL)
	}
}

func TestWelcome_UnknownCommandReportsError(t *testing.T) {
	t.Parallel()
	m := newWelcomeModel()
	m = typeString(m, "/wat")
	m, _ = m.UpdateInner(keyType(tea.KeyEnter))
	if m.chosen != nil {
		t.Errorf("chosen set for unknown command: %+v", m.chosen)
	}
	if !strings.Contains(m.error, "unknown command") {
		t.Errorf("error doesn't mention 'unknown command': %q", m.error)
	}
}

func TestWelcome_EmptyEnterReportsHint(t *testing.T) {
	t.Parallel()
	m := newWelcomeModel()
	m, _ = m.UpdateInner(keyType(tea.KeyEnter))
	if m.chosen != nil {
		t.Errorf("chosen set on empty Enter: %+v", m.chosen)
	}
	if !strings.Contains(m.error, "/attach") {
		t.Errorf("empty-Enter error should mention /attach: %q", m.error)
	}
}

func TestWelcome_HelpShowsHint(t *testing.T) {
	t.Parallel()
	m := newWelcomeModel()
	m = typeString(m, "/help")
	m, _ = m.UpdateInner(keyType(tea.KeyEnter))
	if m.chosen != nil {
		t.Errorf("/help set chosen unexpectedly: %+v", m.chosen)
	}
	if m.hint == "" {
		t.Errorf("/help should populate m.hint")
	}
}

func TestWelcome_EscClearsInputThenQuits(t *testing.T) {
	t.Parallel()
	m := newWelcomeModel()
	m = typeString(m, "/spawn")
	// First Esc should clear the input, not quit.
	m, cmd := m.UpdateInner(keyType(tea.KeyEsc))
	if m.input.Value() != "" {
		t.Errorf("first Esc didn't clear input: %q", m.input.Value())
	}
	if cmd != nil {
		// tea.Quit returns a non-nil cmd; first Esc shouldn't.
		t.Errorf("first Esc returned a cmd; want nil (cmd=%T)", cmd)
	}
	// Second Esc on empty input should quit.
	_, cmd = m.UpdateInner(keyType(tea.KeyEsc))
	if cmd == nil {
		t.Errorf("second Esc on empty input didn't return a quit cmd")
	}
}

func TestWelcome_View_RendersErrorAndCheatSheet(t *testing.T) {
	t.Parallel()
	m := newWelcomeModel()
	m.SetSize(80, 24)
	m.error = "core-agent binary not found"
	out := m.View()
	for _, want := range []string{
		"no endpoint selected",
		"/attach",
		"/help",
		"core-agent binary not found",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("view missing %q:\n%s", want, out)
		}
	}
}

func TestWelcome_KeystrokeClearsStaleError(t *testing.T) {
	t.Parallel()
	m := newWelcomeModel()
	m.error = "old error"
	m, _ = m.UpdateInner(keyMsg('a'))
	if m.error != "" {
		t.Errorf("keystroke didn't clear stale error: %q", m.error)
	}
}
