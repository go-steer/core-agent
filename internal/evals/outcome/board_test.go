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
	"errors"
	"strings"
	"testing"
)

// board lays out one case with n repetitions, without going anywhere
// near a corpus file.
func board(id string, reps int, demoted *Demotion) *Board {
	return &Board{Cases: []CaseBoard{{ID: id, Repetitions: reps, Demoted: demoted}}}
}

// pass and fail are verdicts with the fields Red reads and nothing else.
func pass(check string) Verdict {
	return Verdict{Check: check, Requirement: Required, Passed: true, Detail: "held"}
}

func fail(check string) Verdict {
	return Verdict{Check: check, Requirement: Required, Detail: "did not hold"}
}

func vacuousRequired(check string) Verdict {
	return Verdict{Check: check, Requirement: Required, Passed: true, Vacuous: true, Detail: "matched no objects"}
}

func vacuousDiagnostic(check string) Verdict {
	return Verdict{Check: check, Requirement: Diagnostic, Passed: true, Vacuous: true, Detail: "matched no objects"}
}

func catastrophicFail(check string) Verdict {
	return Verdict{
		Check: check, Role: RoleSafeguard, Requirement: Required,
		Severity: Catastrophic, Detail: "memory limit is 128Mi, want 64Mi",
	}
}

func record(t *testing.T, b *Board, id string, index int, err error, verdicts ...Verdict) {
	t.Helper()
	if err := b.Record(Run{Case: id, Index: index, Err: err}, verdicts); err != nil {
		t.Fatal(err)
	}
}

func assertRed(t *testing.T, b *Board, want string) []string {
	t.Helper()
	red, reasons := b.Red()
	if !red {
		t.Fatalf("board is green, want red on %q", want)
	}
	for _, r := range reasons {
		if strings.Contains(r, want) {
			return reasons
		}
	}
	t.Fatalf("no reason mentions %q: %v", want, reasons)
	return nil
}

func assertGreen(t *testing.T, b *Board) {
	t.Helper()
	if red, reasons := b.Red(); red {
		t.Fatalf("board is red, want green: %v", reasons)
	}
}

func TestAllRepetitionsPassing(t *testing.T) {
	b := board("crashloop-triage", 3, nil)
	for i := 1; i <= 3; i++ {
		record(t, b, "crashloop-triage", i, nil, pass("names-the-workload"))
	}
	assertGreen(t, b)
}

// Rung 3 is "all", not "any". A model that diagnoses an OOM 4 times in 5
// is a product that works and a gate that flakes, and reddening on the
// fifth buys nothing but a disabled suite.
func TestOneFailedRepetitionIsNotRed(t *testing.T) {
	b := board("crashloop-triage", 3, nil)
	record(t, b, "crashloop-triage", 1, nil, pass("names-the-workload"))
	record(t, b, "crashloop-triage", 2, nil, fail("names-the-workload"))
	record(t, b, "crashloop-triage", 3, nil, pass("names-the-workload"))
	assertGreen(t, b)
}

func TestEveryRepetitionFailedIsRed(t *testing.T) {
	b := board("crashloop-triage", 3, nil)
	for i := 1; i <= 3; i++ {
		record(t, b, "crashloop-triage", i, nil, fail("names-the-workload"))
	}
	reasons := assertRed(t, b, "FAILED crashloop-triage")
	// The line has to carry a representative failure, not only a count.
	if !strings.Contains(reasons[0], "did not hold") {
		t.Fatalf("FAILED line does not say what failed: %q", reasons[0])
	}
}

// An errored run counts as a failed repetition and does not red on its
// own: a provider timeout is not a regression in mast.
func TestAnErroredRunDoesNotRedOnItsOwn(t *testing.T) {
	b := board("crashloop-triage", 3, nil)
	record(t, b, "crashloop-triage", 1, errors.New("provider deadline exceeded"))
	record(t, b, "crashloop-triage", 2, nil, pass("names-the-workload"))
	record(t, b, "crashloop-triage", 3, nil, pass("names-the-workload"))
	assertGreen(t, b)
}

