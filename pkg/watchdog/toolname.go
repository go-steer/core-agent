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
// threshold here is far above the args-identical detector's and why
// this signal defers to it. See DefaultToolNameRun for the tuning.

package watchdog

import "fmt"

// DefaultToolNameRun is the consecutive same-name run length that trips
// RepeatedToolNameSignal in the default signal set.
//
// The number is a false-positive budget, not a loop-detection
// preference. Dropping args from the key means this signal cannot tell
// a stuck agent from a productive one working through a list, so the
// threshold has to sit above the longest legitimate single-tool run we
// are willing to interrupt — and a Critical alert under
// --watchdog=enforce halts the agent, so "interrupt" is the accurate
// word.
//
// Twelve was the first guess, on the reasoning that it matches
// DefaultDominantWindow. It is wrong, and the package says so already:
// TestDominantToolCallSignal_DoesNotTripOnLegitimateWork asserts that
// twelve read_file calls over twelve distinct paths is work, not a
// loop. Shipping a name-keyed detector at twelve would halt the exact
// sequence a sibling detector's false-positive test certifies as fine.
// Twenty clears that case with margin and still bounds the pathology
// this exists for, which is unbounded grinding rather than a long list.
//
// It is emphatically NOT tuned to catch the nine-call mark_task_done
// loop above — nine is well under twenty, and no threshold low enough
// to catch it survives the paragraph above. That specific shape is
// fixed at the tool layer instead (mark_task_done now reports the
// repeat rather than acknowledging it again), which is the better place
// for it anyway: a tool that cannot do anything the second time in a
// turn should say so itself, not wait for a behavioral detector to
// infer it. This signal is the backstop for the general case — any tool
// ground on long enough with varying arguments — and it is deliberately
// the last line of defence, not the first.
const DefaultToolNameRun = 20

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
// Severity is Critical, matching the other loop detectors, on the same
// reasoning: twenty consecutive calls to one tool with nothing else
// interleaved is not exploration, and under --watchdog=enforce the
// agent should stop rather than keep paying for it.
//
// Known false positive: a genuine sweep longer than Threshold — an
// agent reading twenty-five files in a row, or issuing twenty-five
// reads against twenty-five distinct resources. This is the reason the
// threshold is twenty and not five, and the reason the alert text names
// the possibility instead of asserting a loop. An operator who runs
// workloads with long single-tool sweeps should construct
// DefaultWatchdog with their own signal list, or raise Threshold.
type RepeatedToolNameSignal struct {
	// Threshold is the consecutive same-name run length that trips.
	Threshold int

	// DeferRun is the run length that hands the window back to
	// RepeatedToolCallSignal. When the current run is not just
	// same-name but same-name-and-same-args, that detector owns it and
	// has already alerted at its own (lower) threshold; re-reporting
	// here would put two Critical alerts, and under
	// --watchdog=feedback two blocks of prompt text, in front of one
	// behavior. Mirrors DominantToolCallSignal.DeferRun, including the
	// convention that zero or negative disables the deference for an
	// operator wiring this signal without the repeat detector.
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
		Severity: SeverityCritical,
		Reason: fmt.Sprintf(
			"agent has called %s %d times in a row with nothing else interleaved — possible tool loop with varying arguments, which the args-matching detectors cannot count. If this is a legitimate sweep over %d distinct targets, raise the repeated-tool-name threshold; otherwise consider /interrupt and a different prompt phrasing.",
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
