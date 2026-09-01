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
	"sync"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/go-steer/core-agent/v2/pkg/attach"
	"github.com/go-steer/core-agent/v2/pkg/usage"
	"github.com/go-steer/core-agent/v2/pkg/watchdog"
)

// terminalRecorder collects the terminal frames an agent emits. Wired
// through SetOperatorEventEmitter, the same seam the attach broadcaster
// uses, so what it sees is exactly what a subscriber would.
type terminalRecorder struct {
	mu   sync.Mutex
	kind []string // "" for turn-complete, the TurnError.Kind otherwise
}

func (r *terminalRecorder) attachTo(a *Agent) {
	a.SetOperatorEventEmitter(func(eventType string, payload any) {
		r.mu.Lock()
		defer r.mu.Unlock()
		switch eventType {
		case attach.EventTurnComplete:
			r.kind = append(r.kind, "turn-complete")
		case attach.EventTurnError:
			te, _ := payload.(attach.TurnError)
			r.kind = append(r.kind, "turn-error:"+te.Kind)
		}
	})
}

func (r *terminalRecorder) frames() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.kind...)
}

// TestRun_InTurnGuardrailHalt_EmitsExactlyOneTerminalFrame is the #818
// part-1 regression, run over both in-turn guardrails.
//
// A guardrail that trips mid-turn emits its own turn-error carrying the
// reason and then calls Interrupt. Before this fix the cancellation that
// followed was classified by Run's cleanup like any other, so the turn
// produced a SECOND terminal frame — a contentless `canceled` — breaking
// the one-terminal-frame-per-turn contract and stacking a redundant
// warning block under the one that actually explains the halt.
//
// Drives the real Run loop with a runaway tool-call loop, because the
// bug is in how two independently-correct paths compose, not in either
// one's decision logic.
func TestRun_InTurnGuardrailHalt_EmitsExactlyOneTerminalFrame(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		options  func() []Option
		wantKind string
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
			wantKind: attach.TurnErrorCostCeiling,
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
			wantKind: attach.TurnErrorWatchdog,
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
					"The guardrail already reported this turn's outcome; the cancellation "+
					"it caused must not add a contentless second frame (#818).", len(got), got)
			}
			if want := "turn-error:" + tc.wantKind; got[0] != want {
				t.Errorf("terminal frame = %q, want %q — the surviving frame must be the "+
					"one carrying the operator-facing reason", got[0], want)
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

// TestRun_PostTurnGuardrailTrip_KeepsTurnComplete pins the shape #818
// part 1 deliberately leaves alone: a guardrail that trips at the turn
// BOUNDARY emits its turn-error from the post-turn hook, and the turn —
// which finished, and produced an answer — still reports turn-complete.
//
// Two frames, but not the same defect: neither is contentless and
// dropping either loses something (the reason the agent will start
// refusing turns, or the completion a consumer is waiting on). Modelling
// a trip as a non-terminal notification is the protocol fix; until then
// this is the documented shape, and this test exists so a future change
// to it is a deliberate one.
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

	got := rec.frames()
	want := []string{"turn-error:" + attach.TurnErrorWatchdog, "turn-complete"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("terminal frames = %v, want %v (the boundary-trip shape is unchanged by #818)", got, want)
	}
}
