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
	"slices"
	"sync"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/go-steer/core-agent/v2/pkg/attach"
	"github.com/go-steer/core-agent/v2/pkg/usage"
	"github.com/go-steer/core-agent/v2/pkg/watchdog"
)

// terminalRecorder collects the frames an agent emits. Wired through
// SetOperatorEventEmitter, the same seam the attach broadcaster uses, so
// what it sees is exactly what a subscriber would.
//
// Terminal and non-terminal frames go into SEPARATE slices, which is the
// point of the type since #891: the contract these tests exist to guard
// is "exactly one terminal frame per turn", and that assertion is only
// worth anything if a `guardrail-trip` cannot accidentally satisfy it.
type terminalRecorder struct {
	mu    sync.Mutex
	kind  []string // "turn-complete", or "turn-error:<kind>"
	trips []attach.GuardrailTrip
	all   []string // every frame above, interleaved, in emission order
}

func (r *terminalRecorder) attachTo(a *Agent) {
	a.SetOperatorEventEmitter(func(eventType string, payload any) {
		r.mu.Lock()
		defer r.mu.Unlock()
		switch eventType {
		case attach.EventTurnComplete:
			r.kind = append(r.kind, "turn-complete")
			r.all = append(r.all, "turn-complete")
		case attach.EventTurnError:
			te, _ := payload.(attach.TurnError)
			r.kind = append(r.kind, "turn-error:"+te.Kind)
			r.all = append(r.all, "turn-error:"+te.Kind)
		case attach.EventGuardrailTrip:
			gt, _ := payload.(attach.GuardrailTrip)
			r.trips = append(r.trips, gt)
			r.all = append(r.all, "guardrail-trip:"+gt.Guardrail)
		}
	})
}

func (r *terminalRecorder) frames() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.kind...)
}

// order returns terminal and non-terminal frames interleaved. A trip has
// to REACH the client before the frame it explains, or an operator reads
// a bare `canceled` and only learns why afterwards.
func (r *terminalRecorder) order() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.all...)
}

// guardrailTrips returns the non-terminal trips, in order.
func (r *terminalRecorder) guardrailTrips() []attach.GuardrailTrip {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]attach.GuardrailTrip(nil), r.trips...)
}

// assertOneTrip checks that exactly one guardrail-trip went out, naming
// the expected guardrail and shape, and that it carries a reason — an
// empty one would technically satisfy the protocol while losing the only
// thing the event exists to deliver.
func assertOneTrip(t *testing.T, rec *terminalRecorder, guardrail string, haltedTurn bool) {
	t.Helper()
	trips := rec.guardrailTrips()
	if len(trips) != 1 {
		t.Fatalf("got %d guardrail-trip events %+v, want exactly 1", len(trips), trips)
	}
	if trips[0].Guardrail != guardrail {
		t.Errorf("trip names guardrail %q, want %q", trips[0].Guardrail, guardrail)
	}
	if trips[0].HaltedTurn != haltedTurn {
		t.Errorf("halted_turn = %v, want %v — a client reads this to know whether a "+
			"canceled turn-error or a turn-complete follows", trips[0].HaltedTurn, haltedTurn)
	}
	if trips[0].Reason == "" {
		t.Error("trip carries no reason; the event exists to tell the operator why the agent is about to refuse everything")
	}
}

