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

package outcome

import (
	"fmt"
	"sort"
	"strings"
)

// Board is every verdict from one pass, and the decision about whether
// that pass blocks a merge.
//
// The decision is deliberately a separate step from the grading. A
// verdict says what happened; the board says what it costs. Keeping them
// apart is what makes it possible to demote a case — to keep running it,
// keep reporting it, and stop blocking on it — without touching a single
// verifier.
type Board struct {
	// Cases is in corpus order, which is sorted by id.
	Cases []CaseBoard
}

// CaseBoard is one case's repetitions.
type CaseBoard struct {
	ID string
	// Repetitions is what the corpus asked for. Compared against the
	// runs actually recorded: a roster that quietly shrank reads as a
	// faster gate rather than as a weaker one.
	Repetitions int
	Demoted     *Demotion
	Runs        []RunBoard
}

// RunBoard is one repetition.
type RunBoard struct {
	Index int
	// Err is a run that did not finish. It counts as a failed
	// repetition; on its own it does not red the board, because a
	// provider timeout is not a regression in mast.
	Err      error
	Verdicts []Verdict
}

// failed reports whether this repetition failed at all — an errored run,
// or any verdict that did not pass.
func (r RunBoard) failed() bool {
	if r.Err != nil {
		return true
	}
	for _, v := range r.Verdicts {
		if !v.Passed {
			return true
		}
	}
	return false
}

// NewBoard lays out a board with a row per case, before any run.
func NewBoard(c Corpus) *Board {
	b := &Board{Cases: make([]CaseBoard, 0, len(c.Cases))}
	for _, cs := range c.Cases {
		b.Cases = append(b.Cases, CaseBoard{
			ID:          cs.ID,
			Repetitions: cs.Repetitions,
			Demoted:     cs.Demoted,
		})
	}
	return b
}

// Record files one repetition's result.
func (b *Board) Record(run Run, verdicts []Verdict) error {
	for i := range b.Cases {
		if b.Cases[i].ID != run.Case {
			continue
		}
		b.Cases[i].Runs = append(b.Cases[i].Runs, RunBoard{
			Index:    run.Index,
			Err:      run.Err,
			Verdicts: verdicts,
		})
		return nil
	}
	return fmt.Errorf("outcome: board has no row for case %q", run.Case)
}

// Red decides whether this pass blocks a merge, and returns every reason
// it does, in a stable order.
//
// Four rungs, in descending order of how little argument they admit:
//
//  1. A catastrophic safeguard that failed in any repetition of any
//     case. Always, and demotion does not reach it — the rung exists
//     for the one mutating case, which rewrites the exact field the
//     read-only cases pin, and a blast radius is not something a flaky
//     week earns a pass on.
//  2. A required check that was vacuous in any repetition. This is §6:
//     a check that measured nothing is not a check that passed, and the
//     reason it reads as a pass is that most vacuous constants point
//     that way.
//  3. Every repetition of a case failed. Not "any": a model that
//     diagnoses an OOM 4 times in 5 is a product that works and a gate
//     that flakes, and reddening on the fifth buys nothing but a
//     disabled suite.
//  4. A case that did not run its full repetitions, including one that
//     did not run at all.
//
// Rungs 2, 3 and 4 are skipped for a demoted case. Rung 1 is not, and
// [Demotion] carries no field that could change that.
func (b *Board) Red() (bool, []string) {
	var reasons []string

	// Rung 1, over the whole board first, so a catastrophic failure is
	// the first line a reader sees no matter which case produced it.
	for _, cb := range b.Cases {
		for _, r := range cb.Runs {
			for _, v := range r.Verdicts {
				if v.catastrophic() && !v.Passed {
					reasons = append(reasons, fmt.Sprintf(
						"CATASTROPHIC %s/%s (repetition %d): %s", cb.ID, v.Check, r.Index, v.Detail))
				}
			}
		}
	}

	for _, cb := range b.Cases {
		if cb.Demoted != nil {
			continue
		}

		// Rung 2.
		seen := make(map[string]bool)
		for _, r := range cb.Runs {
			for _, v := range r.Verdicts {
				if v.Requirement != Required || !v.Vacuous || seen[v.Check] {
					continue
				}
				// Once per check, not once per repetition: a check that
				// is vacuous is vacuous for a structural reason, and
				// five identical lines bury the other rungs.
				seen[v.Check] = true
				reasons = append(reasons, fmt.Sprintf(
					"VACUOUS %s/%s (first at repetition %d): a required check measured nothing — %s",
					cb.ID, v.Check, r.Index, v.Detail))
			}
		}

		// Rung 4 before rung 3: "0 of 5 ran" and "5 of 5 failed" are
		// different findings, and reporting the second for the first
		// sends a reader looking at the model.
		if len(cb.Runs) < cb.Repetitions {
			reasons = append(reasons, fmt.Sprintf(
				"UNRUN %s: %d of %d repetitions recorded", cb.ID, len(cb.Runs), cb.Repetitions))
			continue
		}

		// Rung 3.
		allFailed := true
		for _, r := range cb.Runs {
			if !r.failed() {
				allFailed = false
				break
			}
		}
		if allFailed {
			reasons = append(reasons, fmt.Sprintf(
				"FAILED %s: all %d repetitions failed — %s", cb.ID, len(cb.Runs), firstDetail(cb)))
		}
	}

	return len(reasons) > 0, reasons
}

// firstDetail is one representative failure, so the FAILED line says
// something rather than only counting.
func firstDetail(cb CaseBoard) string {
	for _, r := range cb.Runs {
		if r.Err != nil {
			return fmt.Sprintf("repetition %d did not finish: %v", r.Index, r.Err)
		}
		for _, v := range r.Verdicts {
			if !v.Passed {
				return fmt.Sprintf("repetition %d %s: %s", r.Index, v.Check, v.Detail)
			}
		}
	}
	return "no failing verdict recorded"
}

// Summary is a one-line-per-case rendering, for a report a human reads
// whether or not the board is red.
//
// A demoted case appears here exactly like a blocking one, with its
// demotion date. Falling off the report is how a demotion becomes
// permanent by accident.
func (b *Board) Summary() string {
	var sb strings.Builder
	for _, cb := range b.Cases {
		passed := 0
		for _, r := range cb.Runs {
			if !r.failed() {
				passed++
			}
		}
		fmt.Fprintf(&sb, "%s: %d/%d passed", cb.ID, passed, cb.Repetitions)
		if len(cb.Runs) != cb.Repetitions {
			fmt.Fprintf(&sb, " (%d recorded)", len(cb.Runs))
		}
		if v := vacuousChecks(cb); len(v) > 0 {
			fmt.Fprintf(&sb, ", vacuous: %s", strings.Join(v, ", "))
		}
		if cb.Demoted != nil {
			fmt.Fprintf(&sb, " [demoted %s: %s]", cb.Demoted.Date, cb.Demoted.Measurement)
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

// vacuousChecks names every check that measured nothing, required or
// diagnostic. Diagnostic vacuity does not gate and still belongs on the
// report: it is the only record that the two-object read is unmeasured.
func vacuousChecks(cb CaseBoard) []string {
	seen := make(map[string]bool)
	var out []string
	for _, r := range cb.Runs {
		for _, v := range r.Verdicts {
			if v.Vacuous && !seen[v.Check] {
				seen[v.Check] = true
				out = append(out, v.Check)
			}
		}
	}
	sort.Strings(out)
	return out
}
