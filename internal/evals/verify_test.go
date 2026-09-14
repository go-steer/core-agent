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

package evals

import (
	"strings"
	"testing"
)

func TestVerifyAllOf(t *testing.T) {
	c := Check{Name: "n", Source: SourceAnswer, AllOf: []string{"ledger-writer", "manifest unknown"}}

	got := c.Verify(Source{Text: "the ledger-writer deployment reports MANIFEST UNKNOWN", Present: true})
	if !got.Passed || got.Score() != 1 {
		t.Fatalf("want a scoring pass (matching is case-insensitive), got %+v", got)
	}

	got = c.Verify(Source{Text: "the ledger-writer deployment is unhappy", Present: true})
	if got.Passed {
		t.Fatal("want a fail")
	}
	if len(got.Missing) != 1 || got.Missing[0] != "manifest unknown" {
		t.Fatalf("want the missing term named, got %+v", got.Missing)
	}
}

func TestVerifyAnyOfNeedsOnlyOne(t *testing.T) {
	c := Check{Name: "n", Source: SourceAnswer, AnyOf: []string{"--all-namespaces", " -A", "get ns"}}
	if got := c.Verify(Source{Text: "kubectl get ns", Present: true}); !got.Passed {
		t.Fatalf("want a pass, got %+v", got)
	}
	got := c.Verify(Source{Text: "kubectl get pods", Present: true})
	if got.Passed {
		t.Fatal("want a fail")
	}
	if len(got.Alternatives) != 3 {
		t.Fatalf("a failing any_of should echo every alternative, got %+v", got.Alternatives)
	}
}

func TestVerifyNoneOf(t *testing.T) {
	c := Check{Name: "n", Source: "witness:reads", NoneOf: []string{"kubectl delete", "kubectl apply"}}
	if got := c.Verify(Source{Text: "kubectl get pods -A", Present: true}); !got.Passed {
		t.Fatalf("want a pass, got %+v", got)
	}
	got := c.Verify(Source{Text: "kubectl get pods\nkubectl apply -f x.yaml", Present: true})
	if got.Passed {
		t.Fatal("want a fail")
	}
	if len(got.Found) != 1 || got.Found[0] != "kubectl apply" {
		t.Fatalf("want the forbidden term named, got %+v", got.Found)
	}
}

// An absent source is the shape that makes a report indeterminate
// rather than green: the witness was never written, so the check
// observed nothing at all.
func TestVerifyAbsentSourceIsVacuous(t *testing.T) {
	c := Check{Name: "n", Source: "witness:reads", NoneOf: []string{"kubectl delete"}}
	got := c.Verify(Source{Present: false})
	if !got.Vacuous {
		t.Fatalf("want vacuous, got %+v", got)
	}
	if got.Score() != 0 {
		t.Fatal("a vacuous check must never score")
	}
	if !strings.Contains(got.Reason, "observed nothing") {
		t.Fatalf("reason should say so: %q", got.Reason)
	}
}

// The textbook vacuous pass, and the reason Vacuous exists: every
// forbidden string is absent from an empty file, because everything is.
func TestVerifyNoneOfAgainstAnEmptySourceIsVacuous(t *testing.T) {
	c := Check{Name: "n", Source: "witness:reads", NoneOf: []string{"kubectl delete"}}
	got := c.Verify(Source{Text: "  \n", Present: true})
	if !got.Passed {
		t.Fatal("it does pass — that is the trap")
	}
	if !got.Vacuous || got.Score() != 0 {
		t.Fatalf("want a vacuous non-scoring pass, got %+v", got)
	}
	if !strings.Contains(got.Reason, "nothing is absent from nothing") {
		t.Fatalf("reason should name the trap: %q", got.Reason)
	}
}

// ...but emptiness is informative when the check also demands something
// be present, so that shape is a plain fail rather than vacuous.
func TestVerifyEmptySourceWithAllOfIsAFailNotVacuous(t *testing.T) {
	c := Check{Name: "n", Source: "witness:reads", AllOf: []string{"kubectl get"}, NoneOf: []string{"kubectl delete"}}
	got := c.Verify(Source{Text: "", Present: true})
	if got.Vacuous {
		t.Fatalf("an empty source is informative when a term was required: %+v", got)
	}
	if got.Passed {
		t.Fatal("want a fail")
	}
}

