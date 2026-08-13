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
	"testing"
	"time"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/pkg/usage"
)

// Most of cost-ceiling enforcement is a thin wrapper over the usage
// tracker + the agent's existing pending-flag pattern. Tests focus
// on the enforcement contract — the wiring into Run() is exercised
// via the integration tests in agent_test.go's existing run-loop
// coverage; here we cover the cost-decision logic directly so
// failures point at the right code.

func TestCostCeiling_Active(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		c      CostCeiling
		active bool
	}{
		{"both zero → inactive", CostCeiling{}, false},
		{"only turn set → active", CostCeiling{MaxTurnUSD: 0.10}, true},
		{"only session set → active", CostCeiling{MaxSessionUSD: 1.00}, true},
		{"both set → active", CostCeiling{MaxTurnUSD: 0.1, MaxSessionUSD: 1.0}, true},
		{"negative → inactive (treated like 0)", CostCeiling{MaxTurnUSD: -1}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.c.active(); got != tc.active {
				t.Errorf("active() = %v, want %v", got, tc.active)
			}
		})
	}
}

func TestIsCostCeilingExceeded(t *testing.T) {
	t.Parallel()
	if IsCostCeilingExceeded(nil) {
		t.Errorf("nil error should not match")
	}
	if IsCostCeilingExceeded(errors.New("random")) {
		t.Errorf("non-costCeilingError should not match")
	}
	err := &costCeilingError{reason: "test"}
	if !IsCostCeilingExceeded(err) {
		t.Errorf("costCeilingError should match")
	}
	if !IsCostCeilingExceeded(error(err)) {
		t.Errorf("wrapped in error interface should still match")
	}
}

func TestMaybeEnforceCostCeiling_DisabledIsNoOp(t *testing.T) {
	t.Parallel()
	tr := usage.NewTracker()
	tr.Append("test", 1_000_000, 0, usage.Pricing{InputPerMTok: 100}) // big spend
	a := &Agent{tracker: tr /* no costCeiling configured */}
	a.maybeEnforceCostCeiling()
	tripped, _ := a.CostCeilingTripped()
	if tripped {
		t.Errorf("ceiling should not trip when none configured")
	}
}

// Pricing is per-million-tokens, so the test math here picks token
// counts + per-Mtok rates that yield round dollar costs at the
// ceiling boundary.
//   1.5M tokens × $0.10/Mtok = $0.15
//   50K tokens × $0.10/Mtok = $0.005
//   800K tokens × $1/Mtok = $0.80
//   500K tokens × $1/Mtok = $0.50

func TestMaybeEnforceCostCeiling_PerTurn_Trips(t *testing.T) {
	t.Parallel()
	tr := usage.NewTracker()
	a := &Agent{
		tracker:     tr,
		costCeiling: CostCeiling{MaxTurnUSD: 0.10},
	}
	// Snapshot at turn start (cost = 0).
	a.snapshotTurnStartCost()
	// Append a turn worth $0.15 — exceeds the $0.10 per-turn cap.
	tr.Append("test", 1_500_000, 0, usage.Pricing{InputPerMTok: 0.10})
	a.maybeEnforceCostCeiling()
	tripped, reason := a.CostCeilingTripped()
	if !tripped {
		t.Fatalf("ceiling should have tripped")
	}
	if !strings.Contains(reason, "per-turn") {
		t.Errorf("reason should mention 'per-turn'; got %q", reason)
	}
	// Reason uses %.4f formatting, so $0.15 renders as "$0.1500".
	if !strings.Contains(reason, "$0.1500") || !strings.Contains(reason, "$0.1000") {
		t.Errorf("reason should include both the spend ($0.1500) and the ceiling ($0.1000); got %q", reason)
	}
	// The reason must name an affordance an operator can actually
	// reach — a Go method name isn't one (#666).
	if !strings.Contains(reason, "/guardrail reset") || !strings.Contains(reason, "guardrails/reset") {
		t.Errorf("reason should point operators at the slash command and the endpoint; got %q", reason)
	}
}

