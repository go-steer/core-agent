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

// Cost-ceiling kill switch (Mechanism not yet in docs/context-management-design.md —
// this is the v2.5 addition tracked in issue #145).
//
// Two bounds, both optional:
//
//   - Per-turn ceiling caps the spend of a single conversation turn (one
//     operator inject → agent done). Bounds the read-file-loop class of
//     bug (issue #144) where a model loops on the same tool call within
//     one turn — total turn cost balloons.
//   - Per-session ceiling caps cumulative spend across the entire
//     session. Bounds slow-burn patterns where each turn is reasonable
//     but the session adds up to more than expected (typical for long-
//     running autonomous deploys).
//
// Enforcement runs in the post-turn hook (same place as compactor and
// checkpointer). When a ceiling trips, the agent:
//
//  1. Emits a structured `turn-error` event with kind=cost_ceiling.
//  2. Sets costCeilingExceeded so the next Run call refuses to start.
//  3. Records the reason on costCeilingReason for /stats and similar
//     surfaces to display.
//
// Reset is operator-driven via Agent.ResetCostCeiling — typically wired
// to a slash command like `/resume-after-cost-ceiling`. There's no
// automatic reset: ceilings are a "stop, get human attention" signal,
// not a throttle.
//
// Limitations:
//
//   - Post-turn timing means a single runaway turn CAN overshoot the
//     per-turn budget before the check fires (all model calls in the
//     turn must complete first). Future enhancement: mid-turn detection
//     via SetOnAppend callback. For v1, post-turn is enough to bound
//     damage to one turn's worth of cost.
//   - Subtask costs (Mechanism-B agentic_* wrappers) are included in
//     the totals via usage.Tracker — they share the same accumulator.

package agent

import (
	"errors"
	"fmt"

	"github.com/go-steer/core-agent/v2/pkg/attach"
)

// CostCeiling configures the per-turn / per-session spend caps the
// post-turn hook enforces. Zero or negative values disable that
// specific ceiling — both fields default to disabled when constructed
// via the zero value.
type CostCeiling struct {
	// MaxTurnUSD is the cap on a single conversation turn's spend
	// (cumulative cost of every model call between one operator
	// inject and the next agent-done state). Tripped → next Run
	// refuses with an ErrCostCeilingExceeded error.
	MaxTurnUSD float64

	// MaxSessionUSD is the cap on the session's cumulative spend
	// across all turns (parent + subtask).
	MaxSessionUSD float64
}

// active reports whether either bound is set (enforcement runs).
func (c CostCeiling) active() bool {
	return c.MaxTurnUSD > 0 || c.MaxSessionUSD > 0
}

// ErrCostCeilingExceeded is returned by Agent.Run when a previous
// turn tripped the cost ceiling and the operator hasn't reset it.
// The error's message carries the specific ceiling that tripped and
// the spend that triggered it.
type costCeilingError struct {
	reason string
}

func (e *costCeilingError) Error() string { return e.reason }

// AsTurnError reports a ceiling refusal as the cost_ceiling kind
// instead of leaving pkg/attach to infer one from the reason prose.
// The classifier is substring-based and the reason matches none of its
// needles, so before this a refused turn recorded `error.type: unknown`
// on gen_ai.agent.invocation.duration — the spend-cap series went dark
// during exactly the incident it exists for (#818).
//
// The payload is the same one maybeEnforceCostCeiling puts on the wire
// when the ceiling first trips, so the trip and the refusals that
// follow it read identically.
// Nil-receiver safe on purpose: both producers return a literal nil
// rather than a typed-nil pointer, so this is unreachable today, but
// the caller is ClassifyTurnError running inside Run's deferred
// cleanup — a panic there takes down the turn's teardown, which is a
// steep price for the classic typed-nil-in-an-error-chain slip.
func (e *costCeilingError) AsTurnError() attach.TurnError {
	if e == nil {
		return costCeilingTurnError("")
	}
	return costCeilingTurnError(e.reason)
}

