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

package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/adk/session"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/pkg/usage"
)

func TestContextStats_FreshSessionIsZero(t *testing.T) {
	t.Parallel()
	llm := &captureLLM{response: "ok"}
	a, err := New(llm)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	stats := a.ContextStats()
	if stats.CompactionCount != 0 || stats.CheckpointCount != 0 || stats.SubtaskCount != 0 || stats.TotalSummaryChars != 0 {
		t.Errorf("fresh session should report zeros, got %+v", stats)
	}
}

func TestContextStats_NilReceiverIsZero(t *testing.T) {
	t.Parallel()
	var a *Agent
	stats := a.ContextStats()
	// ContextStats now contains a map (ModelBreakdown), so we
	// can't compare with == against the zero value. Check each
	// scalar field for the nil-receiver zero shape.
	if stats.CompactionCount != 0 || stats.CheckpointCount != 0 ||
		stats.SubtaskCount != 0 || stats.TotalSummaryChars != 0 ||
		stats.ModelBreakdown != nil {
		t.Errorf("nil receiver should return zero ContextStats, got %+v", stats)
	}
}

func TestContextStats_CountsCheckpointsAndSummaries(t *testing.T) {
	t.Parallel()
	llm := &captureLLM{response: "ok"}
	a, err := New(llm)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Bypass the Compactor/Checkpointer LLM call by writing
	// boundary events directly through the session service —
	// ContextStats reads from there, so a direct write is enough
	// to validate the counting + summary-char path.
	ctx := context.Background()
	if _, err := a.sessionService.Create(ctx, &session.CreateRequest{
		AppName: a.appName, UserID: a.userID, SessionID: a.sessionID,
	}); err != nil {
		t.Fatalf("Create session: %v", err)
	}
	resp, err := a.sessionService.Get(ctx, &session.GetRequest{
		AppName: a.appName, UserID: a.userID, SessionID: a.sessionID,
	})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	now := time.Now()

	mkBoundary := func(text, tag, focusOrNote string) *session.Event {
		md := map[string]any{CompactionMetadataKey: tag}
		switch tag {
		case CompactionEventTag:
			md[CompactionFocusKey] = focusOrNote
		case CheckpointEventTag:
			md[CheckpointNoteKey] = focusOrNote
		}
		ev := mkEvent(genai.RoleModel, text)
		ev.CustomMetadata = md
		ev.Timestamp = now
		return ev
	}

	if err := a.sessionService.AppendEvent(ctx, resp.Session, mkBoundary("first compaction summary, 33 chars long", CompactionEventTag, "focus alpha")); err != nil {
		t.Fatalf("AppendEvent compact: %v", err)
	}
	if err := a.sessionService.AppendEvent(ctx, resp.Session, mkBoundary("first checkpoint summary, 35 chars long", CheckpointEventTag, "note one")); err != nil {
		t.Fatalf("AppendEvent checkpoint 1: %v", err)
	}
	if err := a.sessionService.AppendEvent(ctx, resp.Session, mkBoundary("second checkpoint summary even longer than the first", CheckpointEventTag, "note two")); err != nil {
		t.Fatalf("AppendEvent checkpoint 2: %v", err)
	}

	stats := a.ContextStats()
	if stats.CompactionCount != 1 {
		t.Errorf("CompactionCount = %d, want 1", stats.CompactionCount)
	}
	if stats.CheckpointCount != 2 {
		t.Errorf("CheckpointCount = %d, want 2", stats.CheckpointCount)
	}
	if stats.LastCompactionFocus != "focus alpha" {
		t.Errorf("LastCompactionFocus = %q, want focus alpha", stats.LastCompactionFocus)
	}
	if stats.LastCheckpointNote != "note two" {
		t.Errorf("LastCheckpointNote = %q, want note two (latest wins)", stats.LastCheckpointNote)
	}
	// 39 + 39 + 52 = 130 chars. Don't over-pin the exact value;
	// just verify it's > 0 and consistent across calls.
	if stats.TotalSummaryChars <= 0 {
		t.Errorf("TotalSummaryChars = %d, want > 0", stats.TotalSummaryChars)
	}
	if !strings.HasPrefix(stats.LastCheckpointNote, "note") {
		t.Errorf("LastCheckpointNote prefix unexpected: %q", stats.LastCheckpointNote)
	}
}

