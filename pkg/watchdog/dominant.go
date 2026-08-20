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

// The windowed-density loop detector (#702).
//
// The two loop signals that shipped before this one divide the space
// cleanly on paper: RepeatedToolCallSignal owns a *run* of one call,
// AlternatingCycleSignal owns a repeating *sequence* and explicitly
// skips blocks made of a single repeated call because the repeat
// detector already covers them. The shape that falls between them is
// mostly one repeated call with occasional other calls wedged in.
// The repeat detector resets its run length on any non-matching call,
// so an interleave restarts the count from zero; the cycle detector
// sees a nearly-uniform block and hands it back to the repeat
// detector. Neither converges quickly.
//
// This is not a miss — it is a convergence gap, and the difference
// matters for how the detector is tuned. In GKE UAT session
// 019ffb77-3616-77a6-b06d-a5815b0929fe the backstop did fire: 22
// byte-identical gke_list_clusters calls over two minutes and twenty
// seconds, halted by repeated-tool-call at call 22. The loop was
// visibly degenerate from about the fourth call, and the interleaves
// were what stretched a threshold of 5 into 22 calls and a large
// share of a $0.77 session.
//
// So the job here is to reach the same verdict sooner on the same
// shape, without loosening either existing detector's contract and
// without raising a second alert for a pattern one of them already
// owns. Density over a sliding window does that: it does not care
// where the interleaves fall, only that one call dominates recent
// activity.
package watchdog

import "fmt"

// Default tuning for DominantToolCallSignal. Exported so an operator
// building a custom signal list can see what they are deviating from.
//
// 8-of-12 is chosen against the failure it was drawn from: the UAT
// loop reaches it around the tenth call rather than the twenty-second,
// while still requiring that two thirds of a full window be one call.
// DefaultDominantDeferRun tracks DefaultRepeatThreshold — see the
// DeferRun field for why the two are tied.
const (
	DefaultDominantWindow      = 12
	DefaultDominantThreshold   = 8
	DefaultDominantDeferRun    = DefaultRepeatThreshold
	DefaultDominantDeferPeriod = DefaultCycleMaxPeriod
)

// DominantToolCallSignal trips when a single tool call accounts for
// Threshold of the last Window observations — the interleaved loop
// neither the consecutive-repeat nor the alternating-cycle detector
// converges on quickly (#702).
//
// Args are compared canonicalized (canonicalArgs, see canonical.go),
// the same treatment AlternatingCycleSignal uses and for the same
// reason: counting occurrences means keying a map, and the repeat
// detector's pairwise path-suffix relation is not transitive, so it
// cannot key one.
//
// Severity is Critical, matching the other two loop detectors. The
// justification is theirs as well: the dominant call's arguments are
// equivalent every time, so its results are too, and an agent
// spending two thirds of a window re-asking one answered question is
// not making progress.
//
// The known false positive is a hand-rolled polling loop — the same
// one the cycle detector documents, and the same answer:
// wait_and_verify (#648) is the supported way to wait, and an
// operator who wants the pattern anyway constructs DefaultWatchdog
// with their own signal list. Note that a poll tight enough to put 8
// identical calls in a 12-call window already trips the repeat
// detector at 5 consecutive unless something interleaves, so this
// signal does not make a legitimate poll materially more halt-prone
// than it already was.
type DominantToolCallSignal struct {
	// Window is how many recent tool calls are considered.
	Window int
	// Threshold is how many of those must be the same call.
	Threshold int
	// DeferRun is the consecutive-run length that hands the window
	// back to RepeatedToolCallSignal: when the dominant call has ever
	// reached a run that long inside the current window, this signal
	// stays quiet, because that run is what the repeat detector
	// exists to catch and one behavior should not raise two alerts —
	// under --watchdog=feedback a duplicate alert is duplicated
	// prompt text, not just a duplicated log line.
	//
	// This is a deliberate, explicit coupling to the repeat
	// detector's threshold rather than a guess: AlternatingCycleSignal
	// makes the same deference structurally, by skipping uniform
	// blocks. Zero or negative disables it, which is what an operator
	// wiring this signal *without* the repeat detector wants.
	DeferRun int

	// DeferCyclePeriod is the same deference toward
	// AlternatingCycleSignal: when the whole window is a clean
	// repetition of a block of period 2..DeferCyclePeriod, that
	// detector owns it. Without this, a → a → b → a → a → b reads as
	// a dominant call (8 of 12 are `a`) *and* as a 3-call cycle, and
	// would raise two Critical alerts for one loop. Zero or negative
	// disables it.
	//
	// This is not a hole: the cycle detector reaches such a window
	// first — three laps of a 3-call block is nine calls, where the
	// density threshold needs twelve — so deferring costs nothing but
	// the duplicate. A period longer than the cycle detector scans
	// (a → a → a → a → b, period 5) does not match here and is
	// exactly the case this signal is for.
	DeferCyclePeriod int

	history []dominantCall
	tripped string // key already alerted on; "" when clear
}

