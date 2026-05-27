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

	"github.com/go-steer/core-agent/usage"
)

func TestRunSubtask_RejectsInvalidSpec(t *testing.T) {
	t.Parallel()
	llm := &captureLLM{response: "n/a"}
	a, err := New(llm)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cases := []struct {
		name string
		spec SubtaskSpec
	}{
		{"empty Name", SubtaskSpec{SystemPrompt: "x", UserMessage: "y"}},
		{"empty SystemPrompt", SubtaskSpec{Name: "x", UserMessage: "y"}},
		{"empty UserMessage", SubtaskSpec{Name: "x", SystemPrompt: "y"}},
		{"all empty", SubtaskSpec{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := a.RunSubtask(context.Background(), tc.spec)
			if !errors.Is(err, ErrSubtaskSpecInvalid) {
				t.Errorf("RunSubtask(%+v) error = %v, want ErrSubtaskSpecInvalid", tc.spec, err)
			}
		})
	}
}

func TestRunSubtask_ReturnsDigestFromModel(t *testing.T) {
	t.Parallel()
	llm := &captureLLM{response: "the file's main exports are X, Y, Z."}
	a, err := New(llm)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := a.RunSubtask(context.Background(), SubtaskSpec{
		Name:         "read_file_subtask",
		SystemPrompt: "You read files and summarize.",
		UserMessage:  "read /tmp/foo and summarize",
	})
	if err != nil {
		t.Fatalf("RunSubtask: %v", err)
	}
	if !strings.Contains(res.Digest, "the file's main exports") {
		t.Errorf("Digest = %q, want it to contain the model's response", res.Digest)
	}
	if res.Name != "read_file_subtask" {
		t.Errorf("Name = %q, want preserved", res.Name)
	}
	if res.Truncated {
		t.Errorf("Truncated = true, want false (model produced final text on first turn)")
	}
}

func TestRunSubtask_DoesNotSeeParentHistory(t *testing.T) {
	t.Parallel()
	// The fresh-context property: a subtask's model sees ONLY the
	// SystemPrompt + UserMessage from the spec, never the parent's
	// session events. Smoking-gun test: plant a distinctive parent
	// event, run a subtask, verify the subtask's LLMRequest does
	// NOT include the parent's text.
	llm := &captureLLM{response: "subtask response"}
	a, err := New(llm)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	plantEvent(t, a, genai.RoleUser, "parent's long architectural discussion full of secrets")

	llm.reqs = nil // reset so we only see the subtask's request
	_, err = a.RunSubtask(context.Background(), SubtaskSpec{
		Name:         "fresh_context_check",
		SystemPrompt: "you are a focused subtask",
		UserMessage:  "do the focused thing",
	})
	if err != nil {
		t.Fatalf("RunSubtask: %v", err)
	}
	req := llm.lastRequest()
	if req == nil {
		t.Fatalf("model wasn't called")
	}
	var allText strings.Builder
	for _, c := range req.Contents {
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			if p != nil && p.Text != "" {
				allText.WriteString(p.Text)
				allText.WriteByte('\n')
			}
		}
	}
	combined := allText.String()
	if strings.Contains(combined, "parent's long architectural discussion") {
		t.Errorf("subtask saw parent history; fresh-context invariant broken:\n%s", combined)
	}
	if !strings.Contains(combined, "do the focused thing") {
		t.Errorf("subtask user message missing from request: %q", combined)
	}
}

func TestRunSubtask_UsesSpecModelWhenProvided(t *testing.T) {
	t.Parallel()
	// When SubtaskSpec.Model is set, it overrides the parent's
	// model. This is the cost-efficiency lever — parent runs on
	// the expensive model, subtask wrappers point .Model at a
	// cheaper one.
	parentLLM := &captureLLM{response: "parent model response"}
	subLLM := &captureLLM{response: "sub model response"}
	a, err := New(parentLLM)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := a.RunSubtask(context.Background(), SubtaskSpec{
		Name:         "model_override_check",
		SystemPrompt: "x",
		UserMessage:  "y",
		Model:        subLLM,
	})
	if err != nil {
		t.Fatalf("RunSubtask: %v", err)
	}
	if !strings.Contains(res.Digest, "sub model response") {
		t.Errorf("Digest = %q, want the SUB model's response (override didn't apply)", res.Digest)
	}
	// Parent LLM should NOT have been called for the subtask.
	if len(parentLLM.reqs) > 0 {
		t.Errorf("parent LLM called %d times for subtask; want 0 (override should route to sub LLM)", len(parentLLM.reqs))
	}
}

