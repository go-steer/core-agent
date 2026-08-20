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
	"testing"
)

// defaultDominant is the signal as NewDefaultWatchdog wires it. Tests
// that are about the shipped tuning use this; tests about the
// mechanism construct their own.
func defaultDominant() *DominantToolCallSignal {
	return NewDominantToolCallSignal(DefaultDominantWindow, DefaultDominantThreshold,
		DefaultDominantDeferRun, DefaultDominantDeferPeriod)
}

// interleaved renders the #702 shape: runs of `run` identical calls
// separated by a single different one, `laps` times over.
func interleaved(run, laps int) []ToolCall {
	var out []ToolCall
	for i := 0; i < laps; i++ {
		for j := 0; j < run; j++ {
			out = append(out, call("gke_list_clusters", "{}"))
		}
		out = append(out, call("gke_get_namespace", `{"name":"default"}`))
	}
	return out
}

// TestDominantToolCallSignal_TripsOnTheInterleavedLoop is the
// acceptance test for #702. The GKE UAT loop was one call dominating
// with occasional others wedged in; runs shorter than the repeat
// detector's threshold reset its count from zero, and the cycle
// detector hands nearly-uniform blocks back to it.
//
// Fails on pre-#702 code: neither shipped detector raises anything on
// this sequence at all, which the sibling assertions below pin down.
func TestDominantToolCallSignal_TripsOnTheInterleavedLoop(t *testing.T) {
	t.Parallel()

	calls := interleaved(4, 3) // a a a a X a a a a X a a a a X
	alerts := feed(defaultDominant(), calls...)

	if len(alerts) != 1 {
		t.Fatalf("got %d alerts, want exactly 1: %+v", len(alerts), alerts)
	}
	got := alerts[0]
	if got.Signal != "dominant-tool-call" {
		t.Errorf("Signal = %q, want dominant-tool-call", got.Signal)
	}
	if got.Severity != SeverityCritical {
		t.Errorf("Severity = %q, want critical — the same non-progress the other loop detectors halt on", got.Severity)
	}
	if !strings.Contains(got.Reason, "gke_list_clusters") {
		t.Errorf("Reason omits the dominating tool, so an operator can't tell what looped: %q", got.Reason)
	}
	if !strings.Contains(got.Guidance, "gke_list_clusters") {
		t.Errorf("Guidance omits the dominating tool: %q", got.Guidance)
	}

	// The gap this signal fills: on the identical sequence, neither
	// existing loop detector says a word.
	if a := feed(NewRepeatedToolCallSignal(DefaultRepeatThreshold), calls...); len(a) != 0 {
		t.Errorf("the repeat detector already covered this shape, so #702 would be a tuning change, not a gap: %+v", a)
	}
	if a := feed(NewAlternatingCycleSignal(DefaultCycleMaxPeriod, DefaultCycleRepeats), calls...); len(a) != 0 {
		t.Errorf("the cycle detector already covered this shape: %+v", a)
	}
}

// TestDominantToolCallSignal_ConvergesInsideOneWindow pins the point of
// the change: the alert has to arrive while the loop is still cheap.
// The UAT ran 22 calls over two minutes before the repeat detector
// finally caught a run of five.
func TestDominantToolCallSignal_ConvergesInsideOneWindow(t *testing.T) {
	t.Parallel()

	s := defaultDominant()
	for i, c := range interleaved(4, 4) {
		if a := s.ObserveToolCall(c); a != nil {
			if i+1 > DefaultDominantWindow {
				t.Errorf("alerted on call %d, want no later than the first full window (%d)", i+1, DefaultDominantWindow)
			}
			return
		}
	}
	t.Fatal("never alerted on a 20-call interleaved loop")
}

