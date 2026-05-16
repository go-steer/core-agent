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
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/genai"
)

// lastUserPrompt returns the last user-role text part from req, or
// the empty string when there isn't one. Used to assert what prompt
// the loop handed the LLM for a given turn.
func lastUserPrompt(req *adkmodel.LLMRequest) string {
	if req == nil {
		return ""
	}
	for i := len(req.Contents) - 1; i >= 0; i-- {
		c := req.Contents[i]
		if c == nil || c.Role != genai.RoleUser {
			continue
		}
		for _, p := range c.Parts {
			if p != nil && p.Text != "" {
				return p.Text
			}
		}
	}
	return ""
}

func TestStartAutonomous_RequiresBuild(t *testing.T) {
	t.Parallel()
	_, err := StartAutonomous(context.Background(), nil, "go")
	if err == nil {
		t.Errorf("expected error for nil build")
	}
}

func TestAutonomousHandle_RunsToCompletion(t *testing.T) {
	t.Parallel()
	llm := &stubLLM{scenarios: []scenarioFn{
		doneCallTurn("did the thing"),
		textTurn("all done", 5, 3),
	}}
	h, err := StartAutonomous(context.Background(), buildAgent(llm, "h-complete"), "do the thing")
	if err != nil {
		t.Fatalf("StartAutonomous: %v", err)
	}
	res, err := h.Wait()
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if res.Reason != StopReasonCompleted {
		t.Errorf("Reason = %v, want Completed", res.Reason)
	}
	if h.Status() != AutonomousCompleted {
		t.Errorf("Status = %v, want AutonomousCompleted", h.Status())
	}
}

func TestAutonomousHandle_StopCancelsRun(t *testing.T) {
	t.Parallel()
	// Each turn yields a slow text response so we have time to
	// observe the stop before the loop voluntarily exits.
	llm := &stubLLM{scenarios: []scenarioFn{
		slowTextTurn("stalling", 500*time.Millisecond),
		slowTextTurn("stalling", 500*time.Millisecond),
		slowTextTurn("stalling", 500*time.Millisecond),
		slowTextTurn("stalling", 500*time.Millisecond),
	}}
	h, err := StartAutonomous(context.Background(),
		buildAgent(llm, "h-stop"), "monitor",
		WithMaxTurns(0)) // no cap; we'll Stop manually
	if err != nil {
		t.Fatalf("StartAutonomous: %v", err)
	}

	// Give the first turn a moment to be in flight.
	time.Sleep(100 * time.Millisecond)
	if err := h.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	// Wait should return promptly with a cancelled run.
	doneCh := make(chan struct{})
	go func() {
		_, _ = h.Wait()
		close(doneCh)
	}()
	select {
	case <-doneCh:
	case <-time.After(3 * time.Second):
		t.Fatal("Wait didn't return within 3s of Stop")
	}
	if h.Status() != AutonomousStopped {
		t.Errorf("Status after Stop = %v, want AutonomousStopped", h.Status())
	}
	// Idempotent.
	if err := h.Stop(); err != nil {
		t.Errorf("second Stop should be no-op; got %v", err)
	}
}

func TestAutonomousHandle_PauseHaltsBeforeNextTurn(t *testing.T) {
	t.Parallel()
	llm := &stubLLM{scenarios: []scenarioFn{
		textTurn("turn 1", 1, 1),
		textTurn("turn 2", 1, 1),
		textTurn("turn 3", 1, 1),
		doneCallTurn("ok"),
		textTurn("done", 1, 1),
	}}
	h, err := StartAutonomous(context.Background(),
		buildAgent(llm, "h-pause"), "monitor",
		WithMaxTurns(0))
	if err != nil {
		t.Fatalf("StartAutonomous: %v", err)
	}
	defer h.Stop()

	// Pause asap; the first turn may already be in flight, but the
	// pause takes effect at the next beforeTurn check.
	if err := h.Pause(); err != nil {
		t.Fatalf("Pause: %v", err)
	}

	// Wait until status reflects paused. The pause shows up as soon
	// as the BeforeTurn hook fires on the loop side — i.e. between
	// turns. Give the loop up to 2s.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if h.Status() == AutonomousPaused {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if h.Status() != AutonomousPaused {
		t.Fatalf("Status = %v, want AutonomousPaused", h.Status())
	}

	// Sample the call count, wait, sample again — no new turns
	// should land while paused.
	before := atomic.LoadInt32(&llm.calls)
	time.Sleep(300 * time.Millisecond)
	after := atomic.LoadInt32(&llm.calls)
	if after != before {
		t.Errorf("calls advanced during pause: before=%d after=%d", before, after)
	}

	// Resume; the loop continues until done.
	if err := h.Resume(); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	res, err := h.Wait()
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if res.Reason != StopReasonCompleted {
		t.Errorf("post-resume final reason = %v, want Completed", res.Reason)
	}
}

func TestAutonomousHandle_PauseIdempotent(t *testing.T) {
	t.Parallel()
	llm := &stubLLM{scenarios: []scenarioFn{
		textTurn("t", 1, 1),
		doneCallTurn("ok"),
		textTurn("done", 1, 1),
	}}
	h, err := StartAutonomous(context.Background(), buildAgent(llm, "h-pause-idem"), "g", WithMaxTurns(0))
	if err != nil {
		t.Fatalf("StartAutonomous: %v", err)
	}
	defer h.Stop()
	if err := h.Pause(); err != nil {
		t.Fatalf("first Pause: %v", err)
	}
	if err := h.Pause(); err != nil {
		t.Errorf("second Pause should be no-op; got %v", err)
	}
	if err := h.Resume(); err != nil {
		t.Errorf("Resume: %v", err)
	}
	if err := h.Resume(); err != nil {
		t.Errorf("second Resume should be no-op; got %v", err)
	}
}

