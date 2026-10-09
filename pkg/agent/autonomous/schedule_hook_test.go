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
	"sync"
	"testing"
	"time"

	coretools "github.com/go-steer/core-agent/v2/pkg/tools"
)

// hookRecorder captures every schedule-hook call in order.
type hookRecorder struct {
	mu    sync.Mutex
	calls []coretools.ScheduleEvent
}

func (r *hookRecorder) hook(ev coretools.ScheduleEvent) {
	r.mu.Lock()
	r.calls = append(r.calls, ev)
	r.mu.Unlock()
}

func (r *hookRecorder) snapshot() []coretools.ScheduleEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]coretools.ScheduleEvent(nil), r.calls...)
}

// TestWithScheduleHook_BracketsTheWait pins the hook's contract (#1283):
// the event arrives before the scheduler holds the loop, and the zero
// event once it lets go, so an observer reading "pending" between the
// two calls is reading the truth.
func TestWithScheduleHook_BracketsTheWait(t *testing.T) {
	t.Parallel()
	llm := &stubLLM{scenarios: []scenarioFn{
		scheduleCallTurn(1, "rescan", "polling cluster-A"),
		textTurn("scheduled", 1, 1),
		doneCallTurn("finished after wake"),
		textTurn("ok", 1, 1),
	}}
	rec := &hookRecorder{}
	var duringWait []coretools.ScheduleEvent
	sched := &recordingScheduler{action: func(coretools.ScheduleEvent) error {
		duringWait = rec.snapshot()
		return nil
	}}
	res, err := Run(context.Background(),
		buildAgent(llm, "schedule-hook"),
		"monitor",
		WithScheduler(sched),
		WithScheduleHook(rec.hook),
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Reason != StopReasonCompleted {
		t.Fatalf("Reason = %q, want completed", res.Reason)
	}

	if len(duringWait) != 1 || duringWait[0].Detail != "polling cluster-A" || duringWait[0].WakeAt.IsZero() {
		t.Errorf("hook calls seen while the scheduler held the loop = %+v, want exactly the pending event — an observer polling mid-sleep would see no wake", duringWait)
	}
	got := rec.snapshot()
	if len(got) != 2 {
		t.Fatalf("hook calls = %d (%+v), want 2: the pending event, then the clear", len(got), got)
	}
	if !got[1].WakeAt.IsZero() || got[1].Detail != "" {
		t.Errorf("second hook call = %+v, want the zero event — the wake stays advertised after the subagent woke", got[1])
	}
}

// TestWithScheduleHook_ClearsOnDeferredExit: a scheduler that ends the
// run instead of sleeping still gets the clear, because nothing in this
// process is waiting for that wake any more.
func TestWithScheduleHook_ClearsOnDeferredExit(t *testing.T) {
	t.Parallel()
	llm := &stubLLM{scenarios: []scenarioFn{
		scheduleCallTurn(60, "rescan", "10m cadence"),
		textTurn("scheduled", 1, 1),
	}}
	rec := &hookRecorder{}
	res, err := Run(context.Background(),
		buildAgent(llm, "schedule-hook-defer"),
		"monitor",
		WithScheduler(coretools.ExitOnDeferScheduler()),
		WithScheduleHook(rec.hook),
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Reason != StopReasonDeferred {
		t.Fatalf("Reason = %q, want deferred", res.Reason)
	}
	got := rec.snapshot()
	if len(got) != 2 || got[0].WakeAt.IsZero() || !got[1].WakeAt.IsZero() {
		t.Errorf("hook calls = %+v, want [pending, zero]", got)
	}
}

// TestWithScheduleHook_SeesTheClampedWake: the hook reports the time
// the loop will actually wake, not the one the model asked for.
func TestWithScheduleHook_SeesTheClampedWake(t *testing.T) {
	t.Parallel()
	llm := &stubLLM{scenarios: []scenarioFn{
		scheduleCallTurn(3600, "rescan", "hourly"),
		textTurn("scheduled", 1, 1),
	}}
	rec := &hookRecorder{}
	if _, err := Run(context.Background(),
		buildAgent(llm, "schedule-hook-clamp"),
		"monitor",
		WithScheduler(coretools.ExitOnDeferScheduler()),
		WithMaxDefer(time.Minute),
		WithScheduleHook(rec.hook),
	); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := rec.snapshot()
	if len(got) == 0 {
		t.Fatalf("hook never called")
	}
	if d := time.Until(got[0].WakeAt); d > 2*time.Minute {
		t.Errorf("hooked WakeAt is %v away, want it clamped to the 1m MaxDefer ceiling", d)
	}
}
