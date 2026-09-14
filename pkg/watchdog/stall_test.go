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

// answered builds a successful result carrying a payload digest, which
// is the only shape this signal reads. The digest stands in for "what
// came back"; two calls sharing one are two calls that learned the same
// thing, however differently they asked.
func answered(name, digest string) watchdog.ToolResult {
	return watchdog.ToolResult{Name: name, Digest: digest}
}

func TestNoNewStateSignal(t *testing.T) {
	t.Parallel()

	// repeats returns one fresh result followed by n results carrying
	// that same digest — the shape the signal exists to catch.
	repeats := func(name, digest string, n int) []watchdog.ToolResult {
		out := []watchdog.ToolResult{answered(name, digest)}
		for i := 0; i < n; i++ {
			out = append(out, answered(name, digest))
		}
		return out
	}

	tests := []struct {
		name    string
		results []watchdog.ToolResult
		want    bool
	}{
		{
			name:    "below threshold does not trip",
			results: repeats("read_file", "aaa", 5),
			want:    false,
		},
		{
			name:    "exactly threshold trips",
			results: repeats("read_file", "aaa", 6),
			want:    true,
		},
		{
			// The #905 shape, transposed. Every call is a different
			// (name, args) pair so the args-keyed detectors see nothing,
			// but the cluster keeps answering the same thing.
			name: "the same question asked by different tools still trips",
			results: []watchdog.ToolResult{
				answered("gke_get_k8s_resource", "same"),
				answered("read_file", "same"),
				answered("gke_get_k8s_resource", "same"),
				answered("json_query", "same"),
				answered("gke_get_k8s_resource", "same"),
				answered("grep", "same"),
				answered("read_file", "same"),
			},
			want: true,
		},
		{
			name: "one genuinely new answer resets the run",
			results: []watchdog.ToolResult{
				answered("read_file", "aaa"),
				answered("read_file", "aaa"),
				answered("read_file", "aaa"),
				answered("read_file", "aaa"),
				answered("read_file", "bbb"), // progress
				answered("read_file", "aaa"),
				answered("read_file", "aaa"),
				answered("read_file", "aaa"),
			},
			want: false,
		},
		{
			// A result nothing could fingerprint must not extend a run.
			// Absence of evidence is not evidence of a stall.
			name: "an undigestable result resets rather than counting",
			results: []watchdog.ToolResult{
				answered("read_file", "aaa"),
				answered("read_file", "aaa"),
				answered("read_file", "aaa"),
				answered("read_file", ""), // unknown
				answered("read_file", "aaa"),
				answered("read_file", "aaa"),
				answered("read_file", "aaa"),
			},
			want: false,
		},
		{
			// A poll is the false positive this signal is most exposed
			// to, and wait_and_verify is the bounded, supported way to
			// do one. Observing it would flag the fix.
			name:    "wait_and_verify polling the same state is exempt",
			results: repeats("wait_and_verify", "unchanged", 12),
			want:    false,
		},
		{
			// Failures belong to ToolFailureStreakSignal and no-ops to
			// NoOpStreakSignal. Skipped, not counted as progress —
			// otherwise a stalled agent that errors once every five
			// calls stays under the threshold forever.
			name: "interleaved failures and no-ops neither trip nor rescue",
			results: []watchdog.ToolResult{
				answered("read_file", "aaa"),
				answered("read_file", "aaa"),
				answered("read_file", "aaa"),
				failed("read_file", "boom"),
				noOp("mark_task_done"),
				answered("read_file", "aaa"),
				answered("read_file", "aaa"),
				answered("read_file", "aaa"),
				answered("read_file", "aaa"),
			},
			want: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := watchdog.NewNoNewStateSignal(watchdog.DefaultStallRun, watchdog.DefaultStallMemory)
			var got *watchdog.Alert
			for _, r := range tc.results {
				if a := s.ObserveToolResult(r); a != nil && got == nil {
					got = a
				}
			}
			if (got != nil) != tc.want {
				t.Fatalf("tripped = %v, want %v (alert: %+v)", got != nil, tc.want, got)
			}
		})
	}
}

