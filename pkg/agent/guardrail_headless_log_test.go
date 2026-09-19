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
	"bytes"
	"context"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-steer/core-agent/v2/pkg/attach"
	"github.com/go-steer/core-agent/v2/pkg/usage"
	"github.com/go-steer/core-agent/v2/pkg/watchdog"
)

// #1131. Four things cancel a turn in flight and only one of them said
// so. The other three reported the cut through a.emit, which is a no-op
// until an emitter is registered, and the only non-test registration
// site in the tree is the attach adapter — so on a run without
// --attach-listen the report went nowhere and the operator's ENTIRE
// output was the `anthropic: stream: context canceled` the Interrupt
// produces two layers down. That reads as a provider failure.
//
// The shape that most needs the line is the one that had none: an
// unattended run defaults to watchdog=enforce and a session cost ceiling
// (cmd/core-agent/main.go), so it is simultaneously the likeliest to
// trip a guardrail and the likeliest to have nobody subscribed.
//
// These tests therefore run the real Run loop with NO emitter wired,
// which is the whole point — assertHeadlessAgent below fails the test if
// one sneaks in, because a log assertion that only holds when somebody
// was already listening guards nothing.

// lockedBuffer is a concurrency-safe log sink. The agent logs from the
// event tap and from cleanup goroutines, so a bare bytes.Buffer here is
// a -race failure waiting for a slow machine.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureAgentLog redirects the global logger for the duration of the
// test and returns a reader for what was written.
//
// Callers must NOT be parallel. log.SetOutput is process-global: a
// parallel neighbour's line would land in this buffer, and worse, this
// test's restore would fire while that neighbour is still writing. Go
// resumes top-level parallel tests only after the sequential pass
// finishes, so staying sequential is the whole of the protection.
func captureAgentLog(t *testing.T) *lockedBuffer {
	t.Helper()
	var sink lockedBuffer
	old := log.Writer()
	log.SetOutput(&sink)
	t.Cleanup(func() { log.SetOutput(old) })
	return &sink
}

// assertHeadlessAgent fails unless this agent is the shape under test:
// no operator-event emitter, so every a.emit call is a no-op.
//
// Without this the suite would still pass if someone wired a recorder
// into one of the rigs below, and it would be testing the case that was
// never broken.
func assertHeadlessAgent(t *testing.T, a *Agent) {
	t.Helper()
	a.emitMu.Lock()
	cb := a.operatorEmit
	a.emitMu.Unlock()
	if cb != nil {
		t.Fatal("an operator-event emitter is wired, so this is not a headless run — " +
			"the defect under test is that emit() goes nowhere when nobody is listening")
	}
}

// guardrailCutLines returns the log lines announcing a turn cut.
func guardrailCutLines(sink *lockedBuffer) []string {
	var out []string
	for _, line := range strings.Split(sink.String(), "\n") {
		if strings.Contains(line, "guardrail cut the turn in flight") {
			out = append(out, line)
		}
	}
	return out
}

