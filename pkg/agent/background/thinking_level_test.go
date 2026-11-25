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

package background

import (
	"context"
	"iter"
	"sync"
	"testing"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v3/pkg/agent"
	"github.com/go-steer/core-agent/v3/pkg/models/mock"
)

// levelLLM records the thinking level of every request and answers with
// final text.
type levelLLM struct {
	mu     sync.Mutex
	levels []genai.ThinkingLevel
}

func (l *levelLLM) Name() string { return "level" }

func (l *levelLLM) GenerateContent(_ context.Context, req *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	var lvl genai.ThinkingLevel
	if req.Config != nil && req.Config.ThinkingConfig != nil {
		lvl = req.Config.ThinkingConfig.ThinkingLevel
	}
	l.mu.Lock()
	l.levels = append(l.levels, lvl)
	l.mu.Unlock()
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		yield(&adkmodel.LLMResponse{
			Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "done"}}},
			TurnComplete: true,
		}, nil)
	}
}

func (l *levelLLM) seen() []genai.ThinkingLevel {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]genai.ThinkingLevel(nil), l.levels...)
}

// A background spawn inherits the parent's thinking level, unless its
// template (a declared subagent with its own model.thinking_level) sets
// one.
func TestSpawnTemplate_ThinkingLevel(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		template string
		want     genai.ThinkingLevel
	}{
		{"inherits the parent's", "", genai.ThinkingLevelHigh},
		{"its own wins", "low", genai.ThinkingLevelLow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			child := &levelLLM{}
			prov := &recordingProvider{llm: child}
			mgr := newTemplateManager(t, prov, []SubagentTemplate{{
				Name:          "cluster",
				Instruction:   "triage clusters",
				ModelFactory:  tmplFactory(prov, "cluster-model"),
				ModelID:       "cluster-model",
				ThinkingLevel: tc.template,
			}}, WithDefaultBudgets(Budgets{MaxTurns: 1}))
			defer mgr.Close()
			parentLLM, err := mock.NewEcho().Model(context.Background(), "echo")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := agent.New(parentLLM, agent.WithBackgroundManager(mgr), agent.WithThinkingLevel("high")); err != nil {
				t.Fatalf("agent.New: %v", err)
			}
			h, err := mgr.SpawnTemplate(context.Background(), "", "cluster", RefOverrides{Goal: "look"}, "")
			if err != nil {
				t.Fatalf("SpawnTemplate: %v", err)
			}
			waitDone(t, h)
			seen := child.seen()
			if len(seen) == 0 {
				t.Fatal("the spawned subagent never called its model")
			}
			for _, got := range seen {
				if got != tc.want {
					t.Errorf("spawned subagent sent level %q, want %q (all: %v)", got, tc.want, seen)
				}
			}
		})
	}
}
