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
	"time"

	"google.golang.org/adk/v2/session"

	"github.com/go-steer/core-agent/v3/pkg/attach/wire"
	"github.com/go-steer/core-agent/v3/pkg/usage"
)

// transposePostTurn returns a copy of steps with the two named steps swapped,
// mirroring transpose() in preturn_test.go (#659, #1389).
func transposePostTurn(t *testing.T, steps []postTurnStep, a, b string) []postTurnStep {
	t.Helper()
	out := append([]postTurnStep(nil), steps...)
	i, j := postTurnStepIndex(t, out, a), postTurnStepIndex(t, out, b)
	out[i], out[j] = out[j], out[i]
	return out
}

func postTurnStepIndex(t *testing.T, steps []postTurnStep, name string) int {
	t.Helper()
	for i, s := range steps {
		if s.name == name {
			return i
		}
	}
	t.Fatalf("no post-turn step named %q — it was renamed or removed, and this test is no longer testing anything", name)
	return -1
}

func TestPostTurnSteps_EveryStepIsNamedUniquelyAndSaysWhatItsPositionBuys(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for i, s := range postTurnSteps {
		if s.name == "" {
			t.Errorf("step %d has no name; the permutation tests address steps by name", i)
		}
		if seen[s.name] {
			t.Errorf("duplicate step name %q — transposePostTurn() cannot address it unambiguously", s.name)
		}
		seen[s.name] = true
		if strings.TrimSpace(s.enforces) == "" {
			t.Errorf("step %q does not say what its position enforces", s.name)
		}
		if s.run == nil {
			t.Errorf("step %q has no run func", s.name)
		}
	}
}

// TestPostTurn_1258_ConsumeGuardrailHaltMustPrecedeRecordTurnError pins that
// consume-guardrail-halt populates tc.guardrailHalt before record-turn-error
// builds the durable turn-error row. Transposed, cut_by is empty on the row
// and a refusal-storm or watchdog cut is indistinguishable from an operator
// cancel in the persisted transcript.
func TestPostTurn_1258_ConsumeGuardrailHaltMustPrecedeRecordTurnError(t *testing.T) {
	t.Parallel()

	setup := func(t *testing.T) (*Agent, *turnCleanup) {
		t.Helper()
		h, cleanup := openTestEventLog(t)
		t.Cleanup(cleanup)
		createTestSession(t, h, DefaultAppName, "u", "s-post-1258")
		a, err := New(&recordingLLM{}, WithEventLog(h), WithSession("u", "s-post-1258"))
		if err != nil {
			t.Fatalf("agent.New: %v", err)
		}
		a.markGuardrailHalt(wire.TurnErrorRefusalStorm)
		gen := a.setCancelInFlight(func() {})
		tc := &turnCleanup{
			cancelGen: gen,
			promptID:  "p-1258",
			started:   time.Now(),
			turnErr:   context.Canceled,
		}
		return a, tc
	}

	readCutBy := func(t *testing.T, a *Agent) string {
		t.Helper()
		resp, err := a.eventLog.Service.Get(context.Background(), &session.GetRequest{
			AppName:   a.appName,
			UserID:    a.userID,
			SessionID: a.sessionID,
		})
		if err != nil {
			t.Fatalf("Service.Get: %v", err)
		}
		for ev := range resp.Session.Events().All() {
			if ev.Author == wire.TurnErrorEventAuthor {
				v, _ := ev.CustomMetadata["cut_by"].(string)
				return v
			}
		}
		t.Fatal("no turn-error row found in session")
		return ""
	}

	t.Run("in order: cut_by records the guardrail that cut the turn", func(t *testing.T) {
		a, tc := setup(t)
		a.runPostTurn(tc, postTurnSteps)
		if got := readCutBy(t, a); got != wire.TurnErrorRefusalStorm {
			t.Errorf("cut_by = %q, want %q", got, wire.TurnErrorRefusalStorm)
		}
	})

	t.Run("transposed: cut_by is lost on the durable turn-error row", func(t *testing.T) {
		a, tc := setup(t)
		steps := transposePostTurn(t, postTurnSteps, "consume-guardrail-halt", "record-turn-error")
		a.runPostTurn(tc, steps)
		if got := readCutBy(t, a); got != "" {
			t.Fatalf("cut_by = %q under transposed pipeline, want empty — this subtest proves the one above can fail", got)
		}
	})
}

