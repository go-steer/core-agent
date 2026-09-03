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
	"reflect"
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

// TestWithoutMarkTaskDoneTool_KeepsCheckpointerDropsTool is the #905
// contract: the operator-only posture withholds the model's trigger
// WITHOUT unwiring checkpointing. Both halves matter. Dropping the tool
// is the fix; keeping the checkpointer is what distinguishes this from
// --no-checkpoint, which took /done and the heuristic with it and so
// was too blunt to run on the deployment that needed it.
func TestWithoutMarkTaskDoneTool_KeepsCheckpointerDropsTool(t *testing.T) {
	t.Parallel()
	llm := &captureLLM{response: "ack"}
	a, err := New(llm, WithCheckpointer(NewDefaultCheckpointer()), WithoutMarkTaskDoneTool())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if toolNameRegistered(a, "mark_task_done") {
		t.Errorf("mark_task_done registered with WithoutMarkTaskDoneTool; want it withheld")
	}
	if !a.HasCheckpointer() {
		t.Errorf("HasCheckpointer() = false; want true (/done must still work)")
	}
	// The operator path has to reach the same machinery, not just
	// report that it exists.
	if _, err := a.Checkpoint(context.Background(), "operator note"); errors.Is(err, ErrNoCheckpointer) {
		t.Errorf("Checkpoint returned ErrNoCheckpointer; the checkpointer must stay wired")
	}
}

// TestWithoutMarkTaskDoneTool_NoOpWithoutCheckpointer guards the
// documented "no tool to suppress" case, so the option can be passed
// unconditionally by a caller that resolves the checkpointer later.
func TestWithoutMarkTaskDoneTool_NoOpWithoutCheckpointer(t *testing.T) {
	t.Parallel()
	llm := &captureLLM{response: "ack"}
	a, err := New(llm, WithoutMarkTaskDoneTool())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if toolNameRegistered(a, "mark_task_done") {
		t.Errorf("mark_task_done registered without a checkpointer")
	}
	if a.HasCheckpointer() {
		t.Errorf("HasCheckpointer() = true; the option must not wire one")
	}
}

// TestMarkTaskDonePromptText is a prose regression guard, not a style
// check. Tool descriptions and arg schemas are system-prompt-weight
// text that nothing else reviews (#909), and the two phrases banned
// here are the ones that caused #905: "use this generously" set a
// call-frequency policy the persona could not countermand, and asking
// the `detail` arg for a "completion summary" is what put completion
// reports where operator answers belonged. A reword that reintroduces
// either shape should fail here rather than on a live deployment.
func TestMarkTaskDonePromptText(t *testing.T) {
	t.Parallel()

	// Read it off the constructed tool, not off the constant, so a
	// future edit that leaves markTaskDoneDescription in place but
	// hands functiontool.Config a different string still fails here.
	desc := NewMarkTaskDoneTool(func() *Agent { return nil }).Description()
	if desc == "" {
		t.Fatal("mark_task_done has no description")
	}
	if desc != markTaskDoneDescription {
		t.Errorf("tool description has drifted from markTaskDoneDescription;\n got: %s\nwant: %s", desc, markTaskDoneDescription)
	}
	lowered := strings.ToLower(desc)
	for _, banned := range []string{
		// Frequency instructions. The persona cannot countermand one,
		// so the tool must not set a rate at all — #905 was "generously"
		// specifically, but any of these reproduces it.
		"generously",
		"freely",
		"liberally",
		"whenever you",
		"as often as",
		// Interactive-coding-session examples. They tell a cluster
		// operator's agent it is in the wrong kind of session.
		"shipping a feature",
		"code review",
		"debugging session",
	} {
		if strings.Contains(lowered, banned) {
			t.Errorf("description contains %q; it must not assume a coding session or set a call frequency:\n%s", banned, desc)
		}
	}
	// The observed failure was answering an operator's question with
	// this tool. The description has to rule that out by name.
	for _, want := range []string{"does not answer a question", "in place of answering"} {
		if !strings.Contains(lowered, want) {
			t.Errorf("description does not say %q; that negation is the fix:\n%s", want, desc)
		}
	}

	// Read the tag off the struct the model's schema is generated
	// from, so the guard cannot drift from the shipped text.
	field, ok := reflect.TypeOf(markTaskDoneArgs{}).FieldByName("Detail")
	if !ok {
		t.Fatal("markTaskDoneArgs has no Detail field")
	}
	schema := field.Tag.Get("jsonschema")
	if schema == "" {
		t.Fatal("mark_task_done detail arg has no jsonschema description")
	}
	loweredSchema := strings.ToLower(schema)
	// A string arg description is a writing prompt: it must name a
	// content obligation, never a genre.
	for _, banned := range []string{"completion summary", "one-paragraph summary", "summary of what", "summarize"} {
		if strings.Contains(loweredSchema, banned) {
			t.Errorf("detail schema asks for %q; name what the next turn needs, not a genre:\n%s", banned, schema)
		}
	}
	// The genre words are only safe here as negations, which is why
	// they cannot simply be banned outright. Requiring the negations
	// is what makes the bans above hard to route around: a reword that
	// asks for a report or a recap has to delete one of these to read
	// coherently.
	for _, want := range []string{"not a report to the operator", "not a recap"} {
		if !strings.Contains(loweredSchema, want) {
			t.Errorf("detail schema does not say %q; the genre it is NOT is load-bearing:\n%s", want, schema)
		}
	}
}

