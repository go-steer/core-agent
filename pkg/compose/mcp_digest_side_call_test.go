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

package compose

import (
	"context"
	"iter"
	"sync"
	"testing"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/pkg/agent"
	"github.com/go-steer/core-agent/v2/pkg/models"
)

// sideCallLLM records the side-call name on each request's context.
type sideCallLLM struct {
	mu    sync.Mutex
	names []string
}

func (*sideCallLLM) Name() string { return "side-call" }
func (l *sideCallLLM) GenerateContent(ctx context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	l.mu.Lock()
	l.names = append(l.names, models.SideCallName(ctx))
	l.mu.Unlock()
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		yield(&adkmodel.LLMResponse{
			Content:      &genai.Content{Role: "model", Parts: []*genai.Part{{Text: "digested"}}},
			TurnComplete: true,
		}, nil)
	}
}

// #1206. The digest subtask's events go to a session no capture holds,
// and a failure is swallowed into the raw output, so a provider retry
// inside it has no transcript surface: it must log as a side call. This
// also pins that the label survives RunSubtask's runner to the model.
func TestBuildMCPDigestLLMFallback_IsASideCall(t *testing.T) {
	t.Parallel()
	llm := &sideCallLLM{}
	a, err := agent.New(llm, agent.WithAppName("app"), agent.WithSession("u", "sess-digest-side"))
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	fn := BuildMCPDigestLLMFallback(&a, nil, "")
	if _, err := fn(context.Background(), []byte(`{"pods":[]}`)); err != nil {
		t.Fatalf("fallback: %v", err)
	}
	llm.mu.Lock()
	defer llm.mu.Unlock()
	if len(llm.names) == 0 {
		t.Fatal("no request reached the model")
	}
	for i, n := range llm.names {
		if n != "mcp digest" {
			t.Errorf("request %d marked as side call %q, want \"mcp digest\"", i, n)
		}
	}
}
