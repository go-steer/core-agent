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
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/adk/session"
	"google.golang.org/genai"
)

// autoContinueNoteEvent is a committed continuation note: a user-authored
// event carrying the marker, exactly as InjectAs commits one.
func autoContinueNoteEvent() *session.Event {
	return userTextEvent(AutoContinueNote(time.Now()))
}

// TestTransientTurnError pins the split at the heart of #969: which
// committed ErrorCodes are worth another turn. Every one of these was
// treated identically (terminal) before the fix, so the transient half
// of the table fails on pre-fix code.
func TestTransientTurnError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		code string
		want bool
	}{
		// Transient transport / load. Re-driving is the whole point.
		{"UNAVAILABLE", true},
		{"RESOURCE_EXHAUSTED", true},
		{"DEADLINE_EXCEEDED", true},
		{"503", true},
		{"504", true},
		{"502", true},
		{"429", true},

		// Terminal: identical input produces the identical failure, so a
		// re-drive is a loop with extra steps.
		{"SAFETY", false},
		{"RECITATION", false},
		{"BLOCKLIST", false},
		{"PROHIBITED_CONTENT", false},
		{"SPII", false},
		{"MALFORMED_FUNCTION_CALL", false},
		{"MAX_TOKENS", false},
		{"OTHER", false},
		{"PERMISSION_DENIED", false},
		{"UNAUTHENTICATED", false},
		{"NOT_FOUND", false},
		{"401", false},
		{"403", false},
		{"404", false},

		// INVALID_ARGUMENT is the ambiguous one (#898): the right code
		// for a bad config AND what the provider returns transiently
		// under load. It stays terminal here for the same reason
		// ClassifyTurnError keeps Retryable=false — deciding it is
		// transient is #935's job at the model layer, where the evidence
		// is. Re-driving on it would replay turns we have no evidence
		// are replayable.
		{"INVALID_ARGUMENT", false},
		{"400", false},

		// A cancel is a deliberate stop, never a failure to retry.
		{"CANCELED", false},

		// No code at all reads as terminal: this classifier only ever
		// promotes a code it recognizes.
		{"", false},
		{"   ", false},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			t.Parallel()
			if got := transientTurnError(tc.code); got != tc.want {
				t.Fatalf("transientTurnError(%q) = %v, want %v", tc.code, got, tc.want)
			}
		})
	}
}

// TestClassifyInterruptedTail_ErrorCodeSplit is the behaviour #969
// describes: a session parked by a transient provider error re-drives, a
// session parked by a safety block does not. On pre-fix code every case
// here returns interrupted=false, so the transient rows fail.
func TestClassifyInterruptedTail_ErrorCodeSplit(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name            string
		events          []*session.Event
		wantInterrupted bool
	}{
		{
			name: "transient 503 tail is re-driven",
			events: []*session.Event{
				userTextEvent("what is wrong with the cluster?"),
				errorFinalEvent("UNAVAILABLE"),
			},
			wantInterrupted: true,
		},
		{
			name: "rate-limit tail is re-driven",
			events: []*session.Event{
				userTextEvent("what is wrong with the cluster?"),
				callEvent(defaultAgentName, "inv-1", nil, &genai.FunctionCall{ID: "c1", Name: "bash"}),
				responseEvent(defaultAgentName, &genai.FunctionResponse{ID: "c1", Name: "bash"}),
				errorFinalEvent("RESOURCE_EXHAUSTED"),
			},
			wantInterrupted: true,
		},
		{
			name: "safety block is not re-driven",
			events: []*session.Event{
				userTextEvent("q"),
				errorFinalEvent("SAFETY"),
			},
			wantInterrupted: false,
		},
		{
			name: "auth failure is not re-driven",
			events: []*session.Event{
				userTextEvent("q"),
				errorFinalEvent("PERMISSION_DENIED"),
			},
			wantInterrupted: false,
		},
		{
			name: "malformed function call is not re-driven",
			events: []*session.Event{
				userTextEvent("q"),
				errorFinalEvent("MALFORMED_FUNCTION_CALL"),
			},
			wantInterrupted: false,
		},
		{
			// The classifier must still stop AT the error event rather
			// than walk past it: the response event behind a terminal
			// error is not an interrupted tail.
			name: "terminal error still shields the tail behind it",
			events: []*session.Event{
				userTextEvent("q"),
				callEvent(defaultAgentName, "inv-1", nil, &genai.FunctionCall{ID: "c1", Name: "bash"}),
				responseEvent(defaultAgentName, &genai.FunctionResponse{ID: "c1", Name: "bash"}),
				errorFinalEvent("RECITATION"),
			},
			wantInterrupted: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := ClassifyInterruptedTailVerdict(tc.events)
			if v.Interrupted != tc.wantInterrupted {
				t.Fatalf("Interrupted = %v, want %v", v.Interrupted, tc.wantInterrupted)
			}
			if v.Interrupted && v.InterruptedAt.IsZero() {
				t.Error("interrupted but InterruptedAt is zero — the freshness window would misfire")
			}
			if v.Interrupted && v.DeclineReason != "" {
				t.Errorf("DeclineReason = %q on an interrupted verdict, want empty", v.DeclineReason)
			}
			// A terminal error is an unremarkable stop; only the budget
			// speaks. Anything else here would put a line in the
			// operator's log on every healthy boot scan.
			if !v.Interrupted && v.DeclineReason != "" {
				t.Errorf("DeclineReason = %q, want empty for a terminal-shape decline", v.DeclineReason)
			}
		})
	}
}

