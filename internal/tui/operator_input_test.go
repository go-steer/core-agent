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
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/go-steer/core-agent/agent"
	"github.com/go-steer/core-agent/config"
	"github.com/go-steer/core-agent/internal/testutil"
)

// nopProgramSender is a programSender that discards every event.
// Useful for tests that drive the model synchronously and don't
// care about (or want to assert on) the goroutine-emitted messages
// from startAgentTurn.
type nopProgramSender struct{}

func (nopProgramSender) Send(tea.Msg) {}

// newOperatorInputTestModel mints a Model wired to a real Agent (so
// Inject/DrainInbox/PendingInboxCount are live) without a running
// teatest program. Slash handlers and the queue logic are exercised
// synchronously from the test goroutine. The model is wired to a
// no-op program sender so launchTurn doesn't panic when the goroutine
// tries to deliver turnDoneMsg.
func newOperatorInputTestModel(t *testing.T) *Model {
	t.Helper()
	cfg := config.DefaultConfig()
	fake := &testutil.FakeModel{ModelName: "fake"}
	a, err := agent.New(fake)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	m := NewModel(cfg, a, "dark")
	m.SetProgram(nopProgramSender{})
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	return m
}

func TestHandleSubmitDuringStreaming_InjectsAndEnqueues(t *testing.T) {
	t.Parallel()
	m := newOperatorInputTestModel(t)
	m.state = StateStreaming
	m.textarea.SetValue("a follow-up note")

	m.handleSubmitDuringStreaming()

	if got := m.agent.PendingInboxCount(); got != 1 {
		t.Errorf("inbox count = %d, want 1", got)
	}
	if len(m.queue.entries) != 1 {
		t.Fatalf("queue entries = %d, want 1", len(m.queue.entries))
	}
	if m.queue.entries[0].state != queueQueued {
		t.Errorf("entry state = %v, want queued", m.queue.entries[0].state)
	}
	if m.queue.entries[0].text != "a follow-up note" {
		t.Errorf("entry text = %q, want %q", m.queue.entries[0].text, "a follow-up note")
	}
	if got := m.textarea.Value(); got != "" {
		t.Errorf("textarea not reset: %q", got)
	}
}

func TestHandleSubmitDuringStreaming_EmptyInputNoOp(t *testing.T) {
	t.Parallel()
	m := newOperatorInputTestModel(t)
	m.state = StateStreaming
	m.textarea.SetValue("   ")

	m.handleSubmitDuringStreaming()

	if got := m.agent.PendingInboxCount(); got != 0 {
		t.Errorf("inbox count = %d, want 0 (whitespace-only input should be a no-op)", got)
	}
	if len(m.queue.entries) != 0 {
		t.Errorf("queue entries = %d, want 0", len(m.queue.entries))
	}
}

func TestHandleSubmitDuringStreaming_PromotesToRecallHistory(t *testing.T) {
	t.Parallel()
	m := newOperatorInputTestModel(t)
	m.state = StateStreaming
	m.textarea.SetValue("note one")
	m.handleSubmitDuringStreaming()
	m.textarea.SetValue("note two")
	m.handleSubmitDuringStreaming()

	if len(m.promptHistory) != 2 {
		t.Fatalf("promptHistory len = %d, want 2", len(m.promptHistory))
	}
	if m.promptHistory[0] != "note one" || m.promptHistory[1] != "note two" {
		t.Errorf("promptHistory = %#v", m.promptHistory)
	}
}

func TestHandleSubmitDuringStreaming_SlashRoutesNormally(t *testing.T) {
	t.Parallel()
	m := newOperatorInputTestModel(t)
	m.state = StateStreaming
	m.textarea.SetValue("/help")

	m.handleSubmitDuringStreaming()

	// /help dispatches synchronously and writes HelpText into history.
	if got := m.agent.PendingInboxCount(); got != 0 {
		t.Errorf("inbox count = %d, want 0 (slash commands shouldn't enqueue)", got)
	}
	if len(m.queue.entries) != 0 {
		t.Errorf("queue entries = %d, want 0", len(m.queue.entries))
	}
	last := m.history.Snapshot()
	if len(last) == 0 || !strings.Contains(last[len(last)-1].Text, "core-agent") {
		t.Errorf("expected /help output in last message, got %#v", last)
	}
}

func TestHandleTurnDone_NoInboxReturnsToIdle(t *testing.T) {
	t.Parallel()
	m := newOperatorInputTestModel(t)
	m.state = StateStreaming
	m.autoContinueDepth = 3 // simulate a prior auto-continue chain

	m.handleTurnDone()

	if m.state != StateIdle {
		t.Errorf("state = %v, want StateIdle", m.state)
	}
	if m.autoContinueDepth != 0 {
		t.Errorf("autoContinueDepth = %d, want 0 (reset on empty inbox)", m.autoContinueDepth)
	}
}

