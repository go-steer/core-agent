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
	"errors"
	"fmt"
	"iter"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/adk/session"
	"google.golang.org/adk/tool"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/permissions"
	"github.com/go-steer/core-agent/usage"
)

// stubLLM is a tiny scriptable LLM used by these tests to drive the
// autonomous loop deterministically. Each entry in scenarios maps to
// one GenerateContent call (one underlying LLM round-trip; an
// agent.Run turn can consume several when tools are involved).
type stubLLM struct {
	mu        sync.Mutex
	cursor    int
	scenarios []scenarioFn
	calls     int32
}

type scenarioFn func(ctx context.Context, req *adkmodel.LLMRequest) []stubResp

type stubResp struct {
	resp  *adkmodel.LLMResponse
	err   error
	delay time.Duration
}

func (l *stubLLM) Name() string { return "stub" }

func (l *stubLLM) GenerateContent(ctx context.Context, req *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	atomic.AddInt32(&l.calls, 1)
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		l.mu.Lock()
		if l.cursor >= len(l.scenarios) {
			n := l.cursor
			l.mu.Unlock()
			yield(nil, fmt.Errorf("stubLLM: out of scenarios at call %d", n))
			return
		}
		fn := l.scenarios[l.cursor]
		l.cursor++
		l.mu.Unlock()
		for _, sr := range fn(ctx, req) {
			if sr.delay > 0 {
				select {
				case <-time.After(sr.delay):
				case <-ctx.Done():
					yield(nil, ctx.Err())
					return
				}
			}
			if !yield(sr.resp, sr.err) {
				return
			}
		}
	}
}

// textTurn yields one final text response. Suitable for "no tool
// call this turn" scenarios.
func textTurn(text string, in, out int32) scenarioFn {
	return func(_ context.Context, _ *adkmodel.LLMRequest) []stubResp {
		content := &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: text}}}
		return []stubResp{
			{resp: &adkmodel.LLMResponse{Content: content, Partial: true}},
			{resp: &adkmodel.LLMResponse{
				Content:      content,
				FinishReason: genai.FinishReasonStop,
				TurnComplete: true,
				UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
					PromptTokenCount:     in,
					CandidatesTokenCount: out,
					TotalTokenCount:      in + out,
				},
			}},
		}
	}
}

// doneCallTurn yields a single response calling the done tool. The
// runner will execute the tool, then dispatch another LLM call; the
// caller must script that follow-up too (typically with textTurn).
func doneCallTurn(detail string) scenarioFn {
	return func(_ context.Context, _ *adkmodel.LLMRequest) []stubResp {
		fc := &genai.FunctionCall{
			Name: "report_done",
			Args: map[string]any{"state": "done", "detail": detail},
		}
		content := &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: fc}}}
		return []stubResp{
			{resp: &adkmodel.LLMResponse{
				Content:      content,
				TurnComplete: true,
				FinishReason: genai.FinishReasonStop,
			}},
		}
	}
}

// errTurn yields a transport-level error.
func errTurn(err error) scenarioFn {
	return func(_ context.Context, _ *adkmodel.LLMRequest) []stubResp {
		return []stubResp{{err: err}}
	}
}

// slowTextTurn delays before yielding; used for wallclock tests.
func slowTextTurn(text string, d time.Duration) scenarioFn {
	return func(_ context.Context, _ *adkmodel.LLMRequest) []stubResp {
		content := &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: text}}}
		return []stubResp{
			{delay: d, resp: &adkmodel.LLMResponse{Content: content, Partial: true}},
			{resp: &adkmodel.LLMResponse{
				Content:      content,
				FinishReason: genai.FinishReasonStop,
				TurnComplete: true,
			}},
		}
	}
}

// buildAgent returns a build function that wires the stub LLM with
// the supplied done tool. Used by every test in this file.
func buildAgent(llm *stubLLM, name string) func([]tool.Tool) (*Agent, error) {
	return func(extras []tool.Tool) (*Agent, error) {
		return New(llm,
			WithName(name),
			WithSession("u-test", "s-test-"+name),
			WithTools(extras),
			WithInstruction("test agent; call report_done when finished."),
		)
	}
}

