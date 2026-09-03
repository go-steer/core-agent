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

package watchdog_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/watchdog"
)

// noOp / didWork / failed build the three result shapes the signal
// distinguishes, so a test case reads as the trace it is replaying.
func noOp(name string) watchdog.ToolResult {
	return watchdog.ToolResult{Name: name, NoOp: true}
}

func didWork(name string) watchdog.ToolResult {
	return watchdog.ToolResult{Name: name}
}

func failed(name, err string) watchdog.ToolResult {
	return watchdog.ToolResult{Name: name, Error: err}
}

func TestNoOpStreakSignal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		results []watchdog.ToolResult
		want    bool
	}{
		{
			name:    "below threshold does not trip",
			results: []watchdog.ToolResult{noOp("mark_task_done"), noOp("mark_task_done")},
			want:    false,
		},
		{
			name:    "exactly threshold trips",
			results: []watchdog.ToolResult{noOp("mark_task_done"), noOp("mark_task_done"), noOp("mark_task_done")},
			want:    true,
		},
		{
			// The point of reading the tool's own claim rather than
			// the call: RepeatedToolCallSignal hashes (name, args) and
			// a reworded arg resets it. This signal never sees args.
			name: "reworded args are irrelevant",
			results: []watchdog.ToolResult{
				noOp("mark_task_done"), noOp("mark_task_done"), noOp("mark_task_done"),
			},
			want: true,
		},
		{
			name: "a productive call resets the run",
			results: []watchdog.ToolResult{
				noOp("mark_task_done"), noOp("mark_task_done"),
				didWork("gke_get_k8s_resource"),
				noOp("mark_task_done"), noOp("mark_task_done"),
			},
			want: false,
		},
		{
			// A failure is not a no-op — the two claims are
			// independent, and ToolFailureStreakSignal owns that one.
			name: "a failure resets the run",
			results: []watchdog.ToolResult{
				noOp("record_plan"), noOp("record_plan"),
				failed("gke_get_k8s_resource", "403 Forbidden"),
				noOp("record_plan"), noOp("record_plan"),
			},
			want: false,
		},
		{
			// Three inert calls is three inert calls; which tool went
			// inert does not make the agent any less stuck.
			name: "the streak spans tools",
			results: []watchdog.ToolResult{
				noOp("mark_task_done"), noOp("record_plan"), noOp("mark_task_done"),
			},
			want: true,
		},
		{
			// The shape the whole package's other detectors are
			// tuned for, and the one that must stay quiet.
			name: "an ordinary productive workload never trips",
			results: []watchdog.ToolResult{
				didWork("gke_list_k8s_events"), didWork("gke_get_k8s_resource"),
				noOp("record_plan"),
				didWork("gke_get_k8s_logs"), didWork("gke_describe_k8s_resource"),
				noOp("mark_task_done"),
				didWork("gke_get_k8s_resource"),
				failed("gke_get_k8s_logs", "container not ready"),
				didWork("gke_get_k8s_logs"),
				noOp("record_plan"),
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := watchdog.NewNoOpStreakSignal(watchdog.DefaultNoOpStreak)
			var alerts []watchdog.Alert
			for _, r := range tt.results {
				if a := s.ObserveToolResult(r); a != nil {
					alerts = append(alerts, *a)
				}
			}
			if got := len(alerts) > 0; got != tt.want {
				t.Fatalf("tripped = %v, want %v (alerts: %+v)", got, tt.want, alerts)
			}
			if !tt.want {
				return
			}
			a := alerts[0]
			if a.Signal != "no-op-streak" {
				t.Errorf("Signal = %q, want %q", a.Signal, "no-op-streak")
			}
			// Critical, not Warn: unlike a failure streak there is
			// nothing in flight worth not interrupting.
			if a.Severity != watchdog.SeverityCritical {
				t.Errorf("Severity = %v, want Critical", a.Severity)
			}
			if a.Reason == "" || a.Guidance == "" {
				t.Errorf("Reason/Guidance must both be set; got %q / %q", a.Reason, a.Guidance)
			}
		})
	}
}

// TestNoOpStreakSignal_OneAlertPerStreak: the alert is prompt text
// under --watchdog=feedback. Re-raising it on every no-op past the
// threshold would put the same paragraph in the model's context as
// many times as the loop is long, which is a strange way to tell
// something to stop repeating itself.
func TestNoOpStreakSignal_OneAlertPerStreak(t *testing.T) {
	t.Parallel()
	s := watchdog.NewNoOpStreakSignal(watchdog.DefaultNoOpStreak)
	alerts := 0
	for range 13 {
		if s.ObserveToolResult(noOp("mark_task_done")) != nil {
			alerts++
		}
	}
	if alerts != 1 {
		t.Fatalf("13 consecutive no-ops raised %d alerts, want exactly 1", alerts)
	}
}