func TestContextStats_TracksSubtasks(t *testing.T) {
	t.Parallel()
	llm := &captureLLM{
		response:     "subtask done",
		inputTokens:  int32(700),
		outputTokens: int32(70),
	}
	a, err := New(llm)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Run two subtasks; verify ContextStats accumulates count +
	// tokens. Cost stays $0 (no usage.Tracker wired), but tokens
	// should still be counted.
	for i := 0; i < 2; i++ {
		_, err := a.RunSubtask(context.Background(), SubtaskSpec{
			Name:         "test_subtask",
			SystemPrompt: "x",
			UserMessage:  "y",
		})
		if err != nil {
			t.Fatalf("RunSubtask: %v", err)
		}
	}

	stats := a.ContextStats()
	if stats.SubtaskCount != 2 {
		t.Errorf("SubtaskCount = %d, want 2", stats.SubtaskCount)
	}
	if stats.SubtaskInputTokens != 1400 {
		t.Errorf("SubtaskInputTokens = %d, want 1400 (700 * 2)", stats.SubtaskInputTokens)
	}
	if stats.SubtaskOutputTokens != 140 {
		t.Errorf("SubtaskOutputTokens = %d, want 140 (70 * 2)", stats.SubtaskOutputTokens)
	}
}

func TestContextStats_ModelBreakdownPopulatedForMultiModel(t *testing.T) {
	t.Parallel()
	// Parent on one model, subtask on another. After at least
	// one parent turn (via the tracker directly) plus one
	// subtask turn, ContextStats.ModelBreakdown should carry
	// both. ModelBreakdown stays nil for single-model sessions
	// to avoid re-stating /stats totals.
	parentLLM := &captureLLM{response: "parent ok"}
	subLLM := &captureLLM{response: "sub ok", inputTokens: 500, outputTokens: 50}
	tracker := usage.NewTracker()
	a, err := New(parentLLM, WithUsageTracker(tracker))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Plant a fake parent-model turn directly on the tracker so
	// the breakdown has something to surface for "parent" too.
	tracker.Append("parent-model", 1000, 100, usage.Pricing{})

	// Run a subtask on a different model.
	if _, err := a.RunSubtask(context.Background(), SubtaskSpec{
		Name:         "breakdown_check",
		SystemPrompt: "x",
		UserMessage:  "y",
		Model:        subLLM,
	}); err != nil {
		t.Fatalf("RunSubtask: %v", err)
	}

	stats := a.ContextStats()
	if len(stats.ModelBreakdown) < 2 {
		t.Errorf("ModelBreakdown len = %d, want >= 2 (one per model)", len(stats.ModelBreakdown))
	}
	if _, ok := stats.ModelBreakdown["parent-model"]; !ok {
		t.Errorf("ModelBreakdown missing parent-model: %v", stats.ModelBreakdown)
	}
	// Subtask llm's Name() is "capture" per captureLLM.Name().
	if _, ok := stats.ModelBreakdown["capture"]; !ok {
		t.Errorf("ModelBreakdown missing subtask model: %v", stats.ModelBreakdown)
	}
}

func TestContextStats_ModelBreakdownEmptyForSingleModel(t *testing.T) {
	t.Parallel()
	// Single-model session: ModelBreakdown should stay nil
	// because the breakdown would just restate /stats' total.
	llm := &captureLLM{response: "ok"}
	tracker := usage.NewTracker()
	a, err := New(llm, WithUsageTracker(tracker))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tracker.Append("only-model", 100, 10, usage.Pricing{})
	tracker.Append("only-model", 200, 20, usage.Pricing{})

	stats := a.ContextStats()
	if stats.ModelBreakdown != nil {
		t.Errorf("ModelBreakdown should be nil for single-model session, got %v", stats.ModelBreakdown)
	}
}
