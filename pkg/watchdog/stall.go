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

// The progress-stall signal (#655) — "these two calls ask the same
// question differently."
//
// That sentence is this package's own description of what it could not
// do. Every detector here keys on the CALL: same name, same canonical
// arguments, an A→B→A→B shape, one tool dominating a window. Each of
// them is defeated by the same evasion, and toolname.go spells it out:
// a model that rewords its arguments produces a different key every
// iteration while asking the identical question. #905 is the recorded
// instance — thirteen `mark_task_done` calls with a reworded `detail`
// each time, and every args-keyed detector correctly stayed silent.
//
// noop.go answered that by reading a claim the tool makes about itself.
// It works, and it only works for tools that opted in by setting
// `no_op: true`. A `read_file` that returns the same file for the sixth
// time is not a no-op — it did exactly what it was asked, successfully,
// and has no idea the agent already has those bytes.
//
// This signal reads the ANSWER instead. Two calls that ask the same
// question differently get the same answer back, so a digest of the
// result payload catches the rewording without having to understand it.
// No semantic comparison, no embedding, no second model: just
// "everything arriving is something this turn already had".
//
// # What separates a stall from a poll
//
// A loop and a legitimate poll produce the same observation. An agent
// watching a rollout deliberately reads identical state over and over
// until it changes, and in the GKE and Kubernetes work this project is
// aimed at, that is not an edge case — it is Tuesday. Two consequences,
// both deliberate:
//
//   - Severity is **Warn**, not Critical, so it never halts under
//     --watchdog=enforce. This is the same trade RepeatedToolNameSignal
//     takes: a name alone cannot tell a loop from a sweep, and a result
//     digest alone cannot tell a loop from a poll. Halting a working
//     agent is worse than logging a slow one, and under
//     --watchdog=feedback the Guidance still reaches the only party
//     that can stop making the call.
//   - `wait_and_verify` is **exempt**. It is the tool that exists so
//     polling does not have to be hand-rolled, its contract is "call me
//     until the state changes", and it bounds its own waiting. Counting
//     it here would flag the runtime's own supported way of doing the
//     thing this signal is worried about. A hand-rolled poll — the same
//     `read_file` or `gke_get_k8s_resource` six times — is exactly what
//     should be caught, because nothing bounds it.
//
// # Why the threshold is 6
//
// Between noop.go's 3 and toolname.go's 15, and the reasoning is the
// one both of those use. A no-op needs only 3 because the tool asserted
// inertness and there is no inference to be wrong about. A tool name
// needs 15 because the name is almost no evidence. A repeated result
// payload is strong evidence — the bytes really are identical, so the
// agent really did learn nothing — but it is not a confession, because
// of the poll. Six is where a poll that is genuinely waiting has had
// five chances to see a change, and where a stalled agent has burned
// six calls to stand still.
//
// # Why an unreadable result resets
//
// A result with no digest resets the run rather than extending it.
// Absence of evidence is not evidence of a stall, and a signal that
// treated "I could not tell" as "nothing new" would be inferring
// exactly the thing noop.go was written to stop inferring.

package watchdog

import "fmt"

// DefaultStallRun is the number of consecutive results carrying only
// already-seen payloads that trips NoNewStateSignal. See the file
// comment for why it sits at 6.
const DefaultStallRun = 6

// DefaultStallMemory bounds how many distinct result digests the signal
// remembers. A stall is a local phenomenon — the repeats that matter
// are within a few calls of each other — and an unbounded set on a
// long-running daemon is a slow leak charged to a guardrail. 64 is far
// more than DefaultStallRun needs and small enough to be free.
const DefaultStallMemory = 64

// stallExemptTools are tools whose contract IS to return the same
// answer until something changes. Observing them would make the signal
// fire on the runtime's own supported alternative to hand-rolled
// polling. Keyed by tool name because that is all the result carries.
var stallExemptTools = map[string]bool{
	"wait_and_verify": true,
}

// NoNewStateSignal trips when Threshold tool results in a row all carry
// a payload this turn has already seen.
//
// "In a row" counts successful, non-no-op, non-exempt results. A
// failure is ToolFailureStreakSignal's territory and a self-declared
// no-op is NoOpStreakSignal's; passing them through here would let one
// runaway raise three alerts saying the same thing, and the operator
// reading three lines learns less than the one who reads one.
type NoNewStateSignal struct {
	Threshold int
	Memory    int

	seen    map[string]struct{}
	order   []string // insertion order, for bounded eviction
	run     int
	names   []string
	tripped bool // one alert per run, not one per call past it
}

