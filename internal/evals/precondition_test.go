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

func TestStartupSource(t *testing.T) {
	t.Run("keeps only the agent's own lines", func(t *testing.T) {
		src := StartupSource("some SDK warning\ncore-agent: skills: 1 loaded — gke-triage\nmore noise\n")
		if !src.Present {
			t.Fatal("Present = false")
		}
		if strings.Contains(src.Text, "SDK warning") || strings.Contains(src.Text, "more noise") {
			t.Errorf("foreign stderr survived the filter: %q", src.Text)
		}
		if !strings.Contains(src.Text, "skills: 1 loaded") {
			t.Errorf("agent line did not survive the filter: %q", src.Text)
		}
	})

	// The case that matters most. A process that died before printing
	// anything establishes nothing, and an absent source is vacuous —
	// which is indeterminate — rather than an empty string that a
	// none_of-only precondition would sail through.
	t.Run("absent when the process said nothing", func(t *testing.T) {
		if src := StartupSource("panic: boom\n"); src.Present {
			t.Errorf("Present = true for stderr with no agent lines: %q", src.Text)
		}
		res := Check{Name: "n", Why: "w", Source: SourceStartup, NoneOf: []string{"nope"}}.
			Verify(StartupSource(""))
		if !res.Vacuous {
			t.Error("a precondition against a process that printed nothing was not marked vacuous")
		}
	})
}

func TestCaseValidate_PreconditionSources(t *testing.T) {
	base := func(checks []Check, pres []Check) *Case {
		return &Case{
			ID: "c", PlantedDefect: "d", Fixture: "f", Prompt: "p",
			Checks: checks, Preconditions: pres,
		}
	}
	good := []Check{{Name: "ck", Why: "w", Source: SourceAnswer, AllOf: []string{"x"}}}

	t.Run("a graded check may not read the startup report", func(t *testing.T) {
		c := base([]Check{{Name: "ck", Why: "w", Source: SourceStartup, AllOf: []string{"x"}}}, nil)
		err := c.validate()
		if err == nil || !strings.Contains(err.Error(), "preconditions only") {
			t.Fatalf("got %v, want a refusal naming the tier split", err)
		}
	})

	t.Run("a precondition may not read a witness or the answer", func(t *testing.T) {
		for _, src := range []string{SourceAnswer, "witness:cluster-reads"} {
			c := base(good, []Check{{Name: "pc", Why: "w", Source: src, AllOf: []string{"x"}}})
			if err := c.validate(); err == nil {
				t.Errorf("source %q accepted in a precondition; it would be a graded check that cannot fail the case", src)
			}
		}
	})

	t.Run("names are one space across both lists", func(t *testing.T) {
		c := base(good, []Check{{Name: "ck", Why: "w", Source: SourceStartup, AllOf: []string{"x"}}})
		if err := c.validate(); err == nil || !strings.Contains(err.Error(), "duplicate name") {
			t.Fatalf("got %v, want a duplicate-name refusal: the report keys on it", err)
		}
	})

	t.Run("a well-formed precondition is accepted", func(t *testing.T) {
		c := base(good, []Check{{Name: "pc", Why: "w", Source: SourceStartup, AllOf: []string{"x"}}})
		if err := c.validate(); err != nil {
			t.Fatalf("validate: %v", err)
		}
	})
}

