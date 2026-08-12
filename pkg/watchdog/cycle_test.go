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

// call is shorthand for the observations these tests feed in.
func call(name, args string) ToolCall { return ToolCall{Name: name, Args: args} }

// feed observes each call in order and returns every alert raised.
func feed(s Signal, calls ...ToolCall) []Alert {
	var out []Alert
	for _, c := range calls {
		if a := s.ObserveToolCall(c); a != nil {
			out = append(out, *a)
		}
	}
	return out
}

// TestAlternatingCycleSignal_TripsOnTheUATShape is the acceptance test
// for #649's first half. The loop that survived an operator "stop" and
// /interrupt during the PR #622 GKE UAT was list_agents → check_agent
// repeated indefinitely; the consecutive-repeat detector is structurally
// blind to it, because no call is ever followed by itself.
//
// Fails on pre-#649 code: the default watchdog had one signal and this
// sequence produced no alerts at all.
func TestAlternatingCycleSignal_TripsOnTheUATShape(t *testing.T) {
	t.Parallel()

	s := NewAlternatingCycleSignal(DefaultCycleMaxPeriod, DefaultCycleRepeats)
	alerts := feed(s,
		call("list_agents", "{}"),
		call("check_agent", `{"id":"a1"}`),
		call("list_agents", "{}"),
		call("check_agent", `{"id":"a1"}`),
		call("list_agents", "{}"),
		call("check_agent", `{"id":"a1"}`),
	)

	if len(alerts) != 1 {
		t.Fatalf("got %d alerts, want exactly 1: %+v", len(alerts), alerts)
	}
	got := alerts[0]
	if got.Signal != "alternating-tool-cycle" {
		t.Errorf("Signal = %q, want alternating-tool-cycle", got.Signal)
	}
	if got.Severity != SeverityCritical {
		t.Errorf("Severity = %q, want critical — an unbreakable loop is not advisory", got.Severity)
	}
	for _, want := range []string{"list_agents", "check_agent"} {
		if !strings.Contains(got.Reason, want) {
			t.Errorf("Reason omits %q, so an operator can't tell which loop tripped: %q", want, got.Reason)
		}
		if !strings.Contains(got.Guidance, want) {
			t.Errorf("Guidance omits %q: %q", want, got.Guidance)
		}
	}
}

// TestAlternatingCycleSignal_GuidanceIsModelFacing holds the #159
// two-reader split for the new signal: Guidance is injected into the
// model's context under --watchdog=feedback, so it must not spend
// tokens telling the model about controls only an operator has.
func TestAlternatingCycleSignal_GuidanceIsModelFacing(t *testing.T) {
	t.Parallel()

	s := NewAlternatingCycleSignal(2, 2)
	alerts := feed(s,
		call("a", "{}"), call("b", "{}"),
		call("a", "{}"), call("b", "{}"),
	)
	if len(alerts) != 1 {
		t.Fatalf("got %d alerts, want 1", len(alerts))
	}
	for _, forbidden := range []string{"/interrupt", "--max-turn-cost-usd", "Cost ceiling", "operator"} {
		if strings.Contains(alerts[0].Guidance, forbidden) {
			t.Errorf("Guidance names the operator affordance %q: %q", forbidden, alerts[0].Guidance)
		}
	}
	// The operator half keeps them.
	if !strings.Contains(alerts[0].Reason, "/interrupt") {
		t.Errorf("Reason should still name /interrupt for the operator: %q", alerts[0].Reason)
	}
}

