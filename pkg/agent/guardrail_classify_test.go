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
	"fmt"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/attach"
	"github.com/go-steer/core-agent/v2/pkg/usage"
	"github.com/go-steer/core-agent/v2/pkg/watchdog"
)

// firstGuardrailTrip returns the payload of the first guardrail-trip
// the emitter recorded, or fails the test.
func firstGuardrailTrip(t *testing.T, events []attach.GuardrailTrip) attach.GuardrailTrip {
	t.Helper()
	if len(events) == 0 {
		t.Fatalf("no guardrail-trip was emitted")
	}
	return events[0]
}

// captureGuardrailTrips wires an emitter that keeps every
// guardrail-trip payload, in order.
func captureGuardrailTrips(a *Agent) *[]attach.GuardrailTrip {
	var got []attach.GuardrailTrip
	a.SetOperatorEventEmitter(func(kind string, payload any) {
		if kind != attach.EventGuardrailTrip {
			return
		}
		if gt, ok := payload.(attach.GuardrailTrip); ok {
			got = append(got, gt)
		}
	})
	return &got
}

// There is deliberately no captureTurnErrors alongside the above.
// The refusal these tests check against never reaches an emitter:
// preflightCostCeiling and preflightWatchdog return an iterator that
// yields the error straight to the caller, short-circuiting above the
// tree's only emit(EventTurnError) site, so a capture helper here
// would return an empty slice and read as a producer bug rather than
// as the wire fact it is (#891).

// assertRefusalMatchesTrip checks the anti-drift property both
// TestClassifyRefusal_* tests were written for: one construction site
// feeds the trip and the refusal, so every field the two share has to
// agree.
//
// The two now travel in different frames (#891 — the trip is a
// non-terminal guardrail-trip, the refusal is still a turn-error,
// because a refused turn's outcome is genuinely that it was refused),
// which makes this assertion MORE load-bearing rather than less: they
// no longer share a struct, so nothing but this test stops the two
// descriptions of one halt from wandering apart.
//
// Guardrail and Kind are asserted equal as strings, not merely
// corresponding. attach.GuardrailCostCeiling and
// attach.TurnErrorCostCeiling are separately-declared constants that
// happen to hold the same text, and an operator UI keying one off the
// other would break silently if either were renamed.
//
// Message is the one deliberate exception (#1040). A refusal is not a
// new detection, and a daemon log that repeats the halt verbatim on
// every refused turn reads as N separate runaways — thirteen of them,
// for one halt, on the run that found this. So the refusal leads with
// refusalPrefix and carries the trip's text behind it: still one
// source, still no drift, and now distinguishable. Asserting the
// relationship rather than equality is what keeps that from decaying
// into two independently-worded strings.
func assertRefusalMatchesTrip(t *testing.T, trip attach.GuardrailTrip, refusal attach.TurnError) {
	t.Helper()
	if want := refusalPrefix + trip.Reason; refusal.Message != want {
		t.Errorf("refusal message is not the trip's, prefixed:\n got  = %q\n want = %q", refusal.Message, want)
	}
	if trip.Guardrail != refusal.Kind {
		t.Errorf("trip names guardrail %q but the refusal classifies as kind %q — one halt, two names", trip.Guardrail, refusal.Kind)
	}
	if refusal.Retryable {
		t.Errorf("refusal is Retryable; a halt only an operator can clear must never invite a re-drive (full: %+v)", refusal)
	}
}

// TestClassifyRefusal_CostCeiling drives the real trip and the real
// refusal, then classifies the refusal the way pkg/agent's metrics
// path does.
//
// Fails on pre-#818 code: ClassifyTurnError is substring-based and the
// ceiling's reason prose matches none of its needles, so the refusal
// classified as `unknown` and gen_ai.agent.invocation.duration recorded
// `error.type: unknown` for exactly the turns a spend dashboard exists
// to show.
func TestClassifyRefusal_CostCeiling(t *testing.T) {
	t.Parallel()
	tr := usage.NewTracker()
	a := &Agent{tracker: tr, costCeiling: CostCeiling{MaxTurnUSD: 0.10}}
	emitted := captureGuardrailTrips(a)

	a.snapshotTurnStartCost()
	tr.Append("test", 1_500_000, 0, usage.Pricing{InputPerMTok: 0.10})
	a.maybeEnforceCostCeiling(false)

	err := a.preflightCostCeiling()
	if err == nil {
		t.Fatalf("a tripped ceiling must refuse the next turn")
	}
	got := attach.ClassifyTurnError(err)
	if got.Kind != attach.TurnErrorCostCeiling {
		t.Errorf("Kind = %q, want %q — a refused turn labelled anything else is invisible on a spend dashboard (full: %+v)",
			got.Kind, attach.TurnErrorCostCeiling, got)
	}
	if got.Retryable {
		t.Errorf("Retryable = true, want false — only an operator reset clears a ceiling (full: %+v)", got)
	}

	// The doc comments on AsTurnError claim the refusal and the trip
	// cannot drift apart. That is only true while one construction
	// site feeds both, so assert it rather than assert it in prose.
	assertRefusalMatchesTrip(t, firstGuardrailTrip(t, *emitted), got)
}

