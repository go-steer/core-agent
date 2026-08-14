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

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/adk/tool"
	"google.golang.org/genai"

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

// One termination path: WithStopOnNaturalEnd on its own registers no
// done or return tool, so there is nothing for the model to choose
// between and nothing it can forget to call.
func TestRunAutonomous_StopOnNaturalEndAloneRegistersNoDoneTool(t *testing.T) {
	t.Parallel()
	var extras []tool.Tool
	llm := &stubLLM{scenarios: []scenarioFn{textTurn("done", 1, 1)}}
	build := func(in []tool.Tool) (*agent.Agent, error) {
		extras = in
		return buildAgent(llm, "no-done-tool")(in)
	}
	if _, err := Run(context.Background(), build, "go",
		WithStopOnNaturalEnd(),
		WithMaxTurns(1)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, tl := range extras {
		t.Errorf("driver injected %q; a bounded run that asked for no return tool has none", tl.Name())
	}
}

// ...but a caller that sets BOTH gets the return tool (#745). #730 read
// the pair as alternatives and left the tool unregistered, which is how
// the #728 alias net came to cover only the standing path while bounded
// — the default for declarative-subagent spawns — had no return gesture
// at all.
func TestRunAutonomous_StopOnNaturalEndKeepsAnExplicitReturnTool(t *testing.T) {
	t.Parallel()
	var extras []tool.Tool
	llm := &stubLLM{scenarios: []scenarioFn{textTurn("done", 1, 1)}}
	build := func(in []tool.Tool) (*agent.Agent, error) {
		extras = in
		return buildAgent(llm, "bounded-with-return")(in)
	}
	if _, err := Run(context.Background(), build, "go",
		WithStopOnNaturalEnd(),
		WithReturnTool(ReturnToolConfig{Aliases: []string{"report_done", "mark_task_done"}}),
		WithMaxTurns(1)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := make(map[string]bool, len(extras))
	for _, tl := range extras {
		got[tl.Name()] = true
	}
	for _, want := range []string{"return_result", "report_done", "mark_task_done"} {
		if !got[want] {
			t.Errorf("bounded run was not offered %q; injected %v", want, toolNames(extras))
		}
	}
}

// The loop half of the same fix: calling the return tool in a bounded
// run ends it with the curated result, rather than the natural end
// scraping whatever text happened to follow.
//
// Fails on pre-fix code: mark_task_done was never registered, so the
// call came back as tool-not-found — the exact frame the 2026-08-14 GKE
// UAT recorded — and the run limped to its natural end a turn later.
func TestRunAutonomous_BoundedReturnToolEndsTheRun(t *testing.T) {
	t.Parallel()
	llm := &stubLLM{scenarios: []scenarioFn{
		returnCallTurn("mark_task_done", "RCA: bad image tag on emailservice"),
		// Only reached if the return call failed to end the run.
		textTurn("standing by", 1, 1),
	}}
	res, err := Run(context.Background(), buildAgent(llm, "bounded-return-ends"), "go",
		WithStopOnNaturalEnd(),
		WithReturnTool(ReturnToolConfig{Aliases: []string{"report_done", "mark_task_done"}}),
		WithMaxTurns(5))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Reason != StopReasonCompleted {
		t.Errorf("Reason = %q, want %q", res.Reason, StopReasonCompleted)
	}
	if res.DoneDetail != "RCA: bad image tag on emailservice" {
		t.Errorf("DoneDetail = %q, want the returned result", res.DoneDetail)
	}
	if res.Turns != 1 {
		t.Errorf("Turns = %d, want 1 — the return call ended the run", res.Turns)
	}
}

// returnCallTurn yields a single response calling the named return tool
// (or one of its aliases) with a result payload.
func returnCallTurn(name, result string) scenarioFn {
	return func(_ context.Context, _ *adkmodel.LLMRequest) []stubResp {
		fc := &genai.FunctionCall{Name: name, Args: map[string]any{"result": result}}
		content := &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: fc}}}
		return []stubResp{
			{resp: &adkmodel.LLMResponse{
				Content:      content,
				TurnComplete: true,
				FinishReason: genai.FinishReasonStop,
			}},
		}
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
