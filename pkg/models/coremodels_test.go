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

package models

import (
	"context"
	"iter"
	"testing"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/adk/session"
	"google.golang.org/genai"

	"github.com/go-steer/core-models/llm"

	"github.com/go-steer/core-agent/v2/pkg/usage"
)

// streamed yields what core-models' streaming dialects yield from
// v0.6.1 (llm.Response's contract): partial chunks, none marked
// TurnComplete, then the aggregate, the call's one TurnComplete. The
// last chunk carries usage too, as Gemini's does.
type streamed struct{}

func (streamed) Name() string { return "streamed" }

func (streamed) GenerateContent(_ context.Context, _ *llm.Request, _ bool) iter.Seq2[*llm.Response, error] {
	u := &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 100, CandidatesTokenCount: 10, TotalTokenCount: 110}
	content := genai.NewContentFromText("hi", genai.RoleModel)
	return func(yield func(*llm.Response, error) bool) {
		if !yield(&llm.Response{Content: content, Partial: true, UsageMetadata: u}, nil) {
			return
		}
		yield(&llm.Response{Content: content, TurnComplete: true, UsageMetadata: u}, nil)
	}
}

// TestAdapt_StreamedCallCountsOnce: usage.TurnTap commits on every
// TurnComplete event. core-models v0.6.0's Gemini dialect marked the
// last chunk as well as the aggregate, and a streamed call was billed
// twice; v0.6.1 marks only the aggregate. This pins that core-agent
// counts a call under that contract once.
func TestAdapt_StreamedCallCountsOnce(t *testing.T) {
	t.Parallel()
	m := Adapt(streamed{})
	var tap usage.TurnTap
	commits, input := 0, 0
	for resp, err := range m.GenerateContent(context.Background(), &adkmodel.LLMRequest{}, true) {
		if err != nil {
			t.Fatalf("GenerateContent: %v", err)
		}
		ev := &session.Event{LLMResponse: *resp}
		tap.Observe(ev)
		if u, ok := tap.Commit(ev); ok {
			commits++
			input += u.InputTokens
		}
	}
	if commits != 1 || input != 100 {
		t.Errorf("committed %d turns, %d input tokens; want 1 turn, 100 tokens", commits, input)
	}
}
