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
	"errors"
	"strings"
	"testing"

	"google.golang.org/genai"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/adk/session"
)

func TestCheckpoint_NoCheckpointerReturnsSentinel(t *testing.T) {
	t.Parallel()
	llm := &captureLLM{response: "irrelevant"}
	a, err := New(llm) // no WithCheckpointer
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := a.Checkpoint(context.Background(), ""); !errors.Is(err, ErrNoCheckpointer) {
		t.Errorf("Checkpoint without WithCheckpointer = %v, want ErrNoCheckpointer", err)
	}
}

func TestCheckpoint_EmptyHistoryIsSkipped(t *testing.T) {
	t.Parallel()
	llm := &captureLLM{response: "should not be called"}
	a, err := New(llm, WithCheckpointer(NewDefaultCheckpointer()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := a.Checkpoint(context.Background(), "task X done")
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if !res.Skipped {
		t.Errorf("Checkpoint on empty session should set Skipped=true, got %#v", res)
	}
	if res.TaskNote != "task X done" {
		t.Errorf("TaskNote = %q, want %q (preserved even on skip)", res.TaskNote, "task X done")
	}
	if len(llm.reqs) != 0 {
		t.Errorf("model was called for empty-history Checkpoint; want skipped without LLM call")
	}
}

func TestCheckpoint_WritesCheckpointEventWithNoteAndTag(t *testing.T) {
	t.Parallel()
	llm := &captureLLM{response: "# Task complete\nAuth middleware rewrite shipped."}
	a, err := New(llm, WithCheckpointer(NewDefaultCheckpointer()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	plantEvent(t, a, genai.RoleUser, "let's rewrite the auth middleware")

	res, err := a.Checkpoint(context.Background(), "rewrote middleware, tests green")
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if res.CheckpointEventID == "" {
		t.Errorf("CheckpointEventID empty; want non-empty")
	}
	if !strings.Contains(res.SummaryText, "Auth middleware rewrite shipped") {
		t.Errorf("SummaryText = %q, want model's text", res.SummaryText)
	}
	if res.TaskNote != "rewrote middleware, tests green" {
		t.Errorf("TaskNote = %q, want preserved", res.TaskNote)
	}

	// The event in the session must carry the checkpoint tag,
	// distinguishable from a compaction summary.
	events := loadAllSessionEvents(t, a)
	idx, ev, tag := findLatestBoundary(events)
	if idx < 0 || ev == nil {
		t.Fatalf("checkpoint event not found in session; events=%d", len(events))
	}
	if tag != CheckpointEventTag {
		t.Errorf("tag = %q, want %q", tag, CheckpointEventTag)
	}
	if got := ev.CustomMetadata[CheckpointNoteKey]; got != "rewrote middleware, tests green" {
		t.Errorf("CheckpointNoteKey = %v, want preserved note", got)
	}

	// Verify the system instruction reached the model.
	req := llm.lastRequest()
	if req == nil {
		t.Fatal("model wasn't called")
	}
	if req.Config == nil || req.Config.SystemInstruction == nil {
		t.Fatalf("LLMRequest.Config.SystemInstruction nil")
	}
	sysText := contentText(req.Config.SystemInstruction)
	if !strings.Contains(sysText, "# Task") || !strings.Contains(sysText, "Verification & next steps") {
		t.Errorf("system instruction missing checkpoint sections: %q", sysText)
	}
	if !strings.Contains(sysText, "rewrote middleware, tests green") {
		t.Errorf("system instruction missing task note: %q", sysText)
	}
}

func TestFindLatestBoundary_PrefersNewest(t *testing.T) {
	t.Parallel()
	// When both a summary and a checkpoint exist, the latest by
	// position wins regardless of tag — both act as slicing
	// boundaries.
	older := mkSummaryEvent("older compaction summary")
	intermediate := mkEvent(genai.RoleUser, "between turn")
	newer := mkCheckpointEvent("newer task checkpoint")
	events := []*session.Event{older, intermediate, newer}
	idx, ev, tag := findLatestBoundary(events)
	if idx != 2 || ev == nil {
		t.Fatalf("findLatestBoundary returned idx=%d; want 2 (the checkpoint)", idx)
	}
	if tag != CheckpointEventTag {
		t.Errorf("tag = %q, want %q (newer event won)", tag, CheckpointEventTag)
	}
}

func TestSliceFromBoundary_FindsCheckpoints(t *testing.T) {
	t.Parallel()
	// A checkpoint slices the same way a summary does — but the
	// framing is kind-aware. Checkpoints get the prior-task-
	// complete-conversation-continues prefix (see checkpointPrefix
	// for why we differentiate from compaction's wording).
	pre := mkEvent(genai.RoleUser, "old prompt before checkpoint")
	cp := mkCheckpointEvent("task complete: auth middleware shipped")
	post := mkEvent(genai.RoleUser, "now what?")
	events := []*session.Event{pre, cp, post}

	out := sliceFromBoundary(events)
	if len(out) != 2 {
		t.Fatalf("sliced len = %d, want 2 (checkpoint + post)", len(out))
	}
	framed := contentText(out[0].Content)
	if !strings.Contains(framed, "prior task is complete") {
		t.Errorf("checkpoint should receive checkpoint-specific framing: %q", framed)
	}
	if strings.Contains(framed, "Conversation compacted") {
		t.Errorf("checkpoint should NOT receive compaction-specific framing: %q", framed)
	}
	if !strings.Contains(framed, "task complete: auth middleware") {
		t.Errorf("checkpoint text not preserved: %q", framed)
	}
}

func TestMarkTaskDoneTool_RegisteredWhenCheckpointerWired(t *testing.T) {
	t.Parallel()
	llm := &captureLLM{response: "ack"}
	a, err := New(llm, WithCheckpointer(NewDefaultCheckpointer()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !toolNameRegistered(a, "mark_task_done") {
		t.Errorf("mark_task_done not registered by agent.New when checkpointer is wired")
	}
}

func TestMarkTaskDoneTool_NotRegisteredWithoutCheckpointer(t *testing.T) {
	t.Parallel()
	llm := &captureLLM{response: "ack"}
	a, err := New(llm) // no checkpointer
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if toolNameRegistered(a, "mark_task_done") {
		t.Errorf("mark_task_done registered without WithCheckpointer; want opt-in only")
	}
}

// TestHasCompactorAndCheckpointer pins the surface-gating predicates
// hosts use to decide whether to list /compact and /done in
// /help. Stale predicate values would surface dead slashes to
// operators who passed --no-compact / --no-checkpoint.
func TestHasCompactorAndCheckpointer(t *testing.T) {
	t.Parallel()
	llm := &captureLLM{response: "ack"}

	t.Run("neither", func(t *testing.T) {
		t.Parallel()
		a, err := New(llm)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if a.HasCompactor() || a.HasCheckpointer() {
			t.Errorf("HasCompactor=%v HasCheckpointer=%v; want both false", a.HasCompactor(), a.HasCheckpointer())
		}
	})
	t.Run("compactor_only", func(t *testing.T) {
		t.Parallel()
		a, err := New(llm, WithCompactor(NewDefaultCompactor()))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if !a.HasCompactor() || a.HasCheckpointer() {
			t.Errorf("HasCompactor=%v HasCheckpointer=%v; want true,false", a.HasCompactor(), a.HasCheckpointer())
		}
	})
	t.Run("checkpointer_only", func(t *testing.T) {
		t.Parallel()
		a, err := New(llm, WithCheckpointer(NewDefaultCheckpointer()))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if a.HasCompactor() || !a.HasCheckpointer() {
			t.Errorf("HasCompactor=%v HasCheckpointer=%v; want false,true", a.HasCompactor(), a.HasCheckpointer())
		}
	})
	t.Run("both", func(t *testing.T) {
		t.Parallel()
		a, err := New(llm, WithCompactor(NewDefaultCompactor()), WithCheckpointer(NewDefaultCheckpointer()))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if !a.HasCompactor() || !a.HasCheckpointer() {
			t.Errorf("HasCompactor=%v HasCheckpointer=%v; want both true", a.HasCompactor(), a.HasCheckpointer())
		}
	})
	t.Run("nil_receiver", func(t *testing.T) {
		t.Parallel()
		var a *Agent
		if a.HasCompactor() || a.HasCheckpointer() {
			t.Errorf("nil receiver should report false for both")
		}
	})
}

// TestPrefixForTag pins the kind-aware framing decision. Bug
// surfaced 2026-05-27 smoke: when checkpoint events were wrapped
// with the compaction prefix, gemini-3.5-flash interpreted the
// "# Task" leading section as "fresh start" and re-ran tools the
// summary already recorded. Each tag must map to its purpose-
// specific prefix; unknown tags fall back to compaction (safer
// default).
func TestPrefixForTag(t *testing.T) {
	t.Parallel()
	cases := []struct {
		tag         string
		wantPrefix  string
		mustNotHave string
	}{
		{CompactionEventTag, compactionPrefix, checkpointPrefix},
		{CheckpointEventTag, checkpointPrefix, compactionPrefix},
		{"", compactionPrefix, ""},       // unknown → compaction default
		{"future", compactionPrefix, ""}, // forward-compat fallback
	}
	for _, tc := range cases {
		t.Run(tc.tag, func(t *testing.T) {
			t.Parallel()
			got := prefixForTag(tc.tag)
			if got != tc.wantPrefix {
				t.Errorf("prefixForTag(%q) = %q, want %q", tc.tag, got, tc.wantPrefix)
			}
			if tc.mustNotHave != "" && got == tc.mustNotHave {
				t.Errorf("prefixForTag(%q) returned the wrong prefix (it's the kind we explicitly differentiated from)", tc.tag)
			}
		})
	}
}

func toolNameRegistered(a *Agent, name string) bool {
	for _, tl := range a.Tools() {
		if tl.Name() == name {
			return true
		}
	}
	return false
}

func TestMaybeMarkCheckpointPending_PromotesFlag(t *testing.T) {
	t.Parallel()
	llm := &captureLLM{response: "ack"}
	a, err := New(llm, WithCheckpointer(NewDefaultCheckpointer()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Simulate the mark_task_done handler flipping the in-turn flag.
	a.mu.Lock()
	a.checkpointRequested = true
	a.pendingCheckpointNote = "task X done"
	a.mu.Unlock()

	a.maybeMarkCheckpointPending()

	a.mu.Lock()
	requested := a.checkpointRequested
	pending := a.checkpointPending
	note := a.pendingCheckpointNote
	a.mu.Unlock()
	if requested {
		t.Errorf("checkpointRequested should clear after promotion (single-fire)")
	}
	if !pending {
		t.Errorf("checkpointPending should be true after promotion")
	}
	if note != "task X done" {
		t.Errorf("pendingCheckpointNote should survive promotion, got %q", note)
	}
}

func TestDefaultCheckpointer_ShouldCheckpointAlwaysFalse(t *testing.T) {
	t.Parallel()
	// Heuristic auto-checkpoint is intentionally off in the default
	// implementation. Confirming the contract here so a future
	// change to enable it surfaces as a deliberate test update.
	c := NewDefaultCheckpointer()
	if c.ShouldCheckpoint(context.Background(), nil) {
		t.Errorf("DefaultCheckpointer.ShouldCheckpoint should always return false")
	}
}

func TestCheckpoint_ClearsCompactionPending(t *testing.T) {
	t.Parallel()
	// A checkpoint subsumes any pending compaction — both are
	// slicing boundaries, and re-firing compaction immediately
	// after a checkpoint would just summarize an empty post-
	// boundary slice. The Checkpoint method clears the compaction
	// flag for this reason; this test pins the behavior.
	llm := &captureLLM{response: "summary"}
	a, err := New(llm, WithCheckpointer(NewDefaultCheckpointer()), WithCompactor(NewDefaultCompactor()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	plantEvent(t, a, genai.RoleUser, "some prior turn")

	// Flip both flags as if both post-turn hooks marked us pending.
	a.mu.Lock()
	a.compactionPending = true
	a.checkpointPending = true
	a.mu.Unlock()

	if _, err := a.Checkpoint(context.Background(), ""); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}

	a.mu.Lock()
	cp := a.compactionPending
	chk := a.checkpointPending
	a.mu.Unlock()
	if cp {
		t.Errorf("compactionPending should be cleared by Checkpoint (the checkpoint IS the slicing boundary)")
	}
	if chk {
		t.Errorf("checkpointPending should be cleared by Checkpoint")
	}
}

// mkCheckpointEvent is a test helper mirroring mkSummaryEvent but
// for checkpoint-tagged events.
func mkCheckpointEvent(text string) *session.Event {
	return &session.Event{
		ID: "synthetic-checkpoint",
		LLMResponse: adkmodel.LLMResponse{
			Content: &genai.Content{
				Role:  genai.RoleModel,
				Parts: []*genai.Part{{Text: text}},
			},
			CustomMetadata: map[string]any{
				CompactionMetadataKey: CheckpointEventTag,
			},
		},
	}
}
