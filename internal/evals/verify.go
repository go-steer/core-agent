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
	"fmt"
	"strings"
)

// A Tier is which way a case was run.
type Tier string

const (
	// TierTools is the real run: the agent has its tools and the
	// fixture's world on PATH.
	TierTools Tier = "tools"

	// TierNoAccess is the grader's self-test. The same case, the same
	// prompt, the same checks, and no tools at all.
	//
	// Every objective check must score 0 here or the case does not
	// ship. A check a naked model satisfies is measuring vocabulary,
	// and it measures vocabulary no matter how good the agent is — so
	// this runs in CI beside the real tier rather than being audited
	// once when the case is written.
	TierNoAccess Tier = "no-access"
)

// A Verdict is an aggregate's answer.
type Verdict string

const (
	VerdictPass Verdict = "pass"
	VerdictFail Verdict = "fail"

	// VerdictIndeterminate is the one that earns its keep.
	//
	// A run containing a check that carried no information about it
	// does not get to be green. The failure mode this prevents is
	// specific and quiet: a none_of check against a witness the world
	// never wrote passes, because nothing is absent from nothing, and
	// an aggregate that counts it reports a clean run in which nothing
	// was observed. That is exactly the shape of the credential-less
	// CI green #488 was filed for, one layer up.
	VerdictIndeterminate Verdict = "indeterminate"
)

// A CheckResult is one check's outcome.
type CheckResult struct {
	Name string `json:"name"`
	Why  string `json:"why"`

	// Passed is whether every AllOf term was present and every NoneOf
	// term absent. It is not the score — see Vacuous.
	Passed bool `json:"passed"`

	// Vacuous marks a result that carries no information about the
	// run. A vacuous result never scores, whatever Passed says.
	Vacuous bool `json:"vacuous"`

	// Reason is the human-readable why, always set.
	Reason string `json:"reason"`

	// Missing are AllOf terms that were not found; Found are NoneOf
	// terms that were; Alternatives is the AnyOf set when none of it
	// matched. All three are echoed so a failing check names the
	// string rather than making the reader diff two lists.
	Missing      []string `json:"missing,omitempty"`
	Found        []string `json:"found,omitempty"`
	Alternatives []string `json:"alternatives,omitempty"`
}

// Score is 1 for an informative pass and 0 otherwise.
func (r CheckResult) Score() int {
	if r.Passed && !r.Vacuous {
		return 1
	}
	return 0
}

// A Source is the text a check reads, plus whether it existed at all.
type Source struct {
	Text    string
	Present bool
}

// Verify runs one check against one source.
//
// Matching is case-insensitive fixed-string containment. Not regex: a
// corpus of regexes is a corpus of bugs nobody reviews, and the
// evidence-chain discipline — demand the artefact and the value it
// carried together — is what makes plain containment strong enough.
func (c Check) Verify(src Source) CheckResult {
	res := CheckResult{Name: c.Name, Why: c.Why}

	if !src.Present {
		res.Vacuous = true
		res.Reason = fmt.Sprintf("source %s does not exist — nothing reached it, so this check observed nothing", c.Source)
		return res
	}

	hay := strings.ToLower(src.Text)
	for _, term := range c.AllOf {
		if !strings.Contains(hay, strings.ToLower(term)) {
			res.Missing = append(res.Missing, term)
		}
	}
	anySatisfied := len(c.AnyOf) == 0
	for _, term := range c.AnyOf {
		if strings.Contains(hay, strings.ToLower(term)) {
			anySatisfied = true
			break
		}
	}
	if !anySatisfied {
		res.Alternatives = c.AnyOf
	}
	for _, term := range c.NoneOf {
		if strings.Contains(hay, strings.ToLower(term)) {
			res.Found = append(res.Found, term)
		}
	}
	res.Passed = len(res.Missing) == 0 && len(res.Found) == 0 && len(res.Alternatives) == 0

	// A none_of-only check against an empty source is the textbook
	// vacuous pass: every forbidden string is absent, because
	// everything is. An all_of or any_of term in the same check makes
	// the emptiness informative — the term is genuinely missing — so
	// only the none_of-only shape is marked.
	if strings.TrimSpace(src.Text) == "" && len(c.AllOf) == 0 && len(c.AnyOf) == 0 {
		res.Vacuous = true
		res.Reason = fmt.Sprintf("source %s is empty and this check only forbids strings — nothing is absent from nothing", c.Source)
		return res
	}

	if res.Passed {
		res.Reason = fmt.Sprintf("source %s satisfies every term (%d required, %d alternatives, %d forbidden)", c.Source, len(c.AllOf), len(c.AnyOf), len(c.NoneOf))
		return res
	}
	var why []string
	if len(res.Missing) > 0 {
		why = append(why, "missing "+quoteAll(res.Missing))
	}
	if len(res.Alternatives) > 0 {
		why = append(why, "carries none of "+quoteAll(res.Alternatives))
	}
	if len(res.Found) > 0 {
		why = append(why, "carries forbidden "+quoteAll(res.Found))
	}
	res.Reason = fmt.Sprintf("source %s %s", c.Source, strings.Join(why, "; "))
	return res
}

