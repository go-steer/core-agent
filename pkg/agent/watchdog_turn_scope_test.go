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
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/attach"
	"github.com/go-steer/core-agent/v2/pkg/watchdog"
)

// Turn-scoped enforcement (#1090). A Critical alert that declares
// watchdog.ScopeTurn cuts its turn and leaves the session alone, and
// three such turns in a row escalate to the session halt every Critical
// used to cause on its own.
//
// Why these tests exist, concretely: run 16 of the approval-gate drill
// denied one `alert`, the model correctly stopped re-issuing it and
// recorded the denial with four `mark_task_done` calls, three of those
// came back `no_op: true`, and the daemon halted for the rest of the
// night. Every assertion below is about the size of that consequence.

// turnScopedAlert is the shape NoOpStreakSignal now returns.
func turnScopedAlert(reason string) watchdog.Alert {
	return watchdog.Alert{
		Signal:   "no-op-streak",
		Severity: watchdog.SeverityCritical,
		Scope:    watchdog.ScopeTurn,
		Reason:   reason,
	}
}

// repeatWatchdog hands back the same alerts on every Check, the way a
// signal does when the model keeps doing the thing it tripped on. Reset
// is counted rather than acted on so a test can see that the agent
// scrubbed the evidence after cutting a turn.
type repeatWatchdog struct {
	alerts []watchdog.Alert
	resets int
}

func (r *repeatWatchdog) ObserveToolCall(watchdog.ToolCall) {}
func (r *repeatWatchdog) Check() []watchdog.Alert           { return r.alerts }
func (r *repeatWatchdog) Reset()                            { r.resets++ }

// The headline property. Pre-#1090 this alert set watchdogTripped and
// every later turn was refused at preflight; now it stops the turn it
// arrived in and the session stays available.
func TestDrainWatchdogAlerts_TurnScopedCriticalCutsTheTurnNotTheSession(t *testing.T) {
	t.Parallel()

	w := &fakeWatchdog{pending: []watchdog.Alert{
		turnScopedAlert("3 tool calls in a row (mark_task_done) reported that they changed nothing."),
	}}
	var trips []attach.GuardrailTrip
	a := &Agent{watchdog: w, watchdogEnforce: true}
	a.SetOperatorEventEmitter(func(kind string, payload any) {
		if kind != attach.EventGuardrailTrip {
			return
		}
		trips = append(trips, payload.(attach.GuardrailTrip))
	})

	if !a.drainWatchdogAlerts(true) {
		t.Fatal("a turn-scoped Critical must still stop the turn in flight; the drain reported nothing to cut")
	}
	if tripped, reason := a.WatchdogTripped(); tripped {
		t.Fatalf("a turn-scoped Critical must not halt the session; got tripped=true reason=%q", reason)
	}
	if err := a.preflightWatchdog(); err != nil {
		t.Fatalf("the next turn must be allowed to start after a turn-scoped cut; preflight refused it: %v", err)
	}

	// A halt is written durably (#643) so it survives a pod roll. A cut
	// turn has nothing to restore — re-arming it in the next process
	// would refuse a session this one never refused.
	a.mu.Lock()
	queued := len(a.pendingOutOfBandEvents)
	a.mu.Unlock()
	if queued != 0 {
		t.Errorf("a turn-scoped cut queued %d durable guardrail rows, want 0", queued)
	}

	if len(trips) != 1 {
		t.Fatalf("guardrail-trip events = %d, want 1: %+v", len(trips), trips)
	}
	trip := trips[0]
	if trip.Guardrail != attach.GuardrailWatchdog || !trip.HaltedTurn {
		t.Errorf("trip = %+v, want the watchdog guardrail with halted_turn=true", trip)
	}
	// The operator reads this line to find out how much stopped. It has
	// to say the session did not, and it must not offer the reset the
	// halt message offers, because there is nothing to reset.
	if !strings.Contains(trip.Reason, "NOT halted") {
		t.Errorf("trip reason does not tell the operator the session survived: %q", trip.Reason)
	}
	if !strings.Contains(trip.Reason, "mark_task_done") {
		t.Errorf("trip reason dropped the signal's own explanation: %q", trip.Reason)
	}
}

