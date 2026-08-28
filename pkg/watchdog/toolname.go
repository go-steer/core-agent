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

// The blind spot this closes. Every detector that shipped before it —
// repeated-tool-call, alternating-cycle, dominant-tool-call — keys on
// (name, canonicalArgs). That is the right key when the arguments name
// what the call operates on: re-reading one file, re-listing one
// cluster. It is the wrong key when an argument is model-authored free
// text, because the model rewords it every iteration and each call
// hashes differently while doing exactly the same nothing.
//
// Observed live, session 01a03f1e-e215-7acd-81a9-6e4654d91325: an agent
// answered an operator's question with nine consecutive mark_task_done
// calls in a single invocation, each carrying a freshly-worded
// one-paragraph `detail` about the same closed incident. Nine distinct
// args hashes, so repeated-tool-call's run length reset to 1 on every
// call and dominant-tool-call saw nine singleton keys. Watchdog mode
// was enforce; it reported tripped: false throughout, and the loop
// ended only because the operator hit interrupt.
//
// Keying on the name alone is what sees that. The cost of dropping args
// from the key is that legitimate fan-out — a dozen read_file calls over
// a dozen different files — looks identical to a loop, which is why the
// threshold here is well above the args-identical detector's, why this
// signal defers to it, and why it is the one loop detector that reports
// at Warn rather than Critical. See DefaultToolNameRun for the tuning.

package watchdog

import "fmt"

// DefaultToolNameRun is the consecutive same-name run length that trips
// RepeatedToolNameSignal in the default signal set.
//
// The number is a false-positive budget, not a loop-detection
// preference. Dropping args from the key means this signal cannot tell
// a stuck agent from a productive one working through a list, so the
// threshold has to sit above the longest legitimate single-tool run we
// expect to see.
//
// What the budget costs depends entirely on the severity, and that is
// why this signal is Warn (see RepeatedToolNameSignal). At Critical the
// threshold had to clear every plausible sweep, because a false
// positive under --watchdog=enforce halts a working agent; twenty was
// the smallest number that did, and twenty is high enough that the
// signal has no demonstrated catch — a guardrail priced so as never to
// fire. At Warn a false positive costs one line in the operator log
// and, under --watchdog=feedback, one paragraph of next-turn context
// aimed at a model that is free to disregard it. That is cheap enough
// to buy a threshold low enough to be useful.
//
// Fifteen is where those meet. It clears the one legitimate sweep this
// package actually certifies —
// TestDominantToolCallSignal_DoesNotTripOnLegitimateWork asserts that
// twelve read_file calls over twelve distinct paths is work, not a loop
// — with margin, while sitting low enough to catch grinding before it
// has run for twenty calls. Do not raise it back toward twenty without
// also raising the severity; the two numbers are one decision.
//
// It is still NOT tuned to catch the nine-call mark_task_done loop
// above. Nine is under fifteen, and no threshold low enough to catch it
// survives the paragraphs above. That specific shape is fixed at the
// tool layer instead (mark_task_done now reports the repeat rather than
// acknowledging it again), which is the better place for it anyway: a
// tool that cannot do anything the second time in a turn should say so
// itself, not wait for a behavioral detector to infer it. This signal
// is the backstop for the general case — any tool ground on long enough
// with varying arguments — and it is deliberately the last line of
// defence, not the first.
const DefaultToolNameRun = 15

