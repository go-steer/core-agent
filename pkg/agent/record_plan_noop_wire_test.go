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

// #918: the seam between a tool that knows its call was inert and the
// detector that counts inert calls.
//
// Every other test at this bridge hand-writes the response map — the
// #905 replay a few files over builds `{"status": ..., "no_op": true}`
// by hand — so all of them passed while no shipped tool but
// mark_task_done ever produced that key. The gap was invisible from
// either side: pkg/tools tested an Outcome field, pkg/agent tested a
// literal, and nothing ran a real tool into the real bridge.
//
// So these two drive the actual record_plan tool through the actual
// functiontool marshalling into the actual watchdog. The JSON tag in
// pkg/tools cannot reference ToolResultNoOpKey (pkg/agent imports
// pkg/tools, so the dependency cannot run the other way); this file is
// where the two spellings are pinned to each other.

package agent

import (
	"context"
	"fmt"
	"testing"

	"google.golang.org/adk/tool"

	"github.com/go-steer/core-agent/v2/pkg/permissions"
	"github.com/go-steer/core-agent/v2/pkg/tools"
	"github.com/go-steer/core-agent/v2/pkg/watchdog"
)

// planTurnCtx is gateToolCtx with a caller-chosen invocation ID, which
// is the turn boundary record_plan's repeat guard keys off.
type planTurnCtx struct {
	*gateToolCtx
	invocation string
}

func (c *planTurnCtx) InvocationID() string { return c.invocation }

// runRecordPlan builds one long-lived record_plan tool — the deployed
// shape, since tools.Build runs once per process — and returns a
// closure that invokes it the way the ADK flow does, handing back the
// map that lands in FunctionResponse.Response.
func runRecordPlan(t *testing.T) func(invocation, plan string) map[string]any {
	t.Helper()
	gate := permissions.New(permissions.Options{Mode: permissions.ModeYolo, RequirePlanArtifact: true})
	tl, err := tools.RecordPlan(gate, t.TempDir())
	if err != nil {
		t.Fatalf("tools.RecordPlan: %v", err)
	}
	runner, ok := tl.(interface {
		Run(tool.Context, any) (map[string]any, error)
	})
	if !ok {
		t.Fatalf("%s is not runnable", tl.Name())
	}
	return func(invocation, plan string) map[string]any {
		t.Helper()
		ctx := &planTurnCtx{
			gateToolCtx: &gateToolCtx{Context: context.Background()},
			invocation:  invocation,
		}
		resp, err := runner.Run(ctx, map[string]any{"plan": plan})
		if err != nil {
			t.Fatalf("record_plan(turn %s, %q): %v", invocation, plan, err)
		}
		return resp
	}
}

// TestRecordPlanNoOpKeyMatchesReservedKey: the key record_plan marshals
// is the key toolResponseNoOp reads. Asserted against the response map
// rather than the struct tag so that a functiontool change to how
// results are converted would fail here too — the tag is only the
// contract if the marshaller honours it.
func TestRecordPlanNoOpKeyMatchesReservedKey(t *testing.T) {
	t.Parallel()
	run := runRecordPlan(t)
	const plan = "## Goal\nRoll the deployment back."

	recorded := run("turn-1", plan)
	if toolResponseNoOp(recorded) {
		t.Errorf("the call that wrote the artifact reported itself inert: %v", recorded)
	}
	if _, present := recorded[ToolResultNoOpKey]; present {
		t.Errorf("a productive call still carries %q: %v", ToolResultNoOpKey, recorded)
	}

	revised := run("turn-1", plan+"\n\nThen drain the node.")
	if toolResponseNoOp(revised) {
		t.Errorf("an in-place revision reported itself inert; a revising agent would "+
			"accumulate a streak toward a Critical halt: %v", revised)
	}

	repeat := run("turn-1", plan+"\n\nThen drain the node.")
	if _, present := repeat[ToolResultNoOpKey]; !present {
		t.Fatalf("re-sending an unchanged plan wrote nothing but did not set %q; "+
			"the tool and the detector are spelling the key differently: %v",
			ToolResultNoOpKey, repeat)
	}
	if !toolResponseNoOp(repeat) {
		t.Errorf("%q = %#v, which toolResponseNoOp does not read as a no-op",
			ToolResultNoOpKey, repeat[ToolResultNoOpKey])
	}
}

