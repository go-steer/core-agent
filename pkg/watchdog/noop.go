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

// The no-op streak signal (#907).
//
// Every other loop detector in this package infers repetition from the
// outside: same name, same canonical args, an A→B→A→B shape, one tool
// dominating a window. Each of those is a guess about intent, so each
// carries a threshold tuned against a false-positive budget, and
// toolname.go argues at length — correctly — that no threshold low
// enough to catch a reworded loop survives that budget.
//
// This signal does not guess. It reads a claim the TOOL made about its
// own call: `no_op: true` in the response means "this invocation
// changed nothing." Three of those in a row is not evidence of a loop,
// it is a loop, by the runtime's own accounting. That is why the
// threshold can sit at 3 where the name detector needs 15: there is no
// precision being traded for recall, because there is no inference.
//
// The evidence is #905's live session. Sixteen mark_task_done calls,
// thirteen answered "already recorded for this turn […] Calling this
// again cannot do anything further" — and every default signal stayed
// silent, correctly:
//
//   - RepeatedToolCallSignal keys on (name, canonicalArgs) and the
//     model reworded `detail` every time, resetting the run to 1.
//   - RepeatedToolNameSignal needs a run of 15; a single interleaved
//     gke_get_k8s_resource split the thirteen into 7 + 6.
//   - AlternatingCycleSignal saw no A→B→A→B period.
//   - DominantToolCallSignal saw other tools interleaved.
//   - ToolFailureStreakSignal saw no failures — a no-op is a success.
//
// A tool opts in by putting `no_op: true` in its result. Nothing is
// parsed and no status prose is matched, deliberately: the string is
// the part most likely to be reworded, and markTaskDoneRepeatStatus's
// own doc comment invites exactly that rewording.

package watchdog

import "fmt"

// DefaultNoOpStreak is the number of consecutive self-reported no-op
// tool results that trips NoOpStreakSignal.
//
// Three, and low on purpose. The other detectors' thresholds price in
// the chance they are wrong about a legitimate workload; this one
// cannot be wrong in that direction, because a tool asserting it did
// nothing is not a heuristic about the model's intent. Two would be
// defensible and is not chosen only because a single retry after a
// transient is an ordinary shape; by the third the agent has spent
// three turns' worth of calls to change nothing.
const DefaultNoOpStreak = 3

// NoOpStreakSignal trips when Threshold tool results in a row all
// report themselves as no-ops. Any result that did something — or
// failed — resets the run.
//
// Resetting on a single productive call is the conservative choice and
// is affordable here in a way it is not for RepeatedToolNameSignal: at
// a run of 15, one unrelated call one-twelfth of the way in destroys
// the detection, which is how #905 escaped. At 3 the same interruption
// costs at most two observations, and the loop that is actually stuck
// re-accumulates immediately. #905's own trace splits 13 into 7 + 6 and
// trips on either half.
//
// Severity is Critical, unlike ToolFailureStreakSignal's Warn. A
// failure streak is an evidence problem — the agent might still be
// doing something useful and halting it three RBAC denials into a
// legitimate probe would make the backstop the outage. A no-op streak
// is the runaway itself: the runtime has already established that the
// calls accomplished nothing, so under --watchdog=enforce there is
// nothing being interrupted that was going anywhere.
type NoOpStreakSignal struct {
	Threshold int

	streak  int
	names   []string
	tripped bool // one alert per streak, not one per no-op past it
}

// NewNoOpStreakSignal constructs the signal. Threshold below 2 is
// clamped to 2: a tool reporting one no-op is an ordinary event — a
// retry, an idempotent write, an operator asking twice — and threshold
// 1 would alert on every one of them.
func NewNoOpStreakSignal(threshold int) *NoOpStreakSignal {
	if threshold < 2 {
		threshold = 2
	}
	return &NoOpStreakSignal{Threshold: threshold}
}

// Name implements Signal.
func (s *NoOpStreakSignal) Name() string { return "no-op-streak" }

// ObserveToolCall implements Signal. A call carries no outcome, and the
// whole point of this signal is that the outcome is the evidence; the
// method exists because DefaultWatchdog fans every call across every
// signal.
func (s *NoOpStreakSignal) ObserveToolCall(ToolCall) *Alert { return nil }

// Reset implements Signal.
func (s *NoOpStreakSignal) Reset() {
	s.streak = 0
	s.names = nil
	s.tripped = false
}

// ObserveToolResult implements SignalResultObserver.
func (s *NoOpStreakSignal) ObserveToolResult(tr ToolResult) *Alert {
	if !tr.NoOp {
		s.Reset()
		return nil
	}
	s.streak++
	// Bound the name list at the threshold: past that it is the same
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
		Severity: SeverityCritical,
		Reason: fmt.Sprintf(
			"%d tool calls in a row (%s) reported that they changed nothing. This is not an inference from repetition — each of those tools said the call was inert, so the agent has spent %d calls making no progress. Whatever it is trying to accomplish, this is not the way to accomplish it.",
			s.streak, tools, s.streak,
		),
		Guidance: fmt.Sprintf(
			"Your last %d calls to %s each came back saying they did nothing — the state was already what you were asking them to make it. Repeating them cannot work. Stop calling them. If there is something you still owe the person you are working for, say it in your reply now; if you are genuinely finished, end the turn with that reply instead of another tool call.",
			s.streak, tools,
		),
	}
}
