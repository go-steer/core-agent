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

package usage

import (
	"testing"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/adk/session"
	"google.golang.org/genai"
)

// eventWithUsage builds a *session.Event carrying cumulative usage.
// turnComplete=true marks the terminating chunk of a model turn.
// Event's LLMResponse fields (TurnComplete, UsageMetadata) are set
// via the embedded LLMResponse — session.Event embeds it.
func eventWithUsage(in, out int32, turnComplete bool) *session.Event {
	return &session.Event{
		LLMResponse: adkmodel.LLMResponse{
			TurnComplete: turnComplete,
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
				PromptTokenCount:     in,
				CandidatesTokenCount: out,
				TotalTokenCount:      in + out,
			},
		},
	}
}

// eventBare builds an event with only the LLMResponse.TurnComplete
// field set (no UsageMetadata) — used to test the zero-token guard.
func eventBare(turnComplete bool) *session.Event {
	return &session.Event{LLMResponse: adkmodel.LLMResponse{TurnComplete: turnComplete}}
}

// TestTurnTap_CommitsOncePerTurn is the direct regression test the
// #157 acceptance criteria asks for. Simulates the Gemini stream
// shape: two chunks with running cumulative usage, then the
// TurnComplete chunk with the final cumulative. Expect exactly one
// commit carrying the final cumulative — not the sum of the three
// readings.
func TestTurnTap_CommitsOncePerTurn(t *testing.T) {
	var tap TurnTap
	events := []*session.Event{
		eventWithUsage(500, 200, false),
		eventWithUsage(1500, 600, false),
		eventWithUsage(2000, 1000, true), // TurnComplete
	}

	commits := 0
	var lastCommit TurnUsage
	for _, ev := range events {
		tap.Observe(ev)
		if u, ok := tap.Commit(ev); ok {
			commits++
			lastCommit = u
		}
	}

	if commits != 1 {
		t.Fatalf("commits = %d, want 1 (one Commit per TurnComplete, regardless of chunk count)", commits)
	}
	if lastCommit.InputTokens != 2000 {
		t.Errorf("InputTokens = %d, want 2000 (final cumulative, not sum)", lastCommit.InputTokens)
	}
	if lastCommit.OutputTokens != 1000 {
		t.Errorf("OutputTokens = %d, want 1000 (final cumulative, not sum)", lastCommit.OutputTokens)
	}
}

func TestTurnTap_ResetsBetweenTurns(t *testing.T) {
	var tap TurnTap
	// Turn 1
	tap.Observe(eventWithUsage(1000, 400, false))
	tap.Observe(eventWithUsage(1200, 500, true))
	u1, ok1 := tap.Commit(eventBare(true))
	// Commit called with a zero-usage TurnComplete after observe already
	// updated state; but Commit uses ev arg only for TurnComplete flag,
	// so the ev arg being zero-usage doesn't matter — the tap's
	// captured `last` from the prior Observe is what commits. Verify.
	if !ok1 {
		t.Fatal("turn 1 Commit ok=false, want true (Observe already updated state)")
	}
	if u1.InputTokens != 1200 {
		t.Errorf("turn 1 InputTokens = %d, want 1200", u1.InputTokens)
	}
	// State reset — Peek returns zero.
	if p := tap.Peek(); p.InputTokens != 0 || p.OutputTokens != 0 {
		t.Errorf("after Commit, Peek = %+v, want zero", p)
	}

	// Turn 2 — different values; nothing carried over from turn 1.
	tap.Observe(eventWithUsage(300, 100, true))
	u2, ok2 := tap.Commit(eventBare(true))
	if !ok2 {
		t.Fatal("turn 2 Commit ok=false, want true")
	}
	if u2.InputTokens != 300 || u2.OutputTokens != 100 {
		t.Errorf("turn 2 usage = %+v, want {300,100} (no carryover from turn 1)", u2)
	}
}

func TestTurnTap_NoCommitWithoutTurnComplete(t *testing.T) {
	var tap TurnTap
	tap.Observe(eventWithUsage(500, 200, false))
	_, ok := tap.Commit(eventBare(false))
	if ok {
		t.Error("Commit ok=true without TurnComplete, want false")
	}
}

func TestTurnTap_NoCommitOnZeroTokenTurn(t *testing.T) {
	// A TurnComplete event with no prior UsageMetadata Observes
	// shouldn't create a zero-usage Turn record — guards against the
	// tracker seeing empty turns.
	var tap TurnTap
	_, ok := tap.Commit(eventBare(true))
	if ok {
		t.Error("Commit ok=true for zero-token turn, want false")
	}
}

func TestTurnTap_PeekReflectsRunningTotalMidTurn(t *testing.T) {
	// The TUI status sidebar wants a live running total during a
	// long turn. Peek should reflect the latest Observed usage
	// without needing a Commit.
	var tap TurnTap
	tap.Observe(eventWithUsage(700, 300, false))
	p1 := tap.Peek()
	if p1.InputTokens != 700 || p1.OutputTokens != 300 {
		t.Errorf("Peek mid-turn = %+v, want {700,300}", p1)
	}
	tap.Observe(eventWithUsage(1200, 500, false))
	p2 := tap.Peek()
	if p2.InputTokens != 1200 || p2.OutputTokens != 500 {
		t.Errorf("Peek after second Observe = %+v, want {1200,500}", p2)
	}
}

func TestTurnTap_NilAndUsageMetadataNilAreSafe(t *testing.T) {
	var tap TurnTap
	// nil event: no-op on Observe/Commit.
	tap.Observe(nil)
	if _, ok := tap.Commit(nil); ok {
		t.Error("Commit(nil) ok=true, want false")
	}
	// Event without UsageMetadata: Observe is a no-op.
	tap.Observe(eventBare(false))
	if p := tap.Peek(); p.InputTokens != 0 || p.OutputTokens != 0 {
		t.Errorf("Peek after no-usage Observe = %+v, want zero", p)
	}
}