// The escalation, which is what makes the narrower scope safe: a driver
// that re-drives into the same loop forever must still be stopped.
func TestMaybeTripWatchdog_ConsecutiveTurnCutsHaltTheSession(t *testing.T) {
	t.Parallel()

	w := &repeatWatchdog{alerts: []watchdog.Alert{
		turnScopedAlert("3 tool calls in a row (mark_task_done) reported that they changed nothing."),
	}}
	a := &Agent{watchdog: w, watchdogEnforce: true}

	for turn := 1; turn <= maxConsecutiveWatchdogTurnCuts; turn++ {
		a.observeTurnStartForWatchdog()
		if !a.drainWatchdogAlerts(true) {
			t.Fatalf("turn %d: the drain cut nothing", turn)
		}
		tripped, reason := a.WatchdogTripped()
		want := turn == maxConsecutiveWatchdogTurnCuts
		if tripped != want {
			t.Fatalf("after turn %d of %d: session halted = %v, want %v (reason %q)",
				turn, maxConsecutiveWatchdogTurnCuts, tripped, want, reason)
		}
	}

	_, reason := a.WatchdogTripped()
	if !strings.Contains(reason, "turns in a row") {
		t.Errorf("the escalation halt must say it is about a pattern across turns, not a single one; got %q", reason)
	}
	if !strings.Contains(reason, "/guardrail reset") {
		t.Errorf("a session halt must name the operator affordance that clears it; got %q", reason)
	}

	// Each cut scrubbed the signals' evidence. Without that the signals
	// that latch "one alert per streak" — NoOpStreakSignal does — would
	// never alert again after the first cut, the streak above would be
	// stuck at one, and the loop would run unbounded.
	if w.resets != maxConsecutiveWatchdogTurnCuts-1 {
		t.Errorf("watchdog Reset called %d times over %d cuts, want %d (every cut but the escalation, which the operator's reset clears)",
			w.resets, maxConsecutiveWatchdogTurnCuts, maxConsecutiveWatchdogTurnCuts-1)
	}
}

// One clean turn clears the count. An agent that trips occasionally over
// a long unattended run must never accumulate its way into a halt.
func TestMaybeTripWatchdog_ATurnThatEndsCleanClearsTheCutStreak(t *testing.T) {
	t.Parallel()

	alert := turnScopedAlert("3 tool calls in a row (record_plan) reported that they changed nothing.")
	w := &repeatWatchdog{alerts: []watchdog.Alert{alert}}
	a := &Agent{watchdog: w, watchdogEnforce: true}

	// Two cuts, then a turn with nothing to report, then two more cuts.
	// Five turns, four of them cut, and no halt — because the clean turn
	// in the middle means there is no run of three.
	for _, cut := range []bool{true, true, false, true, true} {
		a.observeTurnStartForWatchdog()
		if cut {
			w.alerts = []watchdog.Alert{alert}
		} else {
			w.alerts = nil
		}
		a.drainWatchdogAlerts(false)
		if tripped, reason := a.WatchdogTripped(); tripped {
			t.Fatalf("a run of at most two cuts halted the session: %q", reason)
		}
	}
}

// A session-scoped Critical in the same drain wins. Otherwise a signal
// that asked for the smaller consequence could downgrade one that asked
// for the larger, just by being first in the slice.
func TestMaybeTripWatchdog_SessionScopedCriticalWinsOverTurnScoped(t *testing.T) {
	t.Parallel()

	a := &Agent{watchdogEnforce: true}
	a.SetOperatorEventEmitter(func(string, any) {})
	a.maybeTripWatchdog([]watchdog.Alert{
		turnScopedAlert("3 inert calls."),
		{Signal: "repeated-tool-call", Severity: watchdog.SeverityCritical, Reason: "looping on read_file 5x."},
	}, true)

	tripped, reason := a.WatchdogTripped()
	if !tripped {
		t.Fatal("a session-scoped Critical must halt the session even when a turn-scoped one came first")
	}
	if !strings.Contains(reason, "repeated-tool-call") {
		t.Errorf("the halt was attributed to the wrong alert: %q", reason)
	}
}

