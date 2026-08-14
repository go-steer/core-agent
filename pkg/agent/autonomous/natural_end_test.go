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

package autonomous

import (
	"context"
	"sync/atomic"
	"testing"

	"google.golang.org/adk/tool"

	"github.com/go-steer/core-agent/v2/pkg/agent"
)

// WithStopOnNaturalEnd ends the run the first time a turn finishes
// without the model asking for another tool (#730).
//
// Without it, agent.Run's own terminating loop is restarted by the
// driver with "continue" until a budget fires — correct for a standing
// worker, ruinous for a delegation with a deliverable, which then runs
// past its own answer and overwrites it.
func TestRunAutonomous_StopOnNaturalEndStopsAtTheFirstTextOnlyTurn(t *testing.T) {
	t.Parallel()
	llm := &stubLLM{scenarios: []scenarioFn{
		textTurn("the answer", 5, 5),
		textTurn("standing by", 5, 5),
		textTurn("still standing by", 5, 5),
	}}
	res, err := Run(context.Background(),
		buildAgent(llm, "natural-end"),
		"go",
		WithStopOnNaturalEnd(),
		WithMaxTurns(3))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Reason != StopReasonCompleted {
		t.Errorf("Reason = %q, want %q", res.Reason, StopReasonCompleted)
	}
	if res.Turns != 1 {
		t.Errorf("Turns = %d, want 1", res.Turns)
	}
	if res.DoneDetail != "the answer" {
		t.Errorf("DoneDetail = %q, want the turn's text — it is the deliverable", res.DoneDetail)
	}
	if got := atomic.LoadInt32(&llm.calls); got != 1 {
		t.Errorf("model calls = %d, want 1 (no re-drive)", got)
	}
}

// The stop condition reads the LAST model response of the turn, not
// "did any tool call happen during it". Using a tool is what a
// delegation does; a turn that calls one and then answers is finished,
// and must end the run.
//
// This is the case the issue's own sketch (count functionCall parts
// across the turn) gets backwards: it would keep re-driving any
// delegation that did real work, which is every delegation worth
// spawning.
func TestRunAutonomous_StopOnNaturalEndAfterAToolCall(t *testing.T) {
	t.Parallel()
	var toolCalls atomic.Int32
	llm := &stubLLM{scenarios: []scenarioFn{
		spinCallTurn(1),               // still inside turn 1
		textTurn("the answer", 5, 5),  // ...which then answers
		textTurn("standing by", 5, 5), // must never be reached
	}}
	res, err := Run(context.Background(),
		buildSpinAgent(llm, "natural-end-tools", &toolCalls),
		"go",
		WithStopOnNaturalEnd(),
		WithMaxTurns(4))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Reason != StopReasonCompleted {
		t.Errorf("Reason = %q, want %q", res.Reason, StopReasonCompleted)
	}
	if res.Turns != 1 {
		t.Errorf("Turns = %d, want 1 — the tool call and the answer are one turn", res.Turns)
	}
	if res.DoneDetail != "the answer" {
		t.Errorf("DoneDetail = %q, want %q", res.DoneDetail, "the answer")
	}
	if got := toolCalls.Load(); got != 1 {
		t.Errorf("tool calls = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&llm.calls); got != 2 {
		t.Errorf("model calls = %d, want 2 (the call and the answer); more means the run was re-driven", got)
	}
}

// One termination path: with WithStopOnNaturalEnd there is no done or
// return tool to register, so nothing for the model to choose between
// and nothing it can forget to call.
func TestRunAutonomous_StopOnNaturalEndRegistersNoDoneTool(t *testing.T) {
	t.Parallel()
	var extras []tool.Tool
	llm := &stubLLM{scenarios: []scenarioFn{textTurn("done", 1, 1)}}
	build := func(in []tool.Tool) (*agent.Agent, error) {
		extras = in
		return buildAgent(llm, "no-done-tool")(in)
	}
	if _, err := Run(context.Background(), build, "go",
		WithStopOnNaturalEnd(),
		// Set alongside the return tool on purpose: the two are
		// alternatives, and natural-end wins rather than silently
		// wiring both ways out.
		WithReturnTool(ReturnToolConfig{Aliases: []string{"report_done"}}),
		WithMaxTurns(1)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, tl := range extras {
		t.Errorf("driver injected %q; a bounded run has no return tool", tl.Name())
	}
}

// An explicit schedule_next_turn outranks the natural end. The two can
// only meet when a caller forces bounded mode onto a scheduled agent,
// but when they do, "the model asked to be woken again" beats "the
// model didn't ask for another tool" — otherwise the schedule tool
// silently stops working, since calling it and then answering leaves
// the turn looking exactly like an ending.
func TestRunAutonomous_ScheduleOutranksNaturalEnd(t *testing.T) {
	t.Parallel()
	llm := &stubLLM{scenarios: []scenarioFn{
		scheduleCallTurn(1, "rescan", "cadence"),
		textTurn("sleeping until the rescan", 1, 1),
		// Turn 2, after the wake: nothing left to do.
		textTurn("all clear", 1, 1),
	}}
	sched := &recordingScheduler{}
	res, err := Run(context.Background(),
		buildAgent(llm, "schedule-vs-natural"),
		"monitor",
		WithStopOnNaturalEnd(),
		WithScheduler(sched),
		WithMaxTurns(5))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := len(sched.Calls()); got != 1 {
		t.Fatalf("Scheduler.BeforeNextTurn calls = %d, want 1 — the deferral was swallowed", got)
	}
	if res.Turns != 2 {
		t.Errorf("Turns = %d, want 2 (the deferral ran, then the next turn ended it)", res.Turns)
	}
	if res.Reason != StopReasonCompleted {
		t.Errorf("Reason = %q, want %q", res.Reason, StopReasonCompleted)
	}
	if res.DoneDetail != "all clear" {
		t.Errorf("DoneDetail = %q, want the post-wake turn's text", res.DoneDetail)
	}
}

// The same rule on the resume path. Run and Resume have drifted apart
// before, and a resumed delegation that never terminates is the
// original bug with an extra step.
func TestResumeAutonomous_StopsOnNaturalEnd(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestEventLog(t)
	defer cleanup()

	llm1 := &stubLLM{scenarios: []scenarioFn{
		textTurn("partial", 1, 1),
		textTurn("more partial", 1, 1),
	}}
	first, err := Run(context.Background(),
		runBuilder(llm1, h, "app", "u", "natural"),
		"go", WithMaxTurns(2))
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if first.Reason != StopReasonMaxTurns {
		t.Fatalf("first Reason = %q, want %q", first.Reason, StopReasonMaxTurns)
	}

	llm2 := &stubLLM{scenarios: []scenarioFn{
		textTurn("the answer", 1, 1),
		textTurn("standing by", 1, 1),
	}}
	res, err := Resume(context.Background(),
		resumeBuilder(llm2, h, "app", "u"),
		SessionRef{Handle: h, AppName: "app", UserID: "u", SessionID: "natural"},
		WithStopOnNaturalEnd(),
		WithMaxTurns(10))
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if res.Reason != StopReasonCompleted {
		t.Errorf("Reason = %q, want %q", res.Reason, StopReasonCompleted)
	}
	if res.DoneDetail != "the answer" {
		t.Errorf("DoneDetail = %q, want the resumed turn's text", res.DoneDetail)
	}
	if res.Turns != 3 {
		t.Errorf("Turns = %d, want 3 (2 carried forward + 1 here)", res.Turns)
	}
}