func TestHandleTurnDone_DrainsInboxAndStartsAutoContinue(t *testing.T) {
	t.Parallel()
	m := newOperatorInputTestModel(t)
	// Simulate state after a normal turn completed: streaming with one
	// queued operator note.
	m.state = StateStreaming
	if err := m.agent.Inject("please also do X"); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	m.queue.enqueue("please also do X")

	m.handleTurnDone()

	// Inbox should be drained.
	if got := m.agent.PendingInboxCount(); got != 0 {
		t.Errorf("inbox count after auto-continue = %d, want 0", got)
	}
	// State should still be streaming because launchTurn fired.
	if m.state != StateStreaming {
		t.Errorf("state = %v, want StateStreaming (auto-continue should re-arm)", m.state)
	}
	// autoContinueDepth should have incremented.
	if m.autoContinueDepth != 1 {
		t.Errorf("autoContinueDepth = %d, want 1", m.autoContinueDepth)
	}
	// The queue entry should now be marked in-flight.
	if len(m.queue.entries) != 1 || m.queue.entries[0].state != queueInFlight {
		t.Fatalf("queue entries = %#v, want one in-flight entry", m.queue.entries)
	}
	// A user message with AutoContinue=true should have been appended.
	snap := m.history.Snapshot()
	var found bool
	for _, msg := range snap {
		if msg.Role == RoleUser && msg.AutoContinue && strings.Contains(msg.Text, "please also do X") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("no auto-continue user message in history: %#v", snap)
	}
}

func TestHandleTurnDone_LimitHaltsAutoContinue(t *testing.T) {
	t.Parallel()
	m := newOperatorInputTestModel(t)
	m.state = StateStreaming
	m.autoContinueDepth = m.autoContinueLimit
	if err := m.agent.Inject("will not be drained"); err != nil {
		t.Fatalf("Inject: %v", err)
	}

	m.handleTurnDone()

	// Inbox should still hold the note — limit refuses to drain.
	if got := m.agent.PendingInboxCount(); got != 1 {
		t.Errorf("inbox count after limit-hit = %d, want 1 (note should stay for next prompt)", got)
	}
	if m.state != StateIdle {
		t.Errorf("state = %v, want StateIdle (limit forces idle)", m.state)
	}
	if m.autoContinueDepth != 0 {
		t.Errorf("autoContinueDepth = %d, want 0 (reset after limit)", m.autoContinueDepth)
	}
	// A system message should have warned the operator.
	snap := m.history.Snapshot()
	var found bool
	for _, msg := range snap {
		if msg.Role == RoleSystem && strings.Contains(msg.Text, "Auto-continue limit reached") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("no limit-reached system message: %#v", snap)
	}
}

func TestHandleTurnDone_MultipleDrainedRendersAsBullets(t *testing.T) {
	t.Parallel()
	m := newOperatorInputTestModel(t)
	m.state = StateStreaming
	if err := m.agent.Inject("first note"); err != nil {
		t.Fatalf("Inject 1: %v", err)
	}
	if err := m.agent.Inject("second note"); err != nil {
		t.Fatalf("Inject 2: %v", err)
	}
	m.queue.enqueue("first note")
	m.queue.enqueue("second note")

	m.handleTurnDone()

	snap := m.history.Snapshot()
	var auto Message
	for _, msg := range snap {
		if msg.Role == RoleUser && msg.AutoContinue {
			auto = msg
			break
		}
	}
	if auto.Text == "" {
		t.Fatalf("no auto-continue user message in history: %#v", snap)
	}
	if !strings.Contains(auto.Text, "- first note") {
		t.Errorf("auto-continue text missing first bullet: %q", auto.Text)
	}
	if !strings.Contains(auto.Text, "- second note") {
		t.Errorf("auto-continue text missing second bullet: %q", auto.Text)
	}
}

func TestFormatAutoContinueInbox_LayersInstruction(t *testing.T) {
	t.Parallel()
	out := agent.FormatAutoContinueInbox([]string{"check the deploy logs", "also tail Sentry"})
	if !strings.HasPrefix(out, "[Operator notes added during the previous task]") {
		t.Errorf("format missing header: %q", out)
	}
	if !strings.Contains(out, "- check the deploy logs") {
		t.Errorf("format missing first bullet: %q", out)
	}
	if !strings.Contains(out, "- also tail Sentry") {
		t.Errorf("format missing second bullet: %q", out)
	}
	if !strings.Contains(out, "`todo` tool") {
		t.Errorf("format missing todo-tool instruction: %q", out)
	}
}

func TestFormatAutoContinueInbox_EmptyReturnsEmpty(t *testing.T) {
	t.Parallel()
	if got := agent.FormatAutoContinueInbox(nil); got != "" {
		t.Errorf("FormatAutoContinueInbox(nil) = %q, want \"\"", got)
	}
}

func TestRenderMessage_AutoContinueUsesCircleArrowGlyph(t *testing.T) {
	t.Parallel()
	m := newOperatorInputTestModel(t)
	auto := Message{Role: RoleUser, Text: "queued note", AutoContinue: true}
	out := m.renderMessage(auto)
	if !strings.Contains(out, "↻") {
		t.Errorf("auto-continue render missing ↻ glyph: %q", out)
	}
	if strings.Contains(out, "❯") {
		t.Errorf("auto-continue render should not include the ❯ glyph: %q", out)
	}
}

func TestRenderMessage_PlainUserStillUsesAngleGlyph(t *testing.T) {
	t.Parallel()
	m := newOperatorInputTestModel(t)
	plain := Message{Role: RoleUser, Text: "manual prompt"}
	out := m.renderMessage(plain)
	if !strings.Contains(out, "❯") {
		t.Errorf("plain user render missing ❯ glyph: %q", out)
	}
	if strings.Contains(out, "↻") {
		t.Errorf("plain user render should not include the ↻ glyph: %q", out)
	}
}
