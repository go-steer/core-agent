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

func TestWelcome_DownArrowMovesCursor(t *testing.T) {
	t.Parallel()
	m := newWelcomeModel()
	if m.cursor != 0 {
		t.Fatalf("initial cursor = %d, want 0", m.cursor)
	}
	m, _ = m.UpdateInner(keyMsg('j'))
	if m.cursor != 1 {
		t.Errorf("after j: cursor = %d, want 1", m.cursor)
	}
	m, _ = m.UpdateInner(keyMsg('j'))
	if m.cursor != 1 {
		t.Errorf("at bottom: cursor = %d, want 1 (clamped)", m.cursor)
	}
	m, _ = m.UpdateInner(keyMsg('k'))
	if m.cursor != 0 {
		t.Errorf("after k: cursor = %d, want 0", m.cursor)
	}
}

func TestWelcome_EnterLocalSetsChosen(t *testing.T) {
	t.Parallel()
	m := newWelcomeModel()
	// cursor starts at 0 = local spawn
	m, _ = m.UpdateInner(keyType(tea.KeyEnter))
	if m.chosen == nil {
		t.Fatal("chosen not set after Enter on local")
	}
	if !m.chosen.LocalSpawn {
		t.Errorf("LocalSpawn = false, want true")
	}
	if m.chosen.RemoteURL != "" {
		t.Errorf("RemoteURL = %q, want empty", m.chosen.RemoteURL)
	}
	if m.stage != welcomeSpawning {
		t.Errorf("stage = %v, want welcomeSpawning", m.stage)
	}
}

func TestWelcome_EnterRemoteEntersURLStage(t *testing.T) {
	t.Parallel()
	m := newWelcomeModel()
	m, _ = m.UpdateInner(keyMsg('j'))           // cursor → 1 (remote)
	m, _ = m.UpdateInner(keyType(tea.KeyEnter)) // pick remote
	if m.stage != welcomeURL {
		t.Errorf("stage = %v, want welcomeURL", m.stage)
	}
	if m.chosen != nil {
		t.Errorf("chosen set prematurely: %+v", m.chosen)
	}
}

func TestWelcome_URLStageSubmitsValidURL(t *testing.T) {
	t.Parallel()
	m := newWelcomeModel()
	m, _ = m.UpdateInner(keyMsg('j'))
	m, _ = m.UpdateInner(keyType(tea.KeyEnter))
	if m.stage != welcomeURL {
		t.Fatalf("not in welcomeURL stage")
	}
	// Type "http://localhost:7777" + Enter
	for _, r := range "http://localhost:7777" {
		m, _ = m.UpdateInner(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	m, _ = m.UpdateInner(keyType(tea.KeyEnter))
	if m.chosen == nil {
		t.Fatal("chosen not set after Enter on valid URL")
	}
	if m.chosen.RemoteURL != "http://localhost:7777" {
		t.Errorf("RemoteURL = %q", m.chosen.RemoteURL)
	}
}

func TestWelcome_URLStageRejectsBadScheme(t *testing.T) {
	t.Parallel()
	m := newWelcomeModel()
	m, _ = m.UpdateInner(keyMsg('j'))
	m, _ = m.UpdateInner(keyType(tea.KeyEnter))
	for _, r := range "ftp://nope" {
		m, _ = m.UpdateInner(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	m, _ = m.UpdateInner(keyType(tea.KeyEnter))
	if m.chosen != nil {
		t.Errorf("chosen set despite bad scheme: %+v", m.chosen)
	}
	if !strings.Contains(m.error, "http://, https://, or unix://") {
		t.Errorf("error doesn't mention valid schemes: %q", m.error)
	}
}

func TestWelcome_URLStageEscReturnsToChoice(t *testing.T) {
	t.Parallel()
	m := newWelcomeModel()
	m, _ = m.UpdateInner(keyMsg('j'))
	m, _ = m.UpdateInner(keyType(tea.KeyEnter))
	if m.stage != welcomeURL {
		t.Fatal("not in welcomeURL stage")
	}
	m, _ = m.UpdateInner(keyType(tea.KeyEsc))
	if m.stage != welcomeChoice {
		t.Errorf("Esc from URL stage: stage = %v, want welcomeChoice", m.stage)
	}
}

func TestWelcome_View_RendersChoices(t *testing.T) {
	t.Parallel()
	m := newWelcomeModel()
	m.SetSize(80, 24)
	out := m.View()
	for _, want := range []string{"Spawn a local agent", "Attach to a remote endpoint", "navigate"} {
		if !strings.Contains(out, want) {
			t.Errorf("view missing %q:\n%s", want, out)
		}
	}
}
