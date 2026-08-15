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

// core-agent's internal LLM calls are one-shots: nothing replays their
// prefix, so a cache entry written for them is billed at the 1.25x write
// premium and never read. Each must mark its context as a one-shot.
//
// The summarizer and checkpointer send their own system instruction and
// no tools; the side question sends neither. Either way the prefix
// diverges from the agentic loop's at the very first block, so the
// byte-exact match can't hit even where the history is identical.
// Tight-budget subtasks are the third shape — see
// TestRunSubtask_SuppressesPromptCacheOnATightBudget.
//
// These assert on the CONTEXT the agent hands the model, not on any
// provider's behaviour, because that context is the whole contract: the
// Anthropic adapter reads it per request (one model.LLM serves both the
// agentic loop and these calls), and a provider without prompt caching
// ignores it.

func TestCompact_SuppressesPromptCache(t *testing.T) {
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
	assertSuppressed(t, llm, "summarizer")
}

func TestCheckpoint_SuppressesPromptCache(t *testing.T) {
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
	assertSuppressed(t, llm, "checkpointer")
}

func TestAskSideQuestion_SuppressesPromptCache(t *testing.T) {
	t.Parallel()
	llm := &captureLLM{response: "It was main.go."}
	a, err := New(llm)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := a.AskSideQuestion(context.Background(), "what was that file again?"); err != nil {
		t.Fatalf("AskSideQuestion: %v", err)
	}
	assertSuppressed(t, llm, "side question")
}

// TestRunSubtask_SuppressesPromptCacheOnATightBudget pins the shape the
// agentic wrappers and the MCP digest fallback both run: two turns, a
// session ID that never recurs, and a terminal turn whose whole payload
// is a large tool_result. Caching it writes an entry nothing can read.
func TestRunSubtask_SuppressesPromptCacheOnATightBudget(t *testing.T) {
	t.Parallel()
	llm := &captureLLM{response: "digest."}
	a, err := New(llm)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := a.RunSubtask(context.Background(), SubtaskSpec{
		Name:         "digest",
		SystemPrompt: "you summarize",
		UserMessage:  "summarize this",
		Budgets:      SubtaskBudgets{MaxTurns: 2},
	}); err != nil {
		t.Fatalf("RunSubtask: %v", err)
	}
	assertSuppressed(t, llm, "2-turn subtask")
}

// The mirror image: a subtask with room to replay its prefix keeps
// caching, because from turn 3 on the reads outrun the one write.
func TestRunSubtask_KeepsPromptCacheOnALongerBudget(t *testing.T) {
	t.Parallel()
	llm := &captureLLM{response: "digest."}
	a, err := New(llm)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := a.RunSubtask(context.Background(), SubtaskSpec{
		Name:         "research",
		SystemPrompt: "you research",
		UserMessage:  "research this",
		Budgets:      SubtaskBudgets{MaxTurns: 5},
	}); err != nil {
		t.Fatalf("RunSubtask: %v", err)
	}
	llm.mu.Lock()
	defer llm.mu.Unlock()
	if len(llm.noPromptCache) != 1 {
		t.Fatalf("subtask made %d model calls, want exactly 1", len(llm.noPromptCache))
	}
	if llm.noPromptCache[0] {
		t.Errorf("a 5-turn subtask suppressed prompt caching; it replays its prefix enough times to pay the write back")
	}
}

func assertSuppressed(t *testing.T, llm *captureLLM, what string) {
	t.Helper()
	llm.mu.Lock()
	defer llm.mu.Unlock()
	if len(llm.noPromptCache) != 1 {
		t.Fatalf("%s made %d model calls, want exactly 1", what, len(llm.noPromptCache))
	}
	if !llm.noPromptCache[0] {
		t.Errorf("%s did not mark its context as a one-shot; it would pay the 1.25x cache-write premium for a read that never comes", what)
	}
}
