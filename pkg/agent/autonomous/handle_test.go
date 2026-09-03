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

package autonomous

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
	_, err := Start(context.Background(), nil, "go")
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
	h, err := Start(context.Background(), buildAgent(llm, "h-complete"), "do the thing")
	if err != nil {
		t.Fatalf("Start: %v", err)
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

// gatedTextTurn returns a scenario that signals `started` when the
// LLM call begins and then BLOCKS until `release` is closed before
// yielding its response. This is the de-flake seam (#397): tests that
// need "a turn is provably in flight" or "pause landed before this
// turn completed" coordinate on channels instead of sleeping and
// hoping the scheduler cooperated.
// closedChan returns an already-closed channel — a gatedTextTurn
// release that never blocks, for turns that only need the started
// signal.
func closedChan() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

func gatedTextTurn(text string, started chan<- struct{}, release <-chan struct{}) scenarioFn {
	return func(ctx context.Context, _ *adkmodel.LLMRequest) []stubResp {
		select {
		case started <- struct{}{}:
		default:
		}
		select {
		case <-release:
		case <-ctx.Done():
		}
		content := &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: text}}}
		return []stubResp{{resp: &adkmodel.LLMResponse{
			Content:      content,
			FinishReason: genai.FinishReasonStop,
			TurnComplete: true,
		}}}
	}
}

