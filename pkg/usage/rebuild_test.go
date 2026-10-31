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

	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
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
	// Persisted events strip TurnComplete (ADK serialization quirk),
	// so rebuild uses UsageMetadata presence as the "committed turn"
	// signal. Each UsageMetadata-bearing event = one AppendUsage.
	// Non-UsageMetadata events are skipped.
	events := []*session.Event{
		// Turn 1 model response.
		mkEvent(100, 150, false, "gemini-3.5-flash"),
		// A follow-up event with no UsageMetadata (e.g. a tool-call
		// event) — should be skipped.
		{},
		// Turn 2 model response.
		mkEvent(200, 80, false, "gemini-3.5-flash"),
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
	// 2 turns committed (both had UsageMetadata; the empty middle
	// event was skipped).
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

func TestRebuildTrackerFromEvents_SkipsZeroTokenEvents(t *testing.T) {
	t.Parallel()
	// Vertex sometimes emits a UsageMetadata block with zero token
	// counts on error paths. Rebuild should skip these so they don't
	// inflate the turn count without adding cost.
	tracker := NewTracker()
	events := []*session.Event{
		mkEvent(0, 0, false, "gemini-3.5-flash"),
		mkEvent(100, 50, false, "gemini-3.5-flash"),
	}
	if err := RebuildTrackerFromEvents(
		t.Context(), tracker, seqFromEvents(events),
		"gemini-3.5-flash",
		func(string) Pricing { return Pricing{} },
	); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tracker.Totals().Turns != 1 {
		t.Errorf("Turns = %d; want 1 (zero-token event should be skipped)", tracker.Totals().Turns)
	}
}

func TestRebuildTrackerFromEvents_NilTracker(t *testing.T) {
	t.Parallel()
	// Nil tracker is a no-op — happens when the tracker feature is
	// disabled entirely (rare, but Setup returns nil in some paths).
	err := RebuildTrackerFromEvents(
		t.Context(), nil,
		seqFromEvents([]*session.Event{mkEvent(100, 50, false, "x")}),
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
		mkEvent(100, 50, false, ""),
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
		if !yield(mkEvent(100, 50, false, "x"), nil) {
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
		seqFromEvents([]*session.Event{mkEvent(100, 50, false, "x")}),
		"x",
		func(string) Pricing { return Pricing{} },
	)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

func TestRebuildTrackerFromEvents_PerModelAndCustomMetadataModelVersion(t *testing.T) {
	t.Parallel()
	tracker := NewTracker()
	// ADK's database persistence drops LLMResponse.ModelVersion, so
	// eventlog and BranchInjectingService stamp CustomMetadata["model_version"].
	subEvent := mkEvent(1_000_000, 500_000, false, "")
	subEvent.Branch = "researcher"
	subEvent.CustomMetadata = map[string]any{
		ModelVersionMetadataKey: "gemini-2.5-flash",
	}
	parentEvent := mkEvent(100_000, 50_000, false, "claude-opus-4-6")

	pricingFor := func(m string) Pricing {
		switch m {
		case "claude-opus-4-6":
			return Pricing{InputPerMTok: 15, OutputPerMTok: 75}
		case "gemini-2.5-flash":
			return Pricing{InputPerMTok: 0.30, OutputPerMTok: 2.50}
		default:
			return Pricing{Unpriced: true}
		}
	}

	if err := RebuildTrackerFromEvents(
		t.Context(), tracker,
		seqFromEvents([]*session.Event{parentEvent, subEvent}),
		"claude-opus-4-6",
		pricingFor,
	); err != nil {
		t.Fatalf("RebuildTrackerFromEvents: %v", err)
	}

	perModel := tracker.TotalsByModel()
	if len(perModel) != 2 {
		t.Fatalf("TotalsByModel len = %d (%v), want 2", len(perModel), perModel)
	}
	// Parent: 0.1 * 15 + 0.05 * 75 = 1.50 + 3.75 = 5.25
	// Subagent: 1.0 * 0.30 + 0.5 * 2.50 = 0.30 + 1.25 = 1.55
	if got := perModel["claude-opus-4-6"].CostUSD; got < 5.249 || got > 5.251 {
		t.Errorf("parent CostUSD = %f, want 5.25", got)
	}
	if got := perModel["gemini-2.5-flash"].CostUSD; got < 1.549 || got > 1.551 {
		t.Errorf("subagent CostUSD = %f, want 1.55", got)
	}
	if got := tracker.Totals().CostUSD; got < 6.799 || got > 6.801 {
		t.Errorf("total CostUSD = %f, want 6.80", got)
	}
}

func TestRebuildTrackerFromEvents_SkipsSkipParentUsageAndReplaysToolSavings(t *testing.T) {
	t.Parallel()
	tracker := NewTracker()

	parentTurn := mkEvent(1000, 200, false, "claude-opus-4-6")
	// SkipParentUsage subtask event (e.g., primary agent's shared MCP digest
	// fallback, #717, #1359) must be skipped on session-tree rebuild.
	skippedSubtask := mkEvent(50_000, 10_000, false, "gemini-2.5-flash")
	skippedSubtask.Branch = "sub.mcp_digest"
	skippedSubtask.CustomMetadata = map[string]any{
		SkipParentUsageMetadataKey: true,
		ModelVersionMetadataKey:    "gemini-2.5-flash",
	}
	// Tool-response savings sidecar in the calling session.
	toolResp := &session.Event{
		LLMResponse: model.LLMResponse{
			Content: &genai.Content{
				Role: "user",
				Parts: []*genai.Part{
					{
						FunctionResponse: &genai.FunctionResponse{
							Name: "mcp_tool",
							Response: map[string]any{
								"savings": map[string]any{
									"path":                   "llm_fallback",
									"original_tokens_est":    float64(12000),
									"digest_tokens_est":      float64(2000),
									"subagent_model":         "gemini-2.5-flash",
									"subagent_input_tokens":  float64(12000),
									"subagent_output_tokens": float64(2000),
								},
							},
						},
					},
				},
			},
		},
	}

	pricingFor := func(m string) Pricing {
		switch m {
		case "claude-opus-4-6":
			return Pricing{InputPerMTok: 10, OutputPerMTok: 50}
		case "gemini-2.5-flash":
			return Pricing{InputPerMTok: 1, OutputPerMTok: 2}
		default:
			return Pricing{Unpriced: true}
		}
	}

	if err := RebuildTrackerFromEvents(
		t.Context(), tracker,
		seqFromEvents([]*session.Event{parentTurn, skippedSubtask, toolResp}),
		"claude-opus-4-6",
		pricingFor,
	); err != nil {
		t.Fatalf("RebuildTrackerFromEvents: %v", err)
	}

	totals := tracker.Totals()
	// 1000 (parent) + 12000 (savings sidecar); the 50,000 skippedSubtask must not appear.
	if totals.InputTokens != 13000 {
		t.Errorf("InputTokens = %d, want 13000 (skip_parent_usage event must be skipped)", totals.InputTokens)
	}
	if totals.OutputTokens != 2200 {
		t.Errorf("OutputTokens = %d, want 2200", totals.OutputTokens)
	}
	// DigestSavings must NOT be replayed on rebuild so
	// WithoutSessionLabels's retiredDigest baseline does not double-count
	// core_agent.digest.subagent.cost_usd across eviction + resume.
	ds := tracker.DigestSavings()
	if ds.AgenticCalls != 0 || ds.AgenticSubagentCostUSD != 0 {
		t.Errorf("DigestSavings = %+v, want zero on rebuild (retiredDigest invariant)", ds)
	}
}

func TestRebuildTrackerFromEvents_SubtaskBranchPreservesLastTurn(t *testing.T) {
	t.Parallel()
	tracker := NewTracker()

	parentTurn := mkEvent(4000, 500, false, "claude-opus-4-6")
	subtaskTurn := mkEvent(800, 100, false, "")
	subtaskTurn.Branch = "sub.compact"
	subtaskTurn.CustomMetadata = map[string]any{
		ModelVersionMetadataKey: "gemini-2.5-flash",
	}

	if err := RebuildTrackerFromEvents(
		t.Context(), tracker,
		seqFromEvents([]*session.Event{parentTurn, subtaskTurn}),
		"claude-opus-4-6",
		func(string) Pricing { return Pricing{InputPerMTok: 1, OutputPerMTok: 1} },
	); err != nil {
		t.Fatalf("RebuildTrackerFromEvents: %v", err)
	}

	if tracker.Totals().InputTokens != 4800 {
		t.Errorf("Totals().InputTokens = %d, want 4800", tracker.Totals().InputTokens)
	}
	// Tracker.Last() must reflect the parent turn, not the side-usage subtask.
	last, ok := tracker.Last()
	if !ok || last.InputTokens != 4000 {
		t.Errorf("Last() = (%+v, %v), want InputTokens=4000 (subtask branch should use AppendSideUsage)", last, ok)
	}
}
