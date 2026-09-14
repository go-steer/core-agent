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

// Package evals is the behavioural eval skeleton (#652): one case, run
// against a real provider, graded by a deterministic verifier.
//
// The distinction this package exists to make is the one nothing else
// in the repository makes. Unit tests prove functions work; the recipe
// checks prove files have the right shape. Neither says whether the
// agent found the thing it was asked to find. That is what a case
// measures, and it is measured the only way it can honestly be
// measured: by reading what the run left behind, never by reading the
// model's account of it.
//
// Three rules are structural rather than stylistic, and each one is
// enforced by code in this package rather than by review:
//
//   - A case names its planted defect. A defect that is not written
//     down cannot be reviewed, and a case nobody can review is a green
//     check with no referent.
//   - A case's prompt may not contain any of its fixture's facts. That
//     is the "withhold the location" rule, and without it the case
//     measures whether the model can copy a noun out of the prompt.
//     Load fails if the prompt leaks a fact (see Case.Bind).
//   - A check that carries no information about the run says so, and
//     an aggregate containing one refuses to report a verdict rather
//     than reporting green. See Vacuous in verify.go.
//
// The corpus is #966 and the metric program is #967. Adding a case here
// must require no Go: a case is a JSON file, its world is a JSON file,
// and the shim that renders that world is shared. If a second case
// needs code, the schema is wrong.
package evals

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

// A Case is one eval: a prompt, the defect planted in the fixture it
// runs against, and the checks that decide whether the agent found it.
type Case struct {
	// ID is the case's stable name. It appears in the report and is
	// the handle an issue refers to.
	ID string `json:"id"`

	// PlantedDefect is prose, and it is required. It says what is
	// wrong with the fixture's world in one sentence — the answer a
	// perfect run would arrive at. A reviewer who disagrees with a
	// check has to be able to read this to know which of the two is
	// wrong.
	PlantedDefect string `json:"planted_defect"`

	// Fixture names the directory under the fixtures root that carries
	// this case's world. The case never names a path: only the fixture
	// catalog knows paths, so a fixture can move without touching the
	// corpus, and a case cannot smuggle a location into a check.
	Fixture string `json:"fixture"`

	// Prompt is what the agent is asked. It must not contain any of
	// the fixture's facts.
	Prompt string `json:"prompt"`

	// Checks are the deterministic verifiers. All of them are the same
	// type on purpose — see Check.
	Checks []Check `json:"checks"`
}

// A Check is the one deterministic verifier type.
//
// One type, not a taxonomy. Every question this skeleton can ask has
// the same shape — "does this witness contain these strings, and not
// those" — and the discipline that makes that enough is the
// evidence-chain rule: a check that demands the observed artefact and
// the value it carried *together* is hard to satisfy without having
// read both, which a single substring match is not.
//
// Source is either "answer" (the agent's final output) or
// "witness:<name>", naming one of the fixture's witnesses. A witness is
// a file the *world* wrote — the fixture's kubectl shim logging what it
// was asked, for instance — never a file this process produced. That
// asymmetry is the point: an eventlog we wrote is the system reporting
// on itself, and it also cannot see a subagent that fanned out
// in-process. What the shim was asked, it was asked by somebody, and
// the somebody does not matter.
type Check struct {
	// Name identifies the check in the report. Required, because a
	// failing check the reader cannot name is a failing check nobody
	// acts on.
	Name string `json:"name"`

	// Why says what property this check is asserting, in prose. Also
	// required: the same argument as PlantedDefect, one level down.
	Why string `json:"why"`

	// Source is "answer" or "witness:<name>".
	Source string `json:"source"`

	// AllOf are fixed strings that must all be present, matched
	// case-insensitively but otherwise literally — no regex, because a
	// corpus of regexes is a corpus of bugs nobody reviews.
	AllOf []string `json:"all_of,omitempty"`

	// AnyOf are fixed strings of which at least one must be present.
	//
	// This is not a convenience. A property with several legitimate
	// expressions — "the agent looked outside the default namespace"
	// is reached by `-n <ns>`, by `--all-namespaces`, or by listing
	// namespaces first — has to be written as AnyOf or written as one
	// arbitrary form, and the arbitrary form measures whether the
	// agent guessed the author's idiom.
	AnyOf []string `json:"any_of,omitempty"`

	// NoneOf are fixed strings that must all be absent.
	//
	// A NoneOf-only check against an empty source passes without
	// learning anything, which is why Vacuous exists. See verify.go.
	NoneOf []string `json:"none_of,omitempty"`
}