func TestRunAutonomous_RequiresBuild(t *testing.T) {
	t.Parallel()
	_, err := RunAutonomous(context.Background(), nil, "go")
	if err == nil || !strings.Contains(err.Error(), "build is required") {
		t.Fatalf("expected build-required error, got %v", err)
	}
}

func TestRunAutonomous_RequiresGoal(t *testing.T) {
	t.Parallel()
	_, err := RunAutonomous(context.Background(),
		func([]tool.Tool) (*Agent, error) { return nil, nil },
		"   ")
	if err == nil || !strings.Contains(err.Error(), "goal is required") {
		t.Fatalf("expected goal-required error, got %v", err)
	}
}

func TestRunAutonomous_StopsOnDoneTool(t *testing.T) {
	t.Parallel()
	llm := &stubLLM{scenarios: []scenarioFn{
		// Turn 1: model decides to finish immediately.
		doneCallTurn("summarized example.txt"),
		// Runner dispatches a follow-up LLM call after the tool
		// runs; that call must have something to return.
		textTurn("all done!", 5, 3),
	}}
	res, err := RunAutonomous(context.Background(), buildAgent(llm, "done"), "do the thing")
	if err != nil {
		t.Fatalf("RunAutonomous: %v", err)
	}
	if res.Reason != StopReasonCompleted {
		t.Errorf("Reason = %q, want %q", res.Reason, StopReasonCompleted)
	}
	if res.DoneDetail != "summarized example.txt" {
		t.Errorf("DoneDetail = %q, want %q", res.DoneDetail, "summarized example.txt")
	}
	if res.Turns != 1 {
		t.Errorf("Turns = %d, want 1", res.Turns)
	}
	if res.FinalText != "all done!" {
		t.Errorf("FinalText = %q, want %q", res.FinalText, "all done!")
	}
}

func TestRunAutonomous_StopsOnMaxTurns(t *testing.T) {
	t.Parallel()
	llm := &stubLLM{scenarios: []scenarioFn{
		textTurn("one", 1, 1),
		textTurn("two", 1, 1),
		textTurn("three", 1, 1),
		textTurn("four (should never run)", 1, 1),
	}}
	res, err := RunAutonomous(context.Background(),
		buildAgent(llm, "max-turns"),
		"keep going",
		WithMaxTurns(3))
	if err != nil {
		t.Fatalf("RunAutonomous: %v", err)
	}
	if res.Reason != StopReasonMaxTurns {
		t.Errorf("Reason = %q, want %q", res.Reason, StopReasonMaxTurns)
	}
	if res.Turns != 3 {
		t.Errorf("Turns = %d, want 3", res.Turns)
	}
	if res.FinalText != "three" {
		t.Errorf("FinalText = %q, want %q", res.FinalText, "three")
	}
}

func TestRunAutonomous_StopsOnMaxTokens(t *testing.T) {
	t.Parallel()
	llm := &stubLLM{scenarios: []scenarioFn{
		textTurn("turn1", 100, 50),
		textTurn("turn2", 100, 50),
		textTurn("turn3 (should not run)", 100, 50),
	}}
	res, err := RunAutonomous(context.Background(),
		buildAgent(llm, "max-tokens"),
		"go",
		WithMaxTokens(150, 0)) // input cap
	if err != nil {
		t.Fatalf("RunAutonomous: %v", err)
	}
	if res.Reason != StopReasonMaxTokens {
		t.Errorf("Reason = %q, want %q", res.Reason, StopReasonMaxTokens)
	}
	if res.Turns != 2 {
		t.Errorf("Turns = %d, want 2 (third turn should be blocked)", res.Turns)
	}
	if res.InputTokens < 150 {
		t.Errorf("InputTokens = %d, want >= 150", res.InputTokens)
	}
}

