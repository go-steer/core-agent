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

package main

import (
	"context"
	"iter"
	"testing"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/pkg/agent"
	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/usage"
)

// cumulativeUsageLLM emits two LLMResponses for one turn: an
// intermediate event carrying running-cumulative UsageMetadata
// (TurnComplete=false), then a final event carrying the final
// cumulative UsageMetadata (TurnComplete=true). This is the exact
// stream shape Gemini produces and the one that triggered the
// "totals exactly 2x last turn" double-count regression in the
// core-tui adapter (see coretui_enabled.go Run loop).
type cumulativeUsageLLM struct {
	interIn, interOut int32 // running cumulative on the intermediate event
	finalIn, finalOut int32 // final cumulative on the TurnComplete event
}

func (l *cumulativeUsageLLM) Name() string { return "gemini-3.5-flash" }

func (l *cumulativeUsageLLM) GenerateContent(_ context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		content := &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "ok"}}}
		if !yield(&adkmodel.LLMResponse{
			Content: content,
			Partial: true,
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
				PromptTokenCount:     l.interIn,
				CandidatesTokenCount: l.interOut,
				TotalTokenCount:      l.interIn + l.interOut,
			},
		}, nil) {
			return
		}
		yield(&adkmodel.LLMResponse{
			Content:      content,
			TurnComplete: true,
			FinishReason: genai.FinishReasonStop,
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
				PromptTokenCount:     l.finalIn,
				CandidatesTokenCount: l.finalOut,
				TotalTokenCount:      l.finalIn + l.finalOut,
			},
		}, nil)
	}
}

// TestCoreAgentAdapterRun_AppendsOncePerTurn locks in the fix for
// the "totals exactly 2x last turn" bug. The bug shape: the
// adapter Appended on every event carrying UsageMetadata, but
// Gemini's UsageMetadata is cumulative across stream chunks in
// one turn, so two events → two Appends summing the running
// totals.
//
// With the fix (commit usage once on TurnComplete, mirroring
// pkg/runner/headless.go tapUsage and pkg/agent/subtask.go), one
// model turn produces exactly one tracker entry carrying the
// final per-turn token counts — not the sum of the intermediate
// and final cumulative readings.
func TestCoreAgentAdapterRun_AppendsOncePerTurn(t *testing.T) {
	llm := &cumulativeUsageLLM{
		interIn: 1500, interOut: 600, // running cumulative mid-stream
		finalIn: 2000, finalOut: 1000, // final cumulative at TurnComplete
	}
	inner, err := agent.New(llm, agent.WithName("test"))
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	tracker := usage.NewTracker()
	adapter := &coreAgentAdapter{
		inner: inner,
		deps: tuiDeps{
			Cfg:     &config.Config{},
			Tracker: tracker,
		},
	}

	for ev, runErr := range adapter.Run(context.Background(), "hi") {
		_ = ev
		if runErr != nil {
			t.Fatalf("adapter.Run: %v", runErr)
		}
	}

	totals := tracker.Totals()
	if totals.Turns != 1 {
		t.Errorf("Tracker.Totals().Turns = %d, want 1 (one model turn, one Append)", totals.Turns)
	}
	if totals.InputTokens != int(llm.finalIn) {
		t.Errorf("Tracker.Totals().InputTokens = %d, want %d (the final per-turn cumulative, NOT the sum of intermediate+final)",
			totals.InputTokens, llm.finalIn)
	}
	if totals.OutputTokens != int(llm.finalOut) {
		t.Errorf("Tracker.Totals().OutputTokens = %d, want %d", totals.OutputTokens, llm.finalOut)
	}
}