// factRef matches ${fact.<key>} in a check term.
var factRef = regexp.MustCompile(`\$\{fact\.([a-z0-9_]+)\}`)

// SourceAnswer is the Source value naming the agent's final output.
const SourceAnswer = "answer"

// witnessPrefix is the Source prefix naming a fixture witness.
const witnessPrefix = "witness:"

// WitnessName returns the witness a Source names, and whether it named
// one at all.
func (c Check) WitnessName() (string, bool) {
	if !strings.HasPrefix(c.Source, witnessPrefix) {
		return "", false
	}
	return strings.TrimPrefix(c.Source, witnessPrefix), true
}

// LoadCase reads a case from disk and validates its shape.
//
// Shape only. The checks that need the fixture — fact expansion and the
// prompt-leak rule — happen in Bind, because they cannot be answered
// without knowing what the facts are.
func LoadCase(path string) (*Case, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("evals: read case: %w", err)
	}
	var c Case
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("evals: parse case %s: %w", path, err)
	}
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("evals: case %s: %w", path, err)
	}
	return &c, nil
}

func (c *Case) validate() error {
	if strings.TrimSpace(c.ID) == "" {
		return fmt.Errorf("id is required")
	}
	if strings.TrimSpace(c.PlantedDefect) == "" {
		return fmt.Errorf("planted_defect is required; a case whose defect is not written down cannot be reviewed")
	}
	if strings.TrimSpace(c.Fixture) == "" {
		return fmt.Errorf("fixture is required")
	}
	if strings.TrimSpace(c.Prompt) == "" {
		return fmt.Errorf("prompt is required")
	}
	if len(c.Checks) == 0 {
		return fmt.Errorf("at least one check is required")
	}
	seen := map[string]bool{}
	for i, ck := range c.Checks {
		where := fmt.Sprintf("checks[%d]", i)
		if strings.TrimSpace(ck.Name) == "" {
			return fmt.Errorf("%s: name is required", where)
		}
		if seen[ck.Name] {
			return fmt.Errorf("%s: duplicate check name %q; the report keys on it", where, ck.Name)
		}
		seen[ck.Name] = true
		if strings.TrimSpace(ck.Why) == "" {
			return fmt.Errorf("%s (%s): why is required; a check nobody can justify is a check nobody can delete", where, ck.Name)
		}
		if ck.Source != SourceAnswer {
			name, ok := ck.WitnessName()
			if !ok || strings.TrimSpace(name) == "" {
				return fmt.Errorf("%s (%s): source must be %q or %q<name>, got %q", where, ck.Name, SourceAnswer, witnessPrefix, ck.Source)
			}
		}
		if len(ck.AllOf) == 0 && len(ck.AnyOf) == 0 && len(ck.NoneOf) == 0 {
			return fmt.Errorf("%s (%s): needs at least one of all_of, any_of, none_of; a check with none of them asserts nothing", where, ck.Name)
		}
		if len(ck.AnyOf) == 1 {
			return fmt.Errorf("%s (%s): any_of with a single term is all_of spelled misleadingly; use all_of", where, ck.Name)
		}
		for _, term := range concat(ck.AllOf, ck.AnyOf, ck.NoneOf) {
			if strings.TrimSpace(term) == "" {
				return fmt.Errorf("%s (%s): empty term; an empty needle is found in every haystack", where, ck.Name)
			}
		}
	}
	return nil
}

