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

// Guardrail state + reset, bridging the agent's two halt switches —
// the enforce-mode behavioral watchdog and the cost ceiling — onto
// GET /guardrails and POST /guardrails/reset (#666, semantics per
// #331).
//
// Both switches have had in-process Reset methods since they shipped
// and no operator-reachable caller. #642 turned them on by default, so
// this is the difference between "the run halted, restart the process"
// and "the run halted, look at why, resume."

package attachadapter

import (
	"fmt"
	"strings"

	"github.com/go-steer/core-agent/v2/pkg/agent"
	"github.com/go-steer/core-agent/v2/pkg/attach"
)

// AttachGuardrails implements attach.GuardrailProvider. Projects the
// agent's watchdog + cost-ceiling state into the wire shape. Nil-safe:
// a capability-only adapter reports every backstop off and nothing
// tripped, which is the truthful answer for an agent with no
// guardrails.
func (ad *Adapter) AttachGuardrails() attach.GuardrailInfo {
	a := ad.Agent()
	if a == nil {
		return attach.GuardrailInfo{Watchdog: attach.WatchdogInfo{Mode: "off"}}
	}
	wdTripped, wdReason := a.WatchdogTripped()
	ccTripped, ccReason := a.CostCeilingTripped()
	lim := a.CostCeilingLimits()
	retrip, spent, _ := a.WouldRetripCostCeiling()
	return attach.GuardrailInfo{
		Watchdog: attach.WatchdogInfo{
			Mode:    a.WatchdogMode(),
			Tripped: wdTripped,
			Reason:  wdReason,
		},
		CostCeiling: attach.CostCeilingInfo{
			MaxTurnUSD:     lim.MaxTurnUSD,
			MaxSessionUSD:  lim.MaxSessionUSD,
			SessionCostUSD: spent,
			Tripped:        ccTripped,
			Reason:         ccReason,
			WouldRetrip:    retrip,
		},
		Halted: wdTripped || ccTripped,
	}
}

// AttachResetGuardrail implements attach.GuardrailResetter.
//
// Order of operations matters and is deliberate:
//
//  1. Decide whether the reset can work BEFORE mutating anything. A
//     per-session cost trip whose spend already exceeds ceiling +
//     requested budget is refused with attach.ErrGuardrailRetrip and
//     leaves the agent untouched — no half-applied budget, and no 200
//     that the next turn undoes.
//  2. Raise the ceiling, then clear the flag. The reverse order leaves
//     a window where a concurrent post-turn enforcement pass re-trips
//     against the old ceiling.
//
// The refusal in (1) is whole-request, not per-guardrail: an
// under-budget guardrail=all on a session that ALSO tripped the
// watchdog clears neither. Clearing just the watchdog would leave the
// session halted anyway (the ceiling still refuses the next turn), so
// a partial 200 would only obscure that the operator's request didn't
// work. Scope to guardrail=watchdog to clear one in isolation.
//
// Budget is applied whenever cost_ceiling is in scope, tripped or not:
// raising the bar before a long run hits it is the same affordance as
// raising it after, and refusing the pre-emptive case would just teach
// operators to wait for the halt.
func (ad *Adapter) AttachResetGuardrail(req attach.GuardrailResetRequest) (attach.GuardrailResetResponse, error) {
	a := ad.Agent()
	if a == nil {
		return attach.GuardrailResetResponse{}, attach.ErrCapabilityNotRegistered
	}
	target := req.Guardrail
	if target == "" {
		target = attach.GuardrailAll
	}
	wantWatchdog := target == attach.GuardrailAll || target == attach.GuardrailWatchdog
	wantCost := target == attach.GuardrailAll || target == attach.GuardrailCostCeiling
	if !wantCost && req.AdditionalBudgetUSD > 0 {
		return attach.GuardrailResetResponse{Reset: []string{}, Guardrails: ad.AttachGuardrails()},
			fmt.Errorf("additional_budget_usd applies to the cost ceiling; use guardrail=%q or %q",
				attach.GuardrailCostCeiling, attach.GuardrailAll)
	}

	var resp attach.GuardrailResetResponse
	resp.Reset = []string{}

	// Step 1 — refuse a provably futile cost reset before touching
	// anything. A disabled session ceiling reports retrip=false, so a
	// per-TURN trip still clears on a bare reset.
	if wantCost {
		ccTripped, _ := a.CostCeilingTripped()
		retrip, spent, ceiling := a.WouldRetripCostCeiling()
		if ccTripped && retrip && spent >= ceiling+req.AdditionalBudgetUSD {
			short := spent - ceiling - req.AdditionalBudgetUSD
			return attach.GuardrailResetResponse{
					Reset:          []string{},
					BudgetAddedUSD: 0,
					Guardrails:     ad.AttachGuardrails(),
				}, fmt.Errorf(
					"%w: session has spent $%.4f against a $%.4f ceiling; add more than $%.4f via additional_budget_usd",
					attach.ErrGuardrailRetrip, spent, ceiling+req.AdditionalBudgetUSD, short,
				)
		}
	}

	// Step 2 — raise the ceiling, then clear flags.
	if wantCost && req.AdditionalBudgetUSD > 0 {
		if _, err := a.AddSessionCostBudget(req.AdditionalBudgetUSD); err != nil {
			return attach.GuardrailResetResponse{Reset: []string{}, Guardrails: ad.AttachGuardrails()}, err
		}
		resp.BudgetAddedUSD = req.AdditionalBudgetUSD
	}
	if wantWatchdog {
		if tripped, _ := a.WatchdogTripped(); tripped {
			a.ResetWatchdog()
			resp.Reset = append(resp.Reset, attach.GuardrailWatchdog)
		}
	}
	if wantCost {
		if tripped, _ := a.CostCeilingTripped(); tripped {
			a.ResetCostCeiling()
			resp.Reset = append(resp.Reset, attach.GuardrailCostCeiling)
		}
	}

	resp.Guardrails = ad.AttachGuardrails()
	resp.Message = guardrailResetMessage(resp)
	return resp, nil
}