func TestAutonomousHandle_StopCancelsRun(t *testing.T) {
	t.Parallel()
	// Turn 1 signals when it is in flight and blocks until released,
	// so Stop() is issued at a KNOWN point instead of after a sleep
	// and a prayer (#397 de-flake).
	turn1Started := make(chan struct{}, 1)
	turn1Release := make(chan struct{})
	llm := &stubLLM{scenarios: []scenarioFn{
		gatedTextTurn("stalling", turn1Started, turn1Release),
		slowTextTurn("stalling", 500*time.Millisecond),
		slowTextTurn("stalling", 500*time.Millisecond),
		slowTextTurn("stalling", 500*time.Millisecond),
	}}
	h, err := Start(context.Background(),
		buildAgent(llm, "h-stop"), "monitor",
		WithMaxTurns(0)) // no cap; we'll Stop manually
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Deterministic: wait for turn 1 to actually be in flight, stop,
	// then release the blocked LLM call so cancellation propagates.
	select {
	case <-turn1Started:
	case <-time.After(3 * time.Second):
		t.Fatal("turn 1 never started")
	}
	if err := h.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	close(turn1Release)
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
	// De-flaked (#397): turn 1 blocks until released and turn 2
	// signals if it ever starts, so the assertions coordinate on
	// KNOWN points instead of sleeps. The old shape sampled the call
	// counter after observing Status()==Paused — but Pause() flips
	// the status immediately from the caller while a turn can still
	// be in flight, so the counter could legally advance once more
	// ("before=2 after=3" flakes under CI load).
	turn1Started := make(chan struct{}, 1)
	turn1Release := make(chan struct{})
	turn2Started := make(chan struct{}, 1)
	llm := &stubLLM{scenarios: []scenarioFn{
		gatedTextTurn("turn 1", turn1Started, turn1Release),
		gatedTextTurn("turn 2", turn2Started, closedChan()),
		textTurn("turn 3", 1, 1),
		doneCallTurn("ok"),
		textTurn("done", 1, 1),
	}}
	h, err := Start(context.Background(),
		buildAgent(llm, "h-pause"), "monitor",
		WithMaxTurns(0))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer h.Stop()

	// Pause while turn 1 is PROVABLY in flight (its LLM call has
	// signaled and is blocked on our release). The pause takes
	// effect at the next beforeTurn check.
	select {
	case <-turn1Started:
	case <-time.After(3 * time.Second):
		t.Fatal("turn 1 never started")
	}
	if err := h.Pause(); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	close(turn1Release)

	// Turn 1 completes; the driver must then park at BeforeTurn.
	// Turn 2 starting would signal turn2Started — assert it doesn't.
	// The window is a negative-assertion bound, not a sync point:
	// the pause landed before turn 1 completed, so the driver is
	// GUARANTEED to see pauseCh at the boundary; if it doesn't, the
	// bug is real, not timing.
	select {
	case <-turn2Started:
		t.Fatal("turn 2 started while paused")
	case <-time.After(200 * time.Millisecond):
	}
	if got := h.Status(); got != AutonomousPaused {
		t.Fatalf("Status = %v, want AutonomousPaused", got)
	}
	if calls := atomic.LoadInt32(&llm.calls); calls != 1 {
		t.Errorf("calls = %d while paused, want exactly 1 (turn 1 only)", calls)
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
	// Same shape as StopUnblocksPause: the four control calls below are
	// all errors on a terminated run, so the run must be unable to
	// terminate while they happen. Turn 1 holds long enough for the
	// first Pause to land mid-turn; turn 2 blocks until Stop cancels
	// the context, which also covers the second Resume racing a run
	// that would otherwise have completed.
	llm := &stubLLM{scenarios: []scenarioFn{
		delayedTextTurn("t", 100*time.Millisecond),
		blockUntilCanceled(),
	}}
	h, err := Start(context.Background(), buildAgent(llm, "h-pause-idem"), "g", WithMaxTurns(0))
	if err != nil {
		t.Fatalf("Start: %v", err)
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

// delayedTextTurn is textTurn with the terminal response held back, so
// a test can be sure a control call (Pause, Inject, ...) lands while
// the turn is still running rather than racing its completion.
func delayedTextTurn(text string, delay time.Duration) scenarioFn {
	inner := textTurn(text, 1, 1)
	return func(ctx context.Context, req *adkmodel.LLMRequest) []stubResp {
		out := inner(ctx, req)
		if len(out) > 0 {
			out[len(out)-1].delay = delay
		}
		return out
	}
}

// blockUntilCanceled never completes on its own: the run can only leave
// this turn when its context is cancelled. Gives a test a run that is
// guaranteed still live, without depending on how fast the loop is.
func blockUntilCanceled() scenarioFn {
	return func(ctx context.Context, _ *adkmodel.LLMRequest) []stubResp {
		<-ctx.Done()
		return []stubResp{{err: ctx.Err()}}
	}
}

func TestAutonomousHandle_StopUnblocksPause(t *testing.T) {
	t.Parallel()
	// Turn 1 is slow enough that Pause lands while it is still in
	// flight, and turn 2 blocks until the run's context is cancelled.
	// Both are load-bearing: with two instant turns the loop could burn
	// through its scenarios and terminate on "out of scenarios" before
	// the main goroutine reached Pause, which then failed the test with
	// "run already terminated" on a busy CI runner.
	llm := &stubLLM{scenarios: []scenarioFn{
		delayedTextTurn("t1", 100*time.Millisecond),
		blockUntilCanceled(),
	}}
	h, err := Start(context.Background(), buildAgent(llm, "h-stop-paused"), "g", WithMaxTurns(0))
	if err != nil {
		t.Fatalf("Start: %v", err)
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
	// inbox block lands on the post-inject turn.
	//
	// The ordering this test asserts — inject lands BEFORE turn 2's
	// pre-turn drain — is made a fact by handshake, not raced for
	// (#916). Turn 1 announces it has started and then blocks until the
	// test has injected, so turn 2 cannot begin until the message is
	// queued. The previous shape slept 50ms into a 200ms stub delay and
	// retried Inject for 2s, which guaranteed the inject SUCCEEDED but
	// not that it beat the drain: under whole-suite load turn 1 could
	// finish first, and an inject arriving mid-turn-2 is correctly
	// deferred to turn 3 (#878/#879). The test then failed on correct
	// behaviour.
	var prompts []string
	var promptsMu sync.Mutex
	recordPrompt := func(req *adkmodel.LLMRequest) {
		promptsMu.Lock()
		prompts = append(prompts, lastUserPrompt(req))
		promptsMu.Unlock()
	}
	turn1Started := make(chan struct{}, 1)
	turn1Release := make(chan struct{})
	llm := &stubLLM{scenarios: []scenarioFn{
		// Turn 1: record, hand the test its window, and hold the turn
		// open until the inject is queued. Same started/release seam as
		// gatedTextTurn above, spelled out inline because this one also
		// has to see the request to record its prompt.
		func(ctx context.Context, req *adkmodel.LLMRequest) []stubResp {
			recordPrompt(req)
			select {
			case turn1Started <- struct{}{}:
			default:
			}
			select {
			case <-turn1Release:
			case <-ctx.Done():
			}
			content := &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "t1"}}}
			return []stubResp{
				{resp: &adkmodel.LLMResponse{Content: content, FinishReason: genai.FinishReasonStop, TurnComplete: true}},
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
	h, err := Start(context.Background(), buildAgent(llm, "h-inject"), "first goal")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Turn 1 is provably in flight and cannot return until we release
	// it, so the agent is constructed (Inject's only failure mode is a
	// nil agent, handle.go:319) and turn 2's pre-turn drain has not run
	// yet. No retry loop and no sleep: both were standing in for this
	// handshake. Release before asserting, so a failed Inject unblocks
	// the run rather than parking it until the stub's context dies.
	select {
	case <-turn1Started:
	case <-time.After(3 * time.Second):
		t.Fatal("turn 1 never started")
	}
	injectErr := h.Inject("priority changed!")
	close(turn1Release)
	if injectErr != nil {
		t.Fatalf("Inject: %v", injectErr)
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
	h, err := Start(context.Background(), buildAgent(llm, "h-pause-term"), "g")
	if err != nil {
		t.Fatalf("Start: %v", err)
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
	h, err := Start(context.Background(), buildAgent(llm, "h-done"), "g")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	select {
	case <-h.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("Done channel didn't close")
	}
}
