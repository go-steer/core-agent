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

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/pkg/attach"
	"github.com/go-steer/core-agent/v2/pkg/watchdog"
)

// Enforce-mode tests (#623). The warn-mode plumbing is covered in
// watchdog_test.go; here we cover the kill switch: a Critical alert
// trips the agent, a tripped agent refuses new turns via preflight,
// and ResetWatchdog clears the trip + resets the underlying signal
// state. Mirrors cost_ceiling_test.go's structure — the enforce path
// is the same halt contract as the cost ceiling.

func TestWithWatchdogEnforce_SetsOption(t *testing.T) {
	t.Parallel()
	o := &options{}
	WithWatchdogEnforce()(o)
	if !o.watchdogEnforce {
		t.Errorf("WithWatchdogEnforce should set options.watchdogEnforce")
	}
}

func TestIsWatchdogTripped(t *testing.T) {
	t.Parallel()
	if IsWatchdogTripped(nil) {
		t.Errorf("nil error should not match")
	}
	if IsWatchdogTripped(errors.New("random")) {
		t.Errorf("non-watchdogError should not match")
	}
	err := &watchdogError{reason: "test"}
	if !IsWatchdogTripped(err) {
		t.Errorf("watchdogError should match")
	}
	if !IsWatchdogTripped(error(err)) {
		t.Errorf("wrapped in error interface should still match")
	}
}

// TestDrainWatchdogAlerts_EnforceTripsOnCritical is the core enforce
// gate. A Critical alert under enforce mode must: set the tripped flag,
// record an operator-facing reason, and emit a watchdog turn-error.
// Fails on pre-#623 code (drainWatchdogAlerts had no enforce path — the
// alert would dispatch to the callback and nothing would trip).
func TestDrainWatchdogAlerts_EnforceTripsOnCritical(t *testing.T) {
	t.Parallel()
	w := &fakeWatchdog{pending: []watchdog.Alert{
		{Signal: "repeated-tool-call", Severity: watchdog.SeverityCritical, Reason: "looping on read_file 5x."},
	}}
	var events []struct {
		kind    string
		payload any
	}
	a := &Agent{watchdog: w, watchdogEnforce: true}
	a.SetOperatorEventEmitter(func(kind string, payload any) {
		events = append(events, struct {
			kind    string
			payload any
		}{kind, payload})
	})

	a.drainWatchdogAlerts()

	tripped, reason := a.WatchdogTripped()
	if !tripped {
		t.Fatalf("enforce mode should trip on a Critical alert")
	}
	if !strings.Contains(reason, "read_file") {
		t.Errorf("reason should carry the alert reason; got %q", reason)
	}
	// The reason must name an affordance an operator can actually
	// reach — a Go method name isn't one (#666).
	if !strings.Contains(reason, "/guardrail reset") || !strings.Contains(reason, "guardrails/reset") {
		t.Errorf("reason should point the operator at the slash command and the endpoint; got %q", reason)
	}
	// A watchdog turn-error must have been emitted.
	var found bool
	for _, e := range events {
		if e.kind != attach.EventTurnError {
			continue
		}
		te, ok := e.payload.(attach.TurnError)
		if !ok {
			t.Fatalf("turn-error payload is %T, want attach.TurnError", e.payload)
		}
		if te.Kind == attach.TurnErrorWatchdog {
			found = true
			if te.Retryable {
				t.Errorf("watchdog turn-error should be non-retryable")
			}
		}
	}
	if !found {
		t.Errorf("expected a turn-error with kind=%q; got events %+v", attach.TurnErrorWatchdog, events)
	}
}

// TestDrainWatchdogAlerts_WarnModeDoesNotTrip guards the mode boundary:
// the SAME Critical alert must NOT halt when enforce is off. Warn mode
// only logs.
func TestDrainWatchdogAlerts_WarnModeDoesNotTrip(t *testing.T) {
	t.Parallel()
	w := &fakeWatchdog{pending: []watchdog.Alert{
		{Signal: "repeated-tool-call", Severity: watchdog.SeverityCritical, Reason: "loop"},
	}}
	var dispatched int
	a := &Agent{
		watchdog:        w, // watchdogEnforce defaults false
		onWatchdogAlert: func(watchdog.Alert) { dispatched++ },
	}
	a.drainWatchdogAlerts()
	if dispatched != 1 {
		t.Errorf("warn mode should still dispatch the alert to the callback; got %d", dispatched)
	}
	if tripped, _ := a.WatchdogTripped(); tripped {
		t.Errorf("warn mode must NOT trip the enforce kill switch")
	}
}