// costCeilingTurnError is the one construction site for the
// cost_ceiling payload — shared by the trip emit and by the refusal
// classification above so the two cannot drift apart.
func costCeilingTurnError(reason string) attach.TurnError {
	return attach.TurnError{
		Kind:      attach.TurnErrorCostCeiling,
		Code:      "cost_ceiling",
		Message:   reason,
		Retryable: false, // operator must reset, not the host
	}
}

var _ attach.SelfClassifyingError = (*costCeilingError)(nil)

// IsCostCeilingExceeded returns true when err was returned by Run
// because a previous turn tripped a configured cost ceiling.
// Operators / hosts use this to distinguish "operator must reset the
// ceiling" from other Run errors that may warrant retry.
func IsCostCeilingExceeded(err error) bool {
	_, ok := err.(*costCeilingError)
	return ok
}

// WithCostCeiling wires per-turn and per-session spend caps. Pass a
// zero-value CostCeiling{} (or 0 in either field) to disable the
// corresponding bound — at least one must be > 0 for enforcement to
// run at all. Mirrors the usual WithX option shape.
func WithCostCeiling(c CostCeiling) Option {
	return func(o *options) { o.costCeiling = c }
}

// ResetCostCeiling clears any tripped cost-ceiling flag, allowing
// the agent to accept new turns again. Typically wired to an
// operator slash command after the operator has reviewed why the
// ceiling tripped. Safe to call even if no ceiling is configured
// or no flag was set — no-op in that case.
//
// A bare reset is enough for a per-TURN trip: the next turn starts
// from a fresh baseline. It is NOT enough for a per-SESSION trip —
// the accumulator is already at or past the ceiling, so the very next
// turn re-trips. Pair it with AddSessionCostBudget (see
// WouldRetripCostCeiling) to hand the session real runway.
func (a *Agent) ResetCostCeiling() {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.costCeilingExceeded = false
	a.costCeilingReason = ""
	a.mu.Unlock()
}