// TestRecordPlanRepeatsTripTheNoOpStreakEndToEnd is the acceptance
// criterion: the loop #906 stopped from minting files is now also
// *observable*, so the agent that keeps re-sending its plan gets halted
// rather than merely getting a politer message each time.
//
// The shape is the live 2.9.0-dev.4 session that motivated #906 — one
// plan, then the same plan again and again inside one turn — driven
// through the real bridge into a real DefaultWatchdog.
func TestRecordPlanRepeatsTripTheNoOpStreakEndToEnd(t *testing.T) {
	t.Parallel()
	run := runRecordPlan(t)
	w := watchdog.NewDefaultWatchdog()
	a := &Agent{watchdog: w}
	seen := map[string]struct{}{}
	const plan = "## Goal\nRoll the deployment back."

	// Gemini leaves FunctionResponse.ID empty (#367), which is the shape
	// the bridge has to handle; the sibling replay uses it for the same
	// reason.
	for range watchdog.DefaultNoOpStreak + 1 {
		a.observeToolResultsForWatchdog(
			wdResultEvent(wdResultPart("", "record_plan", run("turn-1", plan))), seen)
	}

	if alerts := noOpAlerts(t, w); alerts != 1 {
		t.Fatalf("%d record_plan calls (1 write + %d unchanged) raised %d no-op-streak alerts, want 1",
			watchdog.DefaultNoOpStreak+1, watchdog.DefaultNoOpStreak, alerts)
	}
}

// TestRecordPlanRepeatsTripAcrossTurnsToo pins the more surprising half,
// deliberately rather than by accident.
//
// An identical plan is `unchanged` in ANY later turn, not just the one
// that wrote it (`record_plan.go`: "True whether or not it is the same
// turn"), and signal state is not cleared at a turn boundary — nothing
// calls Watchdog.Reset but an operator's own reset. So an agent that
// re-sends its plan verbatim on three consecutive turns halts, even
// though no single turn looks like a loop.
//
// That is the intended reading: any productive tool result resets the
// run, so reaching three means the agent did nothing else at all in
// those turns, and re-filing a plan that is already on disk is not
// work. It is worth a test of its own because the streak crossing a
// turn boundary is the property most likely to be "fixed" by someone
// who assumes the detector is per-turn.
func TestRecordPlanRepeatsTripAcrossTurnsToo(t *testing.T) {
	t.Parallel()
	run := runRecordPlan(t)
	w := watchdog.NewDefaultWatchdog()
	a := &Agent{watchdog: w}
	const plan = "## Goal\nRoll the deployment back."

	for i := range watchdog.DefaultNoOpStreak + 1 {
		// A fresh dedup set per turn, which is what the run loop does:
		// the set is per-turn, the signal state is not.
		a.observeToolResultsForWatchdog(
			wdResultEvent(wdResultPart("", "record_plan",
				run(fmt.Sprintf("turn-%d", i+1), plan))),
			map[string]struct{}{})
	}

	if alerts := noOpAlerts(t, w); alerts != 1 {
		t.Fatalf("the same plan re-sent on %d consecutive turns raised %d no-op-streak alerts, want 1",
			watchdog.DefaultNoOpStreak+1, alerts)
	}
}

// noOpAlerts drains the watchdog and counts no-op-streak alerts,
// asserting each is Critical — a Warn here would not halt anything.
func noOpAlerts(t *testing.T, w *watchdog.DefaultWatchdog) int {
	t.Helper()
	var n int
	for _, al := range w.Check() {
		if al.Signal != "no-op-streak" {
			continue
		}
		n++
		if al.Severity != watchdog.SeverityCritical {
			t.Errorf("Severity = %v, want Critical", al.Severity)
		}
	}
	return n
}