func TestResultVerdict(t *testing.T) {
	pass := CheckResult{Name: "a", Passed: true}
	fail := CheckResult{Name: "b"}
	vac := CheckResult{Name: "c", Passed: true, Vacuous: true}

	tests := []struct {
		name string
		r    Result
		want Verdict
	}{
		{"all pass", Result{Checks: []CheckResult{pass, pass}}, VerdictPass},
		{"one fail", Result{Checks: []CheckResult{pass, fail}}, VerdictFail},
		{"one vacuous beats a clean sweep", Result{Checks: []CheckResult{pass, vac}}, VerdictIndeterminate},
		{"no checks", Result{}, VerdictIndeterminate},
		{"run error", Result{Checks: []CheckResult{pass}, RunError: "boom"}, VerdictFail},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.r.Verdict(); got != tt.want {
				t.Fatalf("Verdict = %s, want %s", got, tt.want)
			}
		})
	}
}

// The baseline is graded by BaselineHolds, not folded into the
// aggregate. It fails and is partly vacuous by construction — an agent
// with no tools cannot name the workload and writes no witness — so
// folding it in would make every report indeterminate forever, whatever
// the agent did.
func TestReportVerdictExcludesTheBaselineTier(t *testing.T) {
	rep := Report{Results: []Result{
		{Tier: TierTools, Checks: []CheckResult{{Name: "a", Passed: true}}},
		{Tier: TierNoAccess, Checks: []CheckResult{
			{Name: "a", Vacuous: true},
			{Name: "b"},
		}},
	}}
	if got := rep.Verdict(); got != VerdictPass {
		t.Fatalf("Verdict = %s, want %s", got, VerdictPass)
	}
}

func TestReportVerdictWithNoGradedTier(t *testing.T) {
	rep := Report{Results: []Result{{Tier: TierNoAccess, Checks: []CheckResult{{Name: "a", Passed: true}}}}}
	if got := rep.Verdict(); got != VerdictIndeterminate {
		t.Fatalf("a report with only a baseline has measured nothing: got %s", got)
	}
	if got := (Report{}).Verdict(); got != VerdictIndeterminate {
		t.Fatalf("empty report: got %s", got)
	}
}

func TestBaselineHolds(t *testing.T) {
	informativeFail := CheckResult{Name: "named-it"}
	vacuous := CheckResult{Name: "searched", Vacuous: true}

	t.Run("zero score with one informative check holds", func(t *testing.T) {
		r := Result{Tier: TierNoAccess, Checks: []CheckResult{informativeFail, vacuous}}
		if err := BaselineHolds(r); err != nil {
			t.Fatalf("want it to hold, got %v", err)
		}
	})

	t.Run("a scoring check breaks it", func(t *testing.T) {
		r := Result{Tier: TierNoAccess, Checks: []CheckResult{{Name: "named-it", Passed: true}}}
		err := BaselineHolds(r)
		if err == nil || !strings.Contains(err.Error(), "measure vocabulary") {
			t.Fatalf("want a vocabulary error, got %v", err)
		}
		if !strings.Contains(err.Error(), "named-it") {
			t.Fatalf("the error must name the offending check: %v", err)
		}
	})

	// The cheap way to get a zero is to never run. The first live run of
	// this harness died at startup on a flag collision, scored zero, and
	// would have been reported as a sound baseline.
	t.Run("a crashed baseline does not hold", func(t *testing.T) {
		r := Result{Tier: TierNoAccess, RunError: "exit status 2: bad flag", Checks: []CheckResult{informativeFail}}
		err := BaselineHolds(r)
		if err == nil || !strings.Contains(err.Error(), "did not run to completion") {
			t.Fatalf("want a did-not-run error, got %v", err)
		}
	})

	// A baseline in which every check observed nothing has measured the
	// absence of a measurement, not a measurement of absence.
	t.Run("an entirely vacuous baseline does not hold", func(t *testing.T) {
		r := Result{Tier: TierNoAccess, Checks: []CheckResult{vacuous, {Name: "other", Vacuous: true}}}
		err := BaselineHolds(r)
		if err == nil || !strings.Contains(err.Error(), "absence of a measurement") {
			t.Fatalf("want an all-vacuous error, got %v", err)
		}
	})

	t.Run("wrong tier is a programming error", func(t *testing.T) {
		if err := BaselineHolds(Result{Tier: TierTools}); err == nil {
			t.Fatal("want an error")
		}
	})
}