func TestRunAutonomous_StopsOnMaxCost(t *testing.T) {
	t.Parallel()
	llm := &stubLLM{scenarios: []scenarioFn{
		textTurn("t1", 1_000_000, 0), // $1 input
		textTurn("t2", 1_000_000, 0), // $1 input -> total $2
		textTurn("t3 (should not run)", 1_000_000, 0),
	}}
	pricing := usage.Pricing{InputPerMTok: 1.0, OutputPerMTok: 0}
	res, err := RunAutonomous(context.Background(),
		buildAgent(llm, "max-cost"),
		"spend",
		WithPricing(pricing),
		WithMaxCost(1.5))
	if err != nil {
		t.Fatalf("RunAutonomous: %v", err)
	}
	if res.Reason != StopReasonMaxCost {
		t.Errorf("Reason = %q, want %q", res.Reason, StopReasonMaxCost)
	}
	if res.Turns != 2 {
		t.Errorf("Turns = %d, want 2", res.Turns)
	}
	if res.CostUSD < 1.5 {
		t.Errorf("CostUSD = %v, want >= 1.5", res.CostUSD)
	}
}

func TestRunAutonomous_StopsOnWallclock(t *testing.T) {
	t.Parallel()
	llm := &stubLLM{scenarios: []scenarioFn{
		slowTextTurn("slow", 80*time.Millisecond),
		slowTextTurn("slow", 80*time.Millisecond),
		slowTextTurn("slow", 80*time.Millisecond),
		slowTextTurn("slow (never)", 80*time.Millisecond),
	}}
	res, err := RunAutonomous(context.Background(),
		buildAgent(llm, "wallclock"),
		"go",
		WithMaxWallclock(150*time.Millisecond))
	if err != nil {
		t.Fatalf("RunAutonomous: %v", err)
	}
	if res.Reason != StopReasonWallclockExceeded {
		t.Errorf("Reason = %q, want %q", res.Reason, StopReasonWallclockExceeded)
	}
	if res.Turns < 1 {
		t.Errorf("Turns = %d, want >= 1", res.Turns)
	}
}

func TestRunAutonomous_StopsOnContextCancel(t *testing.T) {
	t.Parallel()
	llm := &stubLLM{scenarios: []scenarioFn{
		slowTextTurn("slow", 200*time.Millisecond),
		slowTextTurn("never", 200*time.Millisecond),
	}}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	res, err := RunAutonomous(ctx, buildAgent(llm, "ctx-cancel"), "go")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if res.Reason != StopReasonContextCancelled {
		t.Errorf("Reason = %q, want %q", res.Reason, StopReasonContextCancelled)
	}
}

func TestRunAutonomous_RetryPolicy_RetriesThenSucceeds(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("transient 500")
	llm := &stubLLM{scenarios: []scenarioFn{
		errTurn(wantErr),     // turn 1 fails
		doneCallTurn("done"), // turn 2 succeeds
		textTurn("ok", 0, 0), // follow-up after the tool
	}}
	var attempts []int
	policy := func(err error, attempt int) RetryDecision {
		attempts = append(attempts, attempt)
		if !errors.Is(err, wantErr) {
			t.Errorf("policy got unexpected err: %v", err)
		}
		return RetryTurn
	}
	res, err := RunAutonomous(context.Background(),
		buildAgent(llm, "retry"),
		"go",
		WithRetryPolicy(policy))
	if err != nil {
		t.Fatalf("RunAutonomous: %v", err)
	}
	if res.Reason != StopReasonCompleted {
		t.Errorf("Reason = %q, want completed", res.Reason)
	}
	if len(attempts) != 1 || attempts[0] != 1 {
		t.Errorf("attempts = %v, want [1]", attempts)
	}
}

func TestRunAutonomous_RetryPolicy_AbortsOnDecision(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("permanent")
	llm := &stubLLM{scenarios: []scenarioFn{errTurn(wantErr)}}
	policy := func(_ error, _ int) RetryDecision { return AbortRun }
	res, err := RunAutonomous(context.Background(),
		buildAgent(llm, "retry-abort"),
		"go",
		WithRetryPolicy(policy))
	if !errors.Is(err, wantErr) {
		t.Errorf("err = %v, want %v", err, wantErr)
	}
	if res.Reason != StopReasonRetryAborted {
		t.Errorf("Reason = %q, want %q", res.Reason, StopReasonRetryAborted)
	}
}

