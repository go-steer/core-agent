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
	"strconv"
	"strings"
	"testing"
)

// defaultToolName is the signal as NewDefaultWatchdog wires it.
func defaultToolName() *RepeatedToolNameSignal {
	return NewRepeatedToolNameSignal(DefaultToolNameRun, DefaultRepeatThreshold)
}

// reworded is the shape this signal exists for: one tool called n times
// with a model-authored free-text argument that changes every call, so
// every (name, args) key is unique.
func reworded(name string, n int) []ToolCall {
	out := make([]ToolCall, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, call(name, `{"detail":"attempt `+strconv.Itoa(i)+`, same thing said differently"}`))
	}
	return out
}

// TestRepeatedToolNameSignal_TripsOnRewordedArguments is the acceptance
// test. Fails on pre-fix code by construction — the signal does not
// exist — so the sibling assertions carry the weight: they show the
// three shipped detectors are all silent on the identical sequence,
// which is what makes this a gap rather than a tuning change.
func TestRepeatedToolNameSignal_TripsOnRewordedArguments(t *testing.T) {
	t.Parallel()

	calls := reworded("mark_task_done", DefaultToolNameRun)
	alerts := feed(defaultToolName(), calls...)

	if len(alerts) != 1 {
		t.Fatalf("got %d alerts, want exactly 1: %+v", len(alerts), alerts)
	}
	got := alerts[0]
	if got.Signal != "repeated-tool-name" {
		t.Errorf("Signal = %q, want repeated-tool-name", got.Signal)
	}
	// Warn, not Critical, and the one place the severity is pinned: a
	// name-keyed run is a loop or a sweep and this signal cannot tell
	// which, so it must not halt the agent under --watchdog=enforce.
	// Raising this to Critical is a decision about false positives, not
	// a detail — make it here, deliberately, or not at all.
	if got.Severity != SeverityWarn {
		t.Errorf("Severity = %q, want warn — this detector cannot prove the calls are redundant, so it must not halt a working agent", got.Severity)
	}
	if !strings.Contains(got.Reason, "mark_task_done") {
		t.Errorf("Reason omits the looping tool, so an operator can't tell what looped: %q", got.Reason)
	}
	if !strings.Contains(got.Guidance, "mark_task_done") {
		t.Errorf("Guidance omits the looping tool: %q", got.Guidance)
	}

	// The gap: every args-keyed detector sees this as N distinct calls.
	if a := feed(NewRepeatedToolCallSignal(DefaultRepeatThreshold), calls...); len(a) != 0 {
		t.Errorf("the repeat detector already covered reworded args, so this would be a tuning change, not a gap: %+v", a)
	}
	if a := feed(NewAlternatingCycleSignal(DefaultCycleMaxPeriod, DefaultCycleRepeats), calls...); len(a) != 0 {
		t.Errorf("the cycle detector already covered this shape: %+v", a)
	}
	if a := feed(NewDominantToolCallSignal(DefaultDominantWindow, DefaultDominantThreshold,
		DefaultDominantDeferRun, DefaultDominantDeferPeriod), calls...); len(a) != 0 {
		t.Errorf("the density detector already covered this shape: %+v", a)
	}
}

// TestRepeatedToolNameSignal_ClearsTheShippedFalsePositiveCase is the
// tuning gate, and the reason DefaultToolNameRun is 15 and not 12.
// TestDominantToolCallSignal_DoesNotTripOnLegitimateWork certifies a
// window of twelve distinct read_file calls as work; a name-keyed
// detector that fires on that exact sequence would contradict a shipped
// contract. At Warn that contradiction costs noise rather than a halt,
// but a detector whose first act is to disagree with a sibling's
// false-positive test is not one operators will leave switched on.
func TestRepeatedToolNameSignal_ClearsTheShippedFalsePositiveCase(t *testing.T) {
	t.Parallel()

	var sweep []ToolCall
	for i := 0; i < DefaultDominantWindow; i++ {
		sweep = append(sweep, call("read_file", `{"path":"`+strconv.Itoa(i)+`.go"}`))
	}
	if alerts := feed(defaultToolName(), sweep...); len(alerts) != 0 {
		t.Errorf("fired on a %d-file sweep the density detector's own false-positive test calls legitimate: %+v",
			DefaultDominantWindow, alerts)
	}
	if DefaultToolNameRun <= DefaultDominantWindow {
		t.Errorf("DefaultToolNameRun = %d, must exceed DefaultDominantWindow (%d) or the two detectors disagree about the same sequence",
			DefaultToolNameRun, DefaultDominantWindow)
	}
}