// TestAlternatingCycleSignal_DoesNotTripOnLegitimateWork covers the
// false-positive side. A halt is expensive, so the shapes below must
// stay quiet.
func TestAlternatingCycleSignal_DoesNotTripOnLegitimateWork(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		calls []ToolCall
	}{
		{
			// Two laps is ordinary exploration: read, grep, read, grep.
			name: "two laps is not a cycle",
			calls: []ToolCall{
				call("read_file", `{"path":"a.go"}`), call("grep", `{"pattern":"x"}`),
				call("read_file", `{"path":"a.go"}`), call("grep", `{"pattern":"x"}`),
			},
		},
		{
			// Same tools, different arguments each lap — the agent is
			// working through a list, not spinning.
			name: "same tools with varying args",
			calls: []ToolCall{
				call("read_file", `{"path":"a.go"}`), call("grep", `{"pattern":"x"}`),
				call("read_file", `{"path":"b.go"}`), call("grep", `{"pattern":"y"}`),
				call("read_file", `{"path":"c.go"}`), call("grep", `{"pattern":"z"}`),
			},
		},
		{
			// A pure repeat is RepeatedToolCallSignal's alert. Raising a
			// second one for the same behavior doubles operator noise and
			// (under feedback) the injected tokens.
			name: "pure repeat belongs to the other signal",
			calls: []ToolCall{
				call("read_file", `{"path":"a.go"}`), call("read_file", `{"path":"a.go"}`),
				call("read_file", `{"path":"a.go"}`), call("read_file", `{"path":"a.go"}`),
				call("read_file", `{"path":"a.go"}`), call("read_file", `{"path":"a.go"}`),
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if alerts := feed(NewAlternatingCycleSignal(DefaultCycleMaxPeriod, DefaultCycleRepeats), tc.calls...); len(alerts) != 0 {
				t.Errorf("got %d alerts, want 0: %+v", len(alerts), alerts)
			}
		})
	}
}

// TestAlternatingCycleSignal_LongerPeriods checks period 3 and the
// MaxPeriod bound — a cycle longer than the scan window is a miss, not
// a crash, and the bound is what keeps the scan cheap.
func TestAlternatingCycleSignal_LongerPeriods(t *testing.T) {
	t.Parallel()

	t.Run("period 3 trips", func(t *testing.T) {
		t.Parallel()
		var calls []ToolCall
		for i := 0; i < 3; i++ {
			calls = append(calls, call("a", "{}"), call("b", "{}"), call("c", "{}"))
		}
		if alerts := feed(NewAlternatingCycleSignal(4, 3), calls...); len(alerts) != 1 {
			t.Fatalf("got %d alerts, want 1", len(alerts))
		}
	})

	t.Run("period beyond MaxPeriod is out of scope", func(t *testing.T) {
		t.Parallel()
		var calls []ToolCall
		for i := 0; i < 3; i++ {
			calls = append(calls, call("a", "{}"), call("b", "{}"), call("c", "{}"))
		}
		if alerts := feed(NewAlternatingCycleSignal(2, 3), calls...); len(alerts) != 0 {
			t.Errorf("got %d alerts, want 0 (period 3 with MaxPeriod 2)", len(alerts))
		}
	})
}

// TestAlternatingCycleSignal_OneAlertPerPattern mirrors the repeat
// detector's contract: an operator gets one notice per stuck pattern,
// and — since #159 — the model gets one injected block, not one per
// lap. A new pattern after the old one breaks alerts again.
func TestAlternatingCycleSignal_OneAlertPerPattern(t *testing.T) {
	t.Parallel()

	s := NewAlternatingCycleSignal(2, 2)
	first := feed(s,
		call("a", "{}"), call("b", "{}"),
		call("a", "{}"), call("b", "{}"),
		// Three more laps of the same pattern must stay silent.
		call("a", "{}"), call("b", "{}"),
		call("a", "{}"), call("b", "{}"),
	)
	if len(first) != 1 {
		t.Fatalf("same pattern raised %d alerts, want 1: %+v", len(first), first)
	}

	// Break the pattern, then establish a different one.
	second := feed(s,
		call("z", "{}"),
		call("c", "{}"), call("d", "{}"),
		call("c", "{}"), call("d", "{}"),
	)
	if len(second) != 1 {
		t.Fatalf("new pattern raised %d alerts, want 1: %+v", len(second), second)
	}
	if !strings.Contains(second[0].Reason, "c → d") {
		t.Errorf("second alert names the wrong sequence: %q", second[0].Reason)
	}
}

