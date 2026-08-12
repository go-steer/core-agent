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

// Tests for the operator-reset primitives added in #666: the budget
// bump, the would-it-re-trip probe, and the accessors the attach
// surface projects. The behavior that matters here is that a bare
// reset of a per-SESSION trip is a trap — these are the pieces that
// let the reset surface say so instead of returning a useless success.

package agent

import (
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/usage"
)

func TestAddSessionCostBudget_RaisesTheCeiling(t *testing.T) {
	t.Parallel()
	a := &Agent{costCeiling: CostCeiling{MaxTurnUSD: 1, MaxSessionUSD: 10}}
	got, err := a.AddSessionCostBudget(5)
	if err != nil {
		t.Fatalf("AddSessionCostBudget: %v", err)
	}
	if got.MaxSessionUSD != 15 {
		t.Errorf("MaxSessionUSD = %v, want 15", got.MaxSessionUSD)
	}
	if got.MaxTurnUSD != 1 {
		t.Errorf("per-turn ceiling must be untouched, got %v", got.MaxTurnUSD)
	}
	if live := a.CostCeilingLimits(); live.MaxSessionUSD != 15 {
		t.Errorf("CostCeilingLimits = %+v, want the raised ceiling", live)
	}
}

func TestAddSessionCostBudget_RefusesNonPositive(t *testing.T) {
	t.Parallel()
	a := &Agent{costCeiling: CostCeiling{MaxSessionUSD: 10}}
	for _, usd := range []float64{0, -1} {
		if _, err := a.AddSessionCostBudget(usd); err == nil {
			t.Errorf("AddSessionCostBudget(%v) = nil error, want a refusal", usd)
		}
	}
	if a.CostCeilingLimits().MaxSessionUSD != 10 {
		t.Error("a refused bump must not mutate the ceiling")
	}
}

// Adding budget to an agent with NO session ceiling would arm a bound
// the operator never configured — a tighter posture than they asked
// for. Refuse rather than silently start enforcing.
func TestAddSessionCostBudget_RefusesArmingADisabledCeiling(t *testing.T) {
	t.Parallel()
	a := &Agent{costCeiling: CostCeiling{MaxTurnUSD: 1}}
	_, err := a.AddSessionCostBudget(5)
	if err == nil {
		t.Fatal("want a refusal when no per-session ceiling is configured")
	}
	if !strings.Contains(err.Error(), "no per-session cost ceiling") {
		t.Errorf("error = %q, want it to name the missing ceiling", err)
	}
	if a.CostCeilingLimits().MaxSessionUSD != 0 {
		t.Error("refused bump armed a ceiling that was disabled")
	}
}

func TestWouldRetripCostCeiling(t *testing.T) {
	t.Parallel()
	tr := usage.NewTracker()
	tr.Append("test", 12_000_000, 0, usage.Pricing{InputPerMTok: 1}) // $12
	a := &Agent{tracker: tr, costCeiling: CostCeiling{MaxSessionUSD: 10}}

	retrip, spent, ceiling := a.WouldRetripCostCeiling()
	if !retrip {
		t.Errorf("spent $%.2f against a $%.2f ceiling should re-trip", spent, ceiling)
	}
	if spent != 12 || ceiling != 10 {
		t.Errorf("got spent=%v ceiling=%v, want 12/10", spent, ceiling)
	}
	if a.SessionCostUSD() != 12 {
		t.Errorf("SessionCostUSD = %v, want 12", a.SessionCostUSD())
	}

	// Enough budget clears it; the accumulator is untouched.
	if _, err := a.AddSessionCostBudget(5); err != nil {
		t.Fatal(err)
	}
	retrip, spent, ceiling = a.WouldRetripCostCeiling()
	if retrip {
		t.Errorf("$12 spent against a raised $%.2f ceiling should not re-trip", ceiling)
	}
	if spent != 12 {
		t.Errorf("raising the ceiling must not rewrite spend: got %v, want 12", spent)
	}
}

// A per-TURN trip with no session bound is fully recoverable by a bare
// reset: the next turn starts from a fresh baseline.
func TestWouldRetripCostCeiling_NoSessionBound(t *testing.T) {
	t.Parallel()
	tr := usage.NewTracker()
	tr.Append("test", 99_000_000, 0, usage.Pricing{InputPerMTok: 1})
	a := &Agent{tracker: tr, costCeiling: CostCeiling{MaxTurnUSD: 1}}
	if retrip, _, ceiling := a.WouldRetripCostCeiling(); retrip || ceiling != 0 {
		t.Errorf("retrip=%v ceiling=%v, want false/0 with no session bound", retrip, ceiling)
	}
}

// Enforcement must read the CURRENT ceiling, not one captured at
// construction: a reset that adds budget is worthless if the next
// post-turn pass measures against the old bar.
func TestMaybeEnforceCostCeiling_SeesAddedBudget(t *testing.T) {
	t.Parallel()
	tr := usage.NewTracker()
	a := &Agent{tracker: tr, costCeiling: CostCeiling{MaxSessionUSD: 1}}
	a.snapshotTurnStartCost()
	tr.Append("test", 1_200_000, 0, usage.Pricing{InputPerMTok: 1}) // $1.20
	a.maybeEnforceCostCeiling()
	if tripped, _ := a.CostCeilingTripped(); !tripped {
		t.Fatal("should trip at $1.20 against a $1.00 session ceiling")
	}

	if _, err := a.AddSessionCostBudget(5); err != nil {
		t.Fatal(err)
	}
	a.ResetCostCeiling()
	a.snapshotTurnStartCost()
	a.maybeEnforceCostCeiling()
	if tripped, reason := a.CostCeilingTripped(); tripped {
		t.Errorf("re-tripped against the pre-bump ceiling: %s", reason)
	}
}

// The trip reason is the only recovery instruction most operators
// will read, so it must name something they can actually type — not a
// Go method they'd have to be embedding the library to call (#666).
func TestCostCeilingReason_NamesAnOperatorAffordance(t *testing.T) {
	t.Parallel()
	tr := usage.NewTracker()
	a := &Agent{tracker: tr, costCeiling: CostCeiling{MaxSessionUSD: 1}}
	a.snapshotTurnStartCost()
	tr.Append("test", 2_000_000, 0, usage.Pricing{InputPerMTok: 1})
	a.maybeEnforceCostCeiling()
	_, reason := a.CostCeilingTripped()
	for _, want := range []string{"/guardrail reset", "guardrails/reset", "additional_budget_usd"} {
		if !strings.Contains(reason, want) {
			t.Errorf("per-session trip reason %q missing %q", reason, want)
		}
	}
	if strings.Contains(reason, "Agent.ResetCostCeiling") {
		t.Errorf("reason names a Go method an operator can't call: %q", reason)
	}
}