// Preconditions go through Bind like checks do, and the unknown-fact
// rule has to be as fatal here as it is there. An empty expansion in a
// precondition is the worse of the two failures: a precondition that
// always holds is a precondition that is not asked, which puts the case
// straight back in the hole #1061 describes — except now with an
// assertion sitting next to it that says otherwise.
func TestBindExpandsPreconditionTerms(t *testing.T) {
	base := &Case{
		ID: "c", PlantedDefect: "d", Fixture: "f", Prompt: "p",
		Checks: []Check{{Name: "ck", Why: "w", Source: SourceAnswer, AllOf: []string{"x"}}},
	}
	f := &Fixture{Name: "f", Facts: map[string]string{"skill": "gke-triage"}}

	t.Run("expands", func(t *testing.T) {
		c := *base
		c.Preconditions = []Check{{Name: "pc", Why: "w", Source: SourceStartup, AllOf: []string{"${fact.skill}"}}}
		bound, err := c.Bind(f)
		if err != nil {
			t.Fatalf("Bind: %v", err)
		}
		if got := bound.Preconditions[0].AllOf[0]; got != "gke-triage" {
			t.Fatalf("all_of[0] = %q, want the expanded fact", got)
		}
		if got := c.Preconditions[0].AllOf[0]; got != "${fact.skill}" {
			t.Fatalf("Bind mutated the receiver: %q", got)
		}
	})

	t.Run("an unknown fact is fatal", func(t *testing.T) {
		c := *base
		c.Preconditions = []Check{{Name: "pc", Why: "w", Source: SourceStartup, AllOf: []string{"${fact.typo}"}}}
		_, err := c.Bind(f)
		if err == nil || !strings.Contains(err.Error(), "unknown fact") {
			t.Fatalf("want an unknown-fact error, got %v", err)
		}
		// Named as a precondition, not as a check: the reader has to know
		// which of the two lists to open.
		if !strings.Contains(err.Error(), `precondition "pc"`) {
			t.Errorf("error calls it something other than a precondition: %v", err)
		}
	})
}

// An unmet precondition is indeterminate, never a fail, and it outranks
// both a passing check set and a RunError. The distinction is the whole
// point: "the agent did the wrong thing" and "the harness did not find
// out what the agent did" send a reader to different places.
func TestVerdict_UnmetPreconditionIsIndeterminate(t *testing.T) {
	passing := []CheckResult{{Name: "ck", Passed: true}}

	t.Run("checks all pass but the assumption did not hold", func(t *testing.T) {
		r := Result{
			Tier: TierTools, Checks: passing,
			Preconditions: []CheckResult{{Name: "pc", Passed: false, Missing: []string{"gke-triage"}}},
		}
		if got := r.Verdict(); got != VerdictIndeterminate {
			t.Errorf("verdict = %s, want %s — a green here is exactly the failure #1061 describes", got, VerdictIndeterminate)
		}
	})

	t.Run("a vacuous precondition counts as unmet", func(t *testing.T) {
		r := Result{
			Tier: TierTools, Checks: passing,
			Preconditions: []CheckResult{{Name: "pc", Passed: true, Vacuous: true}},
		}
		if got := r.Verdict(); got != VerdictIndeterminate {
			t.Errorf("verdict = %s, want %s", got, VerdictIndeterminate)
		}
	})

	t.Run("it outranks a run error", func(t *testing.T) {
		r := Result{
			Tier: TierTools, Checks: passing, RunError: "exit 1",
			Preconditions: []CheckResult{{Name: "pc", Passed: false}},
		}
		if got := r.Verdict(); got != VerdictIndeterminate {
			t.Errorf("verdict = %s, want %s: a process that was not the one the case describes is not evidence that the agent failed", got, VerdictIndeterminate)
		}
	})

	t.Run("a held precondition changes nothing", func(t *testing.T) {
		r := Result{
			Tier: TierTools, Checks: passing,
			Preconditions: []CheckResult{{Name: "pc", Passed: true}},
		}
		if got := r.Verdict(); got != VerdictPass {
			t.Errorf("verdict = %s, want %s", got, VerdictPass)
		}
	})
}

// Skills load with --no-builtin-tools, so a case's preconditions are
// expected to hold on the baseline exactly as on the tools tier. One
// that did not means the zero came from a process the case does not
// describe — the same argument BaselineHolds already makes about a
// process that died.
func TestBaselineHolds_RejectsAnUnmetPrecondition(t *testing.T) {
	r := Result{
		CaseID: "c", Tier: TierNoAccess,
		Checks:        []CheckResult{{Name: "ck", Passed: false, Reason: "missing"}},
		Preconditions: []CheckResult{{Name: "pc", Passed: false, Reason: "missing \"gke-triage\""}},
	}
	err := BaselineHolds(r)
	if err == nil {
		t.Fatal("BaselineHolds accepted a baseline whose precondition did not hold")
	}
	if !strings.Contains(err.Error(), "pc") {
		t.Errorf("error does not name the precondition: %v", err)
	}
}
