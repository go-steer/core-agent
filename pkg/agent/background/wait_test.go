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
	"strings"
	"sync/atomic"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/genai"
)

// gateLLM blocks inside GenerateContent until release is closed, so a test
// can hold a subagent's turn open and exercise the synchronous-wait timeout
// (#626/D5) deterministically.
type gateLLM struct {
	release chan struct{}
}

func (*gateLLM) Name() string { return "gate" }
func (l *gateLLM) GenerateContent(ctx context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		select {
		case <-l.release:
		case <-ctx.Done():
			return
		}
		content := &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "unblocked"}}}
		yield(&adkmodel.LLMResponse{
			Content:      content,
			FinishReason: genai.FinishReasonStop,
			TurnComplete: true,
		}, nil)
	}
}

// completingLLM signals the autonomous done tool (report_done) on its
// first generation so the run reaches StopReasonCompleted with a completion
// detail — the realistic shape a synchronous wait returns inline. After the
// tool call it returns plain text, so the ADK tool loop terminates rather
// than re-issuing the call.
type completingLLM struct {
	detail string
	called atomic.Bool
}

func (*completingLLM) Name() string { return "completing" }
func (l *completingLLM) GenerateContent(_ context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		if l.called.Swap(true) {
			content := &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "finished"}}}
			yield(&adkmodel.LLMResponse{Content: content, FinishReason: genai.FinishReasonStop, TurnComplete: true}, nil)
			return
		}
		fc := &genai.FunctionCall{Name: "report_done", Args: map[string]any{"state": "done", "detail": l.detail}}
		content := &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: fc}}}
		yield(&adkmodel.LLMResponse{Content: content, FinishReason: genai.FinishReasonStop, TurnComplete: true}, nil)
	}
}

// TestAwaitResult_ReturnsFinalOutput is the wait: true happy path — a
// synchronous spawn blocks until the subagent completes and returns its
// completion report inline as the tool result (#626/D5).
func TestAwaitResult_ReturnsFinalOutput(t *testing.T) {
	t.Parallel()
	prov := &recordingProvider{llm: &completingLLM{detail: "cluster is healthy"}}
	mgr := newTemplateManager(t, prov, []SubagentTemplate{{
		Name:         "cluster",
		Instruction:  "triage",
		ModelFactory: tmplFactory(prov, "cluster-model"),
		ModelID:      "cluster-model",
	}}, WithDefaultBudgets(Budgets{MaxTurns: 2}), WithSyncWaitTimeout(5*time.Second))
	attachEchoParent(t, mgr)
	defer mgr.Close()

	h, err := mgr.SpawnTemplate(context.Background(), "", "cluster", RefOverrides{Goal: "g"}, "")
	if err != nil {
		t.Fatalf("SpawnTemplate: %v", err)
	}
	res := mgr.awaitResult(context.Background(), h)
	if res.Status != "completed" {
		t.Errorf("status = %q, want completed", res.Status)
	}
	if res.Output != "cluster is healthy" {
		t.Errorf("output = %q, want %q", res.Output, "cluster is healthy")
	}
}

// TestAwaitResult_TimesOut proves the sync wait is capped: when the
// subagent outruns syncWaitTimeout, awaitResult returns a "running"
// status (the subagent keeps running; its result is pushed later) rather
// than hanging the parent turn.
func TestAwaitResult_TimesOut(t *testing.T) {
	t.Parallel()
	gate := &gateLLM{release: make(chan struct{})}
	prov := &recordingProvider{llm: gate}
	mgr := newTemplateManager(t, prov, []SubagentTemplate{{
		Name:         "cluster",
		Instruction:  "triage",
		ModelFactory: tmplFactory(prov, "cluster-model"),
		ModelID:      "cluster-model",
	}}, WithDefaultBudgets(Budgets{MaxTurns: 1}), WithSyncWaitTimeout(5*time.Millisecond))
	attachEchoParent(t, mgr)
	defer mgr.Close()

	h, err := mgr.SpawnTemplate(context.Background(), "", "cluster", RefOverrides{Goal: "g"}, "")
	if err != nil {
		t.Fatalf("SpawnTemplate: %v", err)
	}
	res := mgr.awaitResult(context.Background(), h)
	if !strings.HasPrefix(res.Status, "running") {
		t.Errorf("status = %q, want a running/timeout status", res.Status)
	}
	if res.Output != "" {
		t.Errorf("output = %q, want empty on timeout", res.Output)
	}
	// Let the blocked subagent finish cleanly so the goroutine doesn't leak.
	close(gate.release)
	waitDone(t, h)
}

// TestAwaitResult_ParentCancel proves a canceled parent context unblocks
// the wait without waiting out the (longer) sync-wait timeout; the subagent
// continues in the background.
func TestAwaitResult_ParentCancel(t *testing.T) {
	t.Parallel()
	gate := &gateLLM{release: make(chan struct{})}
	prov := &recordingProvider{llm: gate}
	mgr := newTemplateManager(t, prov, []SubagentTemplate{{
		Name:         "cluster",
		Instruction:  "triage",
		ModelFactory: tmplFactory(prov, "cluster-model"),
		ModelID:      "cluster-model",
	}}, WithDefaultBudgets(Budgets{MaxTurns: 1}), WithSyncWaitTimeout(30*time.Second))
	attachEchoParent(t, mgr)
	defer mgr.Close()

	h, err := mgr.SpawnTemplate(context.Background(), "", "cluster", RefOverrides{Goal: "g"}, "")
	if err != nil {
		t.Fatalf("SpawnTemplate: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res := mgr.awaitResult(ctx, h)
	if !strings.HasPrefix(res.Status, "running") {
		t.Errorf("status = %q, want a running/canceled status", res.Status)
	}
	close(gate.release)
	waitDone(t, h)
}