// TestClassifyInterruptedTail_TransientBudget is the runaway bound. The
// budget is spent in committed continuation notes, so the sequence is
// reconstructed from history on every pass rather than held in memory —
// which is what makes it survive the daemon restart it exists to bound.
func TestClassifyInterruptedTail_TransientBudget(t *testing.T) {
	t.Parallel()

	// spent(n) builds a history in which n re-drives have already been
	// made and the provider has just failed transiently again: the
	// operator's question, then n × (continuation note, transient error).
	// The tail is that last error, with n notes behind it.
	spent := func(n int) []*session.Event {
		out := []*session.Event{userTextEvent("what is wrong with the cluster?")}
		for i := 0; i < n; i++ {
			out = append(out, autoContinueNoteEvent(), errorFinalEvent("UNAVAILABLE"))
		}
		return out
	}

	// n == 0 (the first transient error, no note yet) is covered by
	// TestClassifyInterruptedTail_ErrorCodeSplit.
	for n := 1; n < maxTransientRedrives; n++ {
		t.Run("re-drives with "+strconv.Itoa(n)+" notes spent", func(t *testing.T) {
			t.Parallel()
			v := ClassifyInterruptedTailVerdict(spent(n))
			if !v.Interrupted {
				t.Fatalf("Interrupted = false with %d notes spent, want true (budget is %d)", n, maxTransientRedrives)
			}
		})
	}

	t.Run("stops at the budget and says why", func(t *testing.T) {
		t.Parallel()
		v := ClassifyInterruptedTailVerdict(spent(maxTransientRedrives))
		if v.Interrupted {
			t.Fatalf("Interrupted = true with %d notes spent, want false — the budget is not bounding anything", maxTransientRedrives)
		}
		if v.DeclineReason == "" {
			t.Fatal("DeclineReason is empty: an unattended session that stops re-driving must say so, or it is indistinguishable from one that finished")
		}
		if !strings.Contains(v.DeclineReason, "UNAVAILABLE") {
			t.Errorf("DeclineReason = %q, want it to name the last error code", v.DeclineReason)
		}
		if !strings.Contains(v.DeclineReason, strconv.Itoa(maxTransientRedrives)) {
			t.Errorf("DeclineReason = %q, want it to name the count", v.DeclineReason)
		}
	})

	t.Run("a human message resets the budget", func(t *testing.T) {
		t.Parallel()
		// The operator comes back, asks something else, and the provider
		// blips once more. That is a fresh sequence, not the tail of the
		// old one — otherwise a session poisoned once would refuse to
		// self-heal for the rest of its life.
		events := append(spent(maxTransientRedrives),
			userTextEvent("try again, and check the events too"),
			errorFinalEvent("UNAVAILABLE"))
		v := ClassifyInterruptedTailVerdict(events)
		if !v.Interrupted {
			t.Fatalf("Interrupted = false after a human message, want true (DeclineReason = %q)", v.DeclineReason)
		}
	})

	t.Run("intervening model turns do not spend the budget", func(t *testing.T) {
		t.Parallel()
		// A re-drive that SUCCEEDED commits a model answer. The note is
		// still in history behind it, but it is not an unpaid attempt —
		// and the walk must not count model output as one either.
		events := []*session.Event{
			userTextEvent("what is wrong with the cluster?"),
			autoContinueNoteEvent(),
			modelTextEvent("the deployment is ImagePullBackOff"),
			annotationEvent(),
			errorFinalEvent("UNAVAILABLE"),
		}
		v := ClassifyInterruptedTailVerdict(events)
		if !v.Interrupted {
			t.Fatalf("Interrupted = false with one note spent, want true (DeclineReason = %q)", v.DeclineReason)
		}
	})

	t.Run("legacy-marker notes count against the budget", func(t *testing.T) {
		t.Parallel()
		// A note committed by a pre-#615 binary is still an attempt this
		// runtime made. Missing it would let an upgrade silently hand
		// back a fresh budget on a session already looping.
		legacy := func() *session.Event {
			return userTextEvent("[system note] The previous turn was interrupted by a daemon restart at 2026-08-10T00:00:00Z. Continue the task.")
		}
		events := []*session.Event{userTextEvent("q")}
		for i := 0; i < maxTransientRedrives; i++ {
			events = append(events, legacy(), errorFinalEvent("UNAVAILABLE"))
		}
		v := ClassifyInterruptedTailVerdict(events)
		if v.Interrupted {
			t.Fatal("Interrupted = true: legacy-marker notes are not counted, so an upgrade resets a looping session's budget")
		}
	})
}

// TestClassifyInterruptedTail_TerminalErrorEndsATransientSequence guards
// the ordering the budget depends on: the LAST error is what decides, so
// a terminal error arriving after transient ones stops the sequence
// immediately rather than being outvoted by them.
func TestClassifyInterruptedTail_TerminalErrorEndsATransientSequence(t *testing.T) {
	t.Parallel()
	events := []*session.Event{
		userTextEvent("q"),
		autoContinueNoteEvent(),
		errorFinalEvent("UNAVAILABLE"),
		autoContinueNoteEvent(),
		errorFinalEvent("SAFETY"),
	}
	v := ClassifyInterruptedTailVerdict(events)
	if v.Interrupted {
		t.Fatal("Interrupted = true after a safety block, want false — a terminal error must end the sequence")
	}
}