// One cut per turn, however many events the cut turn has left to
// unwind through. A second count would drive the escalation to a halt
// on a single turn's behaviour.
func TestMaybeTripWatchdog_OneCutPerTurn(t *testing.T) {
	t.Parallel()

	w := &repeatWatchdog{alerts: []watchdog.Alert{turnScopedAlert("3 inert calls.")}}
	a := &Agent{watchdog: w, watchdogEnforce: true}
	a.observeTurnStartForWatchdog()

	for i := 0; i < maxConsecutiveWatchdogTurnCuts+2; i++ {
		a.drainWatchdogAlerts(true)
	}
	if tripped, reason := a.WatchdogTripped(); tripped {
		t.Fatalf("repeated drains inside ONE turn halted the session: %q", reason)
	}
	a.mu.Lock()
	streak := a.watchdogTurnCutStreak
	a.mu.Unlock()
	if streak != 1 {
		t.Errorf("cut streak after one turn = %d, want 1", streak)
	}
}

// enforceWatchdogInTurn must act on what the drain decided, not on the
// session flag: a turn-scoped cut leaves that flag clear, and reading it
// back would leave the looping turn running.
func TestEnforceWatchdogInTurn_CutsOnATurnScopedCritical(t *testing.T) {
	t.Parallel()

	w := &fakeWatchdog{pending: []watchdog.Alert{turnScopedAlert("3 inert calls.")}}
	a := &Agent{watchdog: w, watchdogEnforce: true}
	a.SetOperatorEventEmitter(func(string, any) {})

	a.enforceWatchdogInTurn()

	a.mu.Lock()
	got := a.guardrailHaltKind
	a.mu.Unlock()
	if got != attach.TurnErrorWatchdog {
		t.Fatalf("the cut turn is labelled %q, want %q — an unlabelled cut is reported as a bare operator cancel",
			got, attach.TurnErrorWatchdog)
	}
}

// The reset clears the streak too. Leaving it at the threshold would
// re-halt the session on the very next cut, one turn after an operator
// looked at it and said carry on.
func TestResetWatchdog_ClearsTheTurnCutStreak(t *testing.T) {
	t.Parallel()

	a := &Agent{watchdog: &fakeWatchdog{}, watchdogEnforce: true, watchdogTurnCut: true, watchdogTurnCutStreak: maxConsecutiveWatchdogTurnCuts}
	a.ResetWatchdog()

	a.mu.Lock()
	cut, streak := a.watchdogTurnCut, a.watchdogTurnCutStreak
	a.mu.Unlock()
	if cut || streak != 0 {
		t.Errorf("after ResetWatchdog: turn-cut latch = %v, streak = %d; want false, 0", cut, streak)
	}
}

// The acceptance shape, end to end through Run: a turn cut by a
// turn-scoped Critical is followed by a turn that RUNS. This is the
// property #1068, #1074, #1081 and #1086 were each aimed at — a daemon
// working through a refusal without an operator reset — expressed at the
// one layer that decides it.
func TestRun_TurnScopedCritical_NextTurnStillRuns(t *testing.T) {
	t.Parallel()

	w := &fakeWatchdog{pending: []watchdog.Alert{
		turnScopedAlert("3 tool calls in a row (mark_task_done) reported that they changed nothing."),
	}}
	a, err := New(oneShotLLM{},
		WithSession("u-wd-turn", "s-wd-turn"),
		WithWatchdog(w, nil),
		WithWatchdogEnforce(),
	)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	ctx := context.Background()

	for _, err := range a.Run(ctx, "record the denial") {
		_ = err // the cut surfaces as a cancellation; not the subject
	}
	if tripped, reason := a.WatchdogTripped(); tripped {
		t.Fatalf("the no-op streak halted the session: %q", reason)
	}

	for _, err := range a.Run(ctx, "now do the next thing") {
		if err != nil {
			t.Fatalf("the turn after a turn-scoped cut was refused: %v", err)
		}
	}
}
