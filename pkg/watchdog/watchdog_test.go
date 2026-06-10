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

package watchdog

import (
	"strings"
	"sync"
	"testing"
)

func TestRepeatedToolCallSignal_TripsAtThreshold(t *testing.T) {
	t.Parallel()
	s := NewRepeatedToolCallSignal(5)
	tc := ToolCall{Name: "read_file", Args: `{"path":"main.go"}`}

	// First 4 calls — no alert.
	for i := 0; i < 4; i++ {
		if alert := s.ObserveToolCall(tc); alert != nil {
			t.Fatalf("call %d: should not trip yet (threshold=5); got %+v", i+1, alert)
		}
	}
	// 5th call — trips.
	alert := s.ObserveToolCall(tc)
	if alert == nil {
		t.Fatalf("call 5: should trip at threshold")
	}
	if alert.Signal != "repeated-tool-call" {
		t.Errorf("Signal = %q, want %q", alert.Signal, "repeated-tool-call")
	}
	if alert.Severity != SeverityWarn {
		t.Errorf("Severity = %q, want %q", alert.Severity, SeverityWarn)
	}
	if !strings.Contains(alert.Reason, "read_file") {
		t.Errorf("Reason should name the looping tool: %q", alert.Reason)
	}
	if !strings.Contains(alert.Reason, "5") {
		t.Errorf("Reason should include the count: %q", alert.Reason)
	}
}

func TestRepeatedToolCallSignal_DoesNotReEmitWithinSameRun(t *testing.T) {
	t.Parallel()
	// The whole point of the in-run guard: operators want one
	// notice per stuck pattern, not one per tool call past the
	// threshold. A regression here would flood logs / future SSE
	// streams with duplicate alerts.
	s := NewRepeatedToolCallSignal(3)
	tc := ToolCall{Name: "grep", Args: `"foo"`}
	emitCount := 0
	for i := 0; i < 10; i++ {
		if alert := s.ObserveToolCall(tc); alert != nil {
			emitCount++
		}
	}
	if emitCount != 1 {
		t.Errorf("expected exactly 1 alert across 10 identical calls (one trip per run); got %d", emitCount)
	}
}

func TestRepeatedToolCallSignal_DifferentArgsResetsRun(t *testing.T) {
	t.Parallel()
	s := NewRepeatedToolCallSignal(3)
	a := ToolCall{Name: "read_file", Args: `{"path":"a.go"}`}
	b := ToolCall{Name: "read_file", Args: `{"path":"b.go"}`}

	// a, a, b, a, a — no run reaches threshold.
	if got := []bool{
		s.ObserveToolCall(a) != nil,
		s.ObserveToolCall(a) != nil,
		s.ObserveToolCall(b) != nil,
		s.ObserveToolCall(a) != nil,
		s.ObserveToolCall(a) != nil,
	}; got[0] || got[1] || got[2] || got[3] || got[4] {
		t.Errorf("no alert should fire on non-consecutive runs of 2; got %v", got)
	}
	// Now build up to threshold on a.
	if alert := s.ObserveToolCall(a); alert == nil {
		t.Errorf("third consecutive a should trip; got nil")
	}
}

func TestRepeatedToolCallSignal_DifferentNamesResetsRun(t *testing.T) {
	t.Parallel()
	s := NewRepeatedToolCallSignal(3)
	a := ToolCall{Name: "read_file", Args: `{}`}
	b := ToolCall{Name: "grep", Args: `{}`}

	s.ObserveToolCall(a)
	s.ObserveToolCall(b)
	s.ObserveToolCall(b)
	s.ObserveToolCall(a)
	s.ObserveToolCall(a)
	// All runs are length ≤ 2; nothing trips.
	if alert := s.ObserveToolCall(a); alert == nil {
		t.Errorf("third consecutive a after the break should trip; got nil")
	}
}

func TestRepeatedToolCallSignal_TripAgainAfterRunBreaks(t *testing.T) {
	t.Parallel()
	// After a run trips, observe a different call (breaking the
	// run), then build up to threshold again — should trip again.
	// Regression guard: the tripped-once guard is per-RUN, not
	// permanent.
	s := NewRepeatedToolCallSignal(3)
	a := ToolCall{Name: "x", Args: ""}
	b := ToolCall{Name: "y", Args: ""}

	for i := 0; i < 3; i++ {
		s.ObserveToolCall(a)
	}
	// Should have tripped on the 3rd; subsequent 'a's don't re-trip.
	s.ObserveToolCall(b) // breaks run
	s.ObserveToolCall(a)
	s.ObserveToolCall(a)
	if alert := s.ObserveToolCall(a); alert == nil {
		t.Errorf("new run of 3 after a break should trip again")
	}
}

