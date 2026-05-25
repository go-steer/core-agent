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
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestHandleBTWCommand_EmptyArgsShowsUsage(t *testing.T) {
	t.Parallel()
	m := newOperatorInputTestModel(t)

	m.handleBTWCommand("")

	if m.btwOverlay != nil {
		t.Errorf("overlay should not be set on usage-hint path: %#v", m.btwOverlay)
	}
	last := lastSystemMessage(t, m)
	if !strings.Contains(last, "Usage:") {
		t.Errorf("expected usage hint, got %q", last)
	}
}

func TestHandleBTWCommand_OpensPendingOverlay(t *testing.T) {
	t.Parallel()
	m := newOperatorInputTestModel(t)

	m.handleBTWCommand("what was that file again?")

	if m.btwOverlay == nil {
		t.Fatalf("btwOverlay nil after /btw, want non-nil")
	}
	if !m.btwOverlay.pending {
		t.Errorf("overlay.pending = false, want true (model goroutine still running)")
	}
	if m.btwOverlay.Question != "what was that file again?" {
		t.Errorf("overlay.Question = %q, want %q", m.btwOverlay.Question, "what was that file again?")
	}
	if m.btwOverlay.Answer != "" {
		t.Errorf("overlay.Answer = %q, want empty until result arrives", m.btwOverlay.Answer)
	}
}

func TestHandleBTWResult_PopulatesAnswer(t *testing.T) {
	t.Parallel()
	m := newOperatorInputTestModel(t)
	m.btwOverlay = &btwState{Question: "?", pending: true}

	m.handleBTWResult(btwResultMsg{Answer: "It was main.go."})

	if m.btwOverlay == nil {
		t.Fatalf("overlay cleared unexpectedly")
	}
	if m.btwOverlay.pending {
		t.Errorf("overlay.pending = true after result, want false")
	}
	if m.btwOverlay.Answer != "It was main.go." {
		t.Errorf("overlay.Answer = %q, want %q", m.btwOverlay.Answer, "It was main.go.")
	}
	if m.btwOverlay.Err != nil {
		t.Errorf("overlay.Err = %v, want nil", m.btwOverlay.Err)
	}
}

func TestHandleBTWResult_PopulatesError(t *testing.T) {
	t.Parallel()
	m := newOperatorInputTestModel(t)
	m.btwOverlay = &btwState{Question: "?", pending: true}

	m.handleBTWResult(btwResultMsg{Err: errors.New("model upstream 500")})

	if m.btwOverlay == nil {
		t.Fatalf("overlay cleared unexpectedly")
	}
	if m.btwOverlay.pending {
		t.Errorf("overlay.pending = true after error, want false")
	}
	if m.btwOverlay.Err == nil || !strings.Contains(m.btwOverlay.Err.Error(), "500") {
		t.Errorf("overlay.Err = %v, want one wrapping the 500", m.btwOverlay.Err)
	}
}

func TestHandleBTWResult_NoOverlayIsNoOp(t *testing.T) {
	t.Parallel()
	m := newOperatorInputTestModel(t)
	// Operator dismissed before result arrived; ensure result is dropped without panicking.
	m.handleBTWResult(btwResultMsg{Answer: "late"})
	if m.btwOverlay != nil {
		t.Errorf("overlay should stay nil when result lands post-dismiss, got %#v", m.btwOverlay)
	}
}

func TestHandleBTWKey_DismissKeys(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		key  string
	}{
		{"space-string", " "},
		{"space-named", "space"},
		{"enter", "enter"},
		{"esc", "esc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := newOperatorInputTestModel(t)
			m.btwOverlay = &btwState{Question: "?", Answer: "done"}
			var km tea.KeyMsg
			switch tc.key {
			case "enter":
				km = tea.KeyMsg(tea.Key{Type: tea.KeyEnter})
			case "esc":
				km = tea.KeyMsg(tea.Key{Type: tea.KeyEsc})
			case "space", " ":
				km = tea.KeyMsg(tea.Key{Type: tea.KeySpace})
			default:
				km = tea.KeyMsg(tea.Key{Type: tea.KeyRunes, Runes: []rune(tc.key)})
			}
			m.handleBTWKey(km)
			if m.btwOverlay != nil {
				t.Errorf("overlay not dismissed by %q: %#v", tc.key, m.btwOverlay)
			}
		})
	}
}

func TestHandleBTWKey_SwallowsOtherKeys(t *testing.T) {
	t.Parallel()
	m := newOperatorInputTestModel(t)
	m.btwOverlay = &btwState{Question: "?", Answer: "done"}
	// A random letter must NOT dismiss the overlay and must NOT
	// reach the textarea (the overlay is modal-ish).
	m.handleBTWKey(tea.KeyMsg(tea.Key{Type: tea.KeyRunes, Runes: []rune("a")}))
	if m.btwOverlay == nil {
		t.Errorf("overlay dismissed by 'a', want still up")
	}
	if got := m.textarea.Value(); got != "" {
		t.Errorf("textarea filled by 'a' while overlay was up: %q", got)
	}
}

func TestRenderBTWOverlay_PendingShowsAsking(t *testing.T) {
	t.Parallel()
	m := newOperatorInputTestModel(t)
	m.btwOverlay = &btwState{Question: "ping?", pending: true}
	out := m.renderBTWOverlay()
	if !strings.Contains(out, "ping?") {
		t.Errorf("overlay missing question: %q", out)
	}
	if !strings.Contains(out, "Asking") {
		t.Errorf("overlay missing 'Asking' indicator: %q", out)
	}
}

func TestRenderBTWOverlay_ErrorBeatsAnswer(t *testing.T) {
	t.Parallel()
	m := newOperatorInputTestModel(t)
	m.btwOverlay = &btwState{
		Question: "ping?",
		Answer:   "this should be hidden",
		Err:      errors.New("provider rejected"),
	}
	out := m.renderBTWOverlay()
	if !strings.Contains(out, "provider rejected") {
		t.Errorf("overlay missing error text: %q", out)
	}
	if strings.Contains(out, "this should be hidden") {
		t.Errorf("error overlay leaked Answer text: %q", out)
	}
}

func TestRenderBTWOverlay_NilReturnsEmpty(t *testing.T) {
	t.Parallel()
	m := newOperatorInputTestModel(t)
	if got := m.renderBTWOverlay(); got != "" {
		t.Errorf("nil overlay render = %q, want \"\"", got)
	}
}

func TestParseSlash_BTWAndAlias(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in       string
		wantAct  SlashAction
		wantArgs string
	}{
		{"/btw where is foo?", SlashBTW, "where is foo?"},
		{"/by-the-way recap please", SlashBTW, "recap please"},
		{"/btw", SlashBTW, ""},
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
