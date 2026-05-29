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
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/adk/tool"
	"google.golang.org/genai"

	coretools "github.com/go-steer/core-agent/pkg/tools"
)

func TestResumeAutonomous_FromDeferredCheckpoint(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestEventLog(t)
	defer cleanup()

	// Phase 1: run, hit a deferred exit.
	llm1 := &stubLLM{scenarios: []scenarioFn{
		scheduleCallTurn(1, "rescan", "10m cadence"),
		textTurn("scheduled", 1, 1),
	}}
	res1, err := RunAutonomous(context.Background(),
		runBuilder(llm1, h, "app", "u", "deferred-session"),
		"monitor",
		WithScheduler(coretools.ExitOnDeferScheduler()),
	)
	if err != nil {
		t.Fatalf("phase 1 RunAutonomous: %v", err)
	}
	if res1.Reason != StopReasonDeferred {
		t.Fatalf("phase 1 Reason = %q, want deferred", res1.Reason)
	}
	if res1.NextWakeAt.IsZero() {
		t.Fatalf("phase 1 NextWakeAt should be set")
	}

	// Phase 2: resume the same session. The deferred checkpoint's
	// next_wake_at is roughly now+1s. ResumeAutonomous honors it
	// inline (daemon-mode behavior) — for this test we wait it out.
	llm2 := &stubLLM{scenarios: []scenarioFn{
		doneCallTurn("resumed and finished"),
		textTurn("ok", 1, 1),
	}}
	res2, err := ResumeAutonomous(context.Background(),
		resumeBuilder(llm2, h, "app", "u"),
		SessionRef{Handle: h, AppName: "app", UserID: "u", SessionID: "deferred-session"},
	)
	if err != nil {
		t.Fatalf("phase 2 ResumeAutonomous: %v", err)
	}
	if res2.Reason != StopReasonCompleted {
		t.Errorf("phase 2 Reason = %q, want completed", res2.Reason)
	}
	if res2.DoneDetail != "resumed and finished" {
		t.Errorf("phase 2 DoneDetail = %q, want 'resumed and finished'", res2.DoneDetail)
	}
}

// scheduleCallTurn yields a single response calling schedule_next_turn.
// Mirrors doneCallTurn from autonomous_test.go but for the schedule
// tool. The runner executes the tool, then dispatches a follow-up LLM
// call which the caller must script too.
func scheduleCallTurn(wakeInSec int, nextPrompt, detail string) scenarioFn {
	return func(_ context.Context, _ *adkmodel.LLMRequest) []stubResp {
		args := map[string]any{
			"wake_in_sec": wakeInSec,
		}
		if nextPrompt != "" {
			args["next_prompt"] = nextPrompt
		}
		if detail != "" {
			args["detail"] = detail
		}
		fc := &genai.FunctionCall{Name: "schedule_next_turn", Args: args}
		content := &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: fc}}}
		return []stubResp{
			{resp: &adkmodel.LLMResponse{
				Content: content, TurnComplete: true, FinishReason: genai.FinishReasonStop,
			}},
		}
	}
}

// doneAndScheduleSameTurnFn yields BOTH function calls in one
// response, to test that report_done wins over schedule_next_turn.
func doneAndScheduleSameTurnFn(doneDetail string, scheduleSec int) scenarioFn {
	return func(_ context.Context, _ *adkmodel.LLMRequest) []stubResp {
		fcDone := &genai.FunctionCall{
			Name: "report_done",
			Args: map[string]any{"state": "done", "detail": doneDetail},
		}
		fcSched := &genai.FunctionCall{
			Name: "schedule_next_turn",
			Args: map[string]any{"wake_in_sec": scheduleSec},
		}
		content := &genai.Content{
			Role: genai.RoleModel,
			Parts: []*genai.Part{
				{FunctionCall: fcDone},
				{FunctionCall: fcSched},
			},
		}
		return []stubResp{
			{resp: &adkmodel.LLMResponse{
				Content: content, TurnComplete: true, FinishReason: genai.FinishReasonStop,
			}},
		}
	}
}

// recordingScheduler captures every BeforeNextTurn call so tests can
// assert the driver actually consulted the scheduler.
type recordingScheduler struct {
	mu     sync.Mutex
	calls  []coretools.ScheduleEvent
	action func(ev coretools.ScheduleEvent) error
}