func TestRunAutonomous_RetryPolicy_SkipMovesToContinuation(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("flaky")
	llm := &stubLLM{scenarios: []scenarioFn{
		errTurn(wantErr),
		doneCallTurn("done after skip"),
		textTurn("ok", 0, 0),
	}}
	policy := func(_ error, _ int) RetryDecision { return SkipTurn }
	res, err := RunAutonomous(context.Background(),
		buildAgent(llm, "retry-skip"),
		"original goal",
		WithRetryPolicy(policy))
	if err != nil {
		t.Fatalf("RunAutonomous: %v", err)
	}
	if res.Reason != StopReasonCompleted {
		t.Errorf("Reason = %q, want completed", res.Reason)
	}
	if res.DoneDetail != "done after skip" {
		t.Errorf("DoneDetail = %q", res.DoneDetail)
	}
}

func TestRunAutonomous_DefaultAbortsOnError(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("nope")
	llm := &stubLLM{scenarios: []scenarioFn{errTurn(wantErr)}}
	res, err := RunAutonomous(context.Background(), buildAgent(llm, "abort"), "go")
	if !errors.Is(err, wantErr) {
		t.Errorf("err = %v, want %v", err, wantErr)
	}
	if res.Reason != StopReasonRetryAborted {
		t.Errorf("Reason = %q, want %q", res.Reason, StopReasonRetryAborted)
	}
}

func TestRunAutonomous_TracksTokensAndCost(t *testing.T) {
	t.Parallel()
	llm := &stubLLM{scenarios: []scenarioFn{
		textTurn("a", 200, 50),
		textTurn("b", 300, 100),
		doneCallTurn("done"),
		textTurn("ok", 10, 5),
	}}
	tracker := usage.NewTracker()
	pricing := usage.Pricing{InputPerMTok: 2.0, OutputPerMTok: 8.0}
	res, err := RunAutonomous(context.Background(),
		buildAgent(llm, "tracker"),
		"go",
		WithTracker(tracker, pricing))
	if err != nil {
		t.Fatalf("RunAutonomous: %v", err)
	}
	if res.Reason != StopReasonCompleted {
		t.Errorf("Reason = %q, want completed", res.Reason)
	}
	wantIn := 200 + 300 + 10
	wantOut := 50 + 100 + 5
	if res.InputTokens != wantIn {
		t.Errorf("InputTokens = %d, want %d", res.InputTokens, wantIn)
	}
	if res.OutputTokens != wantOut {
		t.Errorf("OutputTokens = %d, want %d", res.OutputTokens, wantOut)
	}
	wantCost := pricing.CostUSD(wantIn, wantOut)
	if diff := res.CostUSD - wantCost; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("CostUSD = %v, want %v", res.CostUSD, wantCost)
	}
	totals := tracker.Totals()
	if totals.InputTokens != wantIn || totals.OutputTokens != wantOut {
		t.Errorf("tracker totals = %+v, want input=%d output=%d", totals, wantIn, wantOut)
	}
}

func TestRunAutonomous_ProgressCallbackFires(t *testing.T) {
	t.Parallel()
	llm := &stubLLM{scenarios: []scenarioFn{
		textTurn("one", 0, 0),
		doneCallTurn("done"),
		textTurn("ok", 0, 0),
	}}
	var (
		mu   sync.Mutex
		seen []int
	)
	cb := func(turn int, _ *session.Event) {
		mu.Lock()
		defer mu.Unlock()
		if len(seen) == 0 || seen[len(seen)-1] != turn {
			seen = append(seen, turn)
		}
	}
	if _, err := RunAutonomous(context.Background(),
		buildAgent(llm, "progress"),
		"go",
		WithProgress(cb)); err != nil {
		t.Fatalf("RunAutonomous: %v", err)
	}
	if len(seen) != 2 || seen[0] != 1 || seen[1] != 2 {
		t.Errorf("turn indices = %v, want [1 2]", seen)
	}
}