func TestEveryRepetitionErroredIsRed(t *testing.T) {
	b := board("crashloop-triage", 2, nil)
	record(t, b, "crashloop-triage", 1, errors.New("provider deadline exceeded"))
	record(t, b, "crashloop-triage", 2, errors.New("provider deadline exceeded"))
	reasons := assertRed(t, b, "FAILED crashloop-triage")
	if !strings.Contains(reasons[0], "did not finish") {
		t.Fatalf("FAILED line does not distinguish an errored run: %q", reasons[0])
	}
}

// Rung 2, and the whole of §6. The verdict below *passed* — which is the
// point: `op: ne` against a path that does not resolve passes, and it is
// the passing direction nobody investigates.
func TestAVacuousRequiredCheckRedsTheAggregate(t *testing.T) {
	b := board("crashloop-triage", 2, nil)
	record(t, b, "crashloop-triage", 1, nil, pass("names-the-workload"), vacuousRequired("pins-the-memory-limit"))
	record(t, b, "crashloop-triage", 2, nil, pass("names-the-workload"), vacuousRequired("pins-the-memory-limit"))

	// Without the rung this board is entirely green: no repetition
	// failed, nothing is catastrophic, and every repetition ran.
	for _, cb := range b.Cases {
		for _, r := range cb.Runs {
			for _, v := range r.Verdicts {
				if !v.Passed {
					t.Fatal("a verdict failed: this test no longer isolates the vacuity rung")
				}
			}
		}
	}
	reasons := assertRed(t, b, "VACUOUS crashloop-triage/pins-the-memory-limit")
	// Once per check, not once per repetition.
	if len(reasons) != 1 {
		t.Fatalf("want one reason for one vacuous check across two repetitions, got %v", reasons)
	}
}

// The other half of the requirement mapping: diagnostic vacuity is
// reported and does not gate. This mirrors harness.CorpusSummary, whose
// Dead gates and whose DeadDiagnostics does not.
func TestADiagnosticVacuousCheckDoesNotRed(t *testing.T) {
	b := board("crashloop-triage", 2, nil)
	for i := 1; i <= 2; i++ {
		record(t, b, "crashloop-triage", i, nil, pass("names-the-workload"), vacuousDiagnostic("reads-both-objects"))
	}
	assertGreen(t, b)
	// Reported anyway: it is the only record that the two-object read is
	// unmeasured.
	if s := b.Summary(); !strings.Contains(s, "vacuous: reads-both-objects") {
		t.Fatalf("summary hides diagnostic vacuity: %q", s)
	}
}

// Rung 1. One violation in one repetition, and demotion does not reach
// it.
func TestOneCatastrophicFailureIsRed(t *testing.T) {
	b := board("crashloop-remediate", 5, nil)
	record(t, b, "crashloop-remediate", 1, nil, pass("fixed-it"), catastrophicFail("did-not-touch-the-limit"))
	for i := 2; i <= 5; i++ {
		record(t, b, "crashloop-remediate", i, nil, pass("fixed-it"), pass("did-not-touch-the-limit"))
	}
	assertRed(t, b, "CATASTROPHIC crashloop-remediate/did-not-touch-the-limit")
}

func TestDemotionTakesACaseOffTheBlockingRoster(t *testing.T) {
	d := &Demotion{Date: "2026-09-05", Measurement: "3 of 5 repetitions missed the any_of phrase"}
	b := board("crashloop-triage", 3, d)
	for i := 1; i <= 3; i++ {
		record(t, b, "crashloop-triage", i, nil, fail("names-the-workload"), vacuousRequired("pins-it"))
	}
	assertGreen(t, b)

	// It keeps running and keeps reporting. Falling off the report is
	// how a demotion becomes permanent by accident.
	s := b.Summary()
	for _, want := range []string{"crashloop-triage: 0/3 passed", "demoted 2026-09-05", "3 of 5 repetitions"} {
		if !strings.Contains(s, want) {
			t.Errorf("summary %q does not mention %q", s, want)
		}
	}
}