func (r *recordingScheduler) BeforeNextTurn(_ context.Context, ev coretools.ScheduleEvent) error {
	r.mu.Lock()
	r.calls = append(r.calls, ev)
	action := r.action
	r.mu.Unlock()
	if action != nil {
		return action(ev)
	}
	return nil
}

func (r *recordingScheduler) Calls() []coretools.ScheduleEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]coretools.ScheduleEvent, len(r.calls))
	copy(out, r.calls)
	return out
}

func TestRunAutonomous_NoSchedulerIgnoresScheduleEmission(t *testing.T) {
	t.Parallel()
	// Without WithScheduler, the schedule_next_turn tool isn't even
	// registered. A model that hallucinates a call to it would get
	// "no such tool". We don't need to script that path; instead
	// just verify the autonomous loop runs normally when no scheduler
	// is wired.
	llm := &stubLLM{scenarios: []scenarioFn{
		doneCallTurn("just done"),
		textTurn("finished", 1, 1),
	}}
	res, err := RunAutonomous(context.Background(), buildAgent(llm, "no-scheduler"), "go")
	if err != nil {
		t.Fatalf("RunAutonomous: %v", err)
	}
	if res.Reason != StopReasonCompleted {
		t.Errorf("Reason = %q, want completed", res.Reason)
	}
}

func TestRunAutonomous_ScheduleEmissionContinuesLoop(t *testing.T) {
	t.Parallel()
	llm := &stubLLM{scenarios: []scenarioFn{
		// Turn 1: call schedule_next_turn(wake_in_sec=1, next_prompt="rescan").
		// wake_in_sec must be > 0 — zero is indistinguishable from
		// "not provided" in Go struct decoding.
		scheduleCallTurn(1, "rescan", "test cadence"),
		// Follow-up after the tool runs.
		textTurn("scheduled", 1, 1),
		// Turn 2: now done.
		doneCallTurn("finished after wake"),
		textTurn("ok", 1, 1),
	}}
	sched := &recordingScheduler{}
	res, err := RunAutonomous(context.Background(),
		buildAgent(llm, "schedule-continues"),
		"monitor",
		WithScheduler(sched),
	)
	if err != nil {
		t.Fatalf("RunAutonomous: %v", err)
	}
	if res.Reason != StopReasonCompleted {
		t.Errorf("Reason = %q, want completed", res.Reason)
	}
	if res.Turns != 2 {
		t.Errorf("Turns = %d, want 2", res.Turns)
	}
	calls := sched.Calls()
	if len(calls) != 1 {
		t.Fatalf("Scheduler.BeforeNextTurn calls = %d, want 1", len(calls))
	}
	if calls[0].NextPrompt != "rescan" {
		t.Errorf("scheduled NextPrompt = %q, want rescan", calls[0].NextPrompt)
	}
	if calls[0].Detail != "test cadence" {
		t.Errorf("scheduled Detail = %q, want 'test cadence'", calls[0].Detail)
	}
}

func TestRunAutonomous_ExitOnDeferSchedulerStops(t *testing.T) {
	t.Parallel()
	llm := &stubLLM{scenarios: []scenarioFn{
		// Turn 1: call schedule_next_turn(wake_in_sec=60).
		scheduleCallTurn(60, "rescan", "10m cadence"),
		textTurn("scheduled", 1, 1),
	}}
	res, err := RunAutonomous(context.Background(),
		buildAgent(llm, "exit-on-defer"),
		"monitor",
		WithScheduler(coretools.ExitOnDeferScheduler()),
	)
	if err != nil {
		t.Fatalf("RunAutonomous: %v", err)
	}
	if res.Reason != StopReasonDeferred {
		t.Errorf("Reason = %q, want %q", res.Reason, StopReasonDeferred)
	}
	if res.NextWakeAt.IsZero() {
		t.Errorf("NextWakeAt should be populated on deferred exit")
	}
	if delta := time.Until(res.NextWakeAt); delta < 50*time.Second || delta > 70*time.Second {
		t.Errorf("NextWakeAt delta = %v, want ~60s", delta)
	}
}