// TestGuardrailCutIsLoggedOnAHeadlessRun drives each of the three arms
// that cut a turn in flight and pins the line an unattended operator
// gets. The fourth, cutTurnForContextBudget, has logged since #975 and
// is what these three were measured against.
//
// Each case asserts three separate things, and they fail for different
// reasons:
//
//   - exactly one line, not none and not one per event in the tail of a
//     cancelled stream;
//   - it names the guardrail, using the same token the metric's
//     error.type carries, so an operator can grep one and find the other;
//   - it carries the arm's own reason text, which is the part that says
//     WHY and the part a generic "a guardrail stopped the turn" line
//     would lose.
func TestGuardrailCutIsLoggedOnAHeadlessRun(t *testing.T) {
	// Deliberately not parallel: captures the global logger.

	tests := []struct {
		name string
		// drive runs one turn on a headless agent, to whatever end.
		drive func(t *testing.T)
		// label is the guardrail token the line must name.
		label string
		// reason is a distinctive fragment of the arm's own text.
		reason string
	}{
		{
			// $10/MTok x 1000 tokens = $0.01 per model call against a
			// $0.05 per-turn ceiling: the trip lands mid-loop.
			name:   "cost ceiling",
			label:  attach.TurnErrorCostCeiling,
			reason: "per-turn cost ceiling exceeded",
			drive: func(t *testing.T) {
				llm := &burnLoopLLM{perCallIn: 1000}
				a, err := New(llm,
					WithSession("u-1131", "s-1131-cost"),
					WithUsageTracker(usage.NewTracker()),
					WithCostCeiling(CostCeiling{MaxTurnUSD: 0.05}),
				)
				if err != nil {
					t.Fatalf("agent.New: %v", err)
				}
				assertHeadlessAgent(t, a)

				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				// Harness-shaped tap so spend accrues per model call,
				// exactly as pkg/runner commits it.
				pricing := usage.Pricing{InputPerMTok: 10}
				var tap usage.TurnTap
				for ev := range a.Run(ctx, "go") {
					tap.Observe(ev)
					if u, ok := tap.Commit(ev); ok && a.tracker != nil {
						a.tracker.AppendUsage(llm.Name(), u, pricing)
					}
				}
				if ctx.Err() != nil {
					t.Fatal("the loop ran to the deadline: the ceiling never cut it")
				}
			},
		},
		{
			// Critical on the first observed tool call, so the halt lands
			// inside the turn rather than at its boundary.
			name:   "watchdog enforce",
			label:  attach.TurnErrorWatchdog,
			reason: "looping on todo.",
			drive: func(t *testing.T) {
				w := &fakeWatchdog{pending: []watchdog.Alert{{
					Signal:   "repeated-tool-call",
					Severity: watchdog.SeverityCritical,
					Reason:   "looping on todo.",
				}}}
				a, err := New(&burnLoopLLM{perCallIn: 1000},
					WithSession("u-1131", "s-1131-wd"),
					WithWatchdog(w, nil),
					WithWatchdogEnforce(),
				)
				if err != nil {
					t.Fatalf("agent.New: %v", err)
				}
				assertHeadlessAgent(t, a)

				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				for range a.Run(ctx, "go") {
				}
				if ctx.Err() != nil {
					t.Fatal("the loop ran to the deadline: the watchdog never cut it")
				}
			},
		},
		{
			// The arm with no event at all (refusal_storm.go withholds
			// one on purpose), so this line is the only live report of it
			// that exists — #1132's stderr emitter would not surface it
			// either.
			name:   "refusal storm",
			label:  attach.TurnErrorRefusalStorm,
			reason: "no operator reset is needed",
			drive: func(t *testing.T) {
				h, cleanup := openTestEventLog(t)
				defer cleanup()
				a, _, _ := refusalStormRig(t, "s-1131-storm", WithEventLog(h))
				assertHeadlessAgent(t, a)
				drainTurn(t, a, "delete prod")
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Also not parallel, for captureAgentLog's reason.
			sink := captureAgentLog(t)
			tc.drive(t)

			lines := guardrailCutLines(sink)
			if len(lines) != 1 {
				t.Fatalf("got %d cut lines, want exactly 1.\nAll output:\n%s\n\n"+
					"None means an unattended operator sees only the `context canceled` "+
					"this cut produces downstream (#1131). More than one means the arm is "+
					"logging per event while the cancelled stream drains.", len(lines), sink.String())
			}
			got := lines[0]
			// Anchored on the sentence, not a bare Contains. The
			// watchdog's own reason text says "watchdog halted the
			// agent", so a substring search for its label is partly
			// scored by the string the OTHER assertion is about — the
			// check would stay green with the name dropped from the
			// format entirely.
			if want := "agent: " + tc.label + " guardrail cut the turn in flight"; !strings.Contains(got, want) {
				t.Errorf("cut line does not open with %q:\n%s\n\n"+
					"The name has to be the token error.type carries on "+
					"gen_ai.agent.invocation.duration, or the log and the metric "+
					"describe the same halt in two vocabularies", want, got)
			}
			if !strings.Contains(got, tc.reason) {
				t.Errorf("cut line does not carry the arm's own reason %q:\n%s\n\n"+
					"Naming the guardrail says which one; the reason is the half that "+
					"says why, and it is what the operator acts on", tc.reason, got)
			}
			if !strings.Contains(got, "not a provider failure") {
				t.Errorf("cut line does not disclaim the cancellation that follows:\n%s\n\n"+
					"The operator reads `stream: context canceled` first and this line "+
					"second; without the disclaimer they are two unrelated-looking events", got)
			}
		})
	}
}