func TestMaybeEnforceCostCeiling_PerTurn_DoesNotTripUnderCap(t *testing.T) {
	t.Parallel()
	tr := usage.NewTracker()
	a := &Agent{
		tracker:     tr,
		costCeiling: CostCeiling{MaxTurnUSD: 0.10},
	}
	a.snapshotTurnStartCost()
	tr.Append("test", 50_000, 0, usage.Pricing{InputPerMTok: 0.10}) // $0.005
	a.maybeEnforceCostCeiling()
	tripped, _ := a.CostCeilingTripped()
	if tripped {
		t.Errorf("ceiling should not trip at $0.005 vs $0.10 cap")
	}
}

func TestMaybeEnforceCostCeiling_PerSession_Trips(t *testing.T) {
	t.Parallel()
	tr := usage.NewTracker()
	// Pre-existing session spend, then add a small turn that
	// individually is under the per-turn cap but pushes the
	// cumulative session over the per-session cap.
	//   pre: 800K × $1/Mtok = $0.80
	//   turn: 500K × $1/Mtok = $0.50 (under per-turn cap of $1.00)
	//   total: $1.30, exceeds per-session cap of $1.00
	tr.Append("test", 800_000, 0, usage.Pricing{InputPerMTok: 1})
	a := &Agent{
		tracker:     tr,
		costCeiling: CostCeiling{MaxTurnUSD: 1.00, MaxSessionUSD: 1.00},
	}
	a.snapshotTurnStartCost() // captures $0.80 as turn start
	tr.Append("test", 500_000, 0, usage.Pricing{InputPerMTok: 1})
	a.maybeEnforceCostCeiling()
	tripped, reason := a.CostCeilingTripped()
	if !tripped {
		t.Fatalf("ceiling should have tripped on session bound")
	}
	if !strings.Contains(reason, "per-session") {
		t.Errorf("reason should mention 'per-session' (per-turn delta was under the per-turn cap); got %q", reason)
	}
}

func TestMaybeEnforceCostCeiling_AlreadyTripped_IsIdempotent(t *testing.T) {
	t.Parallel()
	tr := usage.NewTracker()
	a := &Agent{
		tracker:             tr,
		costCeiling:         CostCeiling{MaxTurnUSD: 0.01},
		costCeilingExceeded: true,
		costCeilingReason:   "already tripped previously",
	}
	a.snapshotTurnStartCost()
	tr.Append("test", 10_000_000, 0, usage.Pricing{InputPerMTok: 0.10}) // $1.00
	a.maybeEnforceCostCeiling()
	tripped, reason := a.CostCeilingTripped()
	if !tripped {
		t.Errorf("should still be tripped (was tripped before)")
	}
	// Reason should be UNCHANGED — re-checks of an already-tripped
	// ceiling don't re-emit or rewrite the reason. Tested because the
	// existing event-emission path is otherwise indistinguishable
	// from a fresh trip in the SSE stream — operators would see N
	// duplicate turn-error frames per turn if this guard regressed.
	if reason != "already tripped previously" {
		t.Errorf("reason should be unchanged on idempotent re-check; got %q", reason)
	}
}

// oneShotLLM completes each turn with a single TurnComplete response and
// no UsageMetadata — mirroring a harness-driven deployment where the
// agent itself never appends the main-model cost (the harness does that,
// after the turn's cleanup hook runs).
type oneShotLLM struct{}

func (oneShotLLM) Name() string { return "oneshot" }
func (oneShotLLM) GenerateContent(_ context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		yield(&adkmodel.LLMResponse{
			Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "done"}}},
			TurnComplete: true,
		}, nil)
	}
}