// TestDrainWatchdogAlerts_EnforceIgnoresNonCritical: enforce halts only
// on Critical. A Warn alert (e.g. a future low-severity signal) must
// not trip even under enforce.
func TestDrainWatchdogAlerts_EnforceIgnoresNonCritical(t *testing.T) {
	t.Parallel()
	w := &fakeWatchdog{pending: []watchdog.Alert{
		{Signal: "some-future-signal", Severity: watchdog.SeverityWarn, Reason: "advisory"},
	}}
	a := &Agent{watchdog: w, watchdogEnforce: true}
	a.drainWatchdogAlerts()
	if tripped, _ := a.WatchdogTripped(); tripped {
		t.Errorf("enforce should not trip on a non-Critical (warn) alert")
	}
}

// TestDrainWatchdogAlerts_EnforceTripsWithoutCallback: enforcement must
// fire even when no warn-mode callback is wired (the trip logic lives
// outside the onWatchdogAlert==nil early return). A regression here
// would silently disable enforce for hosts that don't set a callback.
func TestDrainWatchdogAlerts_EnforceTripsWithoutCallback(t *testing.T) {
	t.Parallel()
	w := &fakeWatchdog{pending: []watchdog.Alert{
		{Signal: "repeated-tool-call", Severity: watchdog.SeverityCritical, Reason: "loop"},
	}}
	a := &Agent{watchdog: w, watchdogEnforce: true /* no onWatchdogAlert */}
	a.drainWatchdogAlerts()
	if tripped, _ := a.WatchdogTripped(); !tripped {
		t.Fatalf("enforce should trip even with no warn-mode callback wired")
	}
}

// TestMaybeTripWatchdog_Idempotent: an already-tripped agent must not
// re-emit or rewrite its reason on a subsequent drain (operators would
// otherwise see duplicate turn-error frames every turn).
func TestMaybeTripWatchdog_Idempotent(t *testing.T) {
	t.Parallel()
	a := &Agent{
		watchdogEnforce: true,
		watchdogTripped: true,
		watchdogReason:  "already tripped previously",
	}
	var emits int
	a.SetOperatorEventEmitter(func(string, any) { emits++ })
	a.maybeTripWatchdog([]watchdog.Alert{
		{Signal: "repeated-tool-call", Severity: watchdog.SeverityCritical, Reason: "new loop"},
	})
	if emits != 0 {
		t.Errorf("already-tripped agent should not re-emit; got %d emits", emits)
	}
	_, reason := a.WatchdogTripped()
	if reason != "already tripped previously" {
		t.Errorf("reason should be unchanged on idempotent re-check; got %q", reason)
	}
}

func TestPreflightWatchdog_NoFlagReturnsNil(t *testing.T) {
	t.Parallel()
	a := &Agent{}
	if err := a.preflightWatchdog(); err != nil {
		t.Errorf("preflight without tripped flag should return nil; got %v", err)
	}
}

func TestPreflightWatchdog_FlagReturnsTypedError(t *testing.T) {
	t.Parallel()
	a := &Agent{
		watchdogTripped: true,
		watchdogReason:  "watchdog halted the agent: loop",
	}
	err := a.preflightWatchdog()
	if err == nil {
		t.Fatalf("preflight with tripped flag should return error")
	}
	if !IsWatchdogTripped(err) {
		t.Errorf("error should be detectable via IsWatchdogTripped")
	}
	if !strings.Contains(err.Error(), "loop") {
		t.Errorf("error message should include the reason; got %q", err.Error())
	}
}

func TestResetWatchdog_ClearsFlagAndResetsSignal(t *testing.T) {
	t.Parallel()
	w := &fakeWatchdog{}
	a := &Agent{
		watchdog:        w,
		watchdogTripped: true,
		watchdogReason:  "test",
	}
	a.ResetWatchdog()
	if tripped, reason := a.WatchdogTripped(); tripped || reason != "" {
		t.Errorf("ResetWatchdog should clear flag+reason; got tripped=%v reason=%q", tripped, reason)
	}
	if w.resets != 1 {
		t.Errorf("ResetWatchdog should reset the underlying signal state; got %d resets", w.resets)
	}
}