// TestClassifyRefusal_Watchdog is the watchdog half. Same pre-#818
// failure, and worse in kind: the reason embeds arbitrary trigger
// prose, so which classifier branch a runaway landed in depended on
// what it happened to be looping on.
func TestClassifyRefusal_Watchdog(t *testing.T) {
	t.Parallel()
	w := &fakeWatchdog{pending: []watchdog.Alert{
		{Signal: "repeated-tool-call", Severity: watchdog.SeverityCritical, Reason: "looping on read_file 5x."},
	}}
	a := &Agent{watchdog: w, watchdogEnforce: true}
	emitted := captureGuardrailTrips(a)

	a.drainWatchdogAlerts(false)

	err := a.preflightWatchdog()
	if err == nil {
		t.Fatalf("a tripped watchdog must refuse the next turn")
	}
	got := attach.ClassifyTurnError(err)
	if got.Kind != attach.TurnErrorWatchdog {
		t.Errorf("Kind = %q, want %q (full: %+v)", got.Kind, attach.TurnErrorWatchdog, got)
	}
	if got.Retryable {
		t.Errorf("Retryable = true, want false — re-driving a refused turn is what the watchdog exists to stop (full: %+v)", got)
	}
	assertRefusalMatchesTrip(t, firstGuardrailTrip(t, *emitted), got)
}

// TestClassifyRefusal_WatchdogReasonCarriesModelText is the sharp edge
// of the watchdog half. The repeated-tool-call alert formats the
// offending tool's name and the first 200 bytes of its JSON args into
// the reason, so the substring classifier was reading model-supplied
// text: pre-#818 this refusal came back `model_not_found`, and a tool
// named parse_* came back `config_error`. `unknown` was the common
// case, not the behaviour.
func TestClassifyRefusal_WatchdogReasonCarriesModelText(t *testing.T) {
	t.Parallel()
	w := &fakeWatchdog{pending: []watchdog.Alert{{
		Signal:   "repeated-tool-call",
		Severity: watchdog.SeverityCritical,
		Reason:   `looping on kubectl_get with identical args. Args: {"q":"pod not found"}`,
	}}}
	a := &Agent{watchdog: w, watchdogEnforce: true}
	a.drainWatchdogAlerts(false)

	err := a.preflightWatchdog()
	if err == nil {
		t.Fatalf("a tripped watchdog must refuse the next turn")
	}
	if got := attach.ClassifyTurnError(err); got.Kind != attach.TurnErrorWatchdog {
		t.Errorf("Kind = %q, want %q — the classifier is scanning text the model chose (full: %+v)",
			got.Kind, attach.TurnErrorWatchdog, got)
	}
}

// TestClassifyRefusal_SurvivesWrapping pins the errors.As half. A
// refusal that some layer has wrapped in context must still classify
// as itself — a direct type assertion would silently regress this the
// first time anyone added a %w.
func TestClassifyRefusal_SurvivesWrapping(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"ceiling", fmt.Errorf("agent: run turn: %w", &costCeilingError{reason: "per-turn cost ceiling exceeded"}), attach.TurnErrorCostCeiling},
		{"watchdog", fmt.Errorf("agent: run turn: %w", &watchdogError{reason: "watchdog halted the agent"}), attach.TurnErrorWatchdog},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := attach.ClassifyTurnError(tc.err); got.Kind != tc.want {
				t.Errorf("Kind = %q, want %q (full: %+v)", got.Kind, tc.want, got)
			}
		})
	}
}
