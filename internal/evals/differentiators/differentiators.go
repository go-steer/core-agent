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

// Package differentiators holds the v0.3 eval scenarios that the
// upstream LangChain SRE harness structurally cannot express
// (docs/v0.3-plan.md W0.3). Its 31-row corpus scores a single
// uninterrupted trajectory against a list of expected tool names;
// nothing in that shape can ask what happens when a remediation is
// interrupted halfway, when a prior effect's outcome is unknown, when
// the budget runs out mid-investigation, or when the operator says no.
//
// Each scenario drives the composed mast runtime — internal/compose's
// root, the internal/effects outbox, the pkg/budget meter, a real SQLite
// session store — with a scripted model, and checks one invariant.
//
// # Three outcomes, not two
//
// A scenario is Pass, Fail, or Broken.
//
//   - Pass: the invariant held.
//   - Fail: the run happened and the invariant was violated. This is a
//     capability gap, and it is a legitimate state for a scenario to be
//     declared in while the capability is unbuilt.
//   - Broken: the fixture could not produce a run at all. This is a
//     harness defect and is never declarable.
//
// That split is what makes W0.3's "fails for the right reason (missing
// capability, not missing fixture)" mechanical rather than a matter of
// authorial care. A scenario that cannot set the situation up returns
// an error and lands in Broken, which no allowlist can absorb; and the
// driver additionally requires every scenario — passing or failing — to
// produce a non-empty evals.Trace, so "the capability is missing" is
// always an observation about a run that happened, never the absence of
// one.
//
// # Expect is the allowlist, and it is checked in both directions
//
// Scenario.Expect declares what the scenario does against today's code.
// The driver fails when the outcome differs from Expect either way: a
// regression in a shipped capability, and a capability landed without
// flipping its scenario, are the same kind of defect. That bidirectional
// check is why the declaration can be a hand-maintained bit without
// rotting — you cannot land W2 without this suite telling you which
// entries to remove.
package differentiators

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/go-steer/core-agent/v3/internal/evals"
)

// Outcome is the three-valued result of running one scenario.
type Outcome int

const (
	// Broken is the zero value on purpose: a scenario that returns
	// before setting an outcome has not run, and "has not run" must
	// never read as a pass.
	Broken Outcome = iota
	// Fail means the run completed and the invariant was violated.
	Fail
	// Pass means the invariant held.
	Pass
)

func (o Outcome) String() string {
	switch o {
	case Pass:
		return "PASS"
	case Fail:
		return "FAIL"
	default:
		return "BROKEN"
	}
}

// Result is what a scenario's Run reports back.
type Result struct {
	// Held is the invariant's verdict.
	Held bool

	// Reason states what was observed, and is required whether the
	// invariant held or not. For a failing scenario it is the evidence
	// that a capability is missing; a scenario that cannot say what it
	// saw should return an error instead.
	Reason string

	// Trace is the run's recorded trajectory, scored through the same
	// adapter the parity corpus uses (internal/evals). The driver
	// requires it to carry at least one call: it is the mechanical
	// proof that the fixture drove a real run.
	Trace evals.Trace
}

// Scenario is one differentiator.
type Scenario struct {
	// ID is the tier-prefixed name from docs/v0.3-plan.md §2, e.g.
	// "E-approval-rejected".
	ID string

	// Invariant is the one sentence the scenario checks. It is the
	// scoreboard row's claim in testable form.
	Invariant string

	// Expect is the outcome this scenario has against today's code.
	// Pass or Fail only; see the package doc on why it is checked in
	// both directions.
	Expect Outcome

	// Blocked names the workstream that flips Expect to Pass and the
	// concrete thing that is missing. Required when Expect is Fail,
	// and must be empty when Expect is Pass — a passing scenario that
	// still claims to be blocked is a stale declaration.
	Blocked string

	// Rows are the docs/v0.3-plan.md §1 scoreboard rows this scenario
	// is the proof for.
	Rows []string

	// Run sets the fixture up, drives it, and checks the invariant.
	// A returned error is Broken: the fixture could not produce a run,
	// which is a harness defect and not an allowlistable failure.
	Run func(ctx context.Context, env Env) (Result, error)
}

// Env carries what a scenario needs from its caller.
type Env struct {
	// Dir is a scratch directory the scenario owns for the duration of
	// the run — session databases, specialist fixtures. House rule #5:
	// callers derive it from os.TempDir (t.TempDir does), never $HOME.
	Dir string
}