// dominantCall is one entry of the sliding window. The canonical key
// is what gets counted; name and args are kept for the alert text,
// which quotes the call as the agent actually made it.
type dominantCall struct {
	key  string
	name string
	args string
}

// argsRetained caps the raw arguments each window entry keeps. The
// only consumer is the alert text, which truncates to the same bound
// anyway, so nothing is lost — but this is per-session state on a
// long-lived daemon, and a window of a dozen multi-kilobyte JSON blobs
// held live for the length of a session is a cost with no reader. The
// canonical key is not capped: it is what the counting compares, and a
// truncated key would collapse two calls that differ only past the cut.
const argsRetained = 200

// NewDominantToolCallSignal constructs the density detector. window
// below 2 is clamped to 2, threshold below 2 is clamped to 2 (one call
// is not a density), and a threshold above window is clamped to window
// — a signal that can never trip is a silent misconfiguration, and
// clamping makes it a strict one instead.
func NewDominantToolCallSignal(window, threshold, deferRun, deferCyclePeriod int) *DominantToolCallSignal {
	if window < 2 {
		window = 2
	}
	if threshold < 2 {
		threshold = 2
	}
	if threshold > window {
		threshold = window
	}
	return &DominantToolCallSignal{
		Window:           window,
		Threshold:        threshold,
		DeferRun:         deferRun,
		DeferCyclePeriod: deferCyclePeriod,
	}
}

// Name implements Signal.
func (s *DominantToolCallSignal) Name() string { return "dominant-tool-call" }

// Reset implements Signal.
func (s *DominantToolCallSignal) Reset() {
	s.history = nil
	s.tripped = ""
}

// ObserveToolCall implements Signal. Appends to the sliding window and
// reports the dominant call when it crosses the density threshold.
func (s *DominantToolCallSignal) ObserveToolCall(tc ToolCall) *Alert {
	s.history = append(s.history, dominantCall{
		key:  tc.Name + "\x00" + canonicalArgs(tc.Args),
		name: tc.Name,
		args: truncate(tc.Args, argsRetained),
	})
	if len(s.history) > s.Window {
		s.history = s.history[len(s.history)-s.Window:]
	}

	top, count := s.dominant()
	if count < s.Threshold {
		// Activity has diversified: re-arm so a later cluster alerts.
		s.tripped = ""
		return nil
	}
	if s.deferred(top.key) {
		// Another detector owns this window. Deliberately does not
		// clear s.tripped: if this signal already reported the
		// pattern, a run or a cycle forming inside it is the same
		// pattern getting worse, not a new one to re-announce.
		return nil
	}
	if s.tripped == top.key {
		return nil // one alert per cluster, not one per call past it
	}
	s.tripped = top.key

	return &Alert{
		Signal:   s.Name(),
		Severity: SeverityCritical,
		Reason: fmt.Sprintf(
			"agent has called %s with equivalent args %d of the last %d tool calls — possible tool loop with interleaved calls, which the consecutive-repeat detector cannot count. Args: %s. If the agent is stuck, consider /interrupt and a different prompt phrasing. Cost ceiling (see --max-turn-cost-usd) is the hard backstop.",
			top.name, count, len(s.history), top.args,
		),
		Guidance: fmt.Sprintf(
			"%d of your last %d tool calls were %s with equivalent arguments (%s). The few other calls in between did not change those arguments, so that call keeps returning the same result and repeating it cannot make progress. Either use what it already told you, change the arguments to ask something new, or stop calling tools and state plainly what you are stuck on.",
			count, len(s.history), top.name, top.args,
		),
	}
}

// deferred reports whether the current window belongs to one of the
// other two loop detectors: a long enough consecutive run of the
// dominant call is RepeatedToolCallSignal's, and a window that is a
// clean short cycle is AlternatingCycleSignal's.
func (s *DominantToolCallSignal) deferred(key string) bool {
	if s.DeferRun > 0 && s.longestRun(key) >= s.DeferRun {
		return true
	}
	if s.DeferCyclePeriod < 2 {
		return false
	}
	keys := make([]string, len(s.history))
	for i, c := range s.history {
		keys[i] = c.key
	}
	for p := 2; p <= s.DeferCyclePeriod && p < len(keys); p++ {
		if blockRepeats(keys, p) {
			return true
		}
	}
	return false
}

// dominant returns the most frequent entry in the window and its
// count. Ties break toward the earliest first occurrence, so the
// reported call is stable as the window slides rather than flipping
// between two equally frequent calls.
func (s *DominantToolCallSignal) dominant() (dominantCall, int) {
	counts := make(map[string]int, len(s.history))
	for _, c := range s.history {
		counts[c.key]++
	}
	var best dominantCall
	bestCount := 0
	for _, c := range s.history {
		if counts[c.key] > bestCount {
			best, bestCount = c, counts[c.key]
		}
	}
	return best, bestCount
}

// longestRun returns the longest run of consecutive entries in the
// window whose key is key.
func (s *DominantToolCallSignal) longestRun(key string) int {
	best, run := 0, 0
	for _, c := range s.history {
		if c.key != key {
			run = 0
			continue
		}
		run++
		if run > best {
			best = run
		}
	}
	return best
}