// TestRepeatedToolNameSignal_DoesNotTripOnLegitimateWork covers the
// rest of the false-positive surface. Warn means a false positive is
// noise rather than a halt, but noise is what gets a detector switched
// off, so anything with a different tool in it has to stay quiet
// however long it runs.
func TestRepeatedToolNameSignal_DoesNotTripOnLegitimateWork(t *testing.T) {
	t.Parallel()

	var interleavedSweep []ToolCall
	for i := 0; i < 40; i++ {
		interleavedSweep = append(interleavedSweep, call("read_file", `{"path":"`+strconv.Itoa(i)+`.go"}`))
		if i%10 == 9 {
			interleavedSweep = append(interleavedSweep, call("grep", `{"q":"x"}`))
		}
	}

	tests := []struct {
		name  string
		calls []ToolCall
	}{
		{"a run one short of the threshold", reworded("mark_task_done", DefaultToolNameRun-1)},
		{"a long sweep broken up by other work", interleavedSweep},
		{
			name: "varied exploration",
			calls: []ToolCall{
				call("read_file", `{"path":"a.go"}`), call("grep", `{"q":"foo"}`),
				call("read_file", `{"path":"b.go"}`), call("grep", `{"q":"bar"}`),
				call("edit_file", `{"path":"a.go"}`), call("run_tests", "{}"),
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if alerts := feed(defaultToolName(), tc.calls...); len(alerts) != 0 {
				t.Errorf("fired on legitimate work: %+v", alerts)
			}
		})
	}
}

// TestRepeatedToolNameSignal_DefersToTheRepeatDetector holds the
// one-behavior-one-alert invariant. A run of byte-identical calls is
// RepeatedToolCallSignal's, and it alerts at 5 — long before this
// signal's 15. Under --watchdog=feedback a duplicate alert is
// duplicated prompt text, not just a duplicated log line, and here the
// duplicate would be the weaker report (Warn, can't prove redundancy)
// shadowing the stronger one (Critical, can).
func TestRepeatedToolNameSignal_DefersToTheRepeatDetector(t *testing.T) {
	t.Parallel()

	identical := make([]ToolCall, 0, DefaultToolNameRun)
	for i := 0; i < DefaultToolNameRun; i++ {
		identical = append(identical, call("gke_list_clusters", "{}"))
	}
	if alerts := feed(defaultToolName(), identical...); len(alerts) != 0 {
		t.Errorf("an args-identical run raised a name alert on top of the repeat detector's: %+v", alerts)
	}
	// Sanity: the repeat detector really does own this sequence.
	if a := feed(NewRepeatedToolCallSignal(DefaultRepeatThreshold), identical...); len(a) != 1 {
		t.Fatalf("repeat detector alerts = %+v, want exactly 1 — otherwise the deference above is a hole", a)
	}

	// With deference off — an operator wiring this signal alone — the
	// same sequence does trip, so the deference is a choice rather than
	// the mechanism.
	if alerts := feed(NewRepeatedToolNameSignal(DefaultToolNameRun, 0), identical...); len(alerts) != 1 {
		t.Errorf("deferRun=0 got %d alerts, want 1", len(alerts))
	}

	// The deference is about the TAIL of the run, not the whole of it: a
	// reworded loop that settles into repeating itself verbatim is the
	// same loop getting worse, and the repeat detector picks it up.
	mixed := append(reworded("mark_task_done", DefaultToolNameRun-1),
		call("mark_task_done", `{"detail":"settled"}`),
		call("mark_task_done", `{"detail":"settled"}`),
		call("mark_task_done", `{"detail":"settled"}`),
		call("mark_task_done", `{"detail":"settled"}`),
		call("mark_task_done", `{"detail":"settled"}`))
	if alerts := feed(defaultToolName(), mixed...); len(alerts) != 1 {
		t.Errorf("got %d alerts, want 1 — the run crosses the threshold before the args-identical tail forms: %+v",
			len(alerts), alerts)
	}
}

