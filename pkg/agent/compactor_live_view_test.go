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
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/glebarez/sqlite"
	adkmodel "google.golang.org/adk/model"
	"google.golang.org/adk/tool"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/pkg/eventlog"
)

// The post-summary view must be live. The runner Gets the session once
// per turn and appends the user's message, then each function call and
// response, AFTER that Get — so a view frozen at Get time sent every
// post-compaction turn the summary with nothing after it, and a tool
// turn's follow-up request without its own call and response.

// liveViewLLM answers plainly while the history is seeded, "[SUMMARY]"
// while asked to summarize, and — on the turn under test — calls the
// noop tool first and answers with text once the tool's response is in.
// It records only the requests of the turn under test.
type liveViewLLM struct {
	mu   sync.Mutex
	mode liveViewMode
	reqs []*adkmodel.LLMRequest
}

type liveViewMode int

const (
	liveViewSeed liveViewMode = iota
	liveViewSummarize
	liveViewTurn
)

func (l *liveViewLLM) setMode(m liveViewMode) {
	l.mu.Lock()
	l.mode = m
	l.mu.Unlock()
}

func (*liveViewLLM) Name() string { return "live-view" }

func (l *liveViewLLM) GenerateContent(_ context.Context, req *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	l.mu.Lock()
	mode := l.mode
	if mode == liveViewTurn {
		l.reqs = append(l.reqs, req)
	}
	calls := len(l.reqs)
	l.mu.Unlock()
	part := &genai.Part{Text: "done"}
	switch {
	case calls > 3:
		// A model that never sees its tool's response calls the tool
		// again, forever — the live symptom of a frozen view. Stop after
		// a few so the assertions below fail instead of the test hanging.
	case mode == liveViewSeed:
		part = &genai.Part{Text: "agent, models, tools, mcp"}
	case mode == liveViewSummarize:
		part = &genai.Part{Text: "[SUMMARY] we listed the subsystems"}
	case !hasFunctionResponse(req):
		part = &genai.Part{FunctionCall: &genai.FunctionCall{ID: "call-live-1", Name: noopToolName, Args: map[string]any{}}}
	}
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		yield(&adkmodel.LLMResponse{
			Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{part}},
			FinishReason: genai.FinishReasonStop,
			TurnComplete: true,
		}, nil)
	}
}

func hasFunctionResponse(req *adkmodel.LLMRequest) bool {
	for _, c := range req.Contents {
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			if p.FunctionResponse != nil {
				return true
			}
		}
	}
	return false
}

func (l *liveViewLLM) requests() []*adkmodel.LLMRequest {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]*adkmodel.LLMRequest(nil), l.reqs...)
}

func TestCompactedSession_TurnRequestsSeeTheTurnsOwnEvents(t *testing.T) {
	t.Parallel()
	for name, opts := range map[string]func(t *testing.T) []Option{
		"in-memory": func(*testing.T) []Option { return nil },
		"eventlog": func(t *testing.T) []Option {
			h, err := eventlog.Open(context.Background(), sqlite.Open(filepath.Join(t.TempDir(), "s.db")))
			if err != nil {
				t.Fatalf("eventlog.Open: %v", err)
			}
			t.Cleanup(func() { _ = h.Close() })
			return []Option{WithEventLog(h)}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			llm := &liveViewLLM{}
			a, err := New(llm, append(opts(t),
				WithCompactor(NewDefaultCompactor()),
				WithTools([]tool.Tool{newNoopTool(t)}))...)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			// Real turns, not planted events: a durable store keys rows on
			// event ID, and only the runner mints those.
			for _, err := range a.Run(context.Background(), "what are the main subsystems?") {
				if err != nil {
					t.Fatalf("seed Run: %v", err)
				}
			}
			llm.setMode(liveViewSummarize)
			if res, err := a.Compact(context.Background(), ""); err != nil || res.Skipped {
				t.Fatalf("Compact: res=%+v err=%v", res, err)
			}

			llm.setMode(liveViewTurn)
			for _, err := range a.Run(context.Background(), "NEW-USER-MSG which one is riskiest?") {
				if err != nil {
					t.Fatalf("Run: %v", err)
				}
			}

			reqs := llm.requests()
			if len(reqs) < 2 {
				t.Fatalf("model got %d requests on the turn, want 2 (tool call, then answer)", len(reqs))
			}
			if len(reqs) != 2 {
				t.Errorf("model got %d requests on the turn, want 2: it kept calling the tool, so it never saw the response", len(reqs))
			}
			first := requestDump(reqs[0])
			if !strings.Contains(first, "[SUMMARY]") {
				t.Errorf("first request lost the summary:\n%s", first)
			}
			if !strings.Contains(first, "NEW-USER-MSG") {
				t.Errorf("first request after compaction does not contain the user's message:\n%s", first)
			}
			// The other half: what the summary replaced stays out.
			if strings.Contains(first, "what are the main subsystems?") {
				t.Errorf("first request still carries the pre-summary history:\n%s", first)
			}
			second := requestDump(reqs[1])
			for _, want := range []string{"NEW-USER-MSG", "call:" + noopToolName, "response:" + noopToolName} {
				if !strings.Contains(second, want) {
					t.Errorf("tool follow-up request lacks %q:\n%s", want, second)
				}
			}
		})
	}
}

// requestDump renders a request's contents one part per line.
func requestDump(req *adkmodel.LLMRequest) string {
	var b strings.Builder
	for _, c := range req.Contents {
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			switch {
			case p.FunctionCall != nil:
				b.WriteString("[" + c.Role + "] call:" + p.FunctionCall.Name + "\n")
			case p.FunctionResponse != nil:
				b.WriteString("[" + c.Role + "] response:" + p.FunctionResponse.Name + "\n")
			default:
				b.WriteString("[" + c.Role + "] " + p.Text + "\n")
			}
		}
	}
	return b.String()
}
