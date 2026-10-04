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
)

// #1206. An internal one-shot call's response is never a session event
// and its error never a turn error, so a provider retry on it has no
// transcript surface. Each one marks its context with models.AsSideCall,
// which labels the retry's log line, so box A2's counter can tell it
// from a retry a transcript must show instead of failing the run on it.

func assertSideCall(t *testing.T, llm *captureLLM, want string) {
	t.Helper()
	llm.mu.Lock()
	defer llm.mu.Unlock()
	if len(llm.sideCall) == 0 {
		t.Fatalf("%s: no request reached the model", want)
	}
	for i, got := range llm.sideCall {
		if got != want {
			t.Errorf("request %d marked as side call %q, want %q", i, got, want)
		}
	}
}

func TestCompact_IsASideCall(t *testing.T) {
	t.Parallel()
	llm := &captureLLM{response: "# Current state\nsummary."}
	a, err := New(llm, WithCompactor(NewDefaultCompactor()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	plantEvent(t, a, genai.RoleUser, "let's build a thing")
	if _, err := a.Compact(context.Background(), ""); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	assertSideCall(t, llm, "summarizer")
}

func TestCheckpoint_IsASideCall(t *testing.T) {
	t.Parallel()
	llm := &captureLLM{response: "# Checkpoint\nstate."}
	a, err := New(llm, WithCheckpointer(NewDefaultCheckpointer()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	plantEvent(t, a, genai.RoleUser, "let's build a thing")
	if _, err := a.Checkpoint(context.Background(), "note"); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	assertSideCall(t, llm, "summarizer")
}

func TestAskSideQuestion_IsASideCall(t *testing.T) {
	t.Parallel()
	llm := &captureLLM{response: "It was main.go."}
	a, err := New(llm)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := a.AskSideQuestion(context.Background(), "what was that file again?"); err != nil {
		t.Fatalf("AskSideQuestion: %v", err)
	}
	assertSideCall(t, llm, "btw")
}

func TestSessionTitle_IsASideCall(t *testing.T) {
	t.Parallel()
	titler := &captureLLM{response: `"Fix the webhook retry backoff"`}
	a, err := New(&captureLLM{response: "parent answer"}, WithTitleModel(titler))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	a.maybeTitleSession(context.Background(), "the retries on our payment webhook are backing off way too aggressively")
	waitForTitle(t, a)
	assertSideCall(t, titler, "session title")
}

// The agentic loop's own calls are not side calls: their retries are
// the ones a transcript must show.
func TestRun_IsNotASideCall(t *testing.T) {
	t.Parallel()
	llm := &captureLLM{response: "done."}
	a, err := New(llm)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := a.RunSubtask(context.Background(), SubtaskSpec{
		Name: "t", SystemPrompt: "s", UserMessage: "u", Budgets: SubtaskBudgets{MaxTurns: 2},
	}); err != nil {
		t.Fatalf("RunSubtask: %v", err)
	}
	assertSideCall(t, llm, "")
}
