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

// call builds a tool call with distinct arguments, so a run of them
// cannot be mistaken for the args-keyed detectors' territory — this
// signal is supposed to see the run regardless of what the calls say.
func call(name string, i int) watchdog.ToolCall {
	return watchdog.ToolCall{Name: name, Args: fmt.Sprintf(`{"i":%d}`, i)}
}

func TestToolsWithoutTextSignal(t *testing.T) {
	t.Parallel()

	// A trace step: either a call or something the model said.
	type step struct {
		text string // non-empty means the model spoke
	}
	said := func(s string) step { return step{text: s} }
	called := step{}

	tests := []struct {
		name  string
		steps []step
		want  int // alerts
	}{
		{
			name:  "below threshold stays silent",
			steps: repeatStep(called, 4),
			want:  0,
		},
		{
			name:  "exactly the threshold trips",
			steps: repeatStep(called, 5),
			want:  1,
		},
		{
			// The whole contract: one sentence and the count starts over.
			// A model that reports between sweeps is doing the thing the
			// signal wants, and must never be told otherwise.
			name:  "a sentence clears the run",
			steps: append(append(repeatStep(called, 4), said("Checking the deployment next.")), repeatStep(called, 4)...),
			want:  0,
		},
		{
			// Whitespace is not speech. A provider that emits an empty
			// text part between calls must not be able to launder a
			// silent run into a reported one.
			name:  "whitespace does not clear the run",
			steps: append(append(repeatStep(called, 4), said("  \n\t ")), repeatStep(called, 1)...),
			want:  1,
		},
		{
			name:  "one alert per run, not one per call past it",
			steps: repeatStep(called, 20),
			want:  1,
		},
		{
			// After a trip, speech re-arms the signal: the second silent
			// run is a second observation and deserves a second alert.
			name: "speech re-arms after a trip",
			steps: append(append(repeatStep(called, 5), said("Here is what I found so far.")),
				repeatStep(called, 5)...),
			want: 2,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := watchdog.NewToolsWithoutTextSignal(5)
			alerts := 0
			for i, st := range tc.steps {
				var a *watchdog.Alert
				if st.text != "" {
					a = s.ObserveAssistantText(st.text)
				} else {
					a = s.ObserveToolCall(call("gke_get_k8s_resource", i))
				}
				if a != nil {
					alerts++
				}
			}
			if alerts != tc.want {
				t.Fatalf("got %d alerts, want %d", alerts, tc.want)
			}
		})
	}
}

func repeatStep[T any](s T, n int) []T {
	out := make([]T, 0, n)
	for range n {
		out = append(out, s)
	}
	return out
}

// TestToolsWithoutTextSignal_Alert pins the parts of the alert other
// components read: the signal ID that appears in the operator log and
// the metric attribute, the severity that decides whether enforce mode
// halts, and the presence of the looping tool's name in the text a
// model is shown under --watchdog=feedback.
func TestToolsWithoutTextSignal_Alert(t *testing.T) {
	t.Parallel()

	s := watchdog.NewToolsWithoutTextSignal(3)
	var got *watchdog.Alert
	for i := range 3 {
		if a := s.ObserveToolCall(call("gke_get_k8s_logs", i)); a != nil {
			got = a
		}
	}
	if got == nil {
		t.Fatal("three calls with no text raised no alert")
	}
	if got.Signal != "tools-without-text" {
		t.Errorf("Signal = %q, want tools-without-text", got.Signal)
	}
	// Warn is the decision this signal's whole doc argues for: it has
	// compared nothing, so it must never halt a working agent. If this
	// flips to Critical, text.go's Severity paragraph is the thing that
	// has to be re-litigated first.
	if got.Severity != watchdog.SeverityWarn {
		t.Errorf("Severity = %q, want warn — see text.go on why this signal must not halt", got.Severity)
	}
	if !strings.Contains(got.Guidance, "gke_get_k8s_logs") {
		t.Errorf("guidance does not name the tool being called:\n%s", got.Guidance)
	}
	if !strings.Contains(got.Reason, "3 tool calls") {
		t.Errorf("reason does not state the run length:\n%s", got.Reason)
	}
}

// TestToolsWithoutTextSignal_ClampsThreshold covers the degenerate
// construction. At 1 every tool call an agent ever makes would alert,
// because a call necessarily precedes the text reporting on it.
func TestToolsWithoutTextSignal_ClampsThreshold(t *testing.T) {
	t.Parallel()

	s := watchdog.NewToolsWithoutTextSignal(1)
	if a := s.ObserveToolCall(call("read_file", 0)); a != nil {
		t.Fatalf("a single call alerted at threshold 1; want the clamp to 2")
	}
	if a := s.ObserveToolCall(call("read_file", 1)); a == nil {
		t.Fatal("two calls did not alert at the clamped threshold of 2")
	}
}

// TestToolsWithoutTextSignal_ResetClearsTheRun covers the operator
// lever (/guardrail reset) and the turn-cut scrub, which both route
// through Signal.Reset.
func TestToolsWithoutTextSignal_ResetClearsTheRun(t *testing.T) {
	t.Parallel()

	s := watchdog.NewToolsWithoutTextSignal(3)
	s.ObserveToolCall(call("a", 0))
	s.ObserveToolCall(call("b", 1))
	s.Reset()
	if a := s.ObserveToolCall(call("c", 2)); a != nil {
		t.Fatal("the run survived a Reset")
	}
}

// TestToolsWithoutTextIsWiredIntoTheDefaultSet is the wiring test. A
// signal that exists but is not in NewDefaultWatchdog protects nobody,
// and the default set is what every core-agent process runs.
//
// It also proves the text half of the wiring, which is the part a unit
// test on the signal cannot reach: DefaultWatchdog has to implement
// AssistantTextObserver and fan text across its signals, or the run
// would never clear in production no matter what the model said.
func TestToolsWithoutTextIsWiredIntoTheDefaultSet(t *testing.T) {
	t.Parallel()

	w := watchdog.NewDefaultWatchdog()
	obs, ok := any(w).(watchdog.AssistantTextObserver)
	if !ok {
		t.Fatal("DefaultWatchdog does not implement AssistantTextObserver")
	}

	// Distinct tools AND distinct args, so nothing else in the default
	// set can claim this trace: no repeat, no cycle, no dominance, no
	// repeated name, no failures, no no-ops, no repeated payload.
	for i := range watchdog.DefaultToolsWithoutText {
		w.ObserveToolCall(call(fmt.Sprintf("tool_%d", i), i))
	}
	alerts := w.Check()
	if len(alerts) != 1 {
		t.Fatalf("got %d alerts from %d distinct silent calls, want exactly tools-without-text: %+v",
			len(alerts), watchdog.DefaultToolsWithoutText, alerts)
	}
	if alerts[0].Signal != "tools-without-text" {
		t.Fatalf("alert came from %q, want tools-without-text", alerts[0].Signal)
	}

	// And the same trace with the model talking halfway through says
	// nothing at all.
	w.Reset()
	for i := range watchdog.DefaultToolsWithoutText {
		if i == watchdog.DefaultToolsWithoutText/2 {
			obs.ObserveAssistantText("Halfway: the pods are crash-looping on an image pull.")
		}
		w.ObserveToolCall(call(fmt.Sprintf("tool_%d", i), i))
	}
	if alerts := w.Check(); len(alerts) != 0 {
		t.Fatalf("the model spoke mid-run and the watchdog alerted anyway: %+v", alerts)
	}
}
