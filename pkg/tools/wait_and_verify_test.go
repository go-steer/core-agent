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

package tools

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	adkagent "google.golang.org/adk/agent"
	adktool "google.golang.org/adk/tool"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/pkg/config"
)

// pollTarget is a scriptable stand-in for whatever the model wants to
// poll: an MCP get_pods, a fetch_url health check, a stat.
type pollTarget struct {
	name     string
	readOnly bool
	mu       sync.Mutex
	calls    int
	// respond returns the result for the Nth call (1-based).
	respond func(call int) (map[string]any, error)
	// block, when non-nil, is waited on before responding — used to
	// prove one hung poll can't outlive the budget.
	block chan struct{}
}

func (p *pollTarget) Name() string        { return p.name }
func (p *pollTarget) Description() string { return "test poll target" }
func (p *pollTarget) IsLongRunning() bool { return false }
func (p *pollTarget) ReadOnlyHint() bool  { return p.readOnly }
func (p *pollTarget) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{Name: p.name}
}

func (p *pollTarget) Run(ctx adktool.Context, args any) (map[string]any, error) {
	p.mu.Lock()
	p.calls++
	n := p.calls
	p.mu.Unlock()
	if p.block != nil {
		select {
		case <-p.block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return p.respond(n)
}

func (p *pollTarget) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// readyAfter responds "Pending" until the Nth call, then "Running".
func readyAfter(n int) func(int) (map[string]any, error) {
	return func(call int) (map[string]any, error) {
		phase := "Pending"
		if call >= n {
			phase = "Running"
		}
		return map[string]any{"phase": phase}, nil
	}
}

// newTestVerifier builds the tool, drops the interval floor to
// millisecond scale so the loop tests don't sleep for real, and binds
// the supplied catalog.
func newTestVerifier(t *testing.T, opts WaitAndVerifyOptions, catalog ...adktool.Tool) *waitVerifier {
	t.Helper()
	built, err := NewWaitAndVerifyTool(config.DefaultConfig(), opts)
	if err != nil {
		t.Fatalf("NewWaitAndVerifyTool: %v", err)
	}
	wv, ok := built.(*waitAndVerifyTool)
	if !ok {
		t.Fatalf("NewWaitAndVerifyTool returned %T, want *waitAndVerifyTool", built)
	}
	wv.BindCatalog(append([]adktool.Tool{built}, catalog...), nil)
	wv.v.minInterval = time.Millisecond
	return wv.v
}

func TestWaitAndVerify_VerifiesOnTheFirstAttempt(t *testing.T) {
	t.Parallel()
	target := &pollTarget{name: "get_pod", readOnly: true, respond: readyAfter(1)}
	v := newTestVerifier(t, WaitAndVerifyOptions{}, target)

	res, err := v.run(nil, waitAndVerifyArgs{
		Tool:     "get_pod",
		ExpectJQ: `.phase == "Running"`,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Verified || res.Outcome != waitOutcomeVerified {
		t.Fatalf("verified=%v outcome=%q, want a verified result", res.Verified, res.Outcome)
	}
	if res.Attempts != 1 {
		t.Errorf("attempts = %d, want 1 — an already-converged state must not wait", res.Attempts)
	}
	if len(res.Observations) != 1 || !res.Observations[0].Matched {
		t.Errorf("observations = %+v, want one matched entry", res.Observations)
	}
	if !strings.Contains(res.LastResult, "Running") {
		t.Errorf("last_result = %q, want the observed payload as evidence", res.LastResult)
	}
}

func TestWaitAndVerify_PollsUntilTheConditionHolds(t *testing.T) {
	t.Parallel()
	target := &pollTarget{name: "get_pod", readOnly: true, respond: readyAfter(3)}
	v := newTestVerifier(t, WaitAndVerifyOptions{}, target)

	res, err := v.run(nil, waitAndVerifyArgs{
		Tool:            "get_pod",
		ExpectJQ:        `.phase == "Running"`,
		IntervalSeconds: 0.001,
		TimeoutSeconds:  5,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Verified {
		t.Fatalf("want verified, got %+v", res)
	}
	if res.Attempts != 3 || target.callCount() != 3 {
		t.Errorf("attempts = %d, calls = %d, want 3 and 3", res.Attempts, target.callCount())
	}
	if len(res.Observations) != 3 {
		t.Fatalf("observations = %d, want one per attempt", len(res.Observations))
	}
	if res.Observations[0].Matched || !res.Observations[2].Matched {
		t.Errorf("observation match trail is wrong: %+v", res.Observations)
	}
}

// The evidence trail is the point (#639): an unverified wait must say
// so, and must hand the model something to reason about.
func TestWaitAndVerify_TimesOutUnverifiedWithEvidence(t *testing.T) {
	t.Parallel()
	target := &pollTarget{name: "get_pod", readOnly: true, respond: readyAfter(1000)}
	v := newTestVerifier(t, WaitAndVerifyOptions{}, target)

	res, err := v.run(nil, waitAndVerifyArgs{
		Tool:            "get_pod",
		ExpectJQ:        `.phase == "Running"`,
		IntervalSeconds: 0.001,
		TimeoutSeconds:  0.05,
	})
	if err != nil {
		t.Fatalf("a timeout is a result, not an error: %v", err)
	}
	if res.Verified {
		t.Fatal("verified must be false when the condition never held")
	}
	if res.Outcome != waitOutcomeTimeout {
		t.Errorf("outcome = %q, want %q", res.Outcome, waitOutcomeTimeout)
	}
	if len(res.Observations) == 0 || res.LastResult == "" {
		t.Errorf("want observations + a last_result to justify the verdict, got %+v", res)
	}
	if res.Condition == "" {
		t.Error("want the condition restated in the result")
	}
}

// The amplification bound. One model-approved call must not become an
// unbounded number of downstream calls.
func TestWaitAndVerify_MaxAttemptsBoundsTheCallCount(t *testing.T) {
	t.Parallel()
	target := &pollTarget{name: "get_pod", readOnly: true, respond: readyAfter(1000)}
	v := newTestVerifier(t, WaitAndVerifyOptions{}, target)

	res, err := v.run(nil, waitAndVerifyArgs{
		Tool:            "get_pod",
		ExpectJQ:        `.phase == "Running"`,
		IntervalSeconds: 0.001,
		TimeoutSeconds:  30,
		MaxAttempts:     3,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := target.callCount(); got != 3 {
		t.Errorf("polled tool called %d times, want exactly max_attempts (3)", got)
	}
	if res.Outcome != waitOutcomeExhausted {
		t.Errorf("outcome = %q, want %q", res.Outcome, waitOutcomeExhausted)
	}
}

// Read-only by construction: the loop refuses a tool the runtime
// classifies as mutating, because polling would apply it max_attempts
// times.
func TestWaitAndVerify_RefusesAMutatingTool(t *testing.T) {
	t.Parallel()
	target := &pollTarget{name: "scale_deployment", readOnly: false, respond: readyAfter(1)}
	v := newTestVerifier(t, WaitAndVerifyOptions{}, target)

	_, err := v.run(nil, waitAndVerifyArgs{Tool: "scale_deployment", ExpectContains: "Running"})
	if err == nil {
		t.Fatal("want a refusal for a mutating poll target")
	}
	if !strings.Contains(err.Error(), "not classified read-only") {
		t.Errorf("refusal should say why: %v", err)
	}
	if target.callCount() != 0 {
		t.Errorf("the refused tool was called %d times; a refusal must happen before any call", target.callCount())
	}
}

// MCP tools carry no readOnlyHint through ADK's adapter, so they land
// on the mutating side by default. poll_allow is the operator's
// override, and it is the only one.
func TestWaitAndVerify_PollAllowAdmitsAnUnclassifiedTool(t *testing.T) {
	t.Parallel()
	target := &pollTarget{name: "gke_get_pod", readOnly: false, respond: readyAfter(1)}
	v := newTestVerifier(t, WaitAndVerifyOptions{PollAllow: []string{"gke_get_pod"}}, target)

	res, err := v.run(nil, waitAndVerifyArgs{Tool: "gke_get_pod", ExpectContains: "Running"})
	if err != nil {
		t.Fatalf("an allow-listed tool must be pollable: %v", err)
	}
	if !res.Verified {
		t.Errorf("want verified, got %+v", res)
	}
}

func TestWaitAndVerify_RefusesToPollItself(t *testing.T) {
	t.Parallel()
	// Allow-listed on purpose: the never-pollable set must win over
	// operator config, or the budget stops meaning anything under
	// nesting.
	v := newTestVerifier(t, WaitAndVerifyOptions{PollAllow: []string{WaitAndVerifyToolName}})

	_, err := v.run(nil, waitAndVerifyArgs{Tool: WaitAndVerifyToolName, ExpectContains: "x"})
	if err == nil || !strings.Contains(err.Error(), "not classified read-only") {
		t.Fatalf("want a refusal to poll itself, got %v", err)
	}
}

func TestWaitAndVerify_RefusesAskUserEvenWhenAllowListed(t *testing.T) {
	t.Parallel()
	target := &pollTarget{name: "ask_user", readOnly: true, respond: readyAfter(1)}
	v := newTestVerifier(t, WaitAndVerifyOptions{PollAllow: []string{"ask_user"}}, target)

	_, err := v.run(nil, waitAndVerifyArgs{Tool: "ask_user", ExpectContains: "yes"})
	if err == nil {
		t.Fatal("ask_user blocks on a human; polling it on a timer must be refused")
	}
	if target.callCount() != 0 {
		t.Errorf("ask_user was called %d times", target.callCount())
	}
}

func TestWaitAndVerify_UnknownToolListsThePollableOnes(t *testing.T) {
	t.Parallel()
	target := &pollTarget{name: "get_pod", readOnly: true, respond: readyAfter(1)}
	v := newTestVerifier(t, WaitAndVerifyOptions{}, target)

	_, err := v.run(nil, waitAndVerifyArgs{Tool: "get_pods", ExpectContains: "x"})
	if err == nil {
		t.Fatal("want an error for an unknown tool")
	}
	if !strings.Contains(err.Error(), "get_pod") {
		t.Errorf("the error should name the pollable tools so the model can self-correct: %v", err)
	}
}

func TestWaitAndVerify_RefusesWithoutABoundCatalog(t *testing.T) {
	t.Parallel()
	built, err := NewWaitAndVerifyTool(config.DefaultConfig(), WaitAndVerifyOptions{})
	if err != nil {
		t.Fatalf("NewWaitAndVerifyTool: %v", err)
	}
	v := built.(*waitAndVerifyTool).v

	_, err = v.run(nil, waitAndVerifyArgs{Tool: "get_pod", ExpectContains: "x"})
	if err == nil || !strings.Contains(err.Error(), "no tool catalog") {
		t.Fatalf("an unbound waiter must say so explicitly, got %v", err)
	}
}

func TestWaitAndVerify_RequiresACondition(t *testing.T) {
	t.Parallel()
	target := &pollTarget{name: "get_pod", readOnly: true, respond: readyAfter(1)}
	v := newTestVerifier(t, WaitAndVerifyOptions{}, target)

	_, err := v.run(nil, waitAndVerifyArgs{Tool: "get_pod"})
	if err == nil || !strings.Contains(err.Error(), "condition is required") {
		t.Fatalf("a wait with no condition is a sleep; want a refusal, got %v", err)
	}
}

// Exceeding an operator ceiling is an ERROR, not a silent clamp: a
// tool that reports "I waited 900s" for a 300s wait is exactly the
// unenforced claim this milestone is about.
func TestWaitAndVerify_RejectsBudgetsAboveTheOperatorCeiling(t *testing.T) {
	t.Parallel()
	target := &pollTarget{name: "get_pod", readOnly: true, respond: readyAfter(1)}
	v := newTestVerifier(t, WaitAndVerifyOptions{MaxTimeout: 10 * time.Second, MaxAttempts: 4}, target)

	if _, err := v.run(nil, waitAndVerifyArgs{Tool: "get_pod", ExpectContains: "x", TimeoutSeconds: 30}); err == nil ||
		!strings.Contains(err.Error(), "ceiling") {
		t.Errorf("timeout above the ceiling should be rejected, got %v", err)
	}
	if _, err := v.run(nil, waitAndVerifyArgs{Tool: "get_pod", ExpectContains: "x", MaxAttempts: 99}); err == nil ||
		!strings.Contains(err.Error(), "ceiling") {
		t.Errorf("max_attempts above the ceiling should be rejected, got %v", err)
	}
}

// An operator ceiling BELOW the built-in default lowers the default, it
// doesn't brick the tool. Pre-fix this failed: the default 60s budget
// was compared against the ceiling before the model's own request was
// considered, so a recipe capping waits at 30s made every call — even
// one that asked for no budget at all — fail with an error blaming a
// timeout_seconds the model never set.
func TestWaitAndVerify_ATightCeilingLowersTheDefaultInsteadOfFailing(t *testing.T) {
	t.Parallel()
	target := &pollTarget{name: "get_pod", readOnly: true, respond: readyAfter(1)}
	v := newTestVerifier(t, WaitAndVerifyOptions{MaxTimeout: 30 * time.Second}, target)

	res, err := v.run(nil, waitAndVerifyArgs{Tool: "get_pod", ExpectContains: "Running"})
	if err != nil {
		t.Fatalf("run with no explicit budget under a 30s ceiling: %v", err)
	}
	if !res.Verified {
		t.Errorf("result = %+v, want verified", res)
	}
}

// The interval clamps UP instead — the safe direction — and the result
// reports what actually happened.
func TestWaitAndVerify_ClampsTheIntervalUpAndReportsIt(t *testing.T) {
	t.Parallel()
	target := &pollTarget{name: "get_pod", readOnly: true, respond: readyAfter(1)}
	v := newTestVerifier(t, WaitAndVerifyOptions{}, target)
	v.minInterval = 250 * time.Millisecond

	res, err := v.run(nil, waitAndVerifyArgs{Tool: "get_pod", ExpectContains: "Running", IntervalSeconds: 0.001})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.IntervalSeconds != 0.25 {
		t.Errorf("effective interval_seconds = %v, want the clamped 0.25", res.IntervalSeconds)
	}
}

func TestWaitAndVerify_RejectsAMalformedJQExpression(t *testing.T) {
	t.Parallel()
	target := &pollTarget{name: "get_pod", readOnly: true, respond: readyAfter(1)}
	v := newTestVerifier(t, WaitAndVerifyOptions{}, target)

	_, err := v.run(nil, waitAndVerifyArgs{Tool: "get_pod", ExpectJQ: "((("})
	if err == nil || !strings.Contains(err.Error(), "expect_jq") {
		t.Fatalf("want a parse error before any polling, got %v", err)
	}
	if target.callCount() != 0 {
		t.Errorf("a bad expression must cost zero polls, got %d", target.callCount())
	}
}

// A jq expression that can't evaluate against the result's shape will
// never start working. Fail on attempt one instead of burning the
// whole budget re-learning that.
func TestWaitAndVerify_AbortsOnAJQRuntimeErrorInsteadOfBurningTheBudget(t *testing.T) {
	t.Parallel()
	target := &pollTarget{name: "get_pod", readOnly: true, respond: func(int) (map[string]any, error) {
		return map[string]any{"phase": "Running"}, nil
	}}
	v := newTestVerifier(t, WaitAndVerifyOptions{}, target)

	_, err := v.run(nil, waitAndVerifyArgs{
		Tool:            "get_pod",
		ExpectJQ:        `.phase.nested`, // indexing a string
		IntervalSeconds: 0.001,
		TimeoutSeconds:  30,
		MaxAttempts:     50,
	})
	if err == nil {
		t.Fatal("want an error for an expression that cannot evaluate")
	}
	if got := target.callCount(); got != 1 {
		t.Errorf("polled %d times before giving up, want 1", got)
	}
}

// A jq expression can loop forever, and it evaluates outside the polled
// tool's own call. Pre-fix (gojq's Run instead of RunWithContext) this
// test hangs until the 10s guard fires: the wait's deadline bounded the
// tool call but not the condition, so one crafted expression hung the
// whole turn — a bounded primitive that wasn't.
func TestWaitAndVerify_BoundsARunawayJQExpression(t *testing.T) {
	t.Parallel()
	target := &pollTarget{name: "get_pod", readOnly: true, respond: func(int) (map[string]any, error) {
		return map[string]any{"phase": "Pending"}, nil
	}}
	v := newTestVerifier(t, WaitAndVerifyOptions{}, target)

	done := make(chan waitAndVerifyResult, 1)
	go func() {
		res, err := v.run(nil, waitAndVerifyArgs{
			Tool:            "get_pod",
			ExpectJQ:        `[limit(1000000000; repeat(1))] | length > 0`,
			IntervalSeconds: 0.001,
			TimeoutSeconds:  0.2,
		})
		if err != nil {
			t.Errorf("budget expiry is a result, not an error: %v", err)
		}
		done <- res
	}()
	select {
	case res := <-done:
		if res.Verified {
			t.Error("an expression cut off by the budget cannot verify anything")
		}
		// The reason has to survive into the evidence, or the model
		// reads "timeout" and blames the cluster for its own jq.
		if !strings.Contains(res.LastError, "expect_jq") {
			t.Errorf("last_error = %q, want the expect_jq evaluation named", res.LastError)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a runaway expect_jq expression outlived the wait budget")
	}
}

// A failing poll is what a wait is FOR: the API server is rolling, the
// endpoint isn't serving yet. Errors are recorded, not fatal.
func TestWaitAndVerify_TreatsPollErrorsAsTransient(t *testing.T) {
	t.Parallel()
	target := &pollTarget{name: "get_pod", readOnly: true, respond: func(call int) (map[string]any, error) {
		if call < 3 {
			return nil, fmt.Errorf("connection refused")
		}
		return map[string]any{"phase": "Running"}, nil
	}}
	v := newTestVerifier(t, WaitAndVerifyOptions{}, target)

	res, err := v.run(nil, waitAndVerifyArgs{
		Tool:            "get_pod",
		ExpectContains:  "Running",
		IntervalSeconds: 0.001,
		TimeoutSeconds:  5,
	})
	if err != nil {
		t.Fatalf("transient poll failures must not fail the wait: %v", err)
	}
	if !res.Verified || res.Attempts != 3 {
		t.Fatalf("want verified on attempt 3, got %+v", res)
	}
	if res.Observations[0].Error == "" {
		t.Error("the failed attempts should be visible in the evidence trail")
	}
	if res.LastError != "" {
		t.Errorf("last_error = %q, want it cleared by the successful attempt", res.LastError)
	}
}

func TestWaitAndVerify_ReportsAPersistentPollError(t *testing.T) {
	t.Parallel()
	target := &pollTarget{name: "get_pod", readOnly: true, respond: func(int) (map[string]any, error) {
		return nil, fmt.Errorf("connection refused")
	}}
	v := newTestVerifier(t, WaitAndVerifyOptions{}, target)

	res, err := v.run(nil, waitAndVerifyArgs{
		Tool:            "get_pod",
		ExpectContains:  "Running",
		IntervalSeconds: 0.001,
		TimeoutSeconds:  0.05,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Verified || !strings.Contains(res.LastError, "connection refused") {
		t.Errorf("want unverified with the last error surfaced, got %+v", res)
	}
}

func TestWaitAndVerify_HonorsExpectNotContains(t *testing.T) {
	t.Parallel()
	target := &pollTarget{name: "get_pod", readOnly: true, respond: func(call int) (map[string]any, error) {
		if call < 2 {
			return map[string]any{"phase": "CrashLoopBackOff"}, nil
		}
		return map[string]any{"phase": "Running"}, nil
	}}
	v := newTestVerifier(t, WaitAndVerifyOptions{}, target)

	res, err := v.run(nil, waitAndVerifyArgs{
		Tool:              "get_pod",
		ExpectContains:    "Running",
		ExpectNotContains: "CrashLoopBackOff",
		IntervalSeconds:   0.001,
		TimeoutSeconds:    5,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Verified || res.Attempts != 2 {
		t.Errorf("want verified on attempt 2 (both clauses must hold), got %+v", res)
	}
}

// An operator hitting /interrupt, or SIGTERM on the daemon, must not
// have to wait out the poll budget.
func TestWaitAndVerify_StopsOnCancellation(t *testing.T) {
	t.Parallel()
	target := &pollTarget{name: "get_pod", readOnly: true, respond: readyAfter(1000)}
	v := newTestVerifier(t, WaitAndVerifyOptions{}, target)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	res, err := v.run(waitToolCtx{ctx: ctx}, waitAndVerifyArgs{
		Tool:            "get_pod",
		ExpectContains:  "Running",
		IntervalSeconds: 0.01,
		TimeoutSeconds:  60,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Outcome != waitOutcomeCanceled {
		t.Errorf("outcome = %q, want %q", res.Outcome, waitOutcomeCanceled)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("cancellation took %v; the tool context is not threaded into the loop", elapsed)
	}
}

// One hung poll must not outlive the whole budget — otherwise a
// wedged MCP server makes "bounded" a claim we don't keep.
func TestWaitAndVerify_BoundsASingleHungPoll(t *testing.T) {
	t.Parallel()
	blocked := make(chan struct{})
	defer close(blocked)
	target := &pollTarget{
		name:     "get_pod",
		readOnly: true,
		block:    blocked,
		respond:  readyAfter(1),
	}
	v := newTestVerifier(t, WaitAndVerifyOptions{}, target)

	done := make(chan waitAndVerifyResult, 1)
	go func() {
		res, err := v.run(nil, waitAndVerifyArgs{
			Tool:            "get_pod",
			ExpectContains:  "Running",
			IntervalSeconds: 0.001,
			TimeoutSeconds:  0.1,
		})
		if err != nil {
			t.Errorf("run: %v", err)
		}
		done <- res
	}()
	select {
	case res := <-done:
		if res.Verified {
			t.Error("a hung poll cannot verify anything")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a hung poll consumed more than the configured budget")
	}
}

// Toolsets resolve lazily (an MCP server is queried on demand), so the
// waiter has to look inside them rather than only the flat slice.
func TestWaitAndVerify_ResolvesThroughAToolset(t *testing.T) {
	t.Parallel()
	target := &pollTarget{name: "gke_get_pod", readOnly: true, respond: readyAfter(1)}
	built, err := NewWaitAndVerifyTool(config.DefaultConfig(), WaitAndVerifyOptions{})
	if err != nil {
		t.Fatalf("NewWaitAndVerifyTool: %v", err)
	}
	wv := built.(*waitAndVerifyTool)
	wv.BindCatalog([]adktool.Tool{built}, []adktool.Toolset{&stubToolset{name: "gke", tools: []adktool.Tool{target}}})

	res, err := wv.v.run(nil, waitAndVerifyArgs{Tool: "gke_get_pod", ExpectContains: "Running"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Verified {
		t.Errorf("want verified, got %+v", res)
	}
}

// A toolset that can't be reached must not make every other tool
// unpollable.
func TestWaitAndVerify_SurvivesAnUnreachableToolset(t *testing.T) {
	t.Parallel()
	target := &pollTarget{name: "get_pod", readOnly: true, respond: readyAfter(1)}
	built, err := NewWaitAndVerifyTool(config.DefaultConfig(), WaitAndVerifyOptions{})
	if err != nil {
		t.Fatalf("NewWaitAndVerifyTool: %v", err)
	}
	wv := built.(*waitAndVerifyTool)
	wv.BindCatalog([]adktool.Tool{built, target}, []adktool.Toolset{&stubToolset{name: "dead", err: fmt.Errorf("dial tcp: refused")}})

	res, err := wv.v.run(nil, waitAndVerifyArgs{Tool: "get_pod", ExpectContains: "Running"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Verified {
		t.Errorf("want verified, got %+v", res)
	}
}

func TestWaitAndVerify_IsClassifiedReadOnly(t *testing.T) {
	t.Parallel()
	built, err := NewWaitAndVerifyTool(config.DefaultConfig(), WaitAndVerifyOptions{})
	if err != nil {
		t.Fatalf("NewWaitAndVerifyTool: %v", err)
	}
	if !IsReadOnlyTool(built) {
		t.Error("wait_and_verify mutates nothing and refuses to poll anything that does; it should dispatch concurrently")
	}
}

func TestWaitAndVerifyOptionsFromConfig(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultConfig()
	cfg.Tools.WaitAndVerify = config.WaitAndVerifyConfig{
		PollAllow:         []string{"gke_get_pod"},
		MaxTimeoutSeconds: 42,
		MaxAttempts:       7,
	}
	opts := WaitAndVerifyOptionsFromConfig(cfg)
	if opts.MaxTimeout != 42*time.Second || opts.MaxAttempts != 7 || len(opts.PollAllow) != 1 {
		t.Errorf("options = %+v, want the configured bounds", opts)
	}
}

// stubToolset is a minimal adktool.Toolset for the resolution tests.
type stubToolset struct {
	name  string
	tools []adktool.Tool
	err   error
}

func (s *stubToolset) Name() string { return s.name }
func (s *stubToolset) Tools(adkagent.ReadonlyContext) ([]adktool.Tool, error) {
	return s.tools, s.err
}

// waitToolCtx adapts a plain context into tool.Context for the
// cancellation test; only the context half is backed.
type waitToolCtx struct {
	adktool.Context
	ctx context.Context
}

func (c waitToolCtx) Deadline() (time.Time, bool) { return c.ctx.Deadline() }
func (c waitToolCtx) Done() <-chan struct{}       { return c.ctx.Done() }
func (c waitToolCtx) Err() error                  { return c.ctx.Err() }
func (c waitToolCtx) Value(key any) any           { return c.ctx.Value(key) }
