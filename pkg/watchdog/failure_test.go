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

func fail(name, msg string) ToolResult { return ToolResult{Name: name, Error: msg} }
func ok(name string) ToolResult        { return ToolResult{Name: name} }

// feed pushes results through the signal and returns every alert it
// emitted, so a test can assert on count as well as content.
func feedResults(s *ToolFailureStreakSignal, results ...ToolResult) []Alert {
	var out []Alert
	for _, r := range results {
		if a := s.ObserveToolResult(r); a != nil {
			out = append(out, *a)
		}
	}
	return out
}

func TestToolFailureStreak_TripsAtTheThreshold(t *testing.T) {
	t.Parallel()
	s := NewToolFailureStreakSignal(3)

	if got := feedResults(s, fail("gke_get_pod", "PermissionDenied"), fail("gke_list_events", "PermissionDenied")); len(got) != 0 {
		t.Fatalf("alerts after 2 failures = %d, want 0 (below threshold)", len(got))
	}
	got := feedResults(s, fail("gke_get_deployment", "PermissionDenied"))
	if len(got) != 1 {
		t.Fatalf("alerts at the threshold = %d, want 1", len(got))
	}
	if got[0].Signal != "tool-failure-streak" {
		t.Errorf("signal = %q, want tool-failure-streak", got[0].Signal)
	}
	if !strings.Contains(got[0].Reason, "gke_get_pod") || !strings.Contains(got[0].Reason, "PermissionDenied") {
		t.Errorf("Reason = %q, want the tool names and the last error", got[0].Reason)
	}
}

// The whole point is the model-facing half: an unattended daemon has
// no operator reading Reason, and the model is the only party that can
// stop asserting things it hasn't observed.
func TestToolFailureStreak_GuidanceForbidsUnverifiedConclusions(t *testing.T) {
	t.Parallel()
	s := NewToolFailureStreakSignal(2)
	got := feedResults(s, fail("kubectl_get", "unreachable"), fail("kubectl_get", "unreachable"))
	if len(got) != 1 {
		t.Fatalf("alerts = %d, want 1", len(got))
	}
	g := got[0].Guidance
	if g == "" {
		t.Fatal("Guidance is empty; feedback mode would fall back to operator-facing Reason")
	}
	for _, want := range []string{"verified", "resolved", "could not check"} {
		if !strings.Contains(g, want) {
			t.Errorf("Guidance = %q, want it to mention %q", g, want)
		}
	}
	// Model-facing text must not offer operator-only levers.
	for _, forbidden := range []string{"/interrupt", "--watchdog", "operator"} {
		if strings.Contains(g, forbidden) {
			t.Errorf("Guidance names the operator affordance %q; the reader here is the model", forbidden)
		}
	}
}

// Warn, never Critical: under --watchdog=enforce (the unattended
// default since #642) a Critical alert halts the agent, and halting
// three denials into a legitimate RBAC probe makes the backstop the
// outage.
func TestToolFailureStreak_IsAdvisoryNotAHalt(t *testing.T) {
	t.Parallel()
	s := NewToolFailureStreakSignal(2)
	got := feedResults(s, fail("a", "boom"), fail("b", "boom"))
	if len(got) != 1 {
		t.Fatalf("alerts = %d, want 1", len(got))
	}
	if got[0].Severity != SeverityWarn {
		t.Errorf("severity = %q, want warn — a failure streak must not halt an enforce-mode agent", got[0].Severity)
	}
}

// One success is evidence, and evidence is the thing being counted.
func TestToolFailureStreak_ASuccessResetsTheRun(t *testing.T) {
	t.Parallel()
	s := NewToolFailureStreakSignal(3)
	got := feedResults(s,
		fail("a", "boom"), fail("b", "boom"),
		ok("c"),
		fail("d", "boom"), fail("e", "boom"),
	)
	if len(got) != 0 {
		t.Fatalf("alerts = %d, want 0: the success in the middle broke the run", len(got))
	}
	if got := feedResults(s, fail("f", "boom")); len(got) != 1 {
		t.Errorf("alerts after the post-success run reaches the threshold = %d, want 1", len(got))
	}
}

// Operators want one notice per stuck pattern, not one per failure
// past it — the same contract RepeatedToolCallSignal keeps, and under
// feedback mode a re-emitting signal is a prompt leak.
func TestToolFailureStreak_EmitsOncePerStreak(t *testing.T) {
	t.Parallel()
	s := NewToolFailureStreakSignal(2)
	got := feedResults(s, fail("a", "x"), fail("a", "x"), fail("a", "x"), fail("a", "x"), fail("a", "x"))
	if len(got) != 1 {
		t.Errorf("alerts = %d, want exactly 1 for one streak", len(got))
	}
}

func TestToolFailureStreak_ReArmsAfterASuccess(t *testing.T) {
	t.Parallel()
	s := NewToolFailureStreakSignal(2)
	if got := feedResults(s, fail("a", "x"), fail("a", "x")); len(got) != 1 {
		t.Fatalf("first streak alerts = %d, want 1", len(got))
	}
	if got := feedResults(s, ok("a"), fail("a", "x"), fail("a", "x")); len(got) != 1 {
		t.Errorf("second streak alerts = %d, want 1 — the tripped guard must clear on success", len(got))
	}
}