// TestHasCompactorAndCheckpointer pins the surface-gating predicates
// hosts use to decide whether to list /compact and /done in
// /help. Stale predicate values would surface dead slashes to
// operators who passed --no-compact / --checkpoint=off.
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

// TestMarkTaskDone_RepeatInOneTurnSaysSo covers the loop the watchdog
// structurally could not see (session
// 01a03f1e-e215-7acd-81a9-6e4654d91325): nine consecutive
// mark_task_done calls in a single invocation, each with a reworded
// detail about the same finished incident, ended only by an operator
// interrupt. Every loop detector keys on (name, canonicalArgs) and the
// rewording changed the hash each time, so watchdog=enforce reported
// tripped: false throughout.
//
// Fails on pre-fix code, which returned "acknowledged" to all nine.
func TestMarkTaskDone_RepeatInOneTurnSaysSo(t *testing.T) {
	t.Parallel()
	llm := &captureLLM{response: "ack"}
	a, err := New(llm, WithCheckpointer(NewDefaultCheckpointer()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	first := markTaskDone(a, "triaged the OOMKill on api-7d9")
	if first.Status != "acknowledged" {
		t.Errorf("first call Status = %q, want acknowledged", first.Status)
	}
	// The call that armed the checkpoint did something, so counting it
	// toward a no-op streak would shorten every streak by one.
	if first.NoOp {
		t.Errorf("first call reported NoOp; it armed the checkpoint")
	}
	for i := 0; i < 8; i++ {
		got := markTaskDone(a, "the OOMKill work is complete, said another way")
		if got.Status == "acknowledged" {
			t.Fatalf("repeat %d was acknowledged again — the reply that kept the live loop running", i+2)
		}
		if got.Status != markTaskDoneRepeatStatus {
			t.Fatalf("repeat %d Status = %q, want the repeat status", i+2, got.Status)
		}
		// The machine-readable half (#907). The Status above says the
		// same thing in English, and English is what gets reworded;
		// watchdog.NoOpStreakSignal reads only this.
		if !got.NoOp {
			t.Fatalf("repeat %d did not set NoOp — the watchdog cannot see the loop", i+2)
		}
	}

	// The repeat must still be a no-op on the flags, not a rollback: the
	// checkpoint fires once between turns and the newest detail wins.
	a.mu.Lock()
	requested := a.checkpointRequested
	note := a.pendingCheckpointNote
	a.mu.Unlock()
	if !requested {
		t.Errorf("checkpointRequested cleared by a repeat call — the checkpoint would be lost")
	}
	if note != "the OOMKill work is complete, said another way" {
		t.Errorf("pendingCheckpointNote = %q, want the latest detail", note)
	}

	// And the turn boundary re-arms it. A session that marks two tasks
	// done in two turns must checkpoint twice.
	a.maybeMarkCheckpointPending()
	if got := markTaskDone(a, "second task"); got.Status != "acknowledged" {
		t.Errorf("next turn's first call Status = %q, want acknowledged — the repeat guard never re-armed", got.Status)
	}
}

// The repeat status has three jobs and the model needs all three: say
// the call did nothing, say why repeating cannot help, and point at the
// thing it has probably left undone (the observed loop happened while
// an operator's question sat unanswered). It must not read as an error,
// which would invite the retry that is the loop again.
func TestMarkTaskDoneRepeatStatus_TellsTheModelWhatToDoInstead(t *testing.T) {
	t.Parallel()

	for _, want := range []string{"already recorded", "cannot do anything further", "answer it now"} {
		if !strings.Contains(markTaskDoneRepeatStatus, want) {
			t.Errorf("repeat status is missing %q: %q", want, markTaskDoneRepeatStatus)
		}
	}
	for _, forbidden := range []string{"error", "failed", "invalid"} {
		if strings.Contains(strings.ToLower(markTaskDoneRepeatStatus), forbidden) {
			t.Errorf("repeat status reads as a failure (%q), which invites a retry: %q", forbidden, markTaskDoneRepeatStatus)
		}
	}
}

// A nil agent is the pre-registration race NewMarkTaskDoneTool
// documents. It must stay a successful no-op rather than becoming a
// repeat report.
func TestMarkTaskDone_NilAgentIsANoOp(t *testing.T) {
	t.Parallel()

	got := markTaskDone(nil, "anything")
	if !strings.Contains(got.Status, "acknowledged") {
		t.Errorf("nil-agent Status = %q, want an acknowledgement", got.Status)
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