// TestOperatorInterruptIsNotLoggedAsAGuardrailCut is the other side of
// the line, and the reason it is worth a test of its own: the fix would
// be just as broken if it logged a guardrail cut for EVERY cancellation.
// An operator pressing stop is not a guardrail, and a run whose log
// blames one for every interrupt is a run whose log cannot be used to
// find the real ones.
//
// Same mechanism, same Interrupt, no guardrail configured.
func TestOperatorInterruptIsNotLoggedAsAGuardrailCut(t *testing.T) {
	// Deliberately not parallel: captures the global logger.
	sink := captureAgentLog(t)

	a, err := New(&burnLoopLLM{perCallIn: 1000}, WithSession("u-1131", "s-1131-int"))
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	assertHeadlessAgent(t, a)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for ev := range a.Run(ctx, "go") {
		if ev != nil {
			a.Interrupt()
		}
	}
	if ctx.Err() != nil {
		t.Fatal("the loop ran to the deadline: Interrupt never cut the turn")
	}

	if lines := guardrailCutLines(sink); len(lines) != 0 {
		t.Errorf("an operator interrupt logged %d guardrail cut lines %v; want none — "+
			"nothing tripped, and a log that says otherwise sends the operator "+
			"looking for a guardrail that did not fire", len(lines), lines)
	}
}

// TestGuardrailTripAtTheTurnBoundaryIsLoggedWithoutClaimingACut covers
// the branch the two above do not: a guardrail that trips as the turn
// FINISHES halts the session without cutting anything.
//
// It is logged, because "the agent will refuse every turn from now on"
// is at least as invisible headless as a cut and at least as worth
// knowing. It must not say the turn was cut, because the turn completed
// and produced an answer — the same halted_turn distinction the wire
// event carries (#891), held on the other channel.
func TestGuardrailTripAtTheTurnBoundaryIsLoggedWithoutClaimingACut(t *testing.T) {
	// Deliberately not parallel: captures the global logger.
	sink := captureAgentLog(t)

	w := &fakeWatchdog{pending: []watchdog.Alert{{
		Signal:   "repeated-tool-call",
		Severity: watchdog.SeverityCritical,
		Reason:   "looping on read_file 5x.",
	}}}
	a, err := New(oneShotLLM{},
		WithSession("u-1131", "s-1131-post"),
		WithWatchdog(w, nil),
		WithWatchdogEnforce(),
	)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	assertHeadlessAgent(t, a)

	for _, err := range a.Run(context.Background(), "hi") {
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	}

	out := sink.String()
	if !strings.Contains(out, "watchdog guardrail tripped:") {
		t.Errorf("a session-halting trip at the turn boundary was not logged:\n%s\n\n"+
			"Headless, this is the only notice an operator gets that the agent has "+
			"stopped accepting turns", out)
	}
	if !strings.Contains(out, "looping on read_file 5x.") {
		t.Errorf("the trip line does not carry the watchdog's reason:\n%s", out)
	}
	if lines := guardrailCutLines(sink); len(lines) != 0 {
		t.Errorf("a boundary trip logged %d cut lines %v; want none — the turn "+
			"completed and answered, and telling the operator it was cut sends them "+
			"looking for a truncated answer that exists", len(lines), lines)
	}
}
