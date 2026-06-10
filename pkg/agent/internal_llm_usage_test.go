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
	"testing"

	"google.golang.org/genai"

	"github.com/go-steer/core-agent/pkg/usage"
)

// Regression coverage for #61 — internal-LLM calls bypassed the
// usage tracker, so /stats under-reported cost AND
// --max-turn/session-cost-usd ceilings (#145) had a hole. These tests
// pin that runSummarizer (compaction + checkpoint) and
// AskSideQuestion (the /btw flow) both roll their usage into the
// tracker. If either regresses, the cost-ceiling kill switch silently
// stops enforcing on those code paths.

func TestCompact_RecordsUsageInTracker(t *testing.T) {
	t.Parallel()
	tr := usage.NewTracker()
	llm := &captureLLM{
		response:     "# Current state\nProject in flight.",
		inputTokens:  1000,
		outputTokens: 200,
	}
	a, err := New(llm,
		WithCompactor(NewDefaultCompactor()),
		WithUsageTracker(tr),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	plantEvent(t, a, genai.RoleUser, "make me a thing")

	if _, err := a.Compact(context.Background(), "focus"); err != nil {
		t.Fatalf("Compact: %v", err)
	}

	totals := tr.Totals()
	if totals.InputTokens != 1000 {
		t.Errorf("tracker InputTokens = %d, want 1000 (summarizer's tokens missing)", totals.InputTokens)
	}
	if totals.OutputTokens != 200 {
		t.Errorf("tracker OutputTokens = %d, want 200", totals.OutputTokens)
	}
	if totals.Turns != 1 {
		t.Errorf("tracker Turns = %d, want 1 (summarizer turn not counted)", totals.Turns)
	}
	// The summarizer uses a.model (captureLLM's Name = "capture") —
	// per-model row should also reflect the call so /context's Models
	// table doesn't under-report the parent's spend (issue #61's
	// second symptom).
	byModel := tr.TotalsByModel()
	if byModel["capture"].Turns != 1 {
		t.Errorf("tracker per-model turns for 'capture' = %d, want 1; got map=%+v",
			byModel["capture"].Turns, byModel)
	}
}

func TestCheckpoint_RecordsUsageInTracker(t *testing.T) {
	t.Parallel()
	// Checkpoint shares runSummarizer with Compact, so this is the
	// same regression on a different operation tag. Worth its own
	// test because a future refactor splitting the two paths would
	// otherwise quietly break only one.
	tr := usage.NewTracker()
	llm := &captureLLM{
		response:     "# Task done\nFinished the auth refactor.",
		inputTokens:  500,
		outputTokens: 80,
	}
	a, err := New(llm,
		WithCheckpointer(NewDefaultCheckpointer()),
		WithUsageTracker(tr),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	plantEvent(t, a, genai.RoleUser, "finish the auth refactor")

	if _, err := a.Checkpoint(context.Background(), "auth refactor wrapped"); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}

	totals := tr.Totals()
	if totals.InputTokens != 500 || totals.OutputTokens != 80 {
		t.Errorf("tracker totals = (in=%d, out=%d), want (500, 80)",
			totals.InputTokens, totals.OutputTokens)
	}
	if totals.Turns != 1 {
		t.Errorf("tracker Turns = %d, want 1", totals.Turns)
	}
}

func TestAskSideQuestion_RecordsUsageInTracker(t *testing.T) {
	t.Parallel()
	// The /btw side-question flow was the second internal-LLM caller
	// bypassing the tracker before #61's fix. AskSideQuestion uses
	// a.model directly (same as runSummarizer) so the rollup path
	// is identical.
	tr := usage.NewTracker()
	llm := &captureLLM{
		response:     "main.go.",
		inputTokens:  300,
		outputTokens: 5,
	}
	a, err := New(llm, WithUsageTracker(tr))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := a.AskSideQuestion(context.Background(), "what was that file again?"); err != nil {
		t.Fatalf("AskSideQuestion: %v", err)
	}

	totals := tr.Totals()
	if totals.InputTokens != 300 || totals.OutputTokens != 5 {
		t.Errorf("tracker totals = (in=%d, out=%d), want (300, 5)",
			totals.InputTokens, totals.OutputTokens)
	}
	if totals.Turns != 1 {
		t.Errorf("tracker Turns = %d, want 1", totals.Turns)
	}
}

func TestRecordInternalLLMUsage_NilSafe(t *testing.T) {
	t.Parallel()
	// All no-op paths: nil receiver, nil tracker, nil model, zero
	// tokens. None should panic — these helpers fire from
	// per-turn-deferred cleanup paths where a panic would tear down
	// the agent.
	var nilAgent *Agent
	nilAgent.recordInternalLLMUsage(100, 50)

	(&Agent{}).recordInternalLLMUsage(100, 50)                         // nil tracker
	(&Agent{tracker: usage.NewTracker()}).recordInternalLLMUsage(0, 0) // zero tokens
	(&Agent{tracker: usage.NewTracker()}).recordInternalLLMUsage(1, 1) // nil model

	// Sanity: with both wired AND non-zero tokens, the call appends.
	tr := usage.NewTracker()
	a := &Agent{tracker: tr, model: &captureLLM{}}
	a.recordInternalLLMUsage(10, 2)
	if got := tr.Totals().Turns; got != 1 {
		t.Errorf("Turns = %d, want 1 after wired + non-zero call", got)
	}
}