func TestRunAutonomous_ContinuationPromptOverridesDefault(t *testing.T) {
	t.Parallel()
	prompts := capturePrompts()
	llm := &stubLLM{scenarios: []scenarioFn{
		prompts.wrap(textTurn("one", 0, 0)),
		prompts.wrap(doneCallTurn("done")),
		prompts.wrap(textTurn("ok", 0, 0)),
	}}
	if _, err := RunAutonomous(context.Background(),
		buildAgent(llm, "cont"),
		"original goal",
		WithContinuationPrompt("what next?")); err != nil {
		t.Fatalf("RunAutonomous: %v", err)
	}
	captured := prompts.snapshot()
	if len(captured) < 2 {
		t.Fatalf("expected at least 2 captured prompts, got %v", captured)
	}
	if captured[0] != "original goal" {
		t.Errorf("first prompt = %q, want %q", captured[0], "original goal")
	}
	if captured[1] != "what next?" {
		t.Errorf("second prompt = %q, want %q", captured[1], "what next?")
	}
}

func TestRunAutonomous_RejectsAskModeWithoutPrompter(t *testing.T) {
	t.Parallel()
	// Ask-mode gate without a prompter would deadlock on the first
	// tool call; the driver must refuse before invoking build or
	// burning an LLM round-trip.
	gate := permissions.New(permissions.Options{Mode: permissions.ModeAsk})
	llm := &stubLLM{scenarios: nil} // never called
	called := false
	build := func([]tool.Tool) (*Agent, error) {
		called = true
		return New(llm, WithSession("u", "s-ask-guard"))
	}
	res, err := RunAutonomous(context.Background(), build, "go",
		WithPermissionsGate(gate))
	if err == nil {
		t.Fatalf("expected guard error, got nil")
	}
	if !strings.Contains(err.Error(), "ask-mode") || !strings.Contains(err.Error(), "Prompter") {
		t.Errorf("err = %v, want mention of ask-mode and Prompter", err)
	}
	if called {
		t.Errorf("build should not be invoked when the guard fires")
	}
	if res.Reason != "" {
		t.Errorf("Reason = %q, want zero value (no run started)", res.Reason)
	}
	if atomic.LoadInt32(&llm.calls) != 0 {
		t.Errorf("LLM was called %d times; guard should have prevented any calls", llm.calls)
	}
}

func TestRunAutonomous_AskModeWithPrompterIsAllowed(t *testing.T) {
	t.Parallel()
	// Same gate but with a prompter wired — guard should not fire.
	gate := permissions.New(permissions.Options{
		Mode:     permissions.ModeAsk,
		Prompter: stubPrompter{},
	})
	llm := &stubLLM{scenarios: []scenarioFn{
		doneCallTurn("ok"),
		textTurn("done", 0, 0),
	}}
	res, err := RunAutonomous(context.Background(),
		buildAgent(llm, "ask-with-prompter"),
		"go",
		WithPermissionsGate(gate))
	if err != nil {
		t.Fatalf("RunAutonomous: %v", err)
	}
	if res.Reason != StopReasonCompleted {
		t.Errorf("Reason = %q, want completed", res.Reason)
	}
}

// stubPrompter satisfies permissions.Prompter for guard-allowed tests
// without ever being asked anything (the run uses no gated tools).
type stubPrompter struct{}

func (stubPrompter) AskApproval(_ context.Context, _ permissions.PromptRequest) (permissions.Decision, error) {
	return permissions.DecisionDeny, nil
}

func TestRunAutonomous_StopsOnPerTurnTimeout(t *testing.T) {
	t.Parallel()
	// One slow turn paired with a short per-turn timeout. The slow
	// turn's iterator should be cancelled by the per-turn context;
	// the default retry policy then aborts the run, returning the
	// underlying ctx error wrapped as RetryAborted.
	llm := &stubLLM{scenarios: []scenarioFn{
		slowTextTurn("slow", 200*time.Millisecond),
	}}
	res, err := RunAutonomous(context.Background(),
		buildAgent(llm, "per-turn"),
		"go",
		WithPerTurnTimeout(40*time.Millisecond))
	if err == nil {
		t.Fatalf("expected an error from the timed-out turn, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context.DeadlineExceeded", err)
	}
	// Default retry policy aborts, so Reason is RetryAborted — not
	// a dedicated "per-turn timeout" reason. The signal users care
	// about (the deadline) is on err.
	if res.Reason != StopReasonRetryAborted {
		t.Errorf("Reason = %q, want %q", res.Reason, StopReasonRetryAborted)
	}
	if res.Turns != 1 {
		t.Errorf("Turns = %d, want 1", res.Turns)
	}
}

