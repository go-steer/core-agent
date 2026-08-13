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
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/google/uuid"

	"github.com/go-steer/core-agent/v2/internal/attachclient"
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

// v7ID builds a UUIDv7 whose embedded timestamp is exactly ms, so the
// ordering tests can lay sessions out on a timeline without waiting.
func v7ID(t *testing.T, ms int64) string {
	t.Helper()
	var b [16]byte
	for i := 0; i < 6; i++ {
		b[i] = byte(ms >> (40 - 8*i))
	}
	b[6] = 0x70 | (b[6] & 0x0f) // version 7
	b[8] = 0x80 | (b[8] & 0x3f) // RFC 9562 variant
	u, err := uuid.FromBytes(b[:])
	if err != nil {
		t.Fatalf("FromBytes: %v", err)
	}
	return u.String()
}

func TestUUIDv7Time(t *testing.T) {
	want := time.UnixMilli(1_770_000_000_000).UTC()
	got, ok := uuidV7Time(v7ID(t, want.UnixMilli()))
	if !ok {
		t.Fatalf("v7 ID not decoded")
	}
	if !got.Equal(want) {
		t.Errorf("uuidV7Time = %v, want %v", got.UTC(), want)
	}
	// Non-v7 IDs aren't an error — listeners may hand out anything,
	// including a v4 UUID from the uuid.NewString fallback path.
	for _, id := range []string{"default", "", "not-a-uuid", uuid.New().String()} {
		if _, ok := uuidV7Time(id); ok {
			t.Errorf("uuidV7Time(%q) reported a creation time", id)
		}
	}
}

func TestOrderEntriesNewestFirst(t *testing.T) {
	base := int64(1_770_000_000_000)
	oldest := v7ID(t, base)
	middle := v7ID(t, base+60_000)
	newest := v7ID(t, base+120_000)

	got := orderEntries([]pickerEntry{
		{SessionID: middle, App: "core-agent", Origin: "local"},
		{SessionID: oldest, App: "core-agent", Origin: "peer-1"},
		{Kind: kindCreate, Origin: "local"},
		{SessionID: newest, App: "core-agent", Origin: "local"},
	})

	want := []string{"", newest, middle, oldest}
	for i, w := range want {
		if got[i].SessionID != w {
			t.Fatalf("row %d = %q, want %q (full order: %v)", i, got[i].SessionID, w, ids(got))
		}
	}
	if got[0].Kind != kindCreate {
		t.Errorf("+ New session sentinel is not pinned to the top")
	}
	if !got[1].CreatedAt.Equal(time.UnixMilli(base + 120_000)) {
		t.Errorf("CreatedAt not stamped from the session ID: %v", got[1].CreatedAt)
	}
}

func TestOrderEntriesFallsBackToLastTouched(t *testing.T) {
	base := int64(1_770_000_000_000)
	v7 := v7ID(t, base)
	created := time.UnixMilli(base)

	got := orderEntries([]pickerEntry{
		{SessionID: "no-timestamps", App: "core-agent"},
		{SessionID: v7, App: "core-agent"},
		// Hand-picked ID, but the listener reports activity after the
		// v7 session was created, so it belongs above it.
		{SessionID: "default", App: "core-agent", LastTouchedAt: created.Add(time.Hour)},
	})

	want := []string{"default", v7, "no-timestamps"}
	for i, w := range want {
		if got[i].SessionID != w {
			t.Fatalf("row %d = %q, want %q (full order: %v)", i, got[i].SessionID, w, ids(got))
		}
	}
}

// TestOrderEntriesIsStable pins the fix for the real complaint: the
// same set of sessions must come back in the same order every
// refresh, whatever order the local + peer fetches happened to
// produce.
func TestOrderEntriesIsStable(t *testing.T) {
	base := int64(1_770_000_000_000)
	rows := []pickerEntry{
		{Kind: kindCreate},
		{SessionID: v7ID(t, base), App: "a", Origin: "peer-1"},
		{SessionID: v7ID(t, base+1000), App: "b", Origin: "local"},
		{SessionID: "default", App: "a", Origin: "local"},
		{SessionID: v7ID(t, base+2000), App: "a", Origin: "peer-2"},
	}
	first := ids(orderEntries(rows))
	shuffled := []pickerEntry{rows[3], rows[4], rows[0], rows[2], rows[1]}
	if second := ids(orderEntries(shuffled)); !slices.Equal(first, second) {
		t.Errorf("order depends on fetch order:\n first: %v\nsecond: %v", first, second)
	}
}

func TestPickerViewAlignsColumns(t *testing.T) {
	base := int64(1_770_000_000_000)
	parsed, err := attachclient.ParseURL("http://127.0.0.1:7777")
	if err != nil {
		t.Fatalf("ParseURL: %v", err)
	}
	m := pickerModel{client: attachclient.New(parsed, "", time.Second), width: 100, height: 24, loading: true}
	m, _ = m.UpdateInner(pickerSessionsLoadedMsg{sessions: []pickerEntry{
		{Kind: kindCreate, Origin: "local"},
		{SessionID: v7ID(t, base), App: "core-agent", User: "platform-oncall@example.com", Origin: "local"},
		{SessionID: v7ID(t, base+1000), App: "core-agent", User: "u", Origin: "peer-west"},
	}})

	var header string
	var rows []string
	for _, line := range strings.Split(stripANSI(m.View()), "\n") {
		switch {
		case strings.Contains(line, "SESSION") && strings.Contains(line, "ORIGIN"):
			header = line
		case strings.Contains(line, "local") && strings.Contains(line, "core-agent"),
			strings.Contains(line, "peer-west"):
			rows = append(rows, line)
		}
	}
	if header == "" || len(rows) != 2 {
		t.Fatalf("unexpected view shape:\n%s", stripANSI(m.View()))
	}
	want := displayIndex(header, "USER")
	for _, r := range rows {
		// Cells start where their header starts, whatever the row's
		// content length.
		if got := displayIndex(r, "platform-oncall@example.com"); got >= 0 && got != want {
			t.Errorf("USER cell at column %d, header at %d\nheader: %q\nrow:    %q", got, want, header, r)
		}
	}
}

// ansiRE matches the SGR sequences lipgloss wraps styled cells in;
// column positions are only meaningful once they're gone.
var ansiRE = regexp.MustCompile("\x1b\\[[0-9;]*m")

func stripANSI(s string) string { return ansiRE.ReplaceAllString(s, "") }

// ids projects session IDs for order assertions.
func ids(entries []pickerEntry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.SessionID
	}
	return out
}

// errFake is a sentinel error for load/create failure paths.
var errFake = errFakeType{}

type errFakeType struct{}

func (errFakeType) Error() string { return "boom" }
