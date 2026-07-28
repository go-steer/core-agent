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
	"testing"

	tea "charm.land/bubbletea/v2"
)

// keyPress builds a KeyPressMsg for a printable rune ('j', 'k', 'q', …).
func keyPress(r rune) tea.KeyPressMsg {
	return tea.KeyPressMsg(tea.Key{Code: r, Text: string(r)})
}

// loadedPicker returns a picker with three rows: the "+ New session"
// sentinel plus two real sessions, mirroring what refreshCmd emits.
func loadedPicker(t *testing.T) pickerModel {
	t.Helper()
	m := pickerModel{loading: true}
	m, cmd := m.UpdateInner(pickerSessionsLoadedMsg{sessions: []pickerEntry{
		{Kind: kindCreate, Origin: "local"},
		{SessionID: "sid-1", App: "app-a", User: "u1", Origin: "local"},
		{SessionID: "sid-2", App: "app-b", User: "u2", Origin: "peer-1"},
	}})
	if cmd != nil {
		t.Fatalf("sessions-loaded should not emit a command")
	}
	if m.loading {
		t.Fatalf("picker still loading after pickerSessionsLoadedMsg")
	}
	return m
}

func TestPickerNavigationAndSelect(t *testing.T) {
	m := loadedPicker(t)

	if m.cursor != 0 {
		t.Fatalf("initial cursor = %d, want 0", m.cursor)
	}

	// Down twice ("down" key + vim 'j') moves to the last row.
	m, _ = m.UpdateInner(tea.KeyPressMsg(tea.Key{Code: tea.KeyDown}))
	m, _ = m.UpdateInner(keyPress('j'))
	if m.cursor != 2 {
		t.Fatalf("cursor after two downs = %d, want 2", m.cursor)
	}

	// Down at the bottom clamps.
	m, _ = m.UpdateInner(keyPress('j'))
	if m.cursor != 2 {
		t.Fatalf("cursor after down at bottom = %d, want 2", m.cursor)
	}

	// Up ("up" key + vim 'k') moves back one row.
	m, _ = m.UpdateInner(tea.KeyPressMsg(tea.Key{Code: tea.KeyUp}))
	if m.cursor != 1 {
		t.Fatalf("cursor after up = %d, want 1", m.cursor)
	}

	// Enter on a real row records the selection.
	m, cmd := m.UpdateInner(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	if cmd != nil {
		t.Fatalf("enter on a session row should not emit a command")
	}
	if m.selected == nil {
		t.Fatalf("enter did not set selected")
	}
	if got, want := m.selected.sessionPath(), "/sessions/app-a/sid-1"; got != want {
		t.Fatalf("selected sessionPath = %q, want %q", got, want)
	}
}

func TestPickerUpClampsAtTop(t *testing.T) {
	m := loadedPicker(t)
	m, _ = m.UpdateInner(keyPress('k'))
	if m.cursor != 0 {
		t.Fatalf("cursor after up at top = %d, want 0", m.cursor)
	}
}

func TestPickerQuitKeys(t *testing.T) {
	for _, msg := range []tea.KeyPressMsg{
		keyPress('q'),
		tea.KeyPressMsg(tea.Key{Code: tea.KeyEscape}),
	} {
		m := loadedPicker(t)
		m, cmd := m.UpdateInner(msg)
		if cmd == nil {
			t.Fatalf("key %q should emit tea.Quit", tea.Key(msg).String())
		}
		if _, ok := cmd().(tea.QuitMsg); !ok {
			t.Fatalf("key %q emitted %T, want tea.QuitMsg", tea.Key(msg).String(), cmd())
		}
		if m.selected != nil {
			t.Fatalf("quit key %q should not select a session", tea.Key(msg).String())
		}
	}
}

func TestPickerKeysIgnoredWhileLoading(t *testing.T) {
	m := pickerModel{loading: true}
	m, cmd := m.UpdateInner(keyPress('q'))
	if cmd != nil {
		t.Fatalf("keys while loading should be ignored, got a command")
	}
	if !m.loading {
		t.Fatalf("loading flag flipped by an ignored key")
	}
}

func TestPickerLoadErrorThenRetry(t *testing.T) {
	m := pickerModel{loading: true}
	m, _ = m.UpdateInner(pickerSessionsLoadedMsg{err: errFake})
	if m.loading {
		t.Fatalf("picker still loading after error")
	}
	if m.error == "" {
		t.Fatalf("error not surfaced")
	}
	// A successful reload clears the error.
	m, _ = m.UpdateInner(pickerSessionsLoadedMsg{sessions: []pickerEntry{{Kind: kindCreate}}})
	if m.error != "" {
		t.Fatalf("error not cleared on successful reload: %q", m.error)
	}
}

func TestPickerSessionCreated(t *testing.T) {
	m := loadedPicker(t)
	m.loading = true // as set by the enter-on-sentinel path
	m, cmd := m.UpdateInner(pickerSessionCreatedMsg{entry: pickerEntry{
		App: "app-a", SessionID: "fresh", Origin: "local",
	}})
	if cmd != nil {
		t.Fatalf("session-created should not emit a command")
	}
	if m.loading {
		t.Fatalf("picker still loading after pickerSessionCreatedMsg")
	}
	if m.selected == nil || m.selected.sessionPath() != "/sessions/app-a/fresh" {
		t.Fatalf("created session not auto-selected: %+v", m.selected)
	}

	// Error path surfaces inline and leaves nothing selected.
	m2 := loadedPicker(t)
	m2.loading = true
	m2, _ = m2.UpdateInner(pickerSessionCreatedMsg{err: errFake})
	if m2.selected != nil {
		t.Fatalf("failed create must not select a session")
	}
	if m2.error == "" {
		t.Fatalf("create error not surfaced")
	}
}

// errFake is a sentinel error for load/create failure paths.
var errFake = errFakeType{}

type errFakeType struct{}

func (errFakeType) Error() string { return "boom" }