// TestRepeatedToolNameSignal_OneAlertPerRun: the alert fires once while
// the run persists. A signal that re-emits every call past the
// threshold is a prompt leak under --watchdog=feedback.
func TestRepeatedToolNameSignal_OneAlertPerRun(t *testing.T) {
	t.Parallel()

	if alerts := feed(defaultToolName(), reworded("mark_task_done", DefaultToolNameRun*3)...); len(alerts) != 1 {
		t.Errorf("got %d alerts over a %d-call run, want 1: %+v", len(alerts), DefaultToolNameRun*3, alerts)
	}
}

// TestRepeatedToolNameSignal_ReArmsAfterADifferentTool: state persists
// across turns and only Reset clears it, so a signal that never re-arms
// goes permanently silent after one loop in a long session.
func TestRepeatedToolNameSignal_ReArmsAfterADifferentTool(t *testing.T) {
	t.Parallel()

	s := defaultToolName()
	if alerts := feed(s, reworded("mark_task_done", DefaultToolNameRun)...); len(alerts) != 1 {
		t.Fatalf("first loop got %d alerts, want 1", len(alerts))
	}
	// A single different tool breaks the run — that is the whole
	// definition of consecutive here.
	if a := s.ObserveToolCall(call("read_file", `{"path":"a.go"}`)); a != nil {
		t.Fatalf("productive work alerted: %+v", a)
	}
	if alerts := feed(s, reworded("mark_task_done", DefaultToolNameRun)...); len(alerts) != 1 {
		t.Errorf("second loop got %d alerts, want 1 — the signal never re-armed", len(alerts))
	}
}

// TestRepeatedToolNameSignal_ResetClears: Reset is what a logical
// session boundary calls, and a signal that keeps its run across one
// carries a dead pattern into new work.
func TestRepeatedToolNameSignal_ResetClears(t *testing.T) {
	t.Parallel()

	s := defaultToolName()
	feed(s, reworded("mark_task_done", DefaultToolNameRun-1)...)
	s.Reset()
	if s.runLength != 0 || s.name != "" || s.tripped || s.hasLastArgs {
		t.Fatalf("Reset left state: name=%q run=%d tripped=%v hasArgs=%v",
			s.name, s.runLength, s.tripped, s.hasLastArgs)
	}
	// One more call would have crossed the threshold before the Reset.
	if alerts := feed(s, reworded("mark_task_done", 1)...); len(alerts) != 0 {
		t.Errorf("post-Reset alerts = %+v, want none — the pre-Reset run leaked", alerts)
	}
}

// TestRepeatedToolNameSignal_GuidanceIsModelFacing holds the #159
// two-reader split: Guidance is injected into the model's context under
// --watchdog=feedback, so it must not spend tokens on controls only an
// operator has.
//
// It must also not claim the calls are identical. They are not — that
// is the entire premise of this detector — and a model told something
// verifiably false about its own transcript is right to discount the
// rest of the message.
func TestRepeatedToolNameSignal_GuidanceIsModelFacing(t *testing.T) {
	t.Parallel()

	alerts := feed(defaultToolName(), reworded("mark_task_done", DefaultToolNameRun)...)
	if len(alerts) != 1 {
		t.Fatalf("got %d alerts, want 1", len(alerts))
	}
	for _, forbidden := range []string{"/interrupt", "--watchdog", "threshold", "operator"} {
		if strings.Contains(alerts[0].Guidance, forbidden) {
			t.Errorf("Guidance names the operator affordance %q: %q", forbidden, alerts[0].Guidance)
		}
	}
	for _, forbidden := range []string{"same result", "identical", "same argument"} {
		if strings.Contains(alerts[0].Guidance, forbidden) {
			t.Errorf("Guidance claims the calls are identical, which is false here: %q", alerts[0].Guidance)
		}
	}
	if !strings.Contains(alerts[0].Reason, "/interrupt") {
		t.Errorf("Reason should still name /interrupt for the operator: %q", alerts[0].Reason)
	}
}