// A Result is one case run at one tier.
type Result struct {
	CaseID        string        `json:"case_id"`
	Fixture       string        `json:"fixture"`
	PlantedDefect string        `json:"planted_defect"`
	Tier          Tier          `json:"tier"`
	Checks        []CheckResult `json:"checks"`

	// Answer is the agent's final output, kept so a failing run can be
	// read without re-running it.
	Answer string `json:"answer,omitempty"`

	// RunError is set when the agent process itself failed. The checks
	// still run — a crashed run whose checks all fail is a clearer
	// report than a bare exit code — but the verdict can never be a
	// pass.
	RunError string `json:"run_error,omitempty"`
}

// Score is the number of informative passes.
func (r Result) Score() int {
	n := 0
	for _, c := range r.Checks {
		n += c.Score()
	}
	return n
}

// Vacuous reports whether any check observed nothing.
func (r Result) Vacuous() bool {
	for _, c := range r.Checks {
		if c.Vacuous {
			return true
		}
	}
	return false
}

// Verdict folds the checks into one answer.
func (r Result) Verdict() Verdict {
	if len(r.Checks) == 0 {
		return VerdictIndeterminate
	}
	if r.Vacuous() {
		return VerdictIndeterminate
	}
	if r.RunError != "" {
		return VerdictFail
	}
	for _, c := range r.Checks {
		if !c.Passed {
			return VerdictFail
		}
	}
	return VerdictPass
}

// BaselineHolds decides whether a no-access run clears the grader's
// self-test.
//
// The bar is a score of zero, and vacuity does not rescue a check that
// scored: a baseline that scores anything means the check can be
// satisfied without tools, which means it is measuring the model's
// vocabulary and will keep measuring vocabulary however the agent
// changes. The case does not ship.
//
// A score of zero is necessary and not sufficient. A baseline that
// never ran also scores zero, and it is the cheaper of the two ways to
// get one: mistype a flag and the process dies at startup, every check
// observes nothing, and the self-test reports that the corpus is sound.
// The first live run of this harness failed exactly that way — the
// no-access tier exited 2 on a flag collision — so the two conditions
// below are the ones that make the zero mean something.
func BaselineHolds(r Result) error {
	if r.Tier != TierNoAccess {
		return fmt.Errorf("evals: BaselineHolds wants a %s result, got %s", TierNoAccess, r.Tier)
	}
	if r.RunError != "" {
		return fmt.Errorf(
			"evals: case %s: the no-access baseline did not run to completion (%s) — a process that died scores zero for a reason that says nothing about the checks, so this is not evidence that they need tools",
			r.CaseID, r.RunError)
	}
	var scored []string
	for _, c := range r.Checks {
		if c.Score() > 0 {
			scored = append(scored, c.Name)
		}
	}
	if len(scored) > 0 {
		return fmt.Errorf(
			"evals: case %s: no-access baseline scored %d — check(s) %s passed with zero tool access, so they measure vocabulary rather than behaviour and the case must not ship",
			r.CaseID, r.Score(), quoteAll(scored))
	}
	informative := 0
	for _, c := range r.Checks {
		if !c.Vacuous {
			informative++
		}
	}
	if informative == 0 {
		return fmt.Errorf(
			"evals: case %s: every no-access check observed nothing, so the baseline's zero is the absence of a measurement rather than a measurement of absence — a case needs at least one check the agent could have satisfied from priors alone for this tier to test anything",
			r.CaseID)
	}
	return nil
}

// A Report is a set of results, and the thing a CI leg exits on.
type Report struct {
	Results []Result `json:"results"`
}

// Verdict is the weakest verdict over the graded tiers: any
// indeterminate makes the whole report indeterminate, then any fail
// makes it a fail.
//
// The no-access tier is deliberately not one of the graded tiers. Its
// contract is BaselineHolds, and by construction it fails and is partly
// vacuous — an agent with no tools cannot name the workload, and no
// witness is written because nothing reached the world. Folding that
// into the aggregate would make every report indeterminate forever,
// whatever the agent did, and an alarm that is always ringing is one
// nobody reads. Callers must consult BaselineHolds separately; the two
// answers together are the result, and evalrun reports the baseline
// first because a tools-tier pass on a case a naked model also passes
// is not evidence of anything.
func (rep Report) Verdict() Verdict {
	graded := 0
	worst := VerdictPass
	for _, r := range rep.Results {
		if r.Tier == TierNoAccess {
			continue
		}
		graded++
		switch r.Verdict() {
		case VerdictIndeterminate:
			return VerdictIndeterminate
		case VerdictFail:
			worst = VerdictFail
		}
	}
	if graded == 0 {
		return VerdictIndeterminate
	}
	return worst
}

func quoteAll(xs []string) string {
	out := make([]string, len(xs))
	for i, x := range xs {
		out[i] = fmt.Sprintf("%q", x)
	}
	return strings.Join(out, ", ")
}
