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
// The two bounds do NOT have the same consequence (#1049), and the
// difference is the bound's own name:
//
//   - A per-SESSION trip halts the session. The accumulator it measures
//     does not reset, so the next turn would trip it again anyway, and
//     the halt is written durably (#643) so a pod roll cannot hand a
//     runaway a fresh budget. Reset is operator-driven via
//     Agent.ResetCostCeiling and wants AddSessionCostBudget beside it.
//   - A per-TURN trip ends its turn and nothing more. No flag, no
//     durable row, next turn starts from a fresh baseline. One
//     expensive turn in an eight-hour unattended run should not end the
//     run, and a single turn's spend should not outlive the pod that
//     spent it.
//
// What keeps the second of those from becoming an unbounded spend loop
// against a driver that re-drives is maxConsecutiveTurnCeilingTrips: N
// turns in a row each hitting the per-turn ceiling escalates to the
// ordinary session halt, with a reason that says that is what happened.
//
// Either way the trip emits a `guardrail-trip` event (#891) carrying
// the reason, and records it on costCeilingReason for /stats and
// similar surfaces. Ceilings are a "get human attention" signal, not a
// throttle: nothing clears a halt automatically.
//
// Enforcement runs at three points — the post-turn hook (same place as
// compactor and checkpointer), the top of the next Run once the prior
// turn has settled (#362), and Run's event tap while the turn is still
// spending (#720).
//
// Limitations:
//
//   - The bound is a floor, not a cap. Even the in-turn tap only fires
//     between events, so a turn overshoots its ceiling by whatever the
//     model call in flight goes on to cost; the boundary passes
//     overshoot by a whole turn when the tap cannot see the spend (a
//     harness that appends the main-model cost after the stream drains).
//   - Subtask costs (Mechanism-B agentic_* wrappers) are included in
//     the totals via usage.Tracker — they share the same accumulator.

package agent

import (
	"errors"
	"fmt"

	"github.com/go-steer/core-agent/v2/pkg/attach"
)