// TestRun_CostCeiling_SettlesAfterHarnessAppend is the #362 regression.
// In harness-driven deployments the harness appends a turn's main-model
// cost AFTER that turn's post-turn hook runs, so the hook's per-turn
// delta misses it and a single runaway turn (#144's read-file loop) can
// never trip the per-turn cap. The fix re-runs enforcement at the top of
// the next Run, once the prior turn is fully settled in the tracker.
//
// Drives the real Run loop (not maybeEnforceCostCeiling directly) because
// the bug is a timing/wiring issue in Run, not in the decision logic:
// the unit tests below already cover the decision, and would pass with or
// without the fix.
func TestRun_CostCeiling_SettlesAfterHarnessAppend(t *testing.T) {
	t.Parallel()

	tr := usage.NewTracker()
	a, err := New(oneShotLLM{},
		WithSession("u-cc", "s-cc"),
		WithUsageTracker(tr),
		WithCostCeiling(CostCeiling{MaxTurnUSD: 0.10}),
	)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	ctx := context.Background()

	// Turn 1: drive to completion. The agent appends nothing itself (no
	// inline subtasks), matching a harness-driven main turn.
	for _, err := range a.Run(ctx, "hi") {
		if err != nil {
			t.Fatalf("turn 1 Run: %v", err)
		}
	}
	if tripped, _ := a.CostCeilingTripped(); tripped {
		t.Fatalf("ceiling tripped at turn-1 cleanup, but the main-model cost hasn't been appended yet")
	}

	// Harness appends turn 1's main-model cost AFTER the cleanup hook:
	// $0.15, over the $0.10 per-turn cap.
	tr.Append("oneshot", 1_500_000, 0, usage.Pricing{InputPerMTok: 0.10})

	// Turn 2: Run must refuse, because settle-time enforcement now sees
	// turn 1's full cost. Before the fix, turn 1's cost fell in the gap
	// between its cleanup and turn 2's start-of-turn snapshot, so the cap
	// never tripped and turn 2 ran normally.
	var gotErr error
	for _, err := range a.Run(ctx, "again") {
		if err != nil {
			gotErr = err
		}
	}
	if !IsCostCeilingExceeded(gotErr) {
		t.Fatalf("turn 2 should have been refused by the per-turn ceiling; got err=%v", gotErr)
	}
	if tripped, reason := a.CostCeilingTripped(); !tripped || !strings.Contains(reason, "per-turn") {
		t.Errorf("expected per-turn ceiling tripped; tripped=%v reason=%q", tripped, reason)
	}
}

func TestResetCostCeiling_ClearsFlag(t *testing.T) {
	t.Parallel()
	a := &Agent{
		costCeilingExceeded: true,
		costCeilingReason:   "test",
	}
	a.ResetCostCeiling()
	tripped, reason := a.CostCeilingTripped()
	if tripped {
		t.Errorf("ResetCostCeiling should clear the tripped flag")
	}
	if reason != "" {
		t.Errorf("ResetCostCeiling should clear the reason; got %q", reason)
	}
}

func TestResetCostCeiling_NilSafe(t *testing.T) {
	t.Parallel()
	var a *Agent
	a.ResetCostCeiling() // should not panic
}

func TestPreflightCostCeiling_NoFlagReturnsNil(t *testing.T) {
	t.Parallel()
	a := &Agent{}
	if err := a.preflightCostCeiling(); err != nil {
		t.Errorf("preflight without tripped flag should return nil; got %v", err)
	}
}

func TestPreflightCostCeiling_FlagReturnsTypedError(t *testing.T) {
	t.Parallel()
	a := &Agent{
		costCeilingExceeded: true,
		costCeilingReason:   "session exceeded $5.00",
	}
	err := a.preflightCostCeiling()
	if err == nil {
		t.Fatalf("preflight with tripped flag should return error")
	}
	if !IsCostCeilingExceeded(err) {
		t.Errorf("error should be detectable via IsCostCeilingExceeded")
	}
	if !strings.Contains(err.Error(), "$5.00") {
		t.Errorf("error message should include the reason; got %q", err.Error())
	}
}