// TestNewRepeatedToolNameSignal_Clamps: a threshold below 2 can never
// describe a run, matching NewRepeatedToolCallSignal's clamp.
func TestNewRepeatedToolNameSignal_Clamps(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ in, want int }{{0, 2}, {1, 2}, {2, 2}, {20, 20}} {
		if s := NewRepeatedToolNameSignal(tc.in, 0); s.Threshold != tc.want {
			t.Errorf("NewRepeatedToolNameSignal(%d).Threshold = %d, want %d", tc.in, s.Threshold, tc.want)
		}
	}
}

// TestNewDefaultWatchdog_WiresTheToolNameDetector is the acceptance
// gate that the signal is reachable from the shipped default — a
// detector nobody constructs is inert — and that adding it did not turn
// a reworded loop into two alerts.
func TestNewDefaultWatchdog_WiresTheToolNameDetector(t *testing.T) {
	t.Parallel()

	w := NewDefaultWatchdog()
	for _, c := range reworded("mark_task_done", DefaultToolNameRun) {
		w.ObserveToolCall(c)
	}
	alerts := w.Check()
	if len(alerts) != 1 || alerts[0].Signal != "repeated-tool-name" {
		t.Fatalf("default watchdog alerts = %+v, want exactly one repeated-tool-name", alerts)
	}
	// Wiring it into the default set is what makes the severity load-
	// bearing: this is the alert an operator running --watchdog=enforce
	// actually gets, and Critical here would halt them.
	if alerts[0].Severity != SeverityWarn {
		t.Errorf("the shipped default emits %q for a name-keyed run; enforce mode would halt on it", alerts[0].Severity)
	}
}

// TestNewDefaultWatchdog_OnlyProvableLoopsHalt pins the severity split
// across the whole default set rather than one signal at a time. Under
// --watchdog=enforce a Critical alert refuses the agent's next turn, so
// which detectors may halt is a product decision, not a per-file one:
// only the three that compare arguments can prove the agent is learning
// nothing. A new signal defaulting to Critical should land here first.
func TestNewDefaultWatchdog_OnlyProvableLoopsHalt(t *testing.T) {
	t.Parallel()

	mayHalt := map[string]bool{
		"repeated-tool-call": true,
		"alternating-cycle":  true,
		"dominant-tool-call": true,
	}
	for _, s := range NewDefaultWatchdog().signals {
		name := s.Name()
		// Drive each signal past its own threshold with the sequence it
		// is built for; the name-keyed run trips the coarse detectors
		// too, which is exactly the overlap we want represented.
		var alerts []Alert
		alerts = append(alerts, feed(s, reworded(name, DefaultToolNameRun*2)...)...)
		s.Reset()
		identical := make([]ToolCall, 0, DefaultToolNameRun*2)
		for i := 0; i < DefaultToolNameRun*2; i++ {
			identical = append(identical, call("read_file", `{"path":"a.go"}`))
		}
		alerts = append(alerts, feed(s, identical...)...)

		for _, a := range alerts {
			if a.Severity == SeverityCritical && !mayHalt[a.Signal] {
				t.Errorf("%s emits Critical, so it halts the agent under --watchdog=enforce; add it to mayHalt deliberately or make it Warn", a.Signal)
			}
		}
	}
}
