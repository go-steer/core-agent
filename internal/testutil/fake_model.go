// Copyright 2026 The Cogo Authors.
// SPDX-License-Identifier: Apache-2.0

// Package testutil provides shared test helpers for Cogo, most notably
// FakeModel — a deterministic implementation of google.golang.org/adk/model.LLM
// that lets us drive end-to-end agent tests without burning real tokens.
package testutil

import (
	"context"
	"iter"
	"strings"

	"google.golang.org/adk/model"
	"google.golang.org/genai"
)

// ScriptedResponse describes one turn of FakeModel output.
//
// TextChunks are emitted as individual streaming Partial events (one per
// chunk) when the caller asks for a stream. After the chunks, FakeModel
// emits a single TurnComplete=true event whose Content is the concatenation
// of all chunks. In non-streaming mode (stream=false) only the final
// TurnComplete event is emitted.
//
// InputTokens / OutputTokens populate the final event's UsageMetadata
// when set, so usage-tracking tests don't need a real model.
type ScriptedResponse struct {
	TextChunks   []string
	InputTokens  int
	OutputTokens int
}

// FakeModel implements model.LLM with scripted output for tests.
//
// Each call to GenerateContent advances the script by one entry. If the
// script is exhausted, an empty TurnComplete response is returned.
type FakeModel struct {
	ModelName string
	Script    []ScriptedResponse

	calls int
}

// Name reports the configured model name (or "fake" if unset).
func (f *FakeModel) Name() string {
	if f.ModelName == "" {
		return "fake"
	}
	return f.ModelName
}

// Calls returns the number of GenerateContent invocations seen so far.
// Useful in tests that want to assert on call count.
func (f *FakeModel) Calls() int { return f.calls }

// GenerateContent yields the next scripted response. When stream is true,
// each TextChunk is emitted as its own Partial event; when false, only the
// final consolidated event is emitted.
func (f *FakeModel) GenerateContent(_ context.Context, _ *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	var sr ScriptedResponse
	if f.calls < len(f.Script) {
		sr = f.Script[f.calls]
	}
	f.calls++

	return func(yield func(*model.LLMResponse, error) bool) {
		if stream {
			for _, chunk := range sr.TextChunks {
				if !yield(&model.LLMResponse{
					Content: textContent(chunk),
					Partial: true,
				}, nil) {
					return
				}
			}
		}
		final := &model.LLMResponse{
			Content:      textContent(concat(sr.TextChunks)),
			TurnComplete: true,
		}
		if sr.InputTokens > 0 || sr.OutputTokens > 0 {
			// Test fixtures only — small token counts, no overflow risk.
			final.UsageMetadata = &genai.GenerateContentResponseUsageMetadata{
				PromptTokenCount:     int32(sr.InputTokens),                   // #nosec G115
				CandidatesTokenCount: int32(sr.OutputTokens),                  // #nosec G115
				TotalTokenCount:      int32(sr.InputTokens + sr.OutputTokens), // #nosec G115
			}
		}
		yield(final, nil)
	}
}

func textContent(text string) *genai.Content {
	return &genai.Content{
		Role:  genai.RoleModel,
		Parts: []*genai.Part{{Text: text}},
	}
}

func concat(chunks []string) string {
	var b strings.Builder
	for _, c := range chunks {
		b.WriteString(c)
	}
	return b.String()
}