// guardrailResetMessage renders the one-liner an operator surface
// echoes back. Says "nothing was tripped" explicitly rather than
// staying silent — a reset that found nothing to clear is a legitimate
// outcome, and silence reads like a failure.
func guardrailResetMessage(resp attach.GuardrailResetResponse) string {
	var parts []string
	if len(resp.Reset) > 0 {
		parts = append(parts, "cleared "+strings.Join(resp.Reset, " + "))
	}
	if resp.BudgetAddedUSD > 0 {
		parts = append(parts, fmt.Sprintf(
			"session ceiling raised by $%.2f to $%.2f",
			resp.BudgetAddedUSD, resp.Guardrails.CostCeiling.MaxSessionUSD,
		))
	}
	if len(parts) == 0 {
		// Distinguish "healthy" from "you scoped the reset to a
		// backstop that wasn't the one holding the session" — the
		// latter looks identical from the Reset list alone, and an
		// operator who reads "nothing to reset" while the session
		// stays halted would reasonably conclude the reset is broken.
		if still := stillTripped(resp.Guardrails); len(still) > 0 {
			return "nothing in scope to reset; session is still halted by " + strings.Join(still, " + ")
		}
		return "no guardrail was tripped; nothing to reset"
	}
	if still := stillTripped(resp.Guardrails); len(still) > 0 {
		parts = append(parts, "still halted by "+strings.Join(still, " + "))
	}
	return strings.Join(parts, "; ")
}

// stillTripped names the guardrails that remain tripped after a
// reset.
func stillTripped(info attach.GuardrailInfo) []string {
	var out []string
	if info.Watchdog.Tripped {
		out = append(out, attach.GuardrailWatchdog)
	}
	if info.CostCeiling.Tripped {
		out = append(out, attach.GuardrailCostCeiling)
	}
	return out
}

// guardrailCostCeilingArmed reports whether a spend bound can actually
// refuse a turn — the honest source for the capabilities frame's
// cost_ceiling feature key, which was hardcoded false before #666.
func guardrailCostCeilingArmed(a *agent.Agent) bool {
	if a == nil {
		return false
	}
	lim := a.CostCeilingLimits()
	return lim.MaxTurnUSD > 0 || lim.MaxSessionUSD > 0
}
