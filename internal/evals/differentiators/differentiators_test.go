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

package differentiators

import (
	"context"
	"strings"
	"testing"
)

// TestScenariosAreWellFormed guards the allowlist itself. The suite's
// both-directions check below is only meaningful if every scenario
// declares an outcome it is allowed to declare.
func TestScenariosAreWellFormed(t *testing.T) {
	if err := Validate(All()); err != nil {
		t.Fatal(err)
	}
	if got, want := len(All()), 6; got != want {
		t.Fatalf("registry has %d scenarios, want the %d named by docs/v0.3-plan.md W0.3 and docs/v0.4-plan.md W8", got, want)
	}
	want := map[string]bool{
		"E-exactly-once":      true,
		"E-ambiguous-refusal": true,
		"E-budget-exhaustion": true,
		"E-approval-rejected": true,
		"E-approval-edited":   true,
		// v0.4 W8. The registry is not frozen at W0.3's five, but it is
		// closed: a scenario nobody declared here is a scenario no plan
		// row is claiming, and the scoreboard would not know about it.
		"E-feedback-capture": true,
	}
	for _, s := range All() {
		if !want[s.ID] {
			t.Errorf("unexpected scenario %q — every scenario is named by a plan row and listed here", s.ID)
		}
		delete(want, s.ID)
	}
	for id := range want {
		t.Errorf("declared scenario %q is missing from the registry", id)
	}
}

// TestDifferentiators runs all six and requires each to land on its
// declared outcome.
//
// A mismatch fails in both directions on purpose:
//
//   - declared Fail, observed Pass — a capability landed and its
//     allowlist entry was not removed. Removing entries is the
//     progress metric (docs/v0.3-plan.md, W0.4).
//   - declared Pass, observed Fail — a shipped capability regressed.
//   - observed Broken — the fixture could not produce a run. Never
//     allowlistable: this is the check that keeps "red for want of a
//     capability" distinguishable from "red for want of a fixture".
func TestDifferentiators(t *testing.T) {
	scenarios := All()
	reports := RunAll(context.Background(), scenarios, t.TempDir())
	if len(reports) != len(scenarios) {
		t.Fatalf("got %d reports for %d scenarios", len(reports), len(scenarios))
	}
	for _, rep := range reports {
		s := rep.Scenario
		t.Run(s.ID, func(t *testing.T) {
			t.Logf("invariant: %s", s.Invariant)
			if rep.Result.Reason != "" {
				t.Logf("observed:  %s", rep.Result.Reason)
			}
			if rep.Err != nil {
				t.Fatalf("BROKEN — the fixture did not produce a run, so this says nothing about mast's capabilities: %v", rep.Err)
			}
			t.Logf("tools:     %v", rep.Result.Trace.CalledTools())
			if rep.Matches() {
				if rep.Outcome == Fail {
					t.Logf("expected-fail, blocked on %s", s.Blocked)
				}
				return
			}
			switch rep.Outcome {
			case Pass:
				t.Fatalf("declared FAIL (blocked on %s) but the invariant now holds — the capability landed; "+
					"flip Expect to Pass, drop Blocked, and say so in the PR (docs/v0.3-plan.md: shrinking the "+
					"expected-fail list is the progress metric)", s.Blocked)
			default:
				t.Fatalf("declared PASS but the invariant no longer holds — regression in shipped behaviour")
			}
		})
	}
}

// TestFixturesAreLive is the mechanical half of W0.3's "fails for the
// right reason". Every scenario, passing or failing, must drive a real
// run: a non-empty trace and a reason that names what was seen. A
// failing scenario whose fixture never got off the ground would be
// evidence about the harness, not about mast.
func TestFixturesAreLive(t *testing.T) {
	for _, rep := range RunAll(context.Background(), All(), t.TempDir()) {
		id := rep.Scenario.ID
		if rep.Err != nil {
			t.Errorf("%s: %v", id, rep.Err)
			continue
		}
		if len(rep.Result.Trace.Calls) == 0 {
			t.Errorf("%s: empty trace", id)
		}
		if strings.TrimSpace(rep.Result.Reason) == "" {
			t.Errorf("%s: no reason recorded", id)
		}
	}
}