func TestRunAutonomous_FinalTextIsLastTurnText(t *testing.T) {
	t.Parallel()
	// Multi-turn run; assert RunResult.FinalText reflects only the
	// text from the last turn that emitted any, not an accumulation
	// of every turn.
	llm := &stubLLM{scenarios: []scenarioFn{
		textTurn("first turn output", 1, 1),
		textTurn("second turn output", 1, 1),
		doneCallTurn("done"),
		textTurn("post-tool follow-up", 0, 0),
	}}
	res, err := RunAutonomous(context.Background(),
		buildAgent(llm, "final-text"),
		"go")
	if err != nil {
		t.Fatalf("RunAutonomous: %v", err)
	}
	if res.Reason != StopReasonCompleted {
		t.Errorf("Reason = %q, want completed", res.Reason)
	}
	if res.Turns != 3 {
		t.Errorf("Turns = %d, want 3", res.Turns)
	}
	if res.FinalText != "post-tool follow-up" {
		t.Errorf("FinalText = %q, want %q (only the last turn's text)", res.FinalText, "post-tool follow-up")
	}
}

func TestRunAutonomous_DoneToolNameOverride(t *testing.T) {
	t.Parallel()
	// Same as the basic done test but the tool is renamed; the model
	// emits a FunctionCall to "all_done" instead.
	llm := &stubLLM{scenarios: []scenarioFn{
		func(_ context.Context, _ *adkmodel.LLMRequest) []stubResp {
			fc := &genai.FunctionCall{Name: "all_done", Args: map[string]any{"state": "done", "detail": "yep"}}
			c := &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: fc}}}
			return []stubResp{{resp: &adkmodel.LLMResponse{
				Content: c, TurnComplete: true, FinishReason: genai.FinishReasonStop,
			}}}
		},
		textTurn("done", 0, 0),
	}}
	res, err := RunAutonomous(context.Background(),
		buildAgent(llm, "rename"),
		"go",
		WithDoneToolName("all_done"))
	if err != nil {
		t.Fatalf("RunAutonomous: %v", err)
	}
	if res.Reason != StopReasonCompleted {
		t.Errorf("Reason = %q, want completed", res.Reason)
	}
	if res.DoneDetail != "yep" {
		t.Errorf("DoneDetail = %q, want yep", res.DoneDetail)
	}
}

// promptCapture records the user-prompt text from each LLM request as
// a side-channel for assertions about which prompt was sent on which
// turn. It wraps existing scenario functions so the test still scripts
// behaviour normally.
type promptCapture struct {
	mu       sync.Mutex
	captured []string
}

func capturePrompts() *promptCapture { return &promptCapture{} }

func (p *promptCapture) wrap(inner scenarioFn) scenarioFn {
	return func(ctx context.Context, req *adkmodel.LLMRequest) []stubResp {
		p.mu.Lock()
		// Walk Contents back-to-front looking for the most recent
		// user message (skipping tool responses). That's the
		// effective "prompt" for this LLM call.
		var found string
		for i := len(req.Contents) - 1; i >= 0; i-- {
			c := req.Contents[i]
			if c == nil || c.Role != genai.RoleUser {
				continue
			}
			hasText := false
			var b strings.Builder
			for _, part := range c.Parts {
				if part != nil && part.Text != "" {
					if b.Len() > 0 {
						b.WriteByte(' ')
					}
					b.WriteString(part.Text)
					hasText = true
				}
			}
			if hasText {
				found = b.String()
				break
			}
		}
		if found != "" {
			if len(p.captured) == 0 || p.captured[len(p.captured)-1] != found {
				p.captured = append(p.captured, found)
			}
		}
		p.mu.Unlock()
		return inner(ctx, req)
	}
}

func (p *promptCapture) snapshot() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.captured))
	copy(out, p.captured)
	return out
}