// One alert per run, not one per call past the threshold. An operator
// reading twenty identical lines learns what one line would have told
// them, and under --watchdog=feedback each one is prompt text the model
// pays for.
func TestNoNewStateSignal_AlertsOncePerRun(t *testing.T) {
	t.Parallel()

	s := watchdog.NewNoNewStateSignal(3, 16)
	s.ObserveToolResult(answered("read_file", "aaa"))
	alerts := 0
	for i := 0; i < 10; i++ {
		if s.ObserveToolResult(answered("read_file", "aaa")) != nil {
			alerts++
		}
	}
	if alerts != 1 {
		t.Fatalf("alerts = %d, want 1", alerts)
	}

	// A new answer ends the run; the next stall is a new event and must
	// alert again, otherwise one trip per session silences the signal
	// for everything after it.
	s.ObserveToolResult(answered("read_file", "bbb"))
	alerts = 0
	for i := 0; i < 5; i++ {
		if s.ObserveToolResult(answered("read_file", "aaa")) != nil {
			alerts++
		}
	}
	if alerts != 1 {
		t.Fatalf("alerts after a break = %d, want 1", alerts)
	}
}

// Warn, never Critical. Under --watchdog=enforce a Critical alert halts
// the agent, and a repeated payload cannot distinguish a stalled agent
// from one polling a cluster that has not converged yet — which is most
// of what this project's agents legitimately do.
func TestNoNewStateSignal_IsWarnSoItCannotHalt(t *testing.T) {
	t.Parallel()

	s := watchdog.NewNoNewStateSignal(3, 16)
	s.ObserveToolResult(answered("gke_get_k8s_resource", "pending"))
	var got *watchdog.Alert
	for i := 0; i < 3 && got == nil; i++ {
		got = s.ObserveToolResult(answered("gke_get_k8s_resource", "pending"))
	}
	if got == nil {
		t.Fatal("expected an alert")
	}
	if got.Severity != watchdog.SeverityWarn {
		t.Fatalf("severity = %q, want %q — Critical halts under enforce, and a poll is indistinguishable from a stall by payload alone",
			got.Severity, watchdog.SeverityWarn)
	}
	if got.Signal != "no-new-state" {
		t.Fatalf("signal = %q", got.Signal)
	}
	// The Guidance is the half an unattended daemon actually acts on,
	// so it has to say what to do instead — not just that something is
	// wrong. Naming the bounded alternative is the actionable part.
	if !strings.Contains(got.Guidance, "wait_and_verify") {
		t.Errorf("guidance does not point at the bounded alternative: %q", got.Guidance)
	}
	if !strings.Contains(got.Guidance, "gke_get_k8s_resource") {
		t.Errorf("guidance does not name the tool that is stalling: %q", got.Guidance)
	}
}

// Reset forgets what has been seen, not just the run in progress. After
// an operator reset the agent starting over with the same reads is
// starting over, and charging it for reads it made before the reset
// would trip the signal on the first thing it did.
func TestNoNewStateSignal_ResetForgetsTheSeenSet(t *testing.T) {
	t.Parallel()

	s := watchdog.NewNoNewStateSignal(3, 16)
	for i := 0; i < 5; i++ {
		s.ObserveToolResult(answered("read_file", "aaa"))
	}
	s.Reset()
	for i := 0; i < 2; i++ {
		if a := s.ObserveToolResult(answered("read_file", "aaa")); a != nil {
			t.Fatalf("tripped %d observations after Reset: %+v", i+1, a)
		}
	}
}

// The false positive that would have mattered most, and the reason the
// turn boundary exists: a daemon woken on a timer to check a cluster
// that has not changed reads the identical state every wake. The
// watchdog is cleared only by an operator Reset, so without a boundary
// the second wake would be six consecutive already-seen results and the
// monitoring agent would be told it was stalling — forever, once per
// wake. This is the shape of this project's own soak runs.
func TestNoNewStateSignal_TurnBoundaryClearsWhatWasSeen(t *testing.T) {
	t.Parallel()

	s := watchdog.NewNoNewStateSignal(watchdog.DefaultStallRun, watchdog.DefaultStallMemory)
	// Six wakes, each reading the same six unchanged resources.
	for wake := 0; wake < 6; wake++ {
		s.ObserveTurnStart()
		for res := 0; res < 6; res++ {
			r := answered("gke_get_k8s_resource", fmt.Sprintf("resource-%d", res))
			if a := s.ObserveToolResult(r); a != nil {
				t.Fatalf("wake %d, read %d: a monitoring agent was flagged as stalling: %+v", wake, res, a)
			}
		}
	}

	// And the boundary must not disarm the detector for the turn it
	// starts: a genuine within-turn stall after any number of wakes
	// still trips.
	s.ObserveTurnStart()
	var got *watchdog.Alert
	for i := 0; i < watchdog.DefaultStallRun+1; i++ {
		if a := s.ObserveToolResult(answered("read_file", "stuck")); a != nil && got == nil {
			got = a
		}
	}
	if got == nil {
		t.Fatal("a within-turn stall after a turn boundary did not trip")
	}
}