func TestRunAutonomous_DoneWinsOverScheduleInSameTurn(t *testing.T) {
	t.Parallel()
	llm := &stubLLM{scenarios: []scenarioFn{
		// Single turn emits BOTH function calls.
		doneAndScheduleSameTurnFn("done now", 60),
		textTurn("finished", 1, 1),
	}}
	sched := &recordingScheduler{}
	res, err := RunAutonomous(context.Background(),
		buildAgent(llm, "done-wins"),
		"monitor",
		WithScheduler(sched),
	)
	if err != nil {
		t.Fatalf("RunAutonomous: %v", err)
	}
	if res.Reason != StopReasonCompleted {
		t.Errorf("Reason = %q, want completed (done should win)", res.Reason)
	}
	// Scheduler should NOT have been consulted — the loop short-
	// circuited on the done signal before checking the schedule
	// channel.
	if calls := sched.Calls(); len(calls) != 0 {
		t.Errorf("Scheduler.BeforeNextTurn calls = %d, want 0 when done wins", len(calls))
	}
}

func TestRunAutonomous_MaxDeferClampsWakeAt(t *testing.T) {
	t.Parallel()
	llm := &stubLLM{scenarios: []scenarioFn{
		// Model tries to defer 1 hour.
		scheduleCallTurn(3600, "rescan", ""),
		textTurn("scheduled", 1, 1),
	}}
	// Exit immediately on defer so we don't need extra scenarios.
	sched := &recordingScheduler{
		action: func(_ coretools.ScheduleEvent) error { return coretools.ErrSchedulerDefer },
	}
	res, err := RunAutonomous(context.Background(),
		buildAgent(llm, "max-defer-clamp"),
		"monitor",
		WithScheduler(sched),
		WithMaxDefer(5*time.Minute), // driver caps at 5 minutes
	)
	if err != nil {
		t.Fatalf("RunAutonomous: %v", err)
	}
	if res.Reason != StopReasonDeferred {
		t.Errorf("Reason = %q, want deferred", res.Reason)
	}
	calls := sched.Calls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 scheduler call, got %d (res.Reason=%v)", len(calls), res.Reason)
	}
	if delta := time.Until(calls[0].WakeAt); delta > 6*time.Minute {
		t.Errorf("WakeAt not clamped: delta=%v, want <=5m", delta)
	}
}

func TestRunAutonomous_SchedulerErrorAbortsRun(t *testing.T) {
	t.Parallel()
	llm := &stubLLM{scenarios: []scenarioFn{
		scheduleCallTurn(1, "", ""),
		textTurn("scheduled", 1, 1),
	}}
	customErr := errors.New("scheduler is sad")
	sched := &recordingScheduler{
		action: func(_ coretools.ScheduleEvent) error { return customErr },
	}
	res, err := RunAutonomous(context.Background(),
		buildAgent(llm, "sched-err"),
		"monitor",
		WithScheduler(sched),
	)
	if err == nil || !strings.Contains(err.Error(), "scheduler is sad") {
		t.Fatalf("expected scheduler error to surface, got err=%v", err)
	}
	if res.Reason != StopReasonRetryAborted {
		t.Errorf("Reason = %q, want %q on scheduler error", res.Reason, StopReasonRetryAborted)
	}
}

func TestRunAutonomous_ScheduleToolMaxDeferRejectsAtTool(t *testing.T) {
	t.Parallel()
	llm := &stubLLM{scenarios: []scenarioFn{
		// Model tries to defer 1 hour against a 1-minute tool cap.
		// The tool rejects, the channel sees no event, the driver
		// doesn't consult the scheduler, and the loop falls through
		// to the default continuation.
		scheduleCallTurn(3600, "rescan", ""),
		textTurn("scheduler rejected; trying done", 1, 1),
		doneCallTurn("gave up on schedule"),
		textTurn("finished", 1, 1),
	}}
	sched := &recordingScheduler{}
	res, err := RunAutonomous(context.Background(),
		buildAgent(llm, "tool-max-defer"),
		"monitor",
		WithScheduler(sched),
		WithScheduleToolMaxDefer(time.Minute),
	)
	if err != nil {
		t.Fatalf("RunAutonomous: %v", err)
	}
	if res.Reason != StopReasonCompleted {
		t.Errorf("Reason = %q, want completed (model adapted after rejection)", res.Reason)
	}
	if calls := sched.Calls(); len(calls) != 0 {
		t.Errorf("Scheduler should not be consulted for tool-rejected calls; got %d", len(calls))
	}
}