// burnLoopLLM is a runaway inside ONE turn: every model call answers
// with the same tool call and reports its own usage, exactly as ADK's
// streaming aggregator delivers it (each model call's final chunk
// carries TurnComplete plus that call's UsageMetadata). Honours ctx so
// a ceiling trip actually truncates the loop, the way a real client
// would.
type burnLoopLLM struct {
	perCallIn, perCallOut int32

	mu    sync.Mutex
	calls int
}

func (*burnLoopLLM) Name() string { return "burn-loop" }
func (l *burnLoopLLM) GenerateContent(ctx context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		if err := ctx.Err(); err != nil {
			yield(nil, err)
			return
		}
		l.mu.Lock()
		l.calls++
		n := l.calls
		l.mu.Unlock()
		yield(&adkmodel.LLMResponse{
			Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{
				FunctionCall: &genai.FunctionCall{
					ID:   fmt.Sprintf("burn_%d", n),
					Name: "todo",
					Args: map[string]any{"i": n},
				},
			}}},
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
				PromptTokenCount:     l.perCallIn,
				CandidatesTokenCount: l.perCallOut,
			},
			TurnComplete: true,
		}, nil)
	}
}

// TestRun_CostCeiling_HaltsAnIntraTurnBurn is the #720 regression.
//
// Enforcement ran only at turn boundaries: the post-turn hook, and
// #362's settle-time re-check at the top of the NEXT Run. A runaway is
// a loop inside ONE turn, and the tracker grows on every model call
// within it, so both boundary checks sit idle through exactly the
// spend they exist to cap — and a turn that never terminates never
// reaches either. --max-turn-cost-usd is documented as the hard
// backstop for this shape (the watchdog's own alert text points
// operators at it), so it has to fire during the turn it is capping.
//
// Drives the real Run loop behind a harness-shaped tracker tap
// (usage.TurnTap, the same discipline pkg/runner uses), because the
// bug is timing in Run, not the decision logic the unit tests above
// already cover.
func TestRun_CostCeiling_HaltsAnIntraTurnBurn(t *testing.T) {
	t.Parallel()

	// $10/MTok input × 1000 tokens = $0.01 per model call, against a
	// $0.05 per-turn ceiling: the trip lands on call 5 of an otherwise
	// unbounded loop.
	pricing := usage.Pricing{InputPerMTok: 10}
	tr := usage.NewTracker()
	llm := &burnLoopLLM{perCallIn: 1000}

	a, err := New(llm,
		WithSession("u-cc-burn", "s-cc-burn"),
		WithUsageTracker(tr),
		WithCostCeiling(CostCeiling{MaxTurnUSD: 0.05}),
	)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	// Harness-shaped tap: commit per TurnComplete, exactly as
	// pkg/runner.tapTracker does.
	var tap usage.TurnTap
	for ev, err := range a.Run(ctx, "go") {
		_ = err // the halt surfaces as a cancellation
		tap.Observe(ev)
		if u, ok := tap.Commit(ev); ok {
			tr.AppendUsage(llm.Name(), u, pricing)
		}
	}
	if ctx.Err() != nil {
		t.Fatal("the turn burned until the deadline: the ceiling never halted it")
	}

	llm.mu.Lock()
	calls := llm.calls
	llm.mu.Unlock()

	tripped, reason := a.CostCeilingTripped()
	if !tripped {
		t.Fatalf("ceiling never tripped after %d model calls costing $%.4f", calls, tr.Totals().CostUSD)
	}
	if !strings.Contains(reason, "per-turn") {
		t.Errorf("trip reason = %q, want the per-turn ceiling", reason)
	}
	t.Logf("halted after %d model calls, $%.4f spent", calls, tr.Totals().CostUSD)
	// Generous bound: the point is that the loop stops soon after the
	// ceiling is crossed, not that it stops on an exact call.
	if calls > 10 {
		t.Errorf("loop ran %d model calls past a ceiling crossed at ~5: enforcement is still waiting for a turn boundary", calls)
	}
}
