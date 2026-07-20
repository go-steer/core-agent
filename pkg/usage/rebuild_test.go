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
	"context"
	"errors"
	"iter"
	"testing"

	"google.golang.org/adk/model"
	"google.golang.org/adk/session"
	"google.golang.org/genai"
)

// mkEvent builds an ADK Event carrying the given usage. Split across
// two calls per turn so the test exercises the cumulative-chunk shape
// TurnTap handles: chunk 1 has partial usage, TurnComplete has final.
func mkEvent(promptTokens, candTokens int32, complete bool, model string) *session.Event {
	return &session.Event{
		LLMResponse: model_LLMResponse(promptTokens, candTokens, complete, model),
	}
}

// model_LLMResponse constructs the embedded LLMResponse in an Event.
// Kept as a helper so tests can build events without repeating the
// nested-struct incantation.
func model_LLMResponse(promptTokens, candTokens int32, complete bool, modelName string) model.LLMResponse {
	return model.LLMResponse{
		UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
			PromptTokenCount:     promptTokens,
			CandidatesTokenCount: candTokens,
			TotalTokenCount:      promptTokens + candTokens,
		},
		TurnComplete: complete,
		ModelVersion: modelName,
	}
}

// seqFromEvents wraps a []*session.Event in the iter.Seq2 signature
// RebuildTrackerFromEvents expects, with nil errors.
func seqFromEvents(events []*session.Event) iter.Seq2[*session.Event, error] {
	return func(yield func(*session.Event, error) bool) {
		for _, ev := range events {
			if !yield(ev, nil) {
				return
			}
		}
	}
}

func TestRebuildTrackerFromEvents_MultiTurn(t *testing.T) {
	t.Parallel()
	tracker := NewTracker()
	events := []*session.Event{
		// Turn 1: mid-stream chunk + final. Cumulative shape.
		mkEvent(100, 50, false, "gemini-3.5-flash"),
		mkEvent(100, 150, true, "gemini-3.5-flash"),
		// Turn 2: single event, TurnComplete=true.
		mkEvent(200, 80, true, "gemini-3.5-flash"),
	}

	err := RebuildTrackerFromEvents(
		t.Context(), tracker, seqFromEvents(events),
		"gemini-3.5-flash",
		func(string) Pricing { return Pricing{InputPerMTok: 1, OutputPerMTok: 5} },
	)
	if err != nil {
		t.Fatalf("RebuildTrackerFromEvents: %v", err)
	}

	totals := tracker.Totals()
	// 2 committed turns (mid-stream chunk with TurnComplete=false didn't
	// commit; the final chunk with TurnComplete=true did).
	if totals.Turns != 2 {
		t.Errorf("Turns = %d; want 2", totals.Turns)
	}
	// Input: 100 + 200 = 300; Output: 150 + 80 = 230.
	if totals.InputTokens != 300 {
		t.Errorf("InputTokens = %d; want 300", totals.InputTokens)
	}
	if totals.OutputTokens != 230 {
		t.Errorf("OutputTokens = %d; want 230", totals.OutputTokens)
	}
}

func TestRebuildTrackerFromEvents_NilTracker(t *testing.T) {
	t.Parallel()
	// Nil tracker is a no-op — happens when the tracker feature is
	// disabled entirely (rare, but Setup returns nil in some paths).
	err := RebuildTrackerFromEvents(
		t.Context(), nil,
		seqFromEvents([]*session.Event{mkEvent(100, 50, true, "x")}),
		"x",
		func(string) Pricing { return Pricing{} },
	)
	if err != nil {
		t.Errorf("nil tracker: unexpected error %v", err)
	}
}

func TestRebuildTrackerFromEvents_ModelFallback(t *testing.T) {
	t.Parallel()
	tracker := NewTracker()
	// Event with empty ModelVersion — should fall back to default.
	events := []*session.Event{
		mkEvent(100, 50, true, ""),
	}
	err := RebuildTrackerFromEvents(
		t.Context(), tracker, seqFromEvents(events),
		"fallback-model",
		func(string) Pricing { return Pricing{} },
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	perModel := tracker.TotalsByModel()
	if _, ok := perModel["fallback-model"]; !ok {
		t.Errorf("expected 'fallback-model' in per-model totals, got %v", perModel)
	}
}

func TestRebuildTrackerFromEvents_IteratorError(t *testing.T) {
	t.Parallel()
	tracker := NewTracker()
	sentinelErr := errors.New("scan failed")
	iterWithErr := func(yield func(*session.Event, error) bool) {
		// First event succeeds.
		if !yield(mkEvent(100, 50, true, "x"), nil) {
			return
		}
		// Second yields an error.
		yield(nil, sentinelErr)
	}

	err := RebuildTrackerFromEvents(
		t.Context(), tracker, iterWithErr,
		"x",
		func(string) Pricing { return Pricing{} },
	)
	if !errors.Is(err, sentinelErr) {
		t.Errorf("expected sentinel error, got %v", err)
	}
	// First turn WAS committed before the error surfaced.
	if tracker.Totals().Turns != 1 {
		t.Errorf("Turns = %d; want 1 (event before the error should have landed)", tracker.Totals().Turns)
	}
}

func TestRebuildTrackerFromEvents_ContextCancel(t *testing.T) {
	t.Parallel()
	tracker := NewTracker()
	ctx, cancel := context.WithCancel(t.Context())
	cancel() // pre-cancel

	err := RebuildTrackerFromEvents(
		ctx, tracker,
		seqFromEvents([]*session.Event{mkEvent(100, 50, true, "x")}),
		"x",
		func(string) Pricing { return Pricing{} },
	)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}