// TestDominantToolCallSignal_DefersToTheRepeatDetector holds the
// one-behavior-one-alert invariant the package already commits to. A
// consecutive run is RepeatedToolCallSignal's; under
// --watchdog=feedback a duplicate alert is duplicated prompt text, not
// just a duplicated log line.
func TestDominantToolCallSignal_DefersToTheRepeatDetector(t *testing.T) {
	t.Parallel()

	a := call("read_file", `{"path":"a.txt"}`)
	// A run of five inside a window that is NOT a clean short cycle,
	// so the run deference is the only thing that can keep this quiet.
	// A pure run would be covered by the cycle deference too (a
	// uniform window repeats at every period), which would hide a
	// broken run check.
	mixed := []ToolCall{
		a, a, a, a, a,
		call("grep", `{"q":"x"}`),
		a, a, a,
		call("grep", `{"q":"y"}`),
		a, a,
	}
	if alerts := feed(defaultDominant(), mixed...); len(alerts) != 0 {
		t.Errorf("a run of %d raised a density alert on top of the repeat detector's: %+v", DefaultRepeatThreshold, alerts)
	}
	// Sanity: the repeat detector really does own this sequence.
	if got := feed(NewRepeatedToolCallSignal(DefaultRepeatThreshold), mixed...); len(got) != 1 {
		t.Fatalf("repeat detector alerts = %+v, want exactly 1 — otherwise the deference above is a hole", got)
	}

	// With deference disabled — the shape an operator wiring this
	// signal alone wants — the same window does trip.
	s := NewDominantToolCallSignal(DefaultDominantWindow, DefaultDominantThreshold, 0, 0)
	if alerts := feed(s, mixed...); len(alerts) != 1 {
		t.Errorf("deferRun=0 got %d alerts, want 1 — the deference must be a choice, not the mechanism", len(alerts))
	}

	// A pure run is the same story with nothing else going on.
	var pure []ToolCall
	for i := 0; i < DefaultDominantWindow; i++ {
		pure = append(pure, a)
	}
	if alerts := feed(defaultDominant(), pure...); len(alerts) != 0 {
		t.Errorf("a pure run raised a density alert on top of the repeat detector's: %+v", alerts)
	}
}

// TestDominantToolCallSignal_DefersToTheCycleDetector is the same
// invariant on the other side. a → a → b reads as both a dominant call
// (8 of 12) and a 3-call cycle; the cycle detector reaches it first,
// at nine calls rather than twelve.
func TestDominantToolCallSignal_DefersToTheCycleDetector(t *testing.T) {
	t.Parallel()

	var calls []ToolCall
	for i := 0; i < 4; i++ {
		calls = append(calls, call("a", "{}"), call("a", "{}"), call("b", "{}"))
	}
	if alerts := feed(defaultDominant(), calls...); len(alerts) != 0 {
		t.Errorf("a clean 3-call cycle raised a density alert on top of the cycle detector's: %+v", alerts)
	}
	// Sanity: the cycle detector really does own this sequence.
	if a := feed(NewAlternatingCycleSignal(DefaultCycleMaxPeriod, DefaultCycleRepeats), calls...); len(a) != 1 {
		t.Fatalf("cycle detector alerts = %+v, want exactly 1 — otherwise the deference above is a hole", a)
	}

	// A period the cycle detector does not scan is not deferred: this
	// is exactly the case the density signal exists for.
	var long []ToolCall
	for i := 0; i < 3; i++ {
		for j := 0; j < 4; j++ {
			long = append(long, call("a", "{}"))
		}
		long = append(long, call("b", "{}"))
	}
	if alerts := feed(defaultDominant(), long...); len(alerts) != 1 {
		t.Errorf("a period-5 block got %d alerts, want 1 — it is past MaxPeriod, so nothing else covers it", len(alerts))
	}
}

// TestDominantToolCallSignal_DoesNotTripOnLegitimateWork covers the
// false-positive side. A Critical alert halts the agent under
// --watchdog=enforce, so ordinary work must stay quiet.
func TestDominantToolCallSignal_DoesNotTripOnLegitimateWork(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		calls []ToolCall
	}{
		{
			name: "varied exploration",
			calls: []ToolCall{
				call("read_file", `{"path":"a.go"}`), call("grep", `{"q":"foo"}`),
				call("read_file", `{"path":"b.go"}`), call("grep", `{"q":"bar"}`),
				call("read_file", `{"path":"c.go"}`), call("grep", `{"q":"baz"}`),
				call("read_file", `{"path":"d.go"}`), call("grep", `{"q":"qux"}`),
				call("edit_file", `{"path":"a.go"}`), call("run_tests", "{}"),
				call("read_file", `{"path":"e.go"}`), call("grep", `{"q":"quux"}`),
			},
		},
		{
			name: "the same tool with different arguments each time",
			calls: []ToolCall{
				call("read_file", `{"path":"1.go"}`), call("read_file", `{"path":"2.go"}`),
				call("read_file", `{"path":"3.go"}`), call("read_file", `{"path":"4.go"}`),
				call("read_file", `{"path":"5.go"}`), call("read_file", `{"path":"6.go"}`),
				call("read_file", `{"path":"7.go"}`), call("read_file", `{"path":"8.go"}`),
				call("read_file", `{"path":"9.go"}`), call("read_file", `{"path":"10.go"}`),
				call("read_file", `{"path":"11.go"}`), call("read_file", `{"path":"12.go"}`),
			},
		},
		{
			name: "one call is frequent but not dominant",
			calls: []ToolCall{
				call("poll", "{}"), call("a", "{}"), call("poll", "{}"), call("b", "{}"),
				call("poll", "{}"), call("c", "{}"), call("poll", "{}"), call("d", "{}"),
				call("poll", "{}"), call("e", "{}"), call("poll", "{}"), call("f", "{}"),
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if alerts := feed(defaultDominant(), tc.calls...); len(alerts) != 0 {
				t.Errorf("halted legitimate work: %+v", alerts)
			}
		})
	}
}

