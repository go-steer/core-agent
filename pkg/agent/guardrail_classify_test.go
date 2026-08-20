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

// firstTurnError returns the payload of the first turn-error the
// emitter recorded, or fails the test.
func firstTurnError(t *testing.T, events []attach.TurnError) attach.TurnError {
	t.Helper()
	if len(events) == 0 {
		t.Fatalf("no turn-error was emitted")
	}
	return events[0]
}

// captureTurnErrors wires an emitter that keeps every turn-error
// payload, in order.
func captureTurnErrors(a *Agent) *[]attach.TurnError {
	var got []attach.TurnError
	a.SetOperatorEventEmitter(func(kind string, payload any) {
		if kind != attach.EventTurnError {
			return
		}
		if te, ok := payload.(attach.TurnError); ok {
			got = append(got, te)
		}
	})
	return &got
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
	emitted := captureTurnErrors(a)

	a.snapshotTurnStartCost()
	tr.Append("test", 1_500_000, 0, usage.Pricing{InputPerMTok: 0.10})
	a.maybeEnforceCostCeiling()

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
	if trip := firstTurnError(t, *emitted); trip != got {
		t.Errorf("trip frame and refusal classification disagree:\n trip = %+v\n refusal = %+v", trip, got)
	}
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
	emitted := captureTurnErrors(a)

	a.drainWatchdogAlerts()

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
	if trip := firstTurnError(t, *emitted); trip != got {
		t.Errorf("trip frame and refusal classification disagree:\n trip = %+v\n refusal = %+v", trip, got)
	}
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
	a.drainWatchdogAlerts()

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