// CostCeilingLimits returns the ceilings currently in force, including
// any runway added since construction via AddSessionCostBudget. Zero
// fields mean that bound is disabled.
func (a *Agent) CostCeilingLimits() CostCeiling {
	if a == nil {
		return CostCeiling{}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.costCeiling
}

// SessionCostUSD reports the session's cumulative spend as the ceiling
// enforcement sees it — the same accumulator a per-session trip is
// measured against. 0 when no usage tracker is wired.
func (a *Agent) SessionCostUSD() float64 {
	if a == nil || a.tracker == nil {
		return 0
	}
	return a.tracker.Totals().CostUSD
}

// AddSessionCostBudget raises the per-session ceiling by usd and
// returns the ceilings that result. This is the ONLY mutation the
// reset surface offers, and deliberately so: the alternatives — zeroing
// the accumulator, or restarting a spend "window" — would make
// Agent.SessionCostUSD, /usage and the eventlog-derived cost disagree
// about what the session actually spent. Raising the bar keeps every
// dollar counted and still hands the operator runway.
//
// usd must be > 0. Raising a disabled (0) session ceiling is refused:
// that would silently ARM a bound the operator never configured, which
// is a tighter posture than they asked for, not a looser one.
func (a *Agent) AddSessionCostBudget(usd float64) (CostCeiling, error) {
	if a == nil {
		return CostCeiling{}, errors.New("agent: nil agent")
	}
	if usd <= 0 {
		return a.CostCeilingLimits(), fmt.Errorf("additional budget must be > 0, got %.4f", usd)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.costCeiling.MaxSessionUSD <= 0 {
		return a.costCeiling, errors.New("no per-session cost ceiling is configured; nothing to raise")
	}
	a.costCeiling.MaxSessionUSD += usd
	return a.costCeiling, nil
}

// WouldRetripCostCeiling reports whether clearing the trip flag right
// now would be immediately undone — the session's accumulated spend is
// already at or past the per-session ceiling, so the next turn's
// enforcement pass trips again before the operator sees any progress.
// Returns the spend and the ceiling so the caller can say so precisely.
//
// This is the check that keeps the reset affordance honest: offering a
// button that provably does nothing is the same
// state-a-property-you-don't-enforce pattern the reset exists to fix.
func (a *Agent) WouldRetripCostCeiling() (retrip bool, spent, ceiling float64) {
	if a == nil {
		return false, 0, 0
	}
	spent = a.SessionCostUSD()
	a.mu.Lock()
	ceiling = a.costCeiling.MaxSessionUSD
	a.mu.Unlock()
	if ceiling <= 0 {
		return false, spent, 0
	}
	return spent >= ceiling, spent, ceiling
}

// CostCeilingTripped reports whether the agent is currently blocking
// new turns because a configured ceiling was exceeded. Exposed for
// /stats and similar UI surfaces; operators surface this alongside
// the totals so the "why is the agent refusing my prompts?" question
// has an obvious answer.
//
// Returns (true, reason) when blocked; (false, "") otherwise.
func (a *Agent) CostCeilingTripped() (bool, string) {
	if a == nil {
		return false, ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.costCeilingExceeded, a.costCeilingReason
}

// maybeEnforceCostCeiling checks the configured ceilings against the
// current tracker totals + the snapshot taken at turn start. Sets the
// costCeilingExceeded flag and emits a turn-error event when either
// ceiling trips. Idempotent — if already tripped, the check is a no-op
// so we don't re-emit on every subsequent call.
//
// Called from two spots, both against the same turnStartCost baseline:
//
//   - The post-turn hook (alongside maybeMarkCompactionPending), right
//     after the user-visible turn boundary closes. This catches in-turn
//     internal appends (subtasks, summarizer) immediately.
//   - The top of the next Run, before turnStartCost is re-snapshotted.
//     In harness-driven deployments the harness appends the just-finished
//     turn's main-model cost AFTER the post-turn hook, so only this
//     settle-time pass sees the full per-turn spend (#362).
//
// ...and, since #720, from Run's event tap while the turn is still
// running — see enforceCostCeilingInTurn. Both boundary call sites are
// kept: the tap only fires on events, so the last append of a turn
// (and anything a harness appends after the stream drains) still needs
// a post-turn and a settle-time pass behind it.
func (a *Agent) maybeEnforceCostCeiling() {
	if a == nil || a.tracker == nil {
		return
	}
	// Snapshot the ceilings under the lock: AddSessionCostBudget can
	// raise MaxSessionUSD at any time from an operator reset (#666).
	a.mu.Lock()
	if a.costCeilingExceeded {
		// Already tripped — no need to re-check or re-emit.
		a.mu.Unlock()
		return
	}
	ceiling := a.costCeiling
	turnStart := a.turnStartCost
	turnStartSet := a.turnStartCostSet
	a.mu.Unlock()
	if !ceiling.active() {
		return
	}

	sessionCost := a.tracker.Totals().CostUSD
	turnCost := sessionCost - turnStart

	var reason string
	switch {
	// The per-turn check needs a baseline from a turn this process
	// actually ran (#643). On a resumed session the tracker is rebuilt
	// with the entire prior spend before the first Run, so with a zero
	// baseline the first turn's "delta" is the whole session history —
	// enough to trip a per-turn ceiling for a turn that has not yet
	// cost a cent. The per-SESSION check below is unaffected: it reads
	// the accumulator directly, which is exactly what should carry
	// across a restart.
	case turnStartSet && ceiling.MaxTurnUSD > 0 && turnCost >= ceiling.MaxTurnUSD:
		reason = fmt.Sprintf(
			"per-turn cost ceiling exceeded: this turn cost $%.4f, ceiling is $%.4f. Agent will refuse new turns until the operator resets it (/guardrail reset, or POST /sessions/{id}/guardrails/reset).",
			turnCost, ceiling.MaxTurnUSD,
		)
	case ceiling.MaxSessionUSD > 0 && sessionCost >= ceiling.MaxSessionUSD:
		reason = fmt.Sprintf(
			"per-session cost ceiling exceeded: session has cost $%.4f, ceiling is $%.4f. Agent will refuse new turns until the operator resets it WITH additional budget (/guardrail reset +N, or POST /sessions/{id}/guardrails/reset with additional_budget_usd) — a bare reset would re-trip on the next turn.",
			sessionCost, ceiling.MaxSessionUSD,
		)
	default:
		return
	}

	a.mu.Lock()
	a.costCeilingExceeded = true
	a.costCeilingReason = reason
	a.mu.Unlock()

	// Durable halt (#643): the trip outlives this process, so a crash
	// or pod roll can't hand the runaway a fresh budget.
	a.queueGuardrailEvent(attach.NewGuardrailTripEvent(attach.GuardrailCostCeiling, reason))

	a.emit(attach.EventTurnError, costCeilingTurnError(reason))
}

// enforceCostCeilingInTurn is the in-turn arm of the cost ceiling
// (#720). It runs from Run's event tap and halts a turn that is still
// spending, rather than waiting for a boundary the turn may never
// reach.
//
// Why a third call site. Both boundary checks fire between turns, and
// a runaway is a loop INSIDE one turn: the model emits a tool call,
// the flow runs it and calls the model again, all within a single Run.
// The tracker grows on every one of those model calls — ADK's
// streaming aggregator marks each call's final chunk TurnComplete, so
// the harness's usage.TurnTap commits per call — so the spend is
// visible in real time while both boundary checks sit idle. #362
// already found boundary-only enforcement insufficient and worked
// around it by re-checking at the top of the FOLLOWING Run, which
// still cannot help a turn that is still running. --max-turn-cost-usd
// is documented as the hard backstop for exactly this shape (the
// watchdog's own alert text points operators at it), so it has to be
// able to fire during the turn it is capping.
//
// A trip cancels the turn in flight via Interrupt, which only cancels
// the per-turn context — it does not mark an operator-interrupt audit,
// so the halt is recorded as a cost-ceiling trip and not mislabeled.
//
// Cheap when unarmed: one uncontended lock read decides it, so a
// session with no ceiling configured (the default) pays nothing per
// event beyond that.
func (a *Agent) enforceCostCeilingInTurn() {
	if a == nil || a.tracker == nil {
		return
	}
	a.mu.Lock()
	armed := a.costCeiling.active() && !a.costCeilingExceeded
	a.mu.Unlock()
	if !armed {
		return
	}
	a.maybeEnforceCostCeiling()
	if exceeded, _ := a.CostCeilingTripped(); exceeded {
		a.Interrupt()
	}
}

// snapshotTurnStartCost captures the current session cost so the
// post-turn hook can compute the delta (turn cost). Called from
// Agent.Run at turn start, before the model is invoked. No-op when
// no ceiling is configured (avoid touching the tracker's mutex when
// we'd ignore the value anyway).
func (a *Agent) snapshotTurnStartCost() {
	if a == nil || a.tracker == nil || !a.CostCeilingLimits().active() {
		return
	}
	cost := a.tracker.Totals().CostUSD
	a.mu.Lock()
	a.turnStartCost = cost
	a.turnStartCostSet = true
	a.mu.Unlock()
}

// preflightCostCeiling returns a non-nil costCeilingError when a
// previous turn tripped the ceiling and the operator hasn't reset
// it. Called at the very top of Run, before any tracker writes or
// model calls — the refusal is structural, not driven by a fresh
// attempt that might also fail.
func (a *Agent) preflightCostCeiling() error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.costCeilingExceeded {
		return nil
	}
	return &costCeilingError{reason: a.costCeilingReason}
}
