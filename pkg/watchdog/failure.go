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

// Tool-outcome observation and the tool-failure-streak signal (#639).
//
// Every signal before this one reads tool *calls*. The failure mode in
// #639 is invisible from calls alone: during the PR #622 GKE UAT an
// agent that could not reach its cluster at all reported the incident
// resolved — "everything is in tip-top shape" — with no tool call
// having verified anything. The calls looked normal. The results were
// the story.
//
// What this catches is deliberately narrow and objective: a run of
// tool calls that all came back as errors, with none succeeding in
// between. No prose is inspected. A detector that tried to recognize
// an over-confident *claim* would be a heuristic about English
// pretending to be a runtime guarantee — which is the exact defect
// this milestone exists to remove.
//
// So this closes the evidence half, not the honesty half: it tells a
// model that has been failing every call that it has verified nothing,
// at the point where it is most likely to start narrating instead of
// reporting. It does not, and cannot, detect a confident conclusion
// drawn from tools that all succeeded and said nothing useful.

package watchdog

import "fmt"

// DefaultFailureStreak is the number of consecutive failed tool calls
// that trips ToolFailureStreakSignal. Three is chosen to sit above
// ordinary exploration — a 404, a missing file, one RBAC denial — and
// below the point where a model has usually stopped gathering evidence
// and started composing an answer.
const DefaultFailureStreak = 3

// ToolResult is the outcome half of a tool invocation. Error is the
// tool's error text, empty for success — the ADK convention is a
// reserved "error" key inside FunctionResponse.Response, and the agent
// wiring flattens that to this field so the watchdog never has to know
// the provider's response shape.
type ToolResult struct {
	Name  string
	Error string

	// NoOp is the tool's own assertion that this invocation changed
	// nothing — the reserved "no_op" key in the response, opted into
	// per tool. It is deliberately not inferred: a signal that guessed
	// inertness from repetition would be the thing every other detector
	// in this package already does, and #905 is the proof that guessing
	// is what fails. See noop.go.
	//
	// A failed call is not a no-op. The two are independent claims and
	// a tool that sets both is reporting an error, which
	// ToolFailureStreakSignal already covers.
	NoOp bool
}

// Failed reports whether the call errored.
func (r ToolResult) Failed() bool { return r.Error != "" }

// ToolResultObserver is the optional half of Watchdog: an
// implementation that also wants to see tool *outcomes* implements it,
// and the agent feeds results through a type assertion.
//
// Deliberately not folded into Watchdog. That interface is documented
// as a plug-in point ("consumers can plug in their own
// implementation"), so widening it would break every third-party
// watchdog at a minor version to add one signal — and a custom
// watchdog that only counts calls stays perfectly valid.
type ToolResultObserver interface {
	ObserveToolResult(ToolResult)
}

// SignalResultObserver is the same extension one level down, for
// signals inside DefaultWatchdog. A Signal that doesn't implement it
// simply never sees results.
type SignalResultObserver interface {
	ObserveToolResult(ToolResult) *Alert
}

// ObserveToolResult fans a tool outcome across every wired signal that
// implements SignalResultObserver. Implements ToolResultObserver.
func (w *DefaultWatchdog) ObserveToolResult(tr ToolResult) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, s := range w.signals {
		ro, ok := s.(SignalResultObserver)
		if !ok {
			continue
		}
		if alert := ro.ObserveToolResult(tr); alert != nil {
			w.alerts = append(w.alerts, *alert)
		}
	}
}

// ToolFailureStreakSignal trips when Threshold tool calls in a row all
// return errors. Any successful call resets the run — the point is
// "this agent currently has no verified evidence," and one success is
// evidence.
//
// Severity is Warn, not Critical, and that is a decision rather than
// an oversight. Under --watchdog=enforce a Critical alert halts the
// agent, and since #642 enforce is the default for unattended runs;
// halting a daemon three denials into a legitimate RBAC probe would
// make the backstop the outage. A failure streak is an evidence
// problem, so it goes where evidence problems belong — the operator
// log, and (under --watchdog=feedback) the model's own next turn.
// Runaway *behavior* is already Critical via the loop detectors.
type ToolFailureStreakSignal struct {
	Threshold int

	streak  int
	names   []string
	lastErr string
	tripped bool // one alert per streak, not one per failure past it
}

// NewToolFailureStreakSignal constructs the signal. Threshold below 2
// is clamped to 2: a single failed call is an ordinary event, not a
// signal, and threshold 1 would alert on every one of them.
func NewToolFailureStreakSignal(threshold int) *ToolFailureStreakSignal {
	if threshold < 2 {
		threshold = 2
	}
	return &ToolFailureStreakSignal{Threshold: threshold}
}

// Name implements Signal.
func (s *ToolFailureStreakSignal) Name() string { return "tool-failure-streak" }

// ObserveToolCall implements Signal. Calls carry no outcome, so this
// signal ignores them; the interface requires the method because
// DefaultWatchdog fans every call across every signal.
func (s *ToolFailureStreakSignal) ObserveToolCall(ToolCall) *Alert { return nil }

// Reset implements Signal.
func (s *ToolFailureStreakSignal) Reset() {
	s.streak = 0
	s.names = nil
	s.lastErr = ""
	s.tripped = false
}

// ObserveToolResult implements SignalResultObserver.
func (s *ToolFailureStreakSignal) ObserveToolResult(tr ToolResult) *Alert {
	if !tr.Failed() {
		s.Reset()
		return nil
	}
	s.streak++
	s.lastErr = tr.Error
	// Bound the name list at the threshold: past that it's the same
	// story with more words, and Reason/Guidance are prompt text.
	if len(s.names) < s.Threshold {
		s.names = append(s.names, tr.Name)
	}
	if s.streak < s.Threshold || s.tripped {
		return nil
	}
	s.tripped = true
	tools := distinctNames(s.names)
	return &Alert{
		Signal:   s.Name(),
		Severity: SeverityWarn,
		Reason: fmt.Sprintf(
			"%d tool calls in a row failed with no successful call in between (%s). The agent has no tool-verified evidence about the state it is being asked to report on; watch its next answer for conclusions it cannot have observed. Last error: %s",
			s.streak, tools, truncate(s.lastErr, 200),
		),
		Guidance: fmt.Sprintf(
			"Your last %d tool calls all failed (%s) and none succeeded in between, so nothing you have run this session has verified the current state of anything. Last error: %s. Do not describe a system as healthy, fixed, or resolved on this basis — you have not observed it. Either find a call that works, or tell the user plainly what you could not check and why, and stop there.",
			s.streak, tools, truncate(s.lastErr, 200),
		),
	}
}

// distinctNames renders the streak's tool names for the alert text,
// collapsing repeats so "gke_get_pod, gke_get_pod, gke_get_pod" reads
// as one name — the repeated-tool-call signal is what covers
// repetition; this one is about outcomes.
func distinctNames(names []string) string {
	if len(names) == 0 {
		return "unknown tools"
	}
	out := make([]string, 0, len(names))
	seen := make(map[string]struct{}, len(names))
	for _, n := range names {
		if n == "" {
			n = "unnamed tool"
		}
		if _, dup := seen[n]; dup {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, n)
	}
	s := out[0]
	for _, n := range out[1:] {
		s += ", " + n
	}
	return s
}