// TestRun_InTurnGuardrailHalt_EmitsExactlyOneTerminalFrame is the #818
// part-1 regression, run over both in-turn guardrails, re-pinned to the
// shape #891 replaced it with.
//
// A guardrail that trips mid-turn reports the trip and then calls
// Interrupt. Before #818 the cancellation that followed was classified by
// Run's cleanup like any other, so the turn produced a SECOND terminal
// frame — a contentless `canceled` — breaking the one-terminal-frame-per-
// turn contract and stacking a redundant warning block under the one that
// actually explains the halt. #818 fixed that by suppressing the
// `canceled`; #891 fixes it the other way round, which is the right way
// round: the trip is not a turn outcome, so it leaves the terminal slot
// entirely and rides a non-terminal `guardrail-trip`. The turn then
// reports what actually happened to it — it was canceled — and the count
// is still one.
//
// So the invariant under test is unchanged and the frame carrying it is
// not. Both halves are asserted, because dropping either reintroduces a
// defect: two terminal frames (#818), or a cut turn whose operator never
// learns why (the whole point of the event).
//
// Drives the real Run loop with a runaway tool-call loop, because the
// bug is in how two independently-correct paths compose, not in either
// one's decision logic.
func TestRun_InTurnGuardrailHalt_EmitsExactlyOneTerminalFrame(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		options       func() []Option
		wantGuardrail string // on the trip
		wantKind      string // on the metric point
	}{
		{
			// $10/MTok × 1000 tokens = $0.01 per model call against a
			// $0.05 per-turn ceiling: the trip lands mid-loop.
			name: "cost ceiling",
			options: func() []Option {
				tr := usage.NewTracker()
				return []Option{
					WithUsageTracker(tr),
					WithCostCeiling(CostCeiling{MaxTurnUSD: 0.05}),
				}
			},
			wantGuardrail: attach.GuardrailCostCeiling,
			wantKind:      attach.TurnErrorCostCeiling,
		},
		{
			// Critical on the first observed tool call, so the halt
			// lands inside the turn rather than at its boundary.
			name: "watchdog enforce",
			options: func() []Option {
				w := &fakeWatchdog{pending: []watchdog.Alert{{
					Signal:   "repeated-tool-call",
					Severity: watchdog.SeverityCritical,
					Reason:   "looping on todo.",
				}}}
				return []Option{WithWatchdog(w, nil), WithWatchdogEnforce()}
			},
			wantGuardrail: attach.GuardrailWatchdog,
			wantKind:      attach.TurnErrorWatchdog,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reader := sdkmetric.NewManualReader()
			mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
			t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

			llm := &burnLoopLLM{perCallIn: 1000}
			opts := append(tc.options(), WithSession("u-818", "s-818"), WithMeterProvider(mp))
			a, err := New(llm, opts...)
			if err != nil {
				t.Fatalf("agent.New: %v", err)
			}
			var rec terminalRecorder
			rec.attachTo(a)

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			t.Cleanup(cancel)

			// Harness-shaped tap so the cost path sees spend accrue
			// per model call, exactly as pkg/runner commits it.
			pricing := usage.Pricing{InputPerMTok: 10}
			var tap usage.TurnTap
			for ev, err := range a.Run(ctx, "go") {
				_ = err // the halt surfaces as a cancellation
				tap.Observe(ev)
				if u, ok := tap.Commit(ev); ok && a.tracker != nil {
					a.tracker.AppendUsage(llm.Name(), u, pricing)
				}
			}
			if ctx.Err() != nil {
				t.Fatal("the loop ran to the deadline: the guardrail never halted it")
			}

			got := rec.frames()
			if len(got) != 1 {
				t.Fatalf("turn emitted %d terminal frames %v, want exactly 1.\n"+
					"A trip is not a turn outcome and must not occupy the terminal "+
					"slot; the cut turn's own `canceled` is the only frame (#818, #891).", len(got), got)
			}
			if want := "turn-error:" + attach.TurnErrorCanceled; got[0] != want {
				t.Errorf("terminal frame = %q, want %q — the turn was cut, and since #891 "+
					"that is what it says", got[0], want)
			}

			// The reason did not vanish with the turn-error that used to
			// carry it: it precedes the cut on its own event, flagged as
			// having taken the turn down with it.
			assertOneTrip(t, &rec, tc.wantGuardrail, true)
			wantOrder := []string{"guardrail-trip:" + tc.wantGuardrail, "turn-error:" + attach.TurnErrorCanceled}
			if order := rec.order(); !slices.Equal(order, wantOrder) {
				t.Errorf("frame order = %v, want %v — the explanation has to arrive before "+
					"the bare cancellation it explains", order, wantOrder)
			}

			// The metric has to agree with the frame. The turn error is
			// a bare context.Canceled, so classifying it would label the
			// halt `canceled` and leave the two series that exist for
			// runaway incidents dark during one.
			var rm metricdata.ResourceMetrics
			if err := reader.Collect(context.Background(), &rm); err != nil {
				t.Fatalf("collect: %v", err)
			}
			pts := invocationPoints(t, rm)
			if len(pts) != 1 {
				t.Fatalf("got %d invocation points, want 1", len(pts))
			}
			if et, _ := invAttr(pts[0].Attributes, AttrErrorType); et != tc.wantKind {
				t.Errorf("%s = %q, want %q — the cancel is how the halt was carried out, "+
					"not what happened to the turn", AttrErrorType, et, tc.wantKind)
			}
		})
	}
}