// TestDominantToolCallSignal_OneAlertPerCluster: the alert fires once
// while the pattern persists. A signal that re-emits every call past
// the threshold is a prompt leak under --watchdog=feedback.
func TestDominantToolCallSignal_OneAlertPerCluster(t *testing.T) {
	t.Parallel()

	if alerts := feed(defaultDominant(), interleaved(4, 6)...); len(alerts) != 1 {
		t.Errorf("got %d alerts over 30 calls of one loop, want 1: %+v", len(alerts), alerts)
	}
}

// TestDominantToolCallSignal_ReArmsAfterActivityDiversifies: state
// persists across turns and only Reset clears it, so a signal that
// never re-arms goes permanently silent after one loop in a long
// session.
func TestDominantToolCallSignal_ReArmsAfterActivityDiversifies(t *testing.T) {
	t.Parallel()

	s := defaultDominant()
	if alerts := feed(s, interleaved(4, 3)...); len(alerts) != 1 {
		t.Fatalf("first loop got %d alerts, want 1", len(alerts))
	}
	// A full window of unrelated work flushes the pattern out.
	for i := 0; i < DefaultDominantWindow; i++ {
		if a := s.ObserveToolCall(call("edit_file", `{"path":"f`+string(rune('a'+i))+`.go"}`)); a != nil {
			t.Fatalf("productive work alerted: %+v", a)
		}
	}
	if alerts := feed(s, interleaved(4, 3)...); len(alerts) != 1 {
		t.Errorf("second loop got %d alerts, want 1 — the signal never re-armed", len(alerts))
	}
}

// TestDominantToolCallSignal_CanonicalizesPathArgs: an agent
// re-reading one file under three spellings is as stuck as one
// re-reading it under a single spelling.
func TestDominantToolCallSignal_CanonicalizesPathArgs(t *testing.T) {
	t.Parallel()

	// Irregular on purpose: a tidy spelling-rotation would be a clean
	// cycle, and the density signal defers those to the cycle detector.
	read := func(p string) ToolCall { return call("read_file", `{"path":"`+p+`"}`) }
	calls := []ToolCall{
		read("main.go"), read("./main.go"), call("grep", `{"q":"x"}`),
		read("main.go"), read("dir/../main.go"), read("main.go"), call("grep", `{"q":"x"}`),
		read("main.go"), read("main.go"), call("grep", `{"q":"y"}`),
		read("main.go"), read("./main.go"),
	}
	if alerts := feed(defaultDominant(), calls...); len(alerts) != 1 {
		t.Errorf("got %d alerts, want 1 — path spellings must not split the count: %+v", len(alerts), alerts)
	}

	// The counterfactual: read literally, the same window has no
	// spelling reaching the threshold, so canonicalization is what
	// makes the loop visible rather than incidental to it.
	counts := map[string]int{}
	for _, c := range calls {
		counts[c.Name+"\x00"+c.Args]++
	}
	for k, n := range counts {
		if n >= DefaultDominantThreshold {
			t.Fatalf("literal key %q already reaches %d — the test does not exercise canonicalization", k, n)
		}
	}
}

// TestDominantToolCallSignal_GuidanceIsModelFacing holds the #159
// two-reader split: Guidance is injected into the model's context
// under --watchdog=feedback, so it must not spend tokens on controls
// only an operator has.
func TestDominantToolCallSignal_GuidanceIsModelFacing(t *testing.T) {
	t.Parallel()

	alerts := feed(defaultDominant(), interleaved(4, 3)...)
	if len(alerts) != 1 {
		t.Fatalf("got %d alerts, want 1", len(alerts))
	}
	for _, forbidden := range []string{"/interrupt", "--max-turn-cost-usd", "Cost ceiling", "operator"} {
		if strings.Contains(alerts[0].Guidance, forbidden) {
			t.Errorf("Guidance names the operator affordance %q: %q", forbidden, alerts[0].Guidance)
		}
	}
	if !strings.Contains(alerts[0].Reason, "/interrupt") {
		t.Errorf("Reason should still name /interrupt for the operator: %q", alerts[0].Reason)
	}
}

