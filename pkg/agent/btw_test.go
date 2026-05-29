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
	"iter"
	"strings"
	"sync"
	"testing"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/genai"
)

// captureLLM is a tiny adkmodel.LLM that records every request it's
// asked to generate, so tests can assert what conversation history
// reached the model. Optionally emits UsageMetadata (inputTokens /
// outputTokens) so cost-rollup tests can verify the parent's
// tracker picks up subtask usage.
type captureLLM struct {
	mu           sync.Mutex
	reqs         []*adkmodel.LLMRequest
	response     string
	err          error
	inputTokens  int32 // optional: include in UsageMetadata on the response
	outputTokens int32 // optional: include in UsageMetadata on the response
}

func (l *captureLLM) Name() string { return "capture" }

func (l *captureLLM) GenerateContent(_ context.Context, req *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	l.mu.Lock()
	l.reqs = append(l.reqs, req)
	resp := l.response
	err := l.err
	in := l.inputTokens
	out := l.outputTokens
	l.mu.Unlock()
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		if err != nil {
			yield(nil, err)
			return
		}
		r := &adkmodel.LLMResponse{
			Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: resp}}},
			TurnComplete: true,
		}
		if in > 0 || out > 0 {
			r.UsageMetadata = &genai.GenerateContentResponseUsageMetadata{
				PromptTokenCount:     in,
				CandidatesTokenCount: out,
				TotalTokenCount:      in + out,
			}
		}
		yield(r, nil)
	}
}

func (l *captureLLM) lastRequest() *adkmodel.LLMRequest {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.reqs) == 0 {
		return nil
	}
	return l.reqs[len(l.reqs)-1]
}

func TestAskSideQuestion_ReturnsModelTextAndHasNoTools(t *testing.T) {
	t.Parallel()
	llm := &captureLLM{response: "It was main.go."}
	a, err := New(llm)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ans, err := a.AskSideQuestion(context.Background(), "what was that file again?")
	if err != nil {
		t.Fatalf("AskSideQuestion: %v", err)
	}
	if ans != "It was main.go." {
		t.Errorf("answer = %q, want %q", ans, "It was main.go.")
	}
	req := llm.lastRequest()
	if req == nil {
		t.Fatalf("no request recorded on the model")
	}
	if len(req.Tools) != 0 {
		t.Errorf("Tools = %d, want 0 (side queries are tool-less)", len(req.Tools))
	}
	if len(req.Contents) == 0 {
		t.Fatalf("Contents empty, want at least the question")
	}
	last := req.Contents[len(req.Contents)-1]
	if last.Role != genai.RoleUser {
		t.Errorf("last content role = %q, want user", last.Role)
	}
	gotQ := ""
	for _, p := range last.Parts {
		if p != nil && p.Text != "" {
			gotQ += p.Text
		}
	}
	if !strings.Contains(gotQ, "what was that file again?") {
		t.Errorf("last content text = %q, want it to include the question", gotQ)
	}
}

func TestAskSideQuestion_EmptyQuestionErrors(t *testing.T) {
	t.Parallel()
	llm := &captureLLM{response: "irrelevant"}
	a, err := New(llm)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := a.AskSideQuestion(context.Background(), "   "); err == nil {
		t.Errorf("expected error for empty question, got nil")
	}
}

func TestAskSideQuestion_PropagatesModelError(t *testing.T) {
	t.Parallel()
	llm := &captureLLM{err: errMockBoom}
	a, err := New(llm)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := a.AskSideQuestion(context.Background(), "ping"); err == nil {
		t.Errorf("expected wrapped model error, got nil")
	} else if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error = %q, want it to wrap boom", err.Error())
	}
}

func TestAskSideQuestion_BypassesAgentRun(t *testing.T) {
	t.Parallel()
	// Use the agent's inbox + a queued message to prove the side
	// question does NOT trigger the pre-turn inbox drain that
	// Agent.Run performs. If AskSideQuestion went through Run, the
	// queued message would land in the model's request — and the
	// inbox would be empty after the call.
	llm := &captureLLM{response: "ack"}
	a, err := New(llm)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := a.Inject("a queued operator note"); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if _, err := a.AskSideQuestion(context.Background(), "side q"); err != nil {
		t.Fatalf("AskSideQuestion: %v", err)
	}
	if got := a.PendingInboxCount(); got != 1 {
		t.Errorf("inbox count after /btw = %d, want 1 (side query must not drain)", got)
	}
	req := llm.lastRequest()
	for _, c := range req.Contents {
		for _, p := range c.Parts {
			if p != nil && strings.Contains(p.Text, "queued operator note") {
				t.Errorf("queued inbox note leaked into side-query request: %#v", p)
			}
		}
	}
}

// errMockBoom is a sentinel error used by TestAskSideQuestion_PropagatesModelError.
var errMockBoom = mockErr("boom")

type mockErr string

func (e mockErr) Error() string { return string(e) }