// The boundary reaches the signal through DefaultWatchdog, not just by
// being callable on the signal directly. A hook nothing fans out is the
// same inert machinery as a signal nothing wires.
func TestDefaultWatchdog_ObserveTurnStartReachesTheSignals(t *testing.T) {
	t.Parallel()

	// Part of a run before the boundary, the rest after. Fanned out, the
	// two halves cannot add up; not fanned out, they reach the threshold
	// together and trip.
	w := watchdog.NewDefaultWatchdog()
	for i := 0; i < 3; i++ {
		w.ObserveToolResult(answered("read_file", "aaa"))
	}
	w.ObserveTurnStart()
	for i := 0; i < watchdog.DefaultStallRun; i++ {
		w.ObserveToolResult(answered("read_file", "aaa"))
	}
	for _, a := range w.Check() {
		if a.Signal == "no-new-state" {
			t.Fatalf("a run spanning a turn boundary was counted as one run: %+v", a)
		}
	}
}

// The memory is bounded, and the bound is a real eviction rather than a
// cap that quietly stops remembering. A digest pushed out by newer ones
// reads as new information again — which is the correct reading, since
// the whole claim is "this turn already had it" and the signal no
// longer knows that it did.
func TestNoNewStateSignal_MemoryIsBoundedAndEvicts(t *testing.T) {
	t.Parallel()

	const memory = 8
	s := watchdog.NewNoNewStateSignal(3, memory)
	s.ObserveToolResult(answered("read_file", "first"))
	// Push "first" out with more than `memory` distinct digests.
	for i := 0; i < memory+2; i++ {
		s.ObserveToolResult(answered("read_file", fmt.Sprintf("d%d", i)))
	}
	// "first" is forgotten, so re-reading it counts as new information
	// and cannot start a run.
	for i := 0; i < 2; i++ {
		if a := s.ObserveToolResult(answered("read_file", "first")); a != nil {
			t.Fatalf("evicted digest still counted as seen: %+v", a)
		}
	}
}

// Threshold 1 or 2 would fire on an agent that read one file twice, so
// the constructor clamps. Guardrails that can be configured into a
// false-positive machine get configured that way.
func TestNoNewStateSignal_ClampsUnusableThresholds(t *testing.T) {
	t.Parallel()

	s := watchdog.NewNoNewStateSignal(1, 0)
	if s.Threshold != 3 {
		t.Fatalf("Threshold = %d, want 3", s.Threshold)
	}
	if s.Memory < s.Threshold {
		t.Fatalf("Memory = %d, want at least Threshold (%d) — a memory shorter than the run can never hold one",
			s.Memory, s.Threshold)
	}
}

// The signal ships in the default set. A detector nobody wires is the
// #642 finding all over again: machinery that is coherent, tested and
// inert as configured.
func TestNoNewStateSignal_IsInTheDefaultSet(t *testing.T) {
	t.Parallel()

	w := watchdog.NewDefaultWatchdog()
	w.ObserveToolResult(answered("read_file", "aaa"))
	for i := 0; i < watchdog.DefaultStallRun; i++ {
		w.ObserveToolResult(answered("read_file", "aaa"))
	}
	var found bool
	for _, a := range w.Check() {
		if a.Signal == "no-new-state" {
			found = true
		}
	}
	if !found {
		t.Fatal("no-new-state did not fire through NewDefaultWatchdog — the detector is not wired")
	}
}

// The shape every other detector in this package structurally misses:
// the model rewords its arguments each iteration, so (name, args) never
// repeats, and the answer is identical every time. This is #905's trace
// with the tool's own no-op confession removed — the case noop.go could
// only catch because mark_task_done opted in, replayed against a tool
// that did not.
func TestNoNewStateSignal_CatchesTheRewordedLoopArgsDetectorsCannot(t *testing.T) {
	t.Parallel()

	w := watchdog.NewDefaultWatchdog()
	for i := 0; i < 13; i++ {
		// A different canonical-args key every iteration: this is what
		// defeats RepeatedToolCall, AlternatingCycle and Dominant.
		w.ObserveToolCall(watchdog.ToolCall{
			Name: "gke_get_k8s_resource",
			Args: fmt.Sprintf(`{"detail":"attempt %d, rephrased"}`, i),
		})
		// And the identical answer every time.
		w.ObserveToolResult(answered("gke_get_k8s_resource", "the-same-pod-status"))
	}

	var signals []string
	for _, a := range w.Check() {
		signals = append(signals, a.Signal)
	}
	if len(signals) == 0 {
		t.Fatal("thirteen identically-answered calls raised nothing")
	}
	var got bool
	for _, s := range signals {
		if s == "no-new-state" {
			got = true
		}
	}
	if !got {
		t.Fatalf("signals = %v, want no-new-state among them", signals)
	}
}