// Bind resolves ${fact.<key>} references in the case's check terms
// against the fixture's facts, and enforces the withhold-the-location
// rule.
//
// The returned Case is a copy with the terms expanded; the receiver is
// untouched, so a caller can bind one case against two fixtures without
// the first binding contaminating the second.
//
// Two failures are deliberate and neither is recoverable:
//
// An unknown fact key is an error rather than an empty expansion. An
// empty term is found in every haystack, so a typo in a fact name would
// silently convert an AllOf check into one that always passes — the
// exact shape of failure this package exists to catch.
//
// A prompt that contains a fact value is an error because the case is
// then measuring the wrong thing. If the prompt says "payments-prod",
// an answer saying "payments-prod" is a copy, not a finding, and the
// no-access baseline will happily score it. The rule is mechanical
// because "did I leak the location" is not a question a reviewer
// reliably asks on the twentieth case.
func (c *Case) Bind(f *Fixture) (*Case, error) {
	bound := *c
	bound.Checks = make([]Check, len(c.Checks))

	if leaked := leakedFacts(c.Prompt, f.Facts); len(leaked) > 0 {
		return nil, fmt.Errorf(
			"evals: case %s: prompt contains fixture fact(s) %s — withhold the location, or the case measures whether the model can copy a noun out of the prompt and the no-access baseline will score it",
			c.ID, strings.Join(leaked, ", "))
	}

	for i, ck := range c.Checks {
		out := ck
		var err error
		if out.AllOf, err = expandTerms(ck.AllOf, f.Facts); err != nil {
			return nil, fmt.Errorf("evals: case %s: check %q: all_of: %w", c.ID, ck.Name, err)
		}
		if out.AnyOf, err = expandTerms(ck.AnyOf, f.Facts); err != nil {
			return nil, fmt.Errorf("evals: case %s: check %q: any_of: %w", c.ID, ck.Name, err)
		}
		if out.NoneOf, err = expandTerms(ck.NoneOf, f.Facts); err != nil {
			return nil, fmt.Errorf("evals: case %s: check %q: none_of: %w", c.ID, ck.Name, err)
		}
		if name, ok := ck.WitnessName(); ok {
			if _, declared := f.Witnesses[name]; !declared {
				return nil, fmt.Errorf("evals: case %s: check %q: fixture %s declares no witness %q (has: %s)",
					c.ID, ck.Name, f.Name, name, strings.Join(sortedKeys(f.Witnesses), ", "))
			}
		}
		bound.Checks[i] = out
	}
	return &bound, nil
}

func expandTerms(terms []string, facts map[string]string) ([]string, error) {
	if len(terms) == 0 {
		return nil, nil
	}
	out := make([]string, len(terms))
	for i, term := range terms {
		var bad []string
		out[i] = factRef.ReplaceAllStringFunc(term, func(m string) string {
			key := factRef.FindStringSubmatch(m)[1]
			val, ok := facts[key]
			if !ok {
				bad = append(bad, key)
				return m
			}
			return val
		})
		if len(bad) > 0 {
			return nil, fmt.Errorf("unknown fact(s) %s in %q (fixture has: %s)",
				strings.Join(bad, ", "), term, strings.Join(sortedKeys(facts), ", "))
		}
	}
	return out, nil
}

// leakedFacts reports which fact values appear in the prompt.
//
// Case-insensitive and substring, which is deliberately over-eager: a
// false positive costs one reworded prompt, and a false negative costs
// a case that scores a copy as a finding for as long as it ships.
func leakedFacts(prompt string, facts map[string]string) []string {
	lower := strings.ToLower(prompt)
	var leaked []string
	for _, key := range sortedKeys(facts) {
		val := strings.TrimSpace(facts[key])
		if val == "" {
			continue
		}
		if strings.Contains(lower, strings.ToLower(val)) {
			leaked = append(leaked, fmt.Sprintf("%s=%q", key, val))
		}
	}
	return leaked
}

func concat(xss ...[]string) []string {
	var out []string
	for _, xs := range xss {
		out = append(out, xs...)
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