func TestToolFailureStreak_ClampsDegenerateThreshold(t *testing.T) {
	t.Parallel()
	s := NewToolFailureStreakSignal(1)
	if s.Threshold != 2 {
		t.Fatalf("threshold = %d, want it clamped to 2", s.Threshold)
	}
	if got := feedResults(s, fail("a", "x")); len(got) != 0 {
		t.Error("a single failed call must not alert; every turn has one eventually")
	}
}

func TestToolFailureStreak_ResetClearsState(t *testing.T) {
	t.Parallel()
	s := NewToolFailureStreakSignal(2)
	feedResults(s, fail("a", "x"))
	s.Reset()
	if got := feedResults(s, fail("a", "x")); len(got) != 0 {
		t.Error("Reset did not clear the in-flight streak")
	}
}

func TestToolFailureStreak_IgnoresToolCalls(t *testing.T) {
	t.Parallel()
	s := NewToolFailureStreakSignal(2)
	for range 10 {
		if a := s.ObserveToolCall(ToolCall{Name: "a", Args: "{}"}); a != nil {
			t.Fatal("calls carry no outcome; this signal must not read them")
		}
	}
}

// Repetition is the loop detectors' job. This alert is about outcomes,
// so three failures of one tool should read as one name.
func TestToolFailureStreak_CollapsesRepeatedNames(t *testing.T) {
	t.Parallel()
	s := NewToolFailureStreakSignal(3)
	got := feedResults(s, fail("gke_get_pod", "x"), fail("gke_get_pod", "x"), fail("gke_get_pod", "x"))
	if len(got) != 1 {
		t.Fatalf("alerts = %d, want 1", len(got))
	}
	if strings.Count(got[0].Reason, "gke_get_pod") != 1 {
		t.Errorf("Reason = %q, want the repeated name collapsed to one mention", got[0].Reason)
	}
}

func TestToolResult_FailedReadsTheErrorField(t *testing.T) {
	t.Parallel()
	if (ToolResult{Name: "a"}).Failed() {
		t.Error("empty Error must read as success")
	}
	if !(ToolResult{Name: "a", Error: "boom"}).Failed() {
		t.Error("non-empty Error must read as failure")
	}
}

// DefaultWatchdog wiring.

func TestDefaultWatchdog_ObservesResultsAndRaisesTheStreakAlert(t *testing.T) {
	t.Parallel()
	w := NewDefaultWatchdog()
	for range DefaultFailureStreak {
		w.ObserveToolResult(ToolResult{Name: "gke_get_pod", Error: "PermissionDenied"})
	}
	alerts := w.Check()
	if len(alerts) != 1 || alerts[0].Signal != "tool-failure-streak" {
		t.Fatalf("alerts = %+v, want one tool-failure-streak alert", alerts)
	}
	if got := w.Check(); got != nil {
		t.Errorf("second Check = %+v, want nil (buffer drains)", got)
	}
}

// The default set must actually include the signal — a constructor
// that quietly drops it would leave every deployment unprotected while
// the docs say otherwise.
func TestNewDefaultWatchdog_IncludesTheFailureStreakSignal(t *testing.T) {
	t.Parallel()
	w := NewDefaultWatchdog()
	var found bool
	for _, s := range w.signals {
		if s.Name() == "tool-failure-streak" {
			found = true
		}
	}
	if !found {
		t.Fatal("default signal set is missing tool-failure-streak")
	}
	if _, ok := any(w).(ToolResultObserver); !ok {
		t.Error("DefaultWatchdog must implement ToolResultObserver or the agent never feeds it results")
	}
}

// A watchdog composed only of call-reading signals must tolerate
// result observation rather than panic — the extension is optional at
// both levels.
func TestDefaultWatchdog_ResultsAreIgnoredByCallOnlySignals(t *testing.T) {
	t.Parallel()
	w := &DefaultWatchdog{signals: []Signal{NewRepeatedToolCallSignal(2)}}
	for range 10 {
		w.ObserveToolResult(ToolResult{Name: "a", Error: "boom"})
	}
	if got := w.Check(); got != nil {
		t.Errorf("alerts = %+v, want none: no wired signal reads results", got)
	}
}

func TestDefaultWatchdog_ResetClearsStreakState(t *testing.T) {
	t.Parallel()
	w := NewDefaultWatchdog()
	w.ObserveToolResult(ToolResult{Name: "a", Error: "boom"})
	w.ObserveToolResult(ToolResult{Name: "a", Error: "boom"})
	w.Reset()
	w.ObserveToolResult(ToolResult{Name: "a", Error: "boom"})
	if got := w.Check(); got != nil {
		t.Errorf("alerts = %+v, want none: Reset must clear the in-flight streak", got)
	}
}

func TestFormatFeedback_CarriesTheStreakGuidance(t *testing.T) {
	t.Parallel()
	s := NewToolFailureStreakSignal(2)
	alerts := feedResults(s, fail("gke_get_pod", "PermissionDenied"), fail("gke_get_pod", "PermissionDenied"))
	block := FormatFeedback(alerts)
	if !strings.Contains(block, FeedbackHeader) || !strings.Contains(block, "tool-failure-streak") {
		t.Fatalf("feedback block = %q, want the header and the signal id", block)
	}
	if !strings.Contains(block, alerts[0].Guidance) {
		t.Error("feedback block dropped the model-facing Guidance")
	}
	if strings.Contains(block, alerts[0].Reason) {
		t.Error("feedback block used operator-facing Reason; Guidance is set, so the fallback must not fire")
	}
}
