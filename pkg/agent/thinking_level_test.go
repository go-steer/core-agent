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

	adkagent "google.golang.org/adk/v2/agent"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// A configured level reaches the model on the agent's own turn, as the
// genai ThinkingLevel core-models maps per model.
func TestWithThinkingLevelReachesTheModel(t *testing.T) {
	t.Parallel()
	rec := &recordingLLM{}
	a, err := New(rec, WithThinkingLevel("xhigh"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, err := range a.RunWithContents(context.Background(), []*genai.Content{
		{Role: genai.RoleUser, Parts: []*genai.Part{{Text: "hi"}}},
	}) {
		if err != nil {
			t.Fatalf("RunWithContents: %v", err)
		}
	}
	req := rec.lastRequest()
	if req == nil || req.Config == nil || req.Config.ThinkingConfig == nil {
		t.Fatalf("request carried no thinking config: %+v", req)
	}
	if got := req.Config.ThinkingConfig.ThinkingLevel; got != genai.ThinkingLevelXhigh {
		t.Errorf("thinking level = %q, want XHIGH", got)
	}
	if a.ThinkingLevel() != "xhigh" {
		t.Errorf("Agent.ThinkingLevel() = %q, want xhigh", a.ThinkingLevel())
	}
}

// Without the option nothing is added, so existing requests are
// unchanged.
func TestNoThinkingLevelAddsNothing(t *testing.T) {
	t.Parallel()
	rec := &recordingLLM{}
	a, err := New(rec, WithThinkingLevel("  "))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, err := range a.RunWithContents(context.Background(), []*genai.Content{
		{Role: genai.RoleUser, Parts: []*genai.Part{{Text: "hi"}}},
	}) {
		if err != nil {
			t.Fatalf("RunWithContents: %v", err)
		}
	}
	if req := rec.lastRequest(); req != nil && req.Config != nil && req.Config.ThinkingConfig != nil {
		t.Errorf("thinking config added without a level: %+v", req.Config.ThinkingConfig)
	}
	if a.ThinkingLevel() != "" {
		t.Errorf("Agent.ThinkingLevel() = %q, want empty", a.ThinkingLevel())
	}
}

// The callback fills a level in only where the request says nothing about
// thinking, and never changes the caller's config.
func TestThinkingLevelCallback(t *testing.T) {
	t.Parallel()
	cb := thinkingLevelCallback(genai.ThinkingLevelHigh)
	for _, tc := range []struct {
		name string
		in   *genai.GenerateContentConfig
		want genai.ThinkingLevel
		keep bool // the request's own thinking settings stand
	}{
		{name: "no config", in: nil, want: genai.ThinkingLevelHigh},
		{name: "config without thinking", in: &genai.GenerateContentConfig{Temperature: genai.Ptr[float32](0.2)}, want: genai.ThinkingLevelHigh},
		{name: "include thoughts only", in: &genai.GenerateContentConfig{ThinkingConfig: &genai.ThinkingConfig{IncludeThoughts: true}}, want: genai.ThinkingLevelHigh},
		{name: "explicit level", in: &genai.GenerateContentConfig{ThinkingConfig: &genai.ThinkingConfig{ThinkingLevel: genai.ThinkingLevelLow}}, want: genai.ThinkingLevelLow, keep: true},
		{name: "explicit budget", in: &genai.GenerateContentConfig{ThinkingConfig: &genai.ThinkingConfig{ThinkingBudget: genai.Ptr[int32](0)}}, want: "", keep: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := &adkmodel.LLMRequest{Config: tc.in}
			var before *genai.ThinkingConfig
			if tc.in != nil && tc.in.ThinkingConfig != nil {
				c := *tc.in.ThinkingConfig
				before = &c
			}
			if resp, err := cb(nil, req); resp != nil || err != nil {
				t.Fatalf("%s: callback returned %v, %v; want nil, nil", tc.name, resp, err)
			}
			if got := req.Config.ThinkingConfig.ThinkingLevel; got != tc.want {
				t.Errorf("%s: level %q, want %q", tc.name, got, tc.want)
			}
			if tc.in != nil {
				if (before == nil) != (tc.in.ThinkingConfig == nil) || (before != nil && *before != *tc.in.ThinkingConfig) {
					t.Errorf("%s: the caller's config was changed: %+v", tc.name, tc.in.ThinkingConfig)
				}
			}
			if tc.name == "include thoughts only" && !req.Config.ThinkingConfig.IncludeThoughts {
				t.Errorf("%s: IncludeThoughts lost", tc.name)
			}
			if tc.name == "config without thinking" && req.Config.Temperature == nil {
				t.Errorf("%s: other config fields lost", tc.name)
			}
		})
	}
}

func runThinkingTurn(t *testing.T, a *Agent) {
	t.Helper()
	for _, err := range a.RunWithContents(context.Background(), []*genai.Content{
		{Role: genai.RoleUser, Parts: []*genai.Part{{Text: "hi"}}},
	}) {
		if err != nil {
			t.Fatalf("RunWithContents: %v", err)
		}
	}
}

// The last WithThinkingLevel wins, and what the accessor reports is what
// goes on the wire (background spawns inherit the accessor's value).
func TestThinkingLevelLastOptionWins(t *testing.T) {
	t.Parallel()
	rec := &recordingLLM{}
	a, err := New(rec, WithThinkingLevel("high"), WithThinkingLevel("low"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	runThinkingTurn(t, a)
	if got := rec.lastRequest().Config.ThinkingConfig.ThinkingLevel; got != genai.ThinkingLevelLow || a.ThinkingLevel() != "low" {
		t.Errorf("wire %q, accessor %q; want LOW and low", got, a.ThinkingLevel())
	}
}

// A caller's own BeforeModelCallback runs first, whatever the option
// order, and a thinking budget it sets stands: the level only fills a
// gap.
func TestThinkingLevelRespectsCallerCallbacks(t *testing.T) {
	t.Parallel()
	rec := &recordingLLM{}
	setBudget := func(_ adkagent.Context, req *adkmodel.LLMRequest) (*adkmodel.LLMResponse, error) {
		if req.Config == nil {
			req.Config = &genai.GenerateContentConfig{}
		}
		req.Config.ThinkingConfig = &genai.ThinkingConfig{ThinkingBudget: genai.Ptr[int32](0)}
		return nil, nil
	}
	a, err := New(rec, WithThinkingLevel("high"), WithBeforeModelCallbacks(setBudget))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	runThinkingTurn(t, a)
	tc := rec.lastRequest().Config.ThinkingConfig
	if tc.ThinkingLevel != "" || tc.ThinkingBudget == nil || *tc.ThinkingBudget != 0 {
		t.Errorf("thinking config %+v; want the caller's budget 0 and no level", tc)
	}
}

func TestThinkingLevelValidated(t *testing.T) {
	t.Parallel()
	if _, err := New(&recordingLLM{}, WithThinkingLevel("extreme")); err == nil {
		t.Fatal(`New with level "extreme": want an error`)
	}
	if _, err := New(&recordingLLM{}, WithThinkingLevel("MAX")); err != nil {
		t.Fatalf(`New with level "MAX": %v`, err)
	}
}
