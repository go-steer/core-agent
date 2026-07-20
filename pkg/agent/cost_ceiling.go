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
func (a *Agent) ResetCostCeiling() {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.costCeilingExceeded = false
	a.costCeilingReason = ""
	a.mu.Unlock()
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

// maybeEnforceCostCeiling is the post-turn hook entry point. Runs
// after each turn finishes; checks the configured ceilings against
// the current tracker totals + the snapshot taken at turn start.
// Sets the costCeilingExceeded flag and emits a turn-error event
// when either ceiling trips. Idempotent — if already tripped, the
// check is a no-op so we don't re-emit on every subsequent turn-end.
//
// Called from the same post-turn hook spot as
// maybeMarkCompactionPending; both run after the user-visible turn
// boundary closes.
func (a *Agent) maybeEnforceCostCeiling() {
	if a == nil || a.tracker == nil || !a.costCeiling.active() {
		return
	}
	a.mu.Lock()
	if a.costCeilingExceeded {
		// Already tripped — no need to re-check or re-emit.
		a.mu.Unlock()
		return
	}
	turnStart := a.turnStartCost
	a.mu.Unlock()

	sessionCost := a.tracker.Totals().CostUSD
	turnCost := sessionCost - turnStart

	var reason string
	switch {
	case a.costCeiling.MaxTurnUSD > 0 && turnCost >= a.costCeiling.MaxTurnUSD:
		reason = fmt.Sprintf(
			"per-turn cost ceiling exceeded: this turn cost $%.4f, ceiling is $%.4f. Agent will refuse new turns until operator calls ResetCostCeiling.",
			turnCost, a.costCeiling.MaxTurnUSD,
		)
	case a.costCeiling.MaxSessionUSD > 0 && sessionCost >= a.costCeiling.MaxSessionUSD:
		reason = fmt.Sprintf(
			"per-session cost ceiling exceeded: session has cost $%.4f, ceiling is $%.4f. Agent will refuse new turns until operator calls ResetCostCeiling.",
			sessionCost, a.costCeiling.MaxSessionUSD,
		)
	default:
		return
	}

	a.mu.Lock()
	a.costCeilingExceeded = true
	a.costCeilingReason = reason
	a.mu.Unlock()

	a.emit(attach.EventTurnError, attach.TurnError{
		Kind:      attach.TurnErrorCostCeiling,
		Code:      "cost_ceiling",
		Message:   reason,
		Retryable: false, // operator must reset, not the host
	})
}

// snapshotTurnStartCost captures the current session cost so the
// post-turn hook can compute the delta (turn cost). Called from
// Agent.Run at turn start, before the model is invoked. No-op when
// no ceiling is configured (avoid touching the tracker's mutex when
// we'd ignore the value anyway).
func (a *Agent) snapshotTurnStartCost() {
	if a == nil || a.tracker == nil || !a.costCeiling.active() {
		return
	}
	cost := a.tracker.Totals().CostUSD
	a.mu.Lock()
	a.turnStartCost = cost
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