// Report pairs a scenario with what it actually did.
type Report struct {
	Scenario Scenario
	Result   Result
	Outcome  Outcome
	// Err is set when the scenario returned one, i.e. when Outcome is
	// Broken.
	Err error
}

// Matches reports whether the observed outcome is the declared one.
func (r Report) Matches() bool { return r.Outcome == r.Scenario.Expect }

// All returns the six differentiator scenarios in a stable order.
func All() []Scenario {
	return []Scenario{
		exactlyOnce(),
		ambiguousRefusal(),
		budgetExhaustion(),
		approvalRejected(),
		approvalEdited(),
		feedbackCapture(),
	}
}

// Validate checks the registry's declarations for internal
// consistency. It is a guard on the allowlist itself: the driver's
// both-directions check is only meaningful if every scenario declares
// an outcome it is allowed to declare.
func Validate(scenarios []Scenario) error {
	if len(scenarios) == 0 {
		return fmt.Errorf("differentiators: empty registry")
	}
	seen := make(map[string]bool, len(scenarios))
	var problems []string
	for _, s := range scenarios {
		switch {
		case s.ID == "":
			problems = append(problems, "a scenario has no ID")
			continue
		case !strings.HasPrefix(s.ID, "E-"):
			problems = append(problems, fmt.Sprintf("%s: deterministic-tier scenarios take the E- prefix (docs/v0.3-plan.md §2)", s.ID))
		}
		if seen[s.ID] {
			problems = append(problems, fmt.Sprintf("%s: duplicate ID", s.ID))
		}
		seen[s.ID] = true
		if s.Invariant == "" {
			problems = append(problems, fmt.Sprintf("%s: no invariant stated", s.ID))
		}
		if s.Run == nil {
			problems = append(problems, fmt.Sprintf("%s: no Run", s.ID))
		}
		if len(s.Rows) == 0 {
			problems = append(problems, fmt.Sprintf("%s: names no scoreboard row", s.ID))
		}
		switch s.Expect {
		case Pass:
			if s.Blocked != "" {
				problems = append(problems, fmt.Sprintf("%s: expects Pass but still declares Blocked %q — stale allowlist entry", s.ID, s.Blocked))
			}
		case Fail:
			if s.Blocked == "" {
				problems = append(problems, fmt.Sprintf("%s: expects Fail but names no blocking workstream", s.ID))
			}
		default:
			problems = append(problems, fmt.Sprintf("%s: Expect must be Pass or Fail, not %v", s.ID, s.Expect))
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return fmt.Errorf("differentiators: %s", strings.Join(problems, "; "))
	}
	return nil
}

// RunAll executes every scenario in its own subdirectory of the
// caller's scratch root and reports what each did. It never returns an
// error: a scenario that blows up is a Broken report, which is the
// point of the three-valued outcome.
func RunAll(ctx context.Context, scenarios []Scenario, root string) []Report {
	reports := make([]Report, 0, len(scenarios))
	for _, s := range scenarios {
		reports = append(reports, run(ctx, s, root))
	}
	return reports
}

func run(ctx context.Context, s Scenario, root string) Report {
	rep := Report{Scenario: s}
	if s.Run == nil {
		rep.Err = fmt.Errorf("scenario %q has no Run", s.ID)
		return rep
	}
	dir, err := scratch(root, s.ID)
	if err != nil {
		rep.Err = err
		return rep
	}
	res, err := s.Run(ctx, Env{Dir: dir})
	rep.Result = res
	if err != nil {
		rep.Err = err
		return rep
	}
	switch {
	case res.Reason == "":
		rep.Err = fmt.Errorf("scenario %q reported no reason; a scenario that cannot say what it saw is Broken, not a verdict", s.ID)
	case len(res.Trace.Calls) == 0:
		// The fixture-liveness check. A scenario whose run made no tool
		// calls did not reach the situation it exists to describe, so
		// whatever it concluded is about the fixture, not the runtime.
		rep.Err = fmt.Errorf("scenario %q produced an empty trace: the fixture did not drive a run, so its verdict is not about mast", s.ID)
	case res.Held:
		rep.Outcome = Pass
	default:
		rep.Outcome = Fail
	}
	return rep
}