// RepeatedToolNameSignal trips when the same tool NAME is called
// Threshold times consecutively, whatever the arguments.
//
// Deliberately the coarsest of the loop detectors. It exists for the
// shape the other three structurally cannot see: a loop whose
// arguments change every iteration without the call meaning anything
// different. Free-text arguments are the common source — a summary, a
// goal, a message, a commit note — because the model rephrases them
// naturally and each rephrasing defeats an args-keyed comparison.
//
// Severity is Warn — the only loop detector that is not Critical, and
// deliberately so. The other three assert something they can prove: the
// calls are identical, so the agent is provably learning nothing. This
// one cannot. A long same-name run is a loop *or* a sweep, and the
// signal has no way to tell which. Under --watchdog=enforce a Critical
// alert halts the agent, so being wrong here would stop an agent that
// was working — the backstop becomes the outage. ToolFailureStreakSignal
// reached the same conclusion from the same place ("halting three
// denials into a legitimate RBAC probe"), and this signal has strictly
// less information than that one does.
//
// Warn still does work. It reaches the operator log in every mode, and
// under --watchdog=feedback it reaches the model's own next turn as
// Guidance — which is the party that can actually stop making the call.
// An unattended daemon gets the feedback path and no halt, which is the
// right trade for an inference this soft.
//
// Known false positive, now survivable: a genuine sweep longer than
// Threshold — an agent reading twenty files in a row, or issuing twenty
// reads against twenty distinct resources. It costs a log line and a
// paragraph of next-turn context, not a halt. This is why the alert
// text names the possibility instead of asserting a loop, and why
// Guidance tells a sweeping agent to keep going. An operator who runs
// workloads with long single-tool sweeps and wants silence should
// construct DefaultWatchdog with their own signal list, or raise
// Threshold.
type RepeatedToolNameSignal struct {
	// Threshold is the consecutive same-name run length that trips.
	Threshold int

	// DeferRun is the run length that hands the window back to
	// RepeatedToolCallSignal. When the current run is not just
	// same-name but same-name-and-same-args, that detector owns it and
	// has already alerted at its own (lower) threshold; re-reporting
	// here would put two alerts, and under --watchdog=feedback two
	// blocks of prompt text, in front of one behavior. It would also be
	// the weaker report shadowing the stronger one — that detector's
	// alert is Critical and can prove the calls are identical, which
	// this one cannot. Mirrors DominantToolCallSignal.DeferRun,
	// including the convention that zero or negative disables the
	// deference for an operator wiring this signal without the repeat
	// detector.
	DeferRun int

	name        string // tool name of the current run
	runLength   int
	argsRun     int // trailing sub-run with canonically-identical args
	lastArgs    string
	tripped     bool // one alert per run, not one per call past the threshold
	hasLastArgs bool
}

// NewRepeatedToolNameSignal constructs a signal with the given
// threshold and repeat-detector deference. Threshold must be ≥ 2; a
// smaller value is clamped, matching NewRepeatedToolCallSignal.
func NewRepeatedToolNameSignal(threshold, deferRun int) *RepeatedToolNameSignal {
	if threshold < 2 {
		threshold = 2
	}
	return &RepeatedToolNameSignal{Threshold: threshold, DeferRun: deferRun}
}

// Name implements Signal.
func (s *RepeatedToolNameSignal) Name() string { return "repeated-tool-name" }

// Reset implements Signal.
func (s *RepeatedToolNameSignal) Reset() {
	s.name = ""
	s.runLength = 0
	s.argsRun = 0
	s.lastArgs = ""
	s.hasLastArgs = false
	s.tripped = false
}

// ObserveToolCall implements Signal. Tracks the consecutive run of
// calls sharing a tool name, alongside the trailing sub-run of calls
// that also share canonically-identical args so the deference to
// RepeatedToolCallSignal can be evaluated.
func (s *RepeatedToolNameSignal) ObserveToolCall(tc ToolCall) *Alert {
	args := canonicalArgs(tc.Args)
	switch {
	case s.runLength > 0 && s.name == tc.Name:
		s.runLength++
		if s.hasLastArgs && s.lastArgs == args {
			s.argsRun++
		} else {
			s.argsRun = 1
		}
	default:
		s.name = tc.Name
		s.runLength = 1
		s.argsRun = 1
		s.tripped = false
	}
	s.lastArgs = args
	s.hasLastArgs = true

	if s.runLength < s.Threshold || s.tripped {
		return nil
	}
	if s.DeferRun > 0 && s.argsRun >= s.DeferRun {
		// The tail of this run is byte-equivalent, so
		// RepeatedToolCallSignal has already reported it. Do not clear
		// s.tripped: an args-identical stretch forming inside a
		// same-name run is the same loop getting worse, not a new one.
		return nil
	}
	s.tripped = true

	return &Alert{
		Signal:   s.Name(),
		Severity: SeverityWarn,
		Reason: fmt.Sprintf(
			"agent has called %s %d times in a row with nothing else interleaved — possible tool loop with varying arguments, which the args-matching detectors cannot count. This does not halt the agent: a run this long is also what a legitimate sweep over %d distinct targets looks like, and the signal cannot tell them apart. If it is a sweep, raise the repeated-tool-name threshold to stop hearing about it; if it is a loop, /interrupt and try a different prompt phrasing.",
			tc.Name, s.runLength, s.runLength,
		),
		// Model-facing half. Unlike the args-identical detectors we
		// cannot tell it the call "returns the same result", because
		// here the arguments genuinely differ — the claim would be
		// false and the model would be right to ignore it. What we can
		// say is that nothing else has happened for a long time, and
		// ask for the one thing that resolves either case: a different
		// kind of step, or an explanation.
		Guidance: fmt.Sprintf(
			"You have called %s %d times in a row and used no other tool in between. If you are working through a list, keep going but say what the list is. If you are not — if each call is another attempt at the same thing with the wording changed — stop calling %s, and instead say plainly what you are trying to accomplish and what is blocking it. Rewording the arguments to the same tool is not progress.",
			tc.Name, s.runLength, tc.Name,
		),
	}
}
