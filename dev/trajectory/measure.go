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

package trajectory

// Observation is one thing a measure noticed, with the evidence for it.
//
// There is deliberately no severity, no score and no pass/fail. A measure
// says what happened and where; deciding whether it was bad is the
// reader's job, and encoding that decision here would make this a grader
// against a rubric that is still moving (see the package doc).
//
// Observations are not all negative. A measure that only ever speaks up
// when something is wrong is indistinguishable, on a clean corpus, from
// a measure that is broken — so where the distinction a measure draws is
// the point, it reports both sides. [KindDelegationDisclosed] is there
// for exactly that reason.
type Observation struct {
	// Kind is a stable slug. Grep for it; do not parse Summary.
	Kind string `json:"kind"`
	// Agent is whose behaviour this is about, or "" when it is about the
	// run as a whole.
	Agent string `json:"agent,omitempty"`
	// Step is the [Step.Index] this anchors to, or -1 when it anchors to
	// nothing in particular.
	Step int `json:"step"`
	// Summary is one line, written to be read next to a step table.
	Summary string `json:"summary"`
	// Evidence is the specifics: quoted payload fields, step numbers,
	// counts. An observation whose evidence you cannot check by hand
	// against the table is a claim, not an observation.
	Evidence []string `json:"evidence,omitempty"`
}

// Measure derives observations from one trajectory.
//
// A measure reads [Step]s and [Frame]s and nothing else — never the run
// directory, never score.py's output, never the network. That is what
// makes a measure re-runnable over the archive years later.
type Measure interface {
	// Name is the measure's stable identifier. It is what a run with no
	// observations reports as having looked — see [WriteObservations],
	// where naming the measures is the difference between "nothing
	// happened" and "these measures saw nothing".
	Name() string
	// Observe returns what the measure found, in trajectory order. Nil
	// is the normal result on a clean run.
	Observe(t *Trajectory) []Observation
}

// Measures is the set applied by default.
//
// It is short on purpose. Every measure here fires on the archive as it
// stood on 2026-09-11 — a measure that reports nothing on every run
// available has not been shown to work, and shipping one is how you get
// an instrument nobody trusts. The candidate this rules out today is
// redundancy-within-an-agent, the "loop rate" #967 names: no run in the
// archive contains a loop, so it would be zero everywhere. It belongs
// with a corpus (#966) that has one.
var Measures = []Measure{
	Delegation{},
}

// Observe runs measures over a trajectory and returns everything they
// found, grouped by measure in the order given.
func Observe(t *Trajectory, measures ...Measure) []Observation {
	if len(measures) == 0 {
		measures = Measures
	}
	var out []Observation
	for _, m := range measures {
		out = append(out, m.Observe(t)...)
	}
	return out
}