func TestResetWatchdog_NilSafe(t *testing.T) {
	t.Parallel()
	var a *Agent
	a.ResetWatchdog() // must not panic
}

// TestRun_WatchdogEnforce_RefusesNextTurn drives the real Run loop: a
// turn whose post-turn drain trips the watchdog must cause the NEXT Run
// to be refused at preflight with a watchdogError — before any model
// call. This is the loop backstop: an auto-continue re-drive of the
// interrupted turn hits this refusal instead of re-issuing the runaway
// tool call. Fails on pre-#623 code (no preflightWatchdog wiring; the
// second turn would run normally).
func TestRun_WatchdogEnforce_RefusesNextTurn(t *testing.T) {
	t.Parallel()
	// fakeWatchdog returns the Critical alert exactly once (from turn
	// 1's post-turn Check), then nil — matching a real signal that
	// trips once per run of identical calls.
	w := &fakeWatchdog{pending: []watchdog.Alert{
		{Signal: "repeated-tool-call", Severity: watchdog.SeverityCritical, Reason: "looping on read_file 5x."},
	}}
	a, err := New(oneShotLLM{},
		WithSession("u-wd", "s-wd"),
		WithWatchdog(w, nil),
		WithWatchdogEnforce(),
	)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	ctx := context.Background()

	// Turn 1: completes normally; the post-turn drain sees the Critical
	// alert and trips enforce.
	for _, err := range a.Run(ctx, "hi") {
		if err != nil {
			t.Fatalf("turn 1 Run: %v", err)
		}
	}
	if tripped, _ := a.WatchdogTripped(); !tripped {
		t.Fatalf("watchdog should have tripped at turn-1 post-turn drain")
	}

	// Turn 2: Run must refuse at preflight.
	var gotErr error
	for _, err := range a.Run(ctx, "again") {
		if err != nil {
			gotErr = err
		}
	}
	if !IsWatchdogTripped(gotErr) {
		t.Fatalf("turn 2 should have been refused by the watchdog; got err=%v", gotErr)
	}

	// After ResetWatchdog, Run accepts turns again.
	a.ResetWatchdog()
	if tripped, _ := a.WatchdogTripped(); tripped {
		t.Fatalf("ResetWatchdog should clear the trip")
	}
	for _, err := range a.Run(ctx, "resumed") {
		if err != nil {
			t.Fatalf("turn 3 after reset should run; got %v", err)
		}
	}
}

// TestRun_WatchdogWarn_DoesNotRefuse: the same tripping alert under warn
// mode (no WithWatchdogEnforce) must NOT block subsequent turns.
func TestRun_WatchdogWarn_DoesNotRefuse(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var alerts int
	w := &fakeWatchdog{pending: []watchdog.Alert{
		{Signal: "repeated-tool-call", Severity: watchdog.SeverityCritical, Reason: "loop"},
	}}
	a, err := New(oneShotLLM{},
		WithSession("u-wd2", "s-wd2"),
		WithWatchdog(w, func(watchdog.Alert) { mu.Lock(); alerts++; mu.Unlock() }),
		// no WithWatchdogEnforce
	)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	ctx := context.Background()
	for _, err := range a.Run(ctx, "hi") {
		if err != nil {
			t.Fatalf("turn 1: %v", err)
		}
	}
	for _, err := range a.Run(ctx, "again") {
		if err != nil {
			t.Fatalf("warn mode must not refuse turn 2; got %v", err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if alerts == 0 {
		t.Errorf("warn mode should still have dispatched the alert")
	}
}

// TestWatchdogMode reports the resolved posture for each wiring
// combination. The accessor is what lets a caller outside pkg/agent
// verify "the backstop is actually on" — the question #642 flipped a
// default to answer — so a refactor that drops WithWatchdogEnforce on
// some construction path fails a test instead of silently shipping an
// un-backstopped daemon.
func TestWatchdogMode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		opts []Option
		want string
	}{
		{"no watchdog wired", nil, "off"},
		{"observe only", []Option{WithWatchdog(&fakeWatchdog{}, nil)}, "warn"},
		{"observe and halt", []Option{WithWatchdog(&fakeWatchdog{}, nil), WithWatchdogEnforce()}, "enforce"},
		// Enforce without a watchdog is a no-op per WithWatchdogEnforce's
		// contract, so the reported mode must stay "off" rather than
		// claiming a backstop that cannot fire.
		{"enforce without a watchdog", []Option{WithWatchdogEnforce()}, "off"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a, err := New(oneShotLLM{}, append([]Option{WithSession("u-wdm", "s-wdm")}, tc.opts...)...)
			if err != nil {
				t.Fatalf("agent.New: %v", err)
			}
			if got := a.WatchdogMode(); got != tc.want {
				t.Errorf("WatchdogMode() = %q, want %q", got, tc.want)
			}
		})
	}

	var nilAgent *Agent
	if got := nilAgent.WatchdogMode(); got != "off" {
		t.Errorf("nil agent WatchdogMode() = %q, want %q", got, "off")
	}
}