// TestPostTurn_1258_DrainOutOfBandEventsMustPrecedeEmitTerminalEvent pins the
// terminal barrier invariant: when emit-terminal-event fires on the SSE stream,
// the durable turn-error row (and any guardrail-trip row) must already be
// committed to the eventlog so a client reading the log on receipt of the
// terminal frame sees the row it names.
func TestPostTurn_1258_DrainOutOfBandEventsMustPrecedeEmitTerminalEvent(t *testing.T) {
	t.Parallel()

	runWith := func(t *testing.T, steps []postTurnStep) (rowsAtTerminalEmit int) {
		t.Helper()
		h, cleanup := openTestEventLog(t)
		t.Cleanup(cleanup)
		createTestSession(t, h, DefaultAppName, "u", "s-post-barrier")
		a, err := New(&recordingLLM{}, WithEventLog(h), WithSession("u", "s-post-barrier"))
		if err != nil {
			t.Fatalf("agent.New: %v", err)
		}
		gen := a.setCancelInFlight(func() {})
		a.SetOperatorEventEmitter(func(eventType string, _ any) {
			if eventType == wire.EventTurnError {
				rowsAtTerminalEmit = countGuardrailRows(t, a, wire.TurnErrorEventAuthor)
			}
		})
		tc := &turnCleanup{
			cancelGen: gen,
			promptID:  "p-barrier",
			started:   time.Now(),
			turnErr:   context.Canceled,
		}
		a.runPostTurn(tc, steps)
		return rowsAtTerminalEmit
	}

	t.Run("in order: durable row is committed before turn-error SSE frame fires", func(t *testing.T) {
		if got := runWith(t, postTurnSteps); got != 1 {
			t.Errorf("turn-error rows committed at SSE emit = %d, want 1", got)
		}
	})

	t.Run("transposed: turn-error SSE frame fires before durable row is committed", func(t *testing.T) {
		steps := transposePostTurn(t, postTurnSteps, "drain-out-of-band-events", "emit-terminal-event")
		if got := runWith(t, steps); got != 0 {
			t.Fatalf("turn-error rows committed at SSE emit = %d, want 0 — this subtest proves the one above can fail", got)
		}
	})
}

// TestPostTurn_1360_ClearCancelInFlightMustPrecedeEmitTerminalEvent pins that
// cancelInFlight is cleared before onTurnEnd and the terminal status-update
// idle frame are emitted, so an external caller reacting to idle (e.g. Compact
// or Checkpoint) is not refused as "turn in flight".
func TestPostTurn_1360_ClearCancelInFlightMustPrecedeEmitTerminalEvent(t *testing.T) {
	t.Parallel()

	runWith := func(t *testing.T, steps []postTurnStep) (inFlightAtIdle bool) {
		t.Helper()
		a := preTurnAgent(t)
		gen := a.setCancelInFlight(func() {})
		a.SetOperatorEventEmitter(func(eventType string, _ any) {
			if eventType == wire.EventStatusUpdate {
				inFlightAtIdle = a.turnInFlight()
			}
		})
		tc := &turnCleanup{
			cancelGen: gen,
			promptID:  "p-idle",
			started:   time.Now(),
		}
		a.runPostTurn(tc, steps)
		return inFlightAtIdle
	}

	t.Run("in order: turnInFlight is false when status-update idle fires", func(t *testing.T) {
		if runWith(t, postTurnSteps) {
			t.Error("turnInFlight() = true during status-update idle emit, want false")
		}
	})

	t.Run("transposed: turnInFlight is still true when status-update idle fires", func(t *testing.T) {
		steps := transposePostTurn(t, postTurnSteps, "clear-cancel-in-flight", "emit-terminal-event")
		if !runWith(t, steps) {
			t.Fatal("turnInFlight() = false under transposed pipeline; this subtest proves the one above can fail")
		}
	})
}

