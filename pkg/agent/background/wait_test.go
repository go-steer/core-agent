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
	"errors"
	"iter"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/pkg/agent/autonomous"
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
		// Standing: this scenario terminates through report_done,
		// which only a standing worker is offered since #730.
		Mode: ModeStanding,
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

// gatedCompletingLLM holds its first generation until release is closed,
// then reports done with detail and answers with finalText — the shape of
// a subagent that outruns the sync-wait cap and still has real findings to
// hand back.
type gatedCompletingLLM struct {
	release   chan struct{}
	detail    string
	finalText string
	called    atomic.Bool
}

func (*gatedCompletingLLM) Name() string { return "gated-completing" }
func (l *gatedCompletingLLM) GenerateContent(ctx context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		if l.called.Swap(true) {
			content := &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: l.finalText}}}
			yield(&adkmodel.LLMResponse{Content: content, FinishReason: genai.FinishReasonStop, TurnComplete: true}, nil)
			return
		}
		select {
		case <-l.release:
		case <-ctx.Done():
			return
		}
		fc := &genai.FunctionCall{Name: "report_done", Args: map[string]any{"state": "done", "detail": l.detail}}
		content := &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: fc}}}
		yield(&adkmodel.LLMResponse{Content: content, FinishReason: genai.FinishReasonStop, TurnComplete: true}, nil)
	}
}

// TestTerminalAlert_CarriesFinalTextAfterSyncWaitTimeout is the #691
// regression gate. #667 made a synchronous spawn return the subagent's
// actual output, but only on the path where the wait completes: once
// abandonWait releases the claim, delivery moves to the terminal alert,
// which rendered DoneDetail alone. So the fix vanished exactly when the
// subagent took long enough to be worth waiting for. In the field a
// 6m21s subagent's diagnosis reached the parent as a one-line status
// claiming a patch it never included, and the parent redid the whole
// investigation over 91 turns and $1.31.
func TestTerminalAlert_CarriesFinalTextAfterSyncWaitTimeout(t *testing.T) {
	t.Parallel()
	const (
		detail    = "diagnosed the FailedMount and provided the proposed patch"
		finalText = "Root cause: secretName typo smtp-credentials-typo; patch: set secretName to smtp-credentials"
	)
	gate := &gatedCompletingLLM{release: make(chan struct{}), detail: detail, finalText: finalText}
	prov := &recordingProvider{llm: gate}
	mgr := newTemplateManager(t, prov, []SubagentTemplate{{
		Name:         "cluster",
		Instruction:  "triage",
		ModelFactory: tmplFactory(prov, "cluster-model"),
		ModelID:      "cluster-model",
		// Standing: this LLM terminates by calling report_done, which
		// only a standing worker is offered since #730.
		Mode: ModeStanding,
	}}, WithDefaultBudgets(Budgets{MaxTurns: 2}), WithSyncWaitTimeout(5*time.Millisecond))
	attachEchoParent(t, mgr)
	defer mgr.Close()

	h, err := mgr.SpawnTemplate(context.Background(), "", "cluster", RefOverrides{Goal: "g"}, "")
	if err != nil {
		t.Fatalf("SpawnTemplate: %v", err)
	}
	if res := mgr.awaitResult(context.Background(), h); !strings.HasPrefix(res.Status, "running") {
		t.Fatalf("status = %q, want the wait to have timed out (the whole point of this test)", res.Status)
	}

	// The wait gave up; delivery is now the async terminal alert's job.
	close(gate.release)
	waitDone(t, h)

	alerts := drainAlerts(mgr, 2*time.Second)
	var completed *Alert
	for i := range alerts {
		if alerts[i].Kind == "completed" {
			completed = &alerts[i]
		}
	}
	if completed == nil {
		t.Fatalf("no completed alert delivered after an abandoned wait; got %+v", alerts)
	}
	if !strings.Contains(completed.Text, detail) {
		t.Errorf("alert text = %q, want it to contain the completion detail %q", completed.Text, detail)
	}
	if !strings.Contains(completed.Text, finalText) {
		t.Errorf("alert text = %q, want it to carry the subagent's final text %q — the deliverable is unreachable without it", completed.Text, finalText)
	}
}

// TestTerminalAlertText_Rendering pins the (kind, text) pairs directly,
// including the outcomes no live-run test reaches cheaply: a
// budget-capped subagent whose findings live only in its last assistant
// text, and a failure that still has something to report. Every
// A rendering ends with the machine-readable stop_reason line (#730)
// only when the stop was not a natural end: `kind` already separates
// completed from failed/deferred/stopped, so a bare completed alert
// means natural and the trailer would be noise on every bullet in the
// parent's prompt.
func TestTerminalAlertText_Rendering(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		status   Status
		result   autonomous.RunResult
		runErr   error
		wantKind string
		wantText string
	}{
		{
			name:     "completed with both",
			status:   StatusCompleted,
			result:   autonomous.RunResult{DoneDetail: "fixed it", FinalText: "here is how", Returned: true},
			wantKind: "completed",
			wantText: "fixed it\n\nfinal_text: here is how",
		},
		{
			name:     "completed with a redundant final text",
			status:   StatusCompleted,
			result:   autonomous.RunResult{DoneDetail: "fixed it", FinalText: "fixed it", Returned: true},
			wantKind: "completed",
			wantText: "fixed it",
		},
		{
			// Returned: the subagent called its return tool and the
			// payload happened to be empty, so the last text stands in.
			name:     "completed with no detail falls back to final text",
			status:   StatusCompleted,
			result:   autonomous.RunResult{FinalText: "here is how", Returned: true},
			wantKind: "completed",
			wantText: "here is how",
		},
		{
			name:     "completed with nothing at all",
			status:   StatusCompleted,
			result:   autonomous.RunResult{Returned: true},
			wantKind: "completed",
			wantText: "(no detail provided)",
		},
		{
			// A budget cap never calls report_done, so the last
			// assistant text is the entire record of the work.
			name:     "deferred keeps the reason and the findings",
			status:   StatusDeferred,
			result:   autonomous.RunResult{Reason: autonomous.StopReasonMaxTurns, FinalText: "got as far as the Secret"},
			wantKind: "deferred",
			wantText: "stopped: max_turns_exceeded\n\nfinal_text: got as far as the Secret\n\nstop_reason: max_steps",
		},
		{
			name:     "failed keeps the error and the findings",
			status:   StatusFailed,
			result:   autonomous.RunResult{FinalText: "got as far as the Secret"},
			runErr:   errors.New("provider exploded"),
			wantKind: "failed",
			wantText: "provider exploded\n\nfinal_text: got as far as the Secret\n\nstop_reason: error",
		},
		{
			// An explicit parent Stop: the parent asked for this, and
			// a cancelled mid-thought is not a finding.
			name:     "stopped stays terse",
			status:   StatusStopped,
			result:   autonomous.RunResult{FinalText: "half a sentence"},
			wantKind: "stopped",
			wantText: "stopped by parent",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			kind, text := terminalAlertText(tc.status, tc.result, tc.runErr)
			if kind != tc.wantKind {
				t.Errorf("kind = %q, want %q", kind, tc.wantKind)
			}
			if text != tc.wantText {
				t.Errorf("text = %q, want %q", text, tc.wantText)
			}
		})
	}
}
