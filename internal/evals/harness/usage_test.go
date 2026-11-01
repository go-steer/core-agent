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

package harness

import (
	"context"
	"iter"
	"strings"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v3/internal/evals"
	"github.com/go-steer/core-agent/v3/internal/evals/evalcompose"
)

type usageModel struct{ responses []*model.LLMResponse }

func (u usageModel) Name() string { return "u" }

func (u usageModel) GenerateContent(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		for _, r := range u.responses {
			if !yield(r, nil) {
				return
			}
		}
	}
}

func TestUsageCounterSumsCompleteResponsesOnly(t *testing.T) {
	c := &usageCounter{inner: usageModel{responses: []*model.LLMResponse{
		{Partial: true, UsageMetadata: &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 999}},
		{UsageMetadata: &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 100, CachedContentTokenCount: 40, CandidatesTokenCount: 20, ThoughtsTokenCount: 5}},
		{}, // a complete response with no usage still counts as a call
	}}}
	for range 2 {
		for range c.GenerateContent(context.Background(), &model.LLMRequest{}, true) {
		}
	}
	got := *c.snapshot()
	want := TokenUsage{Calls: 4, PromptTokens: 200, CachedTokens: 80, OutputTokens: 40, ThoughtsTokens: 10}
	if got != want {
		t.Errorf("usage = %+v, want %+v", got, want)
	}
}

func TestSelectRows(t *testing.T) {
	ds := evals.Dataset{Scenarios: []evals.Scenario{{ID: "a"}, {ID: "b"}, {ID: "c"}}}
	got, err := selectRows(ds, []string{"c", "a"})
	if err != nil || len(got.Scenarios) != 2 || got.Scenarios[0].ID != "a" || got.Scenarios[1].ID != "c" {
		t.Errorf("selectRows = %+v, %v; want a then c, in corpus order", got.Scenarios, err)
	}
	if all, _ := selectRows(ds, nil); len(all.Scenarios) != 3 {
		t.Error("no rows selected should run the whole corpus")
	}
	if _, err := selectRows(ds, []string{"a", "zz"}); err == nil || !strings.Contains(err.Error(), "zz") {
		t.Errorf("an unknown row = %v, want it named", err)
	}
	if len(ds.Scenarios) != 3 {
		t.Error("selectRows modified its input")
	}
}

// TestTheGraderNeverCaches: the zero prompt-cache block is the product
// default (on), so the grader has to turn caching off explicitly, while
// the model under test follows --prompt-cache.
func TestTheGraderNeverCaches(t *testing.T) {
	if graderModelOptions().PromptCache.On() {
		t.Error("the grader is built with prompt caching on")
	}
	if !underTestModelOptions(Config{}).PromptCache.On() {
		t.Error("the model under test does not follow the product default (on) when --prompt-cache is unset")
	}
	off := false
	if underTestModelOptions(Config{PromptCache: evalcompose.PromptCache{Enabled: &off}}).PromptCache.On() {
		t.Error("--prompt-cache=off did not reach the model under test")
	}
}