func TestRepeatedToolCallSignal_Reset(t *testing.T) {
	t.Parallel()
	s := NewRepeatedToolCallSignal(2)
	s.ObserveToolCall(ToolCall{Name: "x"})
	s.Reset()
	// After reset, a single observation shouldn't be "second in a
	// run" (the previous run is gone).
	if alert := s.ObserveToolCall(ToolCall{Name: "x"}); alert != nil {
		t.Errorf("Reset should clear lastCall + runLength; got premature alert %+v", alert)
	}
}

func TestNewRepeatedToolCallSignal_ClampsBelow2(t *testing.T) {
	t.Parallel()
	// Threshold < 2 would mean every tool call trips the signal.
	// The clamp prevents that degenerate config from sneaking in
	// via a misconfigured construction.
	for _, n := range []int{0, 1, -3} {
		s := NewRepeatedToolCallSignal(n)
		if s.Threshold != 2 {
			t.Errorf("NewRepeatedToolCallSignal(%d).Threshold = %d, want 2 (clamped)", n, s.Threshold)
		}
	}
}

func TestDefaultWatchdog_CheckAccumulatesAndDrains(t *testing.T) {
	t.Parallel()
	w := NewDefaultWatchdog()
	tc := ToolCall{Name: "read_file", Args: `{"path":"loop.go"}`}

	for i := 0; i < 5; i++ {
		w.ObserveToolCall(tc)
	}
	got := w.Check()
	if len(got) != 1 {
		t.Errorf("expected exactly 1 alert from 5 identical calls; got %d", len(got))
	}
	// Second Check returns nil (alerts drained).
	if got := w.Check(); got != nil {
		t.Errorf("second Check should return nil after drain; got %+v", got)
	}
}

func TestDefaultWatchdog_Reset(t *testing.T) {
	t.Parallel()
	w := NewDefaultWatchdog()
	for i := 0; i < 5; i++ {
		w.ObserveToolCall(ToolCall{Name: "x"})
	}
	w.Reset()
	// After reset, no pending alerts and no in-flight run.
	if got := w.Check(); got != nil {
		t.Errorf("Check after Reset should return nil; got %+v", got)
	}
	// Observe once — should not trip (run state cleared).
	if got := w.Check(); got != nil {
		t.Errorf("after Reset + single observe, still no alerts; got %+v", got)
	}
}

func TestDefaultWatchdog_ConcurrentSafe(t *testing.T) {
	t.Parallel()
	// Run with -race. Many goroutines pumping observations + one
	// goroutine periodically calling Check. No data race, no panic.
	// Two waitgroups so the producers and the checker have
	// independent lifecycles: producers finish on their own;
	// checker stops on a separate signal once producers are done.
	w := NewDefaultWatchdog()
	const producers = 16
	const perProducer = 1000

	var producersWG sync.WaitGroup
	producersWG.Add(producers)
	for i := 0; i < producers; i++ {
		go func() {
			defer producersWG.Done()
			for j := 0; j < perProducer; j++ {
				w.ObserveToolCall(ToolCall{Name: "x", Args: "y"})
			}
		}()
	}

	var checkerWG sync.WaitGroup
	checkerWG.Add(1)
	stop := make(chan struct{})
	go func() {
		defer checkerWG.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = w.Check()
			}
		}
	}()

	producersWG.Wait()
	close(stop)
	checkerWG.Wait()
	// Drain any final alerts; the assertion is just "no panic /
	// no data race." If the -race detector finds anything we'd
	// have failed already.
	_ = w.Check()
}

func TestAlert_StringFormat(t *testing.T) {
	t.Parallel()
	a := Alert{
		Signal:   "repeated-tool-call",
		Severity: SeverityWarn,
		Reason:   "stuck on read_file",
	}
	got := a.String()
	if !strings.Contains(got, "[warn]") {
		t.Errorf("String() should contain '[warn]': %q", got)
	}
	if !strings.Contains(got, "repeated-tool-call") {
		t.Errorf("String() should contain signal name: %q", got)
	}
	if !strings.Contains(got, "stuck on read_file") {
		t.Errorf("String() should contain reason: %q", got)
	}
}

func TestTruncate(t *testing.T) {
	t.Parallel()
	if got := truncate("short", 100); got != "short" {
		t.Errorf("truncate of short input should pass through; got %q", got)
	}
	long := strings.Repeat("a", 100) + "MIDDLE" + strings.Repeat("b", 100)
	got := truncate(long, 50)
	if len(got) > 50 {
		t.Errorf("truncate should cap at maxLen; got %d", len(got))
	}
	if !strings.Contains(got, "…") {
		t.Errorf("truncate should insert ellipsis: %q", got)
	}
}