// CostCeiling configures the per-turn / per-session spend caps
// enforcement checks against. Zero or negative values disable that
// specific ceiling — both fields default to disabled when constructed
// via the zero value.
type CostCeiling struct {
	// MaxTurnUSD is the cap on a single conversation turn's spend
	// (cumulative cost of every model call between one operator
	// inject and the next agent-done state). Tripped → the turn is
	// cut and a `guardrail-trip` event reports why. The session is
	// NOT halted and the next turn runs (#1049) — until
	// maxConsecutiveTurnCeilingTrips turns in a row trip it, which
	// escalates to the session halt below.
	MaxTurnUSD float64

	// MaxSessionUSD is the cap on the session's cumulative spend
	// across all turns (parent + subtask). Tripped → every subsequent
	// Run refuses with an ErrCostCeilingExceeded error until the
	// operator resets it, and the halt is durable across a restart.
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
// A bare reset is enough for a per-TURN escalation: the streak is
// cleared here and the next turn starts from a fresh baseline. It is
// NOT enough for a per-SESSION trip — the accumulator is already at or
// past the ceiling, so the very next turn re-trips. Pair it with
// AddSessionCostBudget (see WouldRetripCostCeiling) to hand the session
// real runway.
//
// Clearing the streak is the point of the reset on that arm: leaving it
// at the escalation threshold would re-halt the session on the very
// next per-turn trip, one turn after an operator looked at it and said
// carry on (#1049).
func (a *Agent) ResetCostCeiling() {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.costCeilingExceeded = false
	a.costCeilingReason = ""
	a.turnCeilingStreak = 0
	a.turnCeilingTripped = false
	a.mu.Unlock()
	// Release any wake the halt swallowed (#1040). No-op when the
	// watchdog is also tripped. Note this fires even when the reset
	// will immediately re-trip (WouldRetripCostCeiling) — the re-trip
	// happens on the next turn's enforcement pass, which is the
	// operator-visible way to learn that a bare reset was not enough;
	// swallowing the wake here would make that silent instead.
	a.releaseFencedWake()
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
//
// It answers "is the session halted", which since #1049 is a narrower
// question than "did a cost ceiling trip". A per-turn trip ends its
// turn and leaves this false; only the per-session bound and a
// maxConsecutiveTurnCeilingTrips escalation set it. Callers that want
// every trip want the `guardrail-trip` event, not this.
func (a *Agent) CostCeilingTripped() (bool, string) {
	if a == nil {
		return false, ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.costCeilingExceeded, a.costCeilingReason
}

// maybeEnforceCostCeiling checks the configured ceilings against the
// current tracker totals + the snapshot taken at turn start, and emits
// a `guardrail-trip` when either is met or exceeded. What else it does
// depends on which one tripped — see the two-bounds note below.
//
// Idempotent within a halt and within a turn: a session already halted
// short-circuits at the top, and a turn already over its per-turn bound
// is latched by turnCeilingTripped, so neither re-emits.
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
//
// haltedTurn says which of those shapes the caller is (#891): true only
// from the in-turn tap, which cuts the running turn the moment this
// trips, false from both boundary passes, where the turn either already
// completed or has not started. It rides out on the `guardrail-trip`
// event so a client knows whether to expect a `canceled` turn-error or
// a `turn-complete` next. A parameter rather than a turnInFlight()
// probe on purpose: the post-turn hook's in-flight state is an artifact
// of where Run clears its cancel func, and hanging an operator-visible
// field off that is the kind of coupling that breaks silently when the
// cleanup order is rearranged. The call sites know the answer.
//
// Returns true when this pass tripped something, so the in-turn tap
// knows to cut the turn. That used to be read back off
// CostCeilingTripped, which stopped being the same question once a
// per-turn trip stopped setting the session flag (#1049).
//
// THE TWO BOUNDS ARE NOT THE SAME KIND OF EVENT (#1049). A per-SESSION
// trip halts the session: the accumulator it measures does not reset,
// so the next turn would trip it again, and the halt is written durably
// because a pod roll must not hand a runaway a fresh budget. A per-TURN
// trip ends its turn and nothing more — the bound is named for a turn,
// the next turn starts from a fresh baseline, and one expensive turn in
// an eight-hour unattended run should not end the run. It sets no flag
// and writes no durable row; a client learns about it from the
// `guardrail-trip` event, which is exactly what that event is for.
//
// What stops a driver from re-driving into the ceiling forever is
// maxConsecutiveTurnCeilingTrips, not the per-turn halt that used to be
// there. See its doc comment for why the streak and not the bound.
func (a *Agent) maybeEnforceCostCeiling(haltedTurn bool) bool {
	if a == nil || a.tracker == nil {
		return false
	}
	// Snapshot the ceilings under the lock: AddSessionCostBudget can
	// raise MaxSessionUSD at any time from an operator reset (#666).
	a.mu.Lock()
	if a.costCeilingExceeded {
		// Already halted — no need to re-check or re-emit.
		a.mu.Unlock()
		return false
	}
	ceiling := a.costCeiling
	turnStart := a.turnStartCost
	turnStartSet := a.turnStartCostSet
	turnTripped := a.turnCeilingTripped
	a.mu.Unlock()
	if !ceiling.active() {
		return false
	}

	sessionCost := a.tracker.Totals().CostUSD
	turnCost := sessionCost - turnStart

	var reason string
	// halt says whether this trip takes the session down with the turn.
	// Only a per-session trip and a per-turn escalation do.
	halt := true
	switch {
	// The per-turn check needs a baseline from a turn this process
	// actually ran (#643). On a resumed session the tracker is rebuilt
	// with the entire prior spend before the first Run, so with a zero
	// baseline the first turn's "delta" is the whole session history —
	// enough to trip a per-turn ceiling for a turn that has not yet
	// cost a cent. The per-SESSION check below is unaffected: it reads
	// the accumulator directly, which is exactly what should carry
	// across a restart.
	//
	// turnTripped is this turn's once-only latch. costCeilingExceeded
	// used to serve, because a per-turn trip set it; now that one does
	// not, the tap would otherwise re-trip on every remaining event of
	// a turn whose cost is already over the bound.
	case !turnTripped && turnStartSet && ceiling.MaxTurnUSD > 0 && turnCost >= ceiling.MaxTurnUSD:
		streak := a.recordTurnCeilingTrip()
		if streak >= maxConsecutiveTurnCeilingTrips {
			reason = fmt.Sprintf(
				"per-turn cost ceiling halted the session: %d turns in a row each hit the $%.4f per-turn ceiling (the last cost $%.4f). Every one was inside its own bound, so no per-session ceiling caught the pattern. Agent will refuse new turns until the operator resets it (/guardrail reset, or POST /sessions/{id}/guardrails/reset).",
				streak, ceiling.MaxTurnUSD, turnCost,
			)
			break
		}
		halt = false
		reason = fmt.Sprintf(
			"per-turn cost ceiling exceeded: this turn cost $%.4f, ceiling is $%.4f. The turn was stopped; the session is NOT halted and the next turn starts from a fresh per-turn budget. %d in a row now — at %d the session halts and needs an operator reset.",
			turnCost, ceiling.MaxTurnUSD, streak, maxConsecutiveTurnCeilingTrips,
		)
	case ceiling.MaxSessionUSD > 0 && sessionCost >= ceiling.MaxSessionUSD:
		reason = fmt.Sprintf(
			"per-session cost ceiling exceeded: session has cost $%.4f, ceiling is $%.4f. Agent will refuse new turns until the operator resets it WITH additional budget (/guardrail reset +N, or POST /sessions/{id}/guardrails/reset with additional_budget_usd) — a bare reset would re-trip on the next turn.",
			sessionCost, ceiling.MaxSessionUSD,
		)
	default:
		return false
	}

	if halt {
		a.mu.Lock()
		a.costCeilingExceeded = true
		a.costCeilingReason = reason
		a.mu.Unlock()

		// Durable halt (#643): the trip outlives this process, so a
		// crash or pod roll can't hand the runaway a fresh budget. Only
		// a halt is written. A per-turn trip that ends its own turn has
		// nothing to restore — re-arming it on the next process would
		// mean one turn's spend halting a session it never halted.
		a.queueOutOfBandEvent(attach.NewGuardrailTripEvent(attach.GuardrailCostCeiling, reason))
	}

	a.emitGuardrailTrip(attach.GuardrailCostCeiling, reason, haltedTurn)
	return true
}

// maxConsecutiveTurnCeilingTrips is how many turns may end in a per-turn
// ceiling trip, back to back, before the session halts (#1049).
//
// The streak exists because "a per-turn bound bounds a turn" is only
// half an answer. Take it literally and an agent whose driver re-drives
// — a wake loop, auto-continue — spends up to the ceiling per turn,
// forever, and a hard stop has become an unbounded spend loop. That is
// worse than the halt being removed here, and it is the exact shape
// --max-turn-cost-usd was added to bound (#144's read-file loop).
//
// So the bound stays a turn-level bound and the RUNAWAY is what gets a
// session-level answer. Three is deliberately small: one trip is an
// expensive turn, two is a coincidence, three is a pattern nobody is
// watching. A turn that finishes inside its bound clears the count, so
// an agent that occasionally runs hot never reaches it.
//
// MaxSessionUSD remains the primary session bound and is untouched by
// any of this; the streak is what protects an operator who configured
// only a per-turn cap.
const maxConsecutiveTurnCeilingTrips = 3

// recordTurnCeilingTrip latches this turn as tripped and returns the
// resulting consecutive-trip count. Called exactly once per turn, from
// the per-turn arm above, under the latch that arm checks first.
func (a *Agent) recordTurnCeilingTrip() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.turnCeilingTripped = true
	a.turnCeilingStreak++
	return a.turnCeilingStreak
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
// The cancellation it causes IS reported as a `canceled` turn-error —
// that is the cut turn's accurate outcome and its only terminal frame,
// now that the trip has its own non-terminal event to ride (#891,
// replacing #818's suppression).
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
	// Cut the turn on what this pass decided, not on the session flag.
	// A per-turn trip no longer sets that flag (#1049), and the whole
	// point of a per-turn bound is that it stops the turn — reading the
	// session flag back here would have left the runaway turn running.
	if a.maybeEnforceCostCeiling(true) {
		// Mark before cutting so the turn's metric point is labelled
		// with the guardrail rather than the bare `canceled` the
		// Interrupt produces (#818 part 2; see guardrail_halt.go). The
		// `canceled` turn-error itself now stands as this turn's one
		// terminal frame — the trip went out on its own non-terminal
		// event above, so there is nothing left to suppress (#891).
		a.markGuardrailHalt(attach.TurnErrorCostCeiling)
		a.Interrupt()
	}
}

// snapshotTurnStartCost captures the current session cost so the
// post-turn hook can compute the delta (turn cost). Called from
// Agent.Run at turn start, before the model is invoked. No-op when
// no ceiling is configured (avoid touching the tracker's mutex when
// we'd ignore the value anyway).
//
// It also rolls the per-turn ceiling bookkeeping over to the new turn
// (#1049), and the placement is load-bearing. Run calls this AFTER the
// settle-time enforcement pass, which is where a harness-driven
// deployment's previous turn is finally judged (#362) — so by the time
// this runs, turnCeilingTripped is the finished turn's verdict, not a
// half-formed one. A turn that ended inside its bound clears the
// streak; a turn that tripped leaves it standing for this turn to add
// to.
func (a *Agent) snapshotTurnStartCost() {
	if a == nil || a.tracker == nil || !a.CostCeilingLimits().active() {
		return
	}
	cost := a.tracker.Totals().CostUSD
	a.mu.Lock()
	a.turnStartCost = cost
	a.turnStartCostSet = true
	if !a.turnCeilingTripped {
		a.turnCeilingStreak = 0
	}
	a.turnCeilingTripped = false
	a.mu.Unlock()
}

// preflightCostCeiling returns a non-nil costCeilingError when a
// previous turn tripped the ceiling and the operator hasn't reset
// it. Called at the very top of Run, before any tracker writes or
// model calls — the refusal is structural, not driven by a fresh
// attempt that might also fail.
//
// Leads with the refusal rather than repeating the trip verbatim —
// see preflightWatchdog for why (#1040). Same defect on this arm: the
// spend line is identical whether the ceiling just tripped or tripped
// an hour ago, so a log cannot tell one incident from N.
func (a *Agent) preflightCostCeiling() error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.costCeilingExceeded {
		return nil
	}
	return &costCeilingError{reason: refusalReason(a.costCeilingReason)}
}