// loopingLLM answers every invocation with the same tool call — the
// shape a tool-calling flow produces when the model loops INSIDE one
// turn. It honours ctx cancellation the way a real model client does,
// so an enforce-mode halt actually truncates the loop.
type loopingLLM struct {
	calls int

	mu          sync.Mutex
	invocations int
}

func (l *loopingLLM) Name() string { return "looping" }
func (l *loopingLLM) GenerateContent(ctx context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		if err := ctx.Err(); err != nil {
			yield(nil, err)
			return
		}
		l.mu.Lock()
		l.invocations++
		n := l.invocations
		l.mu.Unlock()
		// One identical tool call per invocation, all inside a single
		// Run — the flow stops draining the model stream at the first
		// FunctionCall, executes it, and calls the model again.
		if n <= l.calls {
			yield(&adkmodel.LLMResponse{
				Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{
					FunctionCall: &genai.FunctionCall{
						ID:   fmt.Sprintf("call_%d", n),
						Name: "todo",
						Args: map[string]any{"action": "list"},
					},
				}}},
			}, nil)
			return
		}
		yield(&adkmodel.LLMResponse{
			Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "done"}}},
			TurnComplete: true,
		}, nil)
	}
}

// TestRun_WatchdogEnforce_HaltsAnIntraTurnLoop is the #705 regression.
//
// Every other enforce test drives a fakeWatchdog that hands back a
// canned alert, and every one of them checks the trip only after Run
// has returned. That left the shipped configuration — a real
// DefaultWatchdog, real repeated tool calls — covered nowhere, and hid
// a structural gap: alerts were drained only from the post-turn hook,
// so a loop inside a single turn never tripped anything. An unattended
// daemon whose agent loops within one long turn burned budget with no
// halt and no log line, while the boot line advertised "halts the
// agent".
//
// Asserted at the point that matters: the halt lands mid-turn and cuts
// the loop short, rather than arriving after the turn it should have
// stopped.
func TestRun_WatchdogEnforce_HaltsAnIntraTurnLoop(t *testing.T) {
	t.Parallel()

	// 40 calls, threshold 5: pre-fix every one of them runs and the
	// trip lands after the fact; post-fix the run stops just past the
	// threshold.
	const attempted = 40
	llm := &loopingLLM{calls: attempted}
	a, err := New(llm,
		WithSession("u-wd-loop", "s-wd-loop"),
		WithWatchdog(watchdog.NewDefaultWatchdog(), nil),
		WithWatchdogEnforce(),
	)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}

	for _, err := range a.Run(context.Background(), "go") {
		_ = err // the halt surfaces as a cancellation; not the subject
	}

	tripped, reason := a.WatchdogTripped()
	if !tripped {
		t.Fatal("identical tool calls in one turn did not trip enforce")
	}
	if !strings.Contains(reason, "repeated-tool-call") {
		t.Errorf("trip reason = %q, want the repeated-tool-call signal", reason)
	}

	llm.mu.Lock()
	invocations := llm.invocations
	llm.mu.Unlock()
	t.Logf("model invocations before halt: %d of %d", invocations, attempted)
	if invocations >= attempted {
		t.Errorf("model ran all %d loop iterations before the watchdog halted it: a turn-boundary-only backstop cannot stop an intra-turn loop", invocations)
	}
}