// TestRun_OperatorInterrupt_StillReportsCanceled guards the other side
// of the suppression: only a guardrail's own cancellation is swallowed.
// An operator /interrupt (or a shutdown) cuts the turn through the same
// context, and there is no other frame explaining it, so `canceled` must
// still be the turn's terminal frame.
func TestRun_OperatorInterrupt_StillReportsCanceled(t *testing.T) {
	t.Parallel()
	llm := &burnLoopLLM{perCallIn: 1000}
	a, err := New(llm, WithSession("u-818-int", "s-818-int"))
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	var rec terminalRecorder
	rec.attachTo(a)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	for ev := range a.Run(ctx, "go") {
		if ev != nil {
			a.Interrupt()
		}
	}
	if ctx.Err() != nil {
		t.Fatal("the loop ran to the deadline: Interrupt never cut the turn")
	}

	got := rec.frames()
	if len(got) != 1 || got[0] != "turn-error:"+attach.TurnErrorCanceled {
		t.Errorf("terminal frames = %v, want exactly [turn-error:%s]", got, attach.TurnErrorCanceled)
	}
}

// TestRun_PostTurnGuardrailTrip_KeepsTurnComplete covers the case #818
// part 1 deliberately left alone and #891 finished: a guardrail that
// trips at the turn BOUNDARY reports the trip from the post-turn hook,
// and the turn — which finished, and produced an answer — reports
// turn-complete.
//
// Pre-#891 this was two TERMINAL frames (turn-error:watchdog, then
// turn-complete), which #818 left standing because neither was
// contentless and dropping either lost something real: the reason the
// agent is about to start refusing turns, or the completion a consumer
// is blocked on. The protocol violation was in the modelling, not in
// either frame — nothing had happened to this turn, so calling the trip
// a turn-error was the error. Now the trip is non-terminal and the count
// is one, with no information given up on either side.
//
// halted_turn is false here and true in the in-turn test above, and that
// is the whole reason the field exists: same event, and only this flag
// tells a consumer whether to expect a `canceled` next or a clean finish.
func TestRun_PostTurnGuardrailTrip_KeepsTurnComplete(t *testing.T) {
	t.Parallel()
	w := &fakeWatchdog{pending: []watchdog.Alert{{
		Signal:   "repeated-tool-call",
		Severity: watchdog.SeverityCritical,
		Reason:   "looping on read_file 5x.",
	}}}
	a, err := New(oneShotLLM{},
		WithSession("u-818-post", "s-818-post"),
		WithWatchdog(w, nil),
		WithWatchdogEnforce(),
	)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	var rec terminalRecorder
	rec.attachTo(a)

	for _, err := range a.Run(context.Background(), "hi") {
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	}

	if got, want := rec.frames(), []string{"turn-complete"}; !slices.Equal(got, want) {
		t.Errorf("terminal frames = %v, want %v — the turn finished, and a guardrail "+
			"tripping at its boundary does not change that", got, want)
	}
	assertOneTrip(t, &rec, attach.GuardrailWatchdog, false)

	wantOrder := []string{"guardrail-trip:" + attach.GuardrailWatchdog, "turn-complete"}
	if got := rec.order(); !slices.Equal(got, wantOrder) {
		t.Errorf("frame order = %v, want %v", got, wantOrder)
	}
}
