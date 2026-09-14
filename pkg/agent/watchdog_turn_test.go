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
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/watchdog"
)

// Turn-boundary wiring (#655). NoNewStateSignal scopes "already seen"
// to one turn, and the only thing that makes that true is Run telling
// the watchdog a turn began. A hook nothing calls is the #642 finding
// again — machinery that is coherent, tested and inert as deployed — so
// these tests are about the call site, not the signal.

// turnAwareWatchdog is fakeWatchdog plus the optional TurnObserver half.
type turnAwareWatchdog struct {
	fakeWatchdog
	turns int
}

func (w *turnAwareWatchdog) ObserveTurnStart() { w.turns++ }

// The wiring itself. Fails on pre-#655 code, where Run had no turn
// boundary at all and the count stays 0.
func TestRun_TellsTheWatchdogEachTurnBegan(t *testing.T) {
	t.Parallel()

	w := &turnAwareWatchdog{}
	a, err := New(oneShotLLM{},
		WithSession("u-wdt", "s-wdt"),
		WithWatchdog(w, nil),
	)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	ctx := context.Background()

	for i, prompt := range []string{"first", "second", "third"} {
		for _, err := range a.Run(ctx, prompt) {
			if err != nil {
				t.Fatalf("turn %d: %v", i+1, err)
			}
		}
	}
	if w.turns != 3 {
		t.Fatalf("ObserveTurnStart called %d times over 3 turns, want 3", w.turns)
	}
}

// A refused turn is not a boundary. It never ran, so nothing in it could
// have made progress — and counting it would hand an auto-continue
// re-drive a way to launder a stall one refusal at a time, clearing the
// evidence for free on every turn the guardrail rejects.
func TestRun_RefusedTurnIsNotABoundary(t *testing.T) {
	t.Parallel()

	w := &turnAwareWatchdog{fakeWatchdog: fakeWatchdog{pending: []watchdog.Alert{
		{Signal: "repeated-tool-call", Severity: watchdog.SeverityCritical, Reason: "looping on read_file 5x."},
	}}}
	a, err := New(oneShotLLM{},
		WithSession("u-wdt2", "s-wdt2"),
		WithWatchdog(w, nil),
		WithWatchdogEnforce(),
	)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	ctx := context.Background()

	// Turn 1 runs and its post-turn drain trips enforce.
	for _, err := range a.Run(ctx, "hi") {
		if err != nil {
			t.Fatalf("turn 1: %v", err)
		}
	}
	if tripped, _ := a.WatchdogTripped(); !tripped {
		t.Fatal("expected the turn-1 drain to trip enforce")
	}

	// Turn 2 is refused at preflight, before any boundary.
	var gotErr error
	for _, err := range a.Run(ctx, "again") {
		if err != nil {
			gotErr = err
		}
	}
	if !IsWatchdogTripped(gotErr) {
		t.Fatalf("turn 2 should have been refused; got %v", gotErr)
	}
	if w.turns != 1 {
		t.Fatalf("ObserveTurnStart called %d times, want 1 — a refused turn is not a boundary", w.turns)
	}
}

// A watchdog that predates the optional interface — every third-party
// one, and the fake next door — must be left alone rather than crashed
// into. This is the same contract ToolResultObserver already has.
func TestObserveTurnStartForWatchdog_SkipsWatchdogsWithoutTheHook(t *testing.T) {
	t.Parallel()

	a := &Agent{watchdog: &fakeWatchdog{}}
	a.observeTurnStartForWatchdog() // must not panic

	// And a nil watchdog is the unconfigured default.
	b := &Agent{}
	b.observeTurnStartForWatchdog()
}