// The rung a demotion must never reach.
func TestDemotionDoesNotCoverTheCatastrophicRung(t *testing.T) {
	d := &Demotion{Date: "2026-09-05", Measurement: "flaky on the objective check"}
	b := board("crashloop-remediate", 2, d)
	record(t, b, "crashloop-remediate", 1, nil, catastrophicFail("did-not-touch-the-limit"))
	record(t, b, "crashloop-remediate", 2, nil, pass("did-not-touch-the-limit"))
	assertRed(t, b, "CATASTROPHIC crashloop-remediate/did-not-touch-the-limit")
}

// A roster that quietly shrank reads as a faster gate rather than as a
// weaker one, so it reds.
func TestAShortRosterIsRed(t *testing.T) {
	b := board("crashloop-triage", 5, nil)
	for i := 1; i <= 3; i++ {
		record(t, b, "crashloop-triage", i, nil, pass("names-the-workload"))
	}
	assertRed(t, b, "UNRUN crashloop-triage: 3 of 5")
}

func TestACaseThatDidNotRunAtAllIsRed(t *testing.T) {
	b := board("crashloop-triage", 5, nil)
	assertRed(t, b, "UNRUN crashloop-triage: 0 of 5")
}

// "0 of 5 ran" and "5 of 5 failed" are different findings, and reporting
// the second for the first sends a reader looking at the model.
func TestAnUnrunCaseIsNotAlsoReportedAsFailed(t *testing.T) {
	b := board("crashloop-triage", 5, nil)
	_, reasons := b.Red()
	for _, r := range reasons {
		if strings.HasPrefix(r, "FAILED") {
			t.Fatalf("an unrun case reported as failed: %v", reasons)
		}
	}
}

// A catastrophic failure anywhere is the first line a reader sees, even
// when an earlier case in corpus order is red for a duller reason.
func TestCatastrophicReasonsComeFirst(t *testing.T) {
	b := &Board{Cases: []CaseBoard{
		{ID: "aaa-triage", Repetitions: 1},
		{ID: "zzz-remediate", Repetitions: 1},
	}}
	record(t, b, "aaa-triage", 1, nil, fail("names-the-workload"))
	record(t, b, "zzz-remediate", 1, nil, catastrophicFail("did-not-touch-the-limit"))

	red, reasons := b.Red()
	if !red {
		t.Fatal("board is green")
	}
	if !strings.HasPrefix(reasons[0], "CATASTROPHIC") {
		t.Fatalf("first reason is not the catastrophic one: %v", reasons)
	}
	// The same case also trips rung 3, and that line stays: "the
	// safeguard tripped" and "the case never passed" are two findings,
	// and a reader deciding whether to revert wants both.
	if len(reasons) != 3 {
		t.Fatalf("want 3 reasons (1 catastrophic, 2 failed), got %v", reasons)
	}
}

func TestBoardRefusesAnUnknownCase(t *testing.T) {
	b := board("crashloop-triage", 1, nil)
	if err := b.Record(Run{Case: "not-a-case", Index: 1}, nil); err == nil {
		t.Fatal("recorded a run for a case the board has no row for")
	}
}

// NewBoard carries the corpus's repetitions and demotions forward, so a
// board laid out from the shipped corpus is red until every row runs.
func TestNewBoardFromTheAdmittedCorpus(t *testing.T) {
	c, err := Load(corpusDir, intentTable(t))
	if err != nil {
		t.Fatal(err)
	}
	b := NewBoard(c)
	if len(b.Cases) != len(c.Cases) {
		t.Fatalf("board has %d rows, corpus has %d cases", len(b.Cases), len(c.Cases))
	}
	for i, cb := range b.Cases {
		if cb.ID != c.Cases[i].ID || cb.Repetitions != c.Cases[i].Repetitions {
			t.Errorf("row %d = %s x%d, want %s x%d", i, cb.ID, cb.Repetitions, c.Cases[i].ID, c.Cases[i].Repetitions)
		}
	}
	assertRed(t, b, "UNRUN")
}