func TestAutonomousHandle_StopUnblocksPause(t *testing.T) {
	t.Parallel()
	llm := &stubLLM{scenarios: []scenarioFn{
		textTurn("t1", 1, 1),
		textTurn("t2", 1, 1),
	}}
	h, err := StartAutonomous(context.Background(), buildAgent(llm, "h-stop-paused"), "g", WithMaxTurns(0))
	if err != nil {
		t.Fatalf("StartAutonomous: %v", err)
	}
	if err := h.Pause(); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	// Wait briefly for pause to take effect.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if h.Status() == AutonomousPaused {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Stop should tear down even while paused.
	if err := h.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	doneCh := make(chan struct{})
	go func() {
		_, _ = h.Wait()
		close(doneCh)
	}()
	select {
	case <-doneCh:
	case <-time.After(3 * time.Second):
		t.Fatal("Wait didn't return after Stop while paused")
	}
}

func TestAutonomousHandle_InjectReachesNextTurn(t *testing.T) {
	t.Parallel()
	// Record the prompt of every LLM call so we can assert the
	// inbox block lands on the post-inject turn. Turn 1 sleeps
	// briefly so the test goroutine has a clean window to call
	// Inject after the agent is constructed but before turn 2's
	// pre-turn drain runs.
	var prompts []string
	var promptsMu sync.Mutex
	recordPrompt := func(req *adkmodel.LLMRequest) {
		promptsMu.Lock()
		prompts = append(prompts, lastUserPrompt(req))
		promptsMu.Unlock()
	}
	llm := &stubLLM{scenarios: []scenarioFn{
		// Turn 1: record + small delay so the test can Inject
		// between turns 1 and 2.
		func(_ context.Context, req *adkmodel.LLMRequest) []stubResp {
			recordPrompt(req)
			content := &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "t1"}}}
			return []stubResp{
				{delay: 200 * time.Millisecond,
					resp: &adkmodel.LLMResponse{Content: content, FinishReason: genai.FinishReasonStop, TurnComplete: true}},
			}
		},
		// Turn 2: record + signal done.
		func(_ context.Context, req *adkmodel.LLMRequest) []stubResp {
			recordPrompt(req)
			fc := &genai.FunctionCall{Name: "report_done", Args: map[string]any{"state": "done", "detail": "ok"}}
			content := &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: fc}}}
			return []stubResp{
				{resp: &adkmodel.LLMResponse{Content: content, TurnComplete: true, FinishReason: genai.FinishReasonStop}},
			}
		},
		// Follow-up after the tool call.
		textTurn("done", 1, 1),
	}}
	h, err := StartAutonomous(context.Background(), buildAgent(llm, "h-inject"), "first goal")
	if err != nil {
		t.Fatalf("StartAutonomous: %v", err)
	}
	// Inject during turn 1's delay so the message is queued in
	// time for turn 2's pre-turn drain.
	time.Sleep(50 * time.Millisecond) // let turn 1 start
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := h.Inject("priority changed!"); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	res, err := h.Wait()
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if res.Reason != StopReasonCompleted {
		t.Errorf("Reason = %v, want Completed", res.Reason)
	}

	// Turn 2's prompt must contain the injected message.
	promptsMu.Lock()
	defer promptsMu.Unlock()
	if len(prompts) < 2 {
		t.Fatalf("expected at least 2 recorded prompts (turn 1 + turn 2); got %d: %v", len(prompts), prompts)
	}
	if !strings.Contains(prompts[1], "priority changed!") {
		t.Errorf("turn 2 prompt should contain the injected message; got %q", prompts[1])
	}
	if !strings.Contains(prompts[1], "[Inbox]") {
		t.Errorf("turn 2 prompt should have the [Inbox] header; got %q", prompts[1])
	}
}

func TestAutonomousHandle_PauseAfterTerminalErrors(t *testing.T) {
	t.Parallel()
	llm := &stubLLM{scenarios: []scenarioFn{
		doneCallTurn("done"),
		textTurn("done", 1, 1),
	}}
	h, err := StartAutonomous(context.Background(), buildAgent(llm, "h-pause-term"), "g")
	if err != nil {
		t.Fatalf("StartAutonomous: %v", err)
	}
	_, _ = h.Wait()
	if err := h.Pause(); err == nil {
		t.Errorf("Pause after terminal should error")
	}
	if err := h.Resume(); err == nil {
		t.Errorf("Resume after terminal should error")
	}
}

func TestAutonomousHandle_DoneChannelCloses(t *testing.T) {
	t.Parallel()
	llm := &stubLLM{scenarios: []scenarioFn{
		doneCallTurn("d"),
		textTurn("done", 1, 1),
	}}
	h, err := StartAutonomous(context.Background(), buildAgent(llm, "h-done"), "g")
	if err != nil {
		t.Fatalf("StartAutonomous: %v", err)
	}
	select {
	case <-h.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("Done channel didn't close")
	}
}