// TestTurnOutcomeReducer_StateTransitions exercises the unified turn-outcome
// reducer across continue, cut, escalate, halt, and #1086 watchdog scrubbing.
func TestTurnOutcomeReducer_StateTransitions(t *testing.T) {
	t.Parallel()

	t.Run("watchdog turn cuts scrub signals and escalate on the third consecutive turn", func(t *testing.T) {
		w := &fakeWatchdog{}
		a := &Agent{watchdog: w, watchdogEnforce: true}

		for turn := 1; turn <= maxConsecutiveWatchdogTurnCuts; turn++ {
			a.observeTurnStartForWatchdog()
			out := a.applyTurnSignal(turnSignal{
				kind:           signalWatchdogTurn,
				inTurn:         true,
				watchdogSignal: "no-op-streak",
				rawReason:      "3 inert calls.",
			})
			if turn < maxConsecutiveWatchdogTurnCuts {
				if out.action != turnActionCut || !out.scrubWatchdog || out.haltedSession() {
					t.Fatalf("turn %d: outcome = %+v, want cut with scrubWatchdog=true and haltedSession=false", turn, out)
				}
				// Duplicate signal in the same turn is a no-op (continue).
				dup := a.applyTurnSignal(turnSignal{
					kind:           signalWatchdogTurn,
					inTurn:         true,
					watchdogSignal: "no-op-streak",
					rawReason:      "4 inert calls.",
				})
				if dup.action != turnActionContinue {
					t.Fatalf("turn %d duplicate signal: action = %q, want %q", turn, dup.action, turnActionContinue)
				}
			} else if out.action != turnActionEscalate || !out.haltedSession() {
				t.Fatalf("turn %d: outcome = %+v, want escalate with haltedSession=true", turn, out)
			}
		}
		if w.resets != maxConsecutiveWatchdogTurnCuts-1 {
			t.Errorf("watchdog resets = %d, want %d", w.resets, maxConsecutiveWatchdogTurnCuts-1)
		}
	})

	t.Run("refusal storm cuts turn, marks haltKind, and scrubs watchdog signals (#1086)", func(t *testing.T) {
		w := &fakeWatchdog{}
		a := &Agent{watchdog: w}
		out := a.applyTurnSignal(turnSignal{
			kind:    signalRefusalStorm,
			inTurn:  true,
			repeats: refusalStormThreshold,
		})
		if out.action != turnActionCut || out.haltKind != wire.TurnErrorRefusalStorm || !out.scrubWatchdog {
			t.Fatalf("refusal storm outcome = %+v, want cut + TurnErrorRefusalStorm + scrubWatchdog", out)
		}
		if w.resets != 1 {
			t.Errorf("watchdog resets = %d, want 1 (#1086 scrub)", w.resets)
		}
		if got := a.consumeGuardrailHalt(context.Canceled); got != wire.TurnErrorRefusalStorm {
			t.Errorf("consumeGuardrailHalt = %q, want %q", got, wire.TurnErrorRefusalStorm)
		}
	})

	t.Run("cost ceiling session signal halts immediately", func(t *testing.T) {
		tr := usage.NewTracker()
		a := &Agent{tracker: tr, costCeiling: CostCeiling{MaxSessionUSD: 1.0}}
		out := a.applyTurnSignal(turnSignal{
			kind:        signalCostCeilingSession,
			inTurn:      false,
			sessionCost: 1.25,
			ceilingUSD:  1.0,
		})
		if out.action != turnActionHalt || !out.haltedSession() {
			t.Fatalf("session ceiling outcome = %+v, want halt", out)
		}
		if tripped, _ := a.CostCeilingTripped(); !tripped {
			t.Error("CostCeilingTripped() = false after halt")
		}
	})
}