// TestNoOpStreakSignal_ReplaysTheObservedLoop is the #905 session,
// replayed as results. Sixteen mark_task_done calls, thirteen of them
// answered "already recorded for this turn"; a single interleaved
// gke_get_k8s_resource at the seventh split the run into 7 + 6, which
// is precisely why RepeatedToolNameSignal's run-of-15 never fired.
//
// This is the corpus-based regression #655 asks for, on one transcript:
// if a future retuning stops this trace tripping, the signal has lost
// the only failure it was built from.
func TestNoOpStreakSignal_ReplaysTheObservedLoop(t *testing.T) {
	t.Parallel()

	trace := []watchdog.ToolResult{
		// The first call did real work — it armed the checkpoint.
		didWork("mark_task_done"),
	}
	for range 7 {
		trace = append(trace, noOp("mark_task_done"))
	}
	trace = append(trace, didWork("gke_get_k8s_resource")) // seq 85
	for range 6 {
		trace = append(trace, noOp("mark_task_done"))
	}

	s := watchdog.NewNoOpStreakSignal(watchdog.DefaultNoOpStreak)
	var alerts []watchdog.Alert
	for _, r := range trace {
		if a := s.ObserveToolResult(r); a != nil {
			alerts = append(alerts, *a)
		}
	}

	// Once per half: the interleaved call resets the run, and the
	// second half is a second loop by any honest reading.
	if len(alerts) != 2 {
		t.Fatalf("the observed trace raised %d alerts, want 2 (one per half of the 7+6 split)", len(alerts))
	}
	if !strings.Contains(alerts[0].Guidance, "mark_task_done") {
		t.Errorf("guidance does not name the looping tool:\n%s", alerts[0].Guidance)
	}
}

// TestObservedLoopIsInvisibleFromCallsAlone is the other half of the
// evidence, and the reason this signal exists rather than a retuning.
// It replays the same trace as CALLS — the shape every pre-#907 default
// signal reads — with `detail` reworded each time, exactly as the live
// model did, and asserts the whole default set stays silent.
//
// If a future change makes this trip on calls alone, that is not a
// broken test: it means someone lowered a threshold, and toolname.go's
// false-positive argument needs re-litigating before this file's does.
func TestObservedLoopIsInvisibleFromCallsAlone(t *testing.T) {
	t.Parallel()

	w := watchdog.NewDefaultWatchdog()
	call := func(name, args string) {
		w.ObserveToolCall(watchdog.ToolCall{Name: name, Args: args})
	}

	call("mark_task_done", `{"detail":"closed the OOMKill incident on api-7d9"}`)
	for i := range 7 {
		call("mark_task_done", fmt.Sprintf(`{"detail":"the api-7d9 remediation is complete, phrasing %d"}`, i))
	}
	call("gke_get_k8s_resource", `{"kind":"Deployment","name":"api"}`)
	for i := range 6 {
		call("mark_task_done", fmt.Sprintf(`{"detail":"work on api-7d9 finished, phrasing %d"}`, i+7))
	}

	if alerts := w.Check(); len(alerts) != 0 {
		t.Fatalf("a call-only reading of the observed loop raised %d alerts, want 0 — "+
			"the premise of #907 is that this trace is invisible from calls: %+v", len(alerts), alerts)
	}
}

// TestNoOpStreakSignal_ThresholdFloor: threshold 1 would alert on every
// idempotent write in the process.
func TestNoOpStreakSignal_ThresholdFloor(t *testing.T) {
	t.Parallel()
	s := watchdog.NewNoOpStreakSignal(1)
	if s.ObserveToolResult(noOp("mark_task_done")) != nil {
		t.Fatal("a single no-op raised an alert; threshold must be clamped to at least 2")
	}
	if s.ObserveToolResult(noOp("mark_task_done")) == nil {
		t.Fatal("two no-ops raised nothing; threshold should have clamped to 2, not higher")
	}
}

// TestNoOpStreakSignal_ResetClearsTrip: /clear starts a new logical
// session, so a loop from before it must not suppress the alert for a
// loop after it.
func TestNoOpStreakSignal_ResetClearsTrip(t *testing.T) {
	t.Parallel()
	s := watchdog.NewNoOpStreakSignal(watchdog.DefaultNoOpStreak)
	for range 3 {
		s.ObserveToolResult(noOp("mark_task_done"))
	}
	s.Reset()
	alerts := 0
	for range 3 {
		if s.ObserveToolResult(noOp("mark_task_done")) != nil {
			alerts++
		}
	}
	if alerts != 1 {
		t.Fatalf("after Reset a fresh streak raised %d alerts, want 1", alerts)
	}
}

// TestDefaultWatchdogWiresNoOpStreak: a signal nobody constructs is a
// signal nobody runs. #660's watchdog is CLI-selected, so the default
// set is what every deployment actually gets.
func TestDefaultWatchdogWiresNoOpStreak(t *testing.T) {
	t.Parallel()
	w := watchdog.NewDefaultWatchdog()
	for range watchdog.DefaultNoOpStreak {
		w.ObserveToolResult(noOp("mark_task_done"))
	}
	alerts := w.Check()
	found := false
	for _, a := range alerts {
		if a.Signal == "no-op-streak" {
			found = true
		}
	}
	if !found {
		t.Fatalf("NewDefaultWatchdog did not raise no-op-streak after %d no-ops; got %+v",
			watchdog.DefaultNoOpStreak, alerts)
	}
}
