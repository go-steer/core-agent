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

package attach

import (
	"encoding/json"
	"strings"
	"testing"

	"google.golang.org/adk/session"
)

func TestContextReductionFailedEvent_RoundTrip(t *testing.T) {
	t.Parallel()
	ev := NewContextReductionFailedEvent(ContextReductionCompaction,
		"agent: compaction: model returned no summary text (finish_reason=MAX_TOKENS)", 3, 8)

	// ADK carries an out-of-band row's name in InvocationID, which is
	// the slot guardrail_events.go uses for the same purpose.
	if ev.InvocationID != ContextReductionFailedEventName {
		t.Errorf("invocation id = %q, want %q", ev.InvocationID, ContextReductionFailedEventName)
	}
	op, reason, ok := ContextReductionFailure(ev)
	if !ok {
		t.Fatal("ContextReductionFailure did not recognise its own event")
	}
	if op != ContextReductionCompaction {
		t.Errorf("operation = %q, want %q", op, ContextReductionCompaction)
	}
	// The provider's explanation is the entire point of the row; a
	// reader that summarises it down to a category throws away the
	// only thing that makes the next occurrence diagnosable.
	if !strings.Contains(reason, "MAX_TOKENS") {
		t.Errorf("reason = %q, want the provider explanation carried verbatim", reason)
	}
	if got := ev.CustomMetadata[ctxReductionMetaFailures]; got != 3 {
		t.Errorf("consecutive_failures = %v, want 3", got)
	}
	if got := ev.CustomMetadata[ctxReductionMetaCooldown]; got != 8 {
		t.Errorf("cooldown_turns = %v, want 8", got)
	}
}

// The backoff keys are omitted, not zeroed, for a writer that has no
// backoff — a key that is always present but meaningless for half its
// writers reads as "zero failures so far".
func TestContextReductionFailedEvent_OmitsUnsetBackoff(t *testing.T) {
	t.Parallel()
	ev := NewContextReductionFailedEvent(ContextReductionCheckpoint, "boom", 0, 0)
	for _, k := range []string{ctxReductionMetaFailures, ctxReductionMetaCooldown} {
		if _, present := ev.CustomMetadata[k]; present {
			t.Errorf("metadata key %q present for a writer with no backoff", k)
		}
	}
	op, _, ok := ContextReductionFailure(ev)
	if !ok || op != ContextReductionCheckpoint {
		t.Errorf("ContextReductionFailure = (%q, ok=%v), want (%q, true)", op, ok, ContextReductionCheckpoint)
	}
}

// The row travels as an ordinary session.Event over the existing
// back-compat `agent` SSE frame, so it has to survive a JSON hop. Ints
// come back as float64 there; the reader must not depend on them.
func TestContextReductionFailedEvent_SurvivesJSON(t *testing.T) {
	t.Parallel()
	raw, err := json.Marshal(NewContextReductionFailedEvent(ContextReductionCompaction, "boom", 2, 4))
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var back session.Event
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	op, reason, ok := ContextReductionFailure(&back)
	if !ok {
		t.Fatal("row not recognisable after a JSON round trip")
	}
	if op != ContextReductionCompaction || reason != "boom" {
		t.Errorf("after round trip: operation=%q reason=%q", op, reason)
	}
}

// The row lands in the same session the model reads its history from,
// so it must not become part of the prompt: ADK's content builder drops
// events whose Content is nil, and "an automatic compaction failed" is
// operator-facing news, not something to narrate at the model in the
// middle of its next turn.
func TestContextReductionFailedEvent_CarriesNoModelContent(t *testing.T) {
	t.Parallel()
	ev := NewContextReductionFailedEvent(ContextReductionCompaction, "boom", 1, 2)
	if ev.LLMResponse.Content != nil {
		t.Errorf("row carries model content %+v; it would be replayed into the next prompt", ev.LLMResponse.Content)
	}
}

// Run over an undifferentiated event stream, the reader must claim only
// its own rows.
func TestContextReductionFailure_IgnoresOtherEvents(t *testing.T) {
	t.Parallel()
	other := session.NewEvent("model-turn")
	other.Author = "assistant"
	other.CustomMetadata = map[string]any{ctxReductionMetaOp: ContextReductionCompaction}
	for _, ev := range []*session.Event{nil, session.NewEvent("bare"), other,
		NewGuardrailTripEvent("watchdog", "looping")} {
		if _, _, ok := ContextReductionFailure(ev); ok {
			t.Errorf("claimed a foreign event: %+v", ev)
		}
	}
}
