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
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/usage"
	"github.com/go-steer/core-agent/v2/pkg/watchdog"
)

// A turn-scoped cut does not promise a turn that may never come (#1140).
//
// Both guardrails cut a turn and leave the session alone, and both said
// so as "the next turn starts clean" — present tense, about a turn
// nothing in this package will ever start. In a `-p` one-shot there is
// no next turn: the process exits 1, and the line that was supposed to
// explain the stop told the operator the run was fine. That operator is
// the one most likely to be reading, because a one-shot has no client
// attached and the log line is the whole report.
//
// These are asserted against the reasons the guardrails actually emit,
// over both of them together, because the half of the sentence that is
// identical is the half that was wrong in both.

// cutReasons returns the turn-cut reason each guardrail emits, by
// tripping it for real. Nothing here asserts on the trip itself — the
// trips have their own tests; this is about the words.
func cutReasons(t *testing.T) map[string]string {
	t.Helper()

	// Per-turn cost ceiling: $0.15 spent against a $0.10 bound.
	tr := usage.NewTracker()
	ceil := &Agent{tracker: tr, costCeiling: CostCeiling{MaxTurnUSD: 0.10}}
	var ceilRec terminalRecorder
	ceilRec.attachTo(ceil)
	ceil.snapshotTurnStartCost()
	tr.Append("test", 1_500_000, 0, usage.Pricing{InputPerMTok: 0.10})
	if !ceil.maybeEnforceCostCeiling(true) {
		t.Fatalf("cost ceiling did not trip")
	}

	// Watchdog: one turn-scoped Critical.
	dog := &Agent{
		watchdog: &fakeWatchdog{pending: []watchdog.Alert{
			turnScopedAlert("3 tool calls in a row (mark_task_done) reported that they changed nothing."),
		}},
		watchdogEnforce: true,
	}
	var dogRec terminalRecorder
	dogRec.attachTo(dog)
	if !dog.drainWatchdogAlerts(true) {
		t.Fatalf("watchdog did not cut the turn")
	}

	ceilTrips, dogTrips := ceilRec.guardrailTrips(), dogRec.guardrailTrips()
	if len(ceilTrips) != 1 || len(dogTrips) != 1 {
		t.Fatalf("want one trip from each guardrail, got %d cost-ceiling and %d watchdog", len(ceilTrips), len(dogTrips))
	}
	return map[string]string{
		"cost_ceiling": ceilTrips[0].Reason,
		"watchdog":     dogTrips[0].Reason,
	}
}

// The headline property: neither message asserts that another turn
// happens, and both name the one-shot case explicitly. Naming it is the
// part that has to be checked positively — a message could drop the
// false promise and still leave a `-p` reader with "the session is NOT
// halted" and no idea the run is over.
func TestTurnCutReasonDoesNotPromiseANextTurn(t *testing.T) {
	t.Parallel()

	for guardrail, reason := range cutReasons(t) {
		t.Run(guardrail, func(t *testing.T) {
			// The unconditional promise, in either guardrail's phrasing.
			for _, promise := range []string{
				"the next turn starts",
				"the next turn runs",
				"the next turn begins",
			} {
				if strings.Contains(strings.ToLower(reason), promise) {
					t.Errorf("reason promises a turn nothing will start (%q): %q", promise, reason)
				}
			}
			// And the reader with no next turn is told so.
			if !strings.Contains(reason, "one-shot") {
				t.Errorf("reason does not tell a one-shot run that it ends here: %q", reason)
			}
		})
	}
}

// What the rewording must not cost. The cut line answers "how much
// stopped", and the two facts it carried — the session survived, and the
// streak is climbing towards one that will not — are why an operator
// reads it at all.
func TestTurnCutReasonStillReportsScopeAndStreak(t *testing.T) {
	t.Parallel()

	for guardrail, reason := range cutReasons(t) {
		t.Run(guardrail, func(t *testing.T) {
			if !strings.Contains(reason, "NOT halted") {
				t.Errorf("reason no longer says the session survived: %q", reason)
			}
			if !strings.Contains(reason, "1 in a row now") {
				t.Errorf("reason dropped the streak count: %q", reason)
			}
			if !strings.Contains(reason, "at 3 the session halts") {
				t.Errorf("reason dropped the escalation bound: %q", reason)
			}
		})
	}
}

// The two guardrails say the shared half identically. They are mirror
// images by intent and drifted here before; a const only holds that if
// something reads it back off the emitted strings.
func TestBothGuardrailsShareTheNoNextTurnClause(t *testing.T) {
	t.Parallel()

	for guardrail, reason := range cutReasons(t) {
		if !strings.Contains(reason, turnCutNoNextTurn) {
			t.Errorf("%s cut reason does not carry the shared clause verbatim:\n want: %q\n got:  %q", guardrail, turnCutNoNextTurn, reason)
		}
	}
}

// FOUR things cut a turn in flight, not two, and the review of the first
// pass caught the other two still promising a next turn — one of them on
// the same logGuardrailCut line (#1140).
//
// The two above report through `guardrail-trip`; these two are log-only,
// which makes them the worse instance rather than the lesser one. The
// refusal-storm arm withholds its event deliberately (see
// refusal_storm.go), so on an unattended run its log line is the ONLY
// record that exists — it was telling a process about to exit 1 that the
// next turn starts with an empty refusal map. The context-budget cut
// went further and promised a remedy: "compaction will run first on the
// next turn", which a one-shot never runs.
//
// Reading the log rather than a return value is the point: the log line
// is the artifact under test, and for the refusal storm it is the whole
// surface.
func TestEveryInTurnCutTellsAOneShotRunItEndsHere(t *testing.T) {
	// Deliberately not parallel: captures the global logger.

	for _, tc := range []struct {
		name   string
		needle string // identifies this arm's line in the log
		drive  func(t *testing.T)
	}{
		{
			name:   "refusal storm",
			needle: "no operator reset is needed",
			drive: func(t *testing.T) {
				a, _, _ := refusalStormRig(t, "s-1140-storm")
				drainTurn(t, a, "delete prod")
			},
		},
		{
			name:   "context budget",
			needle: "cut the turn before building a request the provider would reject",
			drive: func(t *testing.T) {
				a := &Agent{tracker: usage.NewTracker()}
				a.cutTurnForContextBudget(190_000, 200_000)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sink := captureAgentLog(t)
			tc.drive(t)

			var line string
			for _, l := range strings.Split(sink.String(), "\n") {
				if strings.Contains(l, tc.needle) {
					line = l
					break
				}
			}
			if line == "" {
				t.Fatalf("no cut line in the log; looked for %q in:\n%s", tc.needle, sink.String())
			}
			for _, promise := range []string{
				"the next turn starts",
				"the next turn runs",
				"on the next turn",
			} {
				if strings.Contains(strings.ToLower(line), promise) {
					t.Errorf("cut line promises a turn nothing will start (%q): %q", promise, line)
				}
			}
			if !strings.Contains(line, turnCutNoNextTurn) {
				t.Errorf("cut line does not carry the shared no-next-turn clause: %q", line)
			}
		})
	}
}