// TestDominantToolCallSignal_ResetClears: Reset is what a logical
// session boundary calls, and a signal that keeps its window across
// one carries a dead pattern into new work.
func TestDominantToolCallSignal_ResetClears(t *testing.T) {
	t.Parallel()

	s := defaultDominant()
	feed(s, interleaved(4, 2)...)
	s.Reset()
	if len(s.history) != 0 || s.tripped != "" {
		t.Fatalf("Reset left state: history=%d tripped=%q", len(s.history), s.tripped)
	}
	// A partial loop that would have completed the count now can't.
	if alerts := feed(s, interleaved(4, 1)...); len(alerts) != 0 {
		t.Errorf("post-Reset alerts = %+v, want none — the pre-Reset window leaked", alerts)
	}
}

// TestDominantToolCallSignal_WindowIsBounded: the history is per-
// session state on a long-lived daemon, so it must not grow with the
// session.
func TestDominantToolCallSignal_WindowIsBounded(t *testing.T) {
	t.Parallel()

	s := defaultDominant()
	for i := 0; i < 500; i++ {
		s.ObserveToolCall(call("read_file", `{"path":"a.go"}`))
	}
	if len(s.history) != DefaultDominantWindow {
		t.Errorf("history = %d entries after 500 calls, want %d", len(s.history), DefaultDominantWindow)
	}

	// The raw args each entry retains are bounded too. Only the alert
	// text reads them, and it truncates to the same bound — a window of
	// multi-kilobyte JSON blobs held live for a session is a cost with
	// no reader.
	big := `{"blob":"` + strings.Repeat("x", 50_000) + `"}`
	s2 := defaultDominant()
	for i := 0; i < DefaultDominantWindow; i++ {
		s2.ObserveToolCall(call("run", big))
	}
	for _, c := range s2.history {
		if len(c.args) > argsRetained {
			t.Fatalf("entry retained %d bytes of args, want at most %d", len(c.args), argsRetained)
		}
	}
	// The counting key is deliberately not truncated: two calls that
	// differ only in the region a truncation elides must stay two
	// calls. truncate() keeps both ends and drops the middle, so the
	// distinguishing byte goes in the middle.
	blob := strings.Repeat("x", 25_000)
	mid := func(n string) string {
		return `{"pre":"` + blob + `","n":` + n + `,"post":"` + blob + `"}`
	}
	s3 := defaultDominant()
	for i := 0; i < 6; i++ {
		s3.ObserveToolCall(call("run", mid("1")))
		s3.ObserveToolCall(call("run", mid("2")))
	}
	if _, count := s3.dominant(); count != DefaultDominantWindow/2 {
		t.Errorf("dominant count = %d, want %d — a truncated key collapsed two distinct calls",
			count, DefaultDominantWindow/2)
	}
}

// TestNewDominantToolCallSignal_Clamps: a threshold above the window
// can never trip, which is a silent misconfiguration. Clamping turns
// it into a strict one.
func TestNewDominantToolCallSignal_Clamps(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name                   string
		window, threshold      int
		wantWindow, wantThresh int
	}{
		{"window below 2", 1, 1, 2, 2},
		{"threshold below 2", 12, 0, 12, 2},
		{"threshold above window", 6, 99, 6, 6},
		{"in range", 12, 8, 12, 8},
	} {
		s := NewDominantToolCallSignal(tc.window, tc.threshold, 0, 0)
		if s.Window != tc.wantWindow || s.Threshold != tc.wantThresh {
			t.Errorf("%s: got window=%d threshold=%d, want %d/%d",
				tc.name, s.Window, s.Threshold, tc.wantWindow, tc.wantThresh)
		}
	}
}

// TestNewDefaultWatchdog_WiresTheDensityDetector is the acceptance gate
// that the signal is reachable from the shipped default — a detector
// nobody constructs is inert — and that adding it did not turn the
// interleaved loop into two alerts.
func TestNewDefaultWatchdog_WiresTheDensityDetector(t *testing.T) {
	t.Parallel()

	w := NewDefaultWatchdog()
	for _, c := range interleaved(4, 3) {
		w.ObserveToolCall(c)
	}
	alerts := w.Check()
	if len(alerts) != 1 || alerts[0].Signal != "dominant-tool-call" {
		t.Fatalf("default watchdog alerts = %+v, want exactly one dominant-tool-call", alerts)
	}
}