func TestRunSubtask_FallsBackToParentModelWhenNil(t *testing.T) {
	t.Parallel()
	// When SubtaskSpec.Model is nil (the default), the subtask
	// uses the parent's model. Confirms the zero-value behavior.
	parentLLM := &captureLLM{response: "from the parent's model"}
	a, err := New(parentLLM)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	parentLLM.reqs = nil
	res, err := a.RunSubtask(context.Background(), SubtaskSpec{
		Name:         "no_override",
		SystemPrompt: "x",
		UserMessage:  "y",
		// Model: nil
	})
	if err != nil {
		t.Fatalf("RunSubtask: %v", err)
	}
	if !strings.Contains(res.Digest, "from the parent's model") {
		t.Errorf("Digest = %q, want parent's model output (nil override should fall through)", res.Digest)
	}
	if len(parentLLM.reqs) == 0 {
		t.Errorf("parent LLM not called; nil override should route to it")
	}
}

func TestRunSubtask_PropagatesCostToParentTracker(t *testing.T) {
	t.Parallel()
	// Subtask cost rolls up to the parent's usage.Tracker so
	// /stats reflects everything. Smoking-gun test: wire a real
	// tracker, run a subtask, verify the tracker has a new turn
	// with the subtask model's name + tokens.
	llm := &captureLLM{
		response:     "ok",
		inputTokens:  int32(500),
		outputTokens: int32(50),
	}
	tracker := usage.NewTracker()
	a, err := New(llm, WithUsageTracker(tracker))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	preTurns := tracker.Totals().Turns

	res, err := a.RunSubtask(context.Background(), SubtaskSpec{
		Name:         "cost_rollup_check",
		SystemPrompt: "x",
		UserMessage:  "y",
	})
	if err != nil {
		t.Fatalf("RunSubtask: %v", err)
	}

	postTurns := tracker.Totals().Turns
	if postTurns <= preTurns {
		t.Errorf("tracker turn count didn't increase after subtask; got %d → %d", preTurns, postTurns)
	}
	if res.InputTokens != 500 {
		t.Errorf("InputTokens = %d, want 500 (from the model's reported usage)", res.InputTokens)
	}
	if res.OutputTokens != 50 {
		t.Errorf("OutputTokens = %d, want 50", res.OutputTokens)
	}
}

func TestRunSubtask_NoSessionLeakIntoParent(t *testing.T) {
	t.Parallel()
	// After a subtask completes, the parent's session.Get
	// (UNWRAPPED) should not contain the subtask's events —
	// they're written under a different sessionID (branch-
	// isolated). This pins the audit-log isolation guarantee.
	llm := &captureLLM{response: "subtask answer"}
	a, err := New(llm)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	plantEvent(t, a, genai.RoleUser, "parent's setup turn")

	preEvents := loadAllSessionEvents(t, a)

	_, err = a.RunSubtask(context.Background(), SubtaskSpec{
		Name:         "isolation_check",
		SystemPrompt: "x",
		UserMessage:  "subtask user message that should NOT appear in parent's session",
	})
	if err != nil {
		t.Fatalf("RunSubtask: %v", err)
	}

	postEvents := loadAllSessionEvents(t, a)
	// The parent's session should have exactly the pre-subtask
	// events. ADK's in-memory service writes the subtask's
	// events under the SUB sessionID, which is a different
	// session row, so the parent's session.Get() returns the
	// same set of events as before.
	if len(postEvents) != len(preEvents) {
		t.Errorf("parent's session changed during subtask: pre=%d, post=%d; want isolation", len(preEvents), len(postEvents))
	}
	for _, ev := range postEvents {
		if ev == nil || ev.Content == nil {
			continue
		}
		for _, p := range ev.Content.Parts {
			if p != nil && strings.Contains(p.Text, "subtask user message that should NOT appear") {
				t.Errorf("subtask user message leaked into parent's session: %q", p.Text)
			}
		}
	}
}