func TestRunAutonomous_DeferredCheckpointPersistsNextWakeAt(t *testing.T) {
	t.Parallel()
	llm := &stubLLM{scenarios: []scenarioFn{
		scheduleCallTurn(60, "rescan", "10m cadence"),
		textTurn("scheduled", 1, 1),
	}}
	res, err := RunAutonomous(context.Background(),
		buildAgent(llm, "deferred-checkpoint"),
		"monitor",
		WithScheduler(coretools.ExitOnDeferScheduler()),
	)
	if err != nil {
		t.Fatalf("RunAutonomous: %v", err)
	}
	if res.Reason != StopReasonDeferred {
		t.Errorf("Reason = %q, want deferred", res.Reason)
	}
	if res.NextWakeAt.IsZero() {
		t.Fatalf("NextWakeAt should be set; full result: %+v", res)
	}
}

func TestScheduleCheckpoint_RoundTrip(t *testing.T) {
	t.Parallel()
	ev := coretools.ScheduleEvent{
		WakeAt:     time.Now().Add(time.Minute).UTC().Truncate(time.Second),
		NextPrompt: "rescan",
	}
	cp := scheduleCheckpoint(RunResult{Turns: 3}, "the goal", "continue", ev)
	if cp.NextWakeAt.IsZero() {
		t.Fatalf("scheduleCheckpoint did not propagate WakeAt")
	}
	// Round-trip through toMap/fromMap (the JSON shape the eventlog
	// persists).
	m := cp.toMap()
	got := checkpointFromMap(m)
	if !got.NextWakeAt.Equal(cp.NextWakeAt) {
		t.Errorf("checkpoint NextWakeAt round-trip lost precision: in=%v out=%v",
			cp.NextWakeAt, got.NextWakeAt)
	}
	if got.ContinuationPrompt != "rescan" {
		t.Errorf("continuation prompt mismatch: %q vs rescan", got.ContinuationPrompt)
	}
}

func TestBackgroundAgentManager_ResolveScheduler(t *testing.T) {
	t.Parallel()
	def := coretools.SleepScheduler()
	mgr := &BackgroundAgentManager{defaultScheduler: def}

	cases := []struct {
		choice  string
		wantNil bool
	}{
		{"", false},              // default
		{"default", false},       // default
		{"sleep", false},         // SleepScheduler
		{"exit_on_defer", false}, // ExitOnDeferScheduler
		{"none", true},           // explicitly no scheduler
	}
	for _, tc := range cases {
		got, err := mgr.resolveScheduler(tc.choice)
		if err != nil {
			t.Errorf("choice=%q: unexpected error %v", tc.choice, err)
			continue
		}
		if tc.wantNil && got != nil {
			t.Errorf("choice=%q: want nil, got %T", tc.choice, got)
		}
		if !tc.wantNil && got == nil {
			t.Errorf("choice=%q: want non-nil scheduler, got nil", tc.choice)
		}
	}

	if _, err := mgr.resolveScheduler("bogus"); !errors.Is(err, ErrUnknownScheduler) {
		t.Errorf("unknown choice should return ErrUnknownScheduler, got %v", err)
	}
}

func TestBackgroundAgentManager_DefaultSchedulerNilWhenUnset(t *testing.T) {
	t.Parallel()
	mgr := &BackgroundAgentManager{} // no defaultScheduler
	got, err := mgr.resolveScheduler("")
	if err != nil {
		t.Fatalf("resolveScheduler: %v", err)
	}
	if got != nil {
		t.Errorf("empty choice with no default should resolve to nil, got %T", got)
	}
}

func TestDefaultSchedulingInstruction_NonEmpty(t *testing.T) {
	t.Parallel()
	if DefaultSchedulingInstruction == "" {
		t.Fatal("DefaultSchedulingInstruction is empty")
	}
	// Guard the load-bearing pieces of the instruction.
	for _, want := range []string{
		"schedule_next_turn",
		"slow cadences",
		"State does not survive",
		"report_done wins",
	} {
		if !strings.Contains(DefaultSchedulingInstruction, want) {
			t.Errorf("DefaultSchedulingInstruction missing %q", want)
		}
	}
}

// guard so unused-import warnings don't fire if a future refactor
// drops the last reference.
var _ = adkmodel.LLMRequest{}
var _ tool.Tool = nil
var _ = atomic.Int32{}