// TestAlternatingCycleSignal_CanonicalizesPaths is the intersection of
// the issue's two halves: a cycle whose args differ only in path
// spelling is still a cycle.
func TestAlternatingCycleSignal_CanonicalizesPaths(t *testing.T) {
	t.Parallel()

	s := NewAlternatingCycleSignal(2, 3)
	alerts := feed(s,
		call("read_file", `{"path":"main.go"}`), call("grep", `{"pattern":"x"}`),
		call("read_file", `{"path":"./main.go"}`), call("grep", `{"pattern":"x"}`),
		call("read_file", `{"path":"dir/../main.go"}`), call("grep", `{"pattern":"x"}`),
	)
	if len(alerts) != 1 {
		t.Fatalf("got %d alerts, want 1 — path spellings split the cycle", len(alerts))
	}
}

// TestAlternatingCycleSignal_Reset clears state at a logical session
// boundary, like every other signal.
func TestAlternatingCycleSignal_Reset(t *testing.T) {
	t.Parallel()

	s := NewAlternatingCycleSignal(2, 2)
	feed(s, call("a", "{}"), call("b", "{}"), call("a", "{}"))
	s.Reset()
	// Post-reset the half-built cycle is gone, so completing it takes a
	// full 2 laps again.
	if alerts := feed(s, call("b", "{}"), call("a", "{}")); len(alerts) != 0 {
		t.Errorf("state survived Reset: %+v", alerts)
	}
	if alerts := feed(s, call("b", "{}"), call("a", "{}")); len(alerts) != 1 {
		t.Errorf("got %d alerts after a fresh cycle, want 1", len(alerts))
	}
}

// TestNewAlternatingCycleSignal_ClampsDegenerateTuning keeps a caller
// from configuring a signal that fires on everything: period 1 is the
// repeat detector's job and a single lap is not a cycle.
func TestNewAlternatingCycleSignal_ClampsDegenerateTuning(t *testing.T) {
	t.Parallel()

	s := NewAlternatingCycleSignal(0, 1)
	if s.MaxPeriod != 2 {
		t.Errorf("MaxPeriod = %d, want 2", s.MaxPeriod)
	}
	if s.Cycles != 2 {
		t.Errorf("Cycles = %d, want 2", s.Cycles)
	}
	if alerts := feed(s, call("a", "{}"), call("b", "{}")); len(alerts) != 0 {
		t.Errorf("one lap tripped a clamped signal: %+v", alerts)
	}
}

// TestNewDefaultWatchdog_WiresBothLoopDetectors is the acceptance gate
// that the new signal is actually reachable from the shipped default —
// a detector nobody constructs is the "wired but inert" failure this
// milestone exists to remove.
func TestNewDefaultWatchdog_WiresBothLoopDetectors(t *testing.T) {
	t.Parallel()

	t.Run("alternating cycle", func(t *testing.T) {
		t.Parallel()
		w := NewDefaultWatchdog()
		for i := 0; i < 3; i++ {
			w.ObserveToolCall(call("list_agents", "{}"))
			w.ObserveToolCall(call("check_agent", `{"id":"a1"}`))
		}
		alerts := w.Check()
		if len(alerts) != 1 || alerts[0].Signal != "alternating-tool-cycle" {
			t.Fatalf("default watchdog alerts = %+v, want one alternating-tool-cycle", alerts)
		}
	})

	t.Run("path-variant repeat", func(t *testing.T) {
		t.Parallel()
		w := NewDefaultWatchdog()
		for _, p := range []string{"main.go", "./main.go", "/workspace/main.go", "main.go", "dir/../main.go"} {
			w.ObserveToolCall(call("read_file", `{"path":"`+p+`"}`))
		}
		alerts := w.Check()
		if len(alerts) != 1 || alerts[0].Signal != "repeated-tool-call" {
			t.Fatalf("default watchdog alerts = %+v, want one repeated-tool-call", alerts)
		}
	})
}