// NewNoNewStateSignal constructs the signal. A threshold below 3 is
// clamped to 3: two identical answers is an ordinary retry, and at 2
// this would fire on every agent that read a file twice.
func NewNoNewStateSignal(threshold, memory int) *NoNewStateSignal {
	if threshold < 3 {
		threshold = 3
	}
	if memory < threshold {
		memory = threshold
	}
	return &NoNewStateSignal{Threshold: threshold, Memory: memory}
}

// Name implements Signal.
func (s *NoNewStateSignal) Name() string { return "no-new-state" }

// ObserveToolCall implements Signal. The evidence this signal reads is
// the result, not the call; the method exists because DefaultWatchdog
// fans every call across every signal.
func (s *NoNewStateSignal) ObserveToolCall(ToolCall) *Alert { return nil }

// Reset implements Signal. It clears the seen set as well as the run,
// because "already seen" is scoped to the window the watchdog is
// currently observing — after an operator reset, the agent starting
// over with the same reads is starting over, not stalling.
func (s *NoNewStateSignal) Reset() {
	s.seen = nil
	s.order = nil
	s.breakRun()
}

// ObserveTurnStart implements SignalTurnObserver. The seen set is
// scoped to one turn, and this is the boundary that enforces it.
//
// Without it the signal would be wrong on the workload this project
// cares most about. A watchdog is cleared only by an operator Reset, so
// its memory otherwise spans the daemon's whole life — and a daemon
// woken every ten minutes to read six resources off a cluster that has
// not changed would, from the second wake onward, produce six
// consecutive already-seen results and trip. That agent is monitoring
// correctly; the stall this signal is looking for is one turn spinning
// in place, not two turns that legitimately saw the same world.
//
// A loop that survives a turn boundary is therefore not this detector's
// to catch, and that is the right split: it is re-driven work, which
// the auto-continue and cost-ceiling machinery already bound.
func (s *NoNewStateSignal) ObserveTurnStart() { s.Reset() }

// breakRun clears the streak without forgetting what has been seen.
// Used when a result carries new information: the agent made progress,
// but everything it has read is still read.
func (s *NoNewStateSignal) breakRun() {
	s.run = 0
	s.names = nil
	s.tripped = false
}

// ObserveToolResult implements SignalResultObserver.
func (s *NoNewStateSignal) ObserveToolResult(tr ToolResult) *Alert {
	// Not our evidence. Deliberately a skip rather than a reset: an
	// interleaved failure does not mean the agent learned something,
	// and treating it as progress would hand every stalled agent a
	// free way to stay under the threshold forever.
	if tr.Failed() || tr.NoOp || stallExemptTools[tr.Name] {
		return nil
	}
	// Nothing to compare. Resets — see the file comment on why absence
	// of evidence is not evidence of a stall.
	if tr.Digest == "" {
		s.breakRun()
		return nil
	}
	if _, dup := s.seen[tr.Digest]; !dup {
		s.remember(tr.Digest)
		s.breakRun()
		return nil
	}
	s.run++
	// Bound the name list at the threshold: past that it is the same
	// story with more words, and Reason/Guidance are prompt text.
	if len(s.names) < s.Threshold {
		s.names = append(s.names, tr.Name)
	}
	if s.run < s.Threshold || s.tripped {
		return nil
	}
	s.tripped = true
	tools := distinctNames(s.names)
	return &Alert{
		Signal:   s.Name(),
		Severity: SeverityWarn,
		Reason: fmt.Sprintf(
			"%d tool results in a row (%s) returned data this turn had already seen — byte-identical to earlier results, so the agent has learned nothing from any of them. The calls differ, which is why the argument-based loop detectors are silent; the answers do not. If this is a deliberate poll, prefer wait_and_verify, which is bounded and exempt from this check.",
			s.run, tools,
		),
		Guidance: fmt.Sprintf(
			"Your last %d calls to %s all came back with information you already had earlier in this turn. Rewording the arguments is not getting you a different answer, and it will not. Either the thing you are looking for is not there — in which case say so — or you need a different source of information, not a different phrasing of the same one. If you are waiting for something in the cluster to change, use wait_and_verify instead of re-reading.",
			s.run, tools,
		),
	}
}

// remember adds a digest to the bounded seen set.
func (s *NoNewStateSignal) remember(d string) {
	if s.seen == nil {
		s.seen = make(map[string]struct{}, s.Memory)
	}
	s.seen[d] = struct{}{}
	s.order = append(s.order, d)
	for len(s.order) > s.Memory {
		delete(s.seen, s.order[0])
		s.order = s.order[1:]
	}
}
