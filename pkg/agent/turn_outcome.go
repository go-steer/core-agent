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

// Unified turn-outcome state machine and reducer for agent guardrails (#1389).
//
// Five mechanisms can cut a turn in flight or halt a session:
//   - per-turn cost ceiling (in-turn tap, boundary, and preflight request check)
//   - per-session cost ceiling
//   - behavioral watchdog (turn-scoped and session-scoped Critical signals)
//   - approval-gate refusal storm (#1081)
//   - in-turn context-window backstop (#975)
//
// Previously each mechanism maintained its own latches on Agent and performed
// its own combination of streak counting, session-halt latching, watchdog
// signal scrubbing (#1086), metric halt-kind marking (#818), logging (#1131),
// durable row persistence (#643, #1258), and Interrupt() calls.
//
// guardrailState groups the per-turn and session-scoped guardrail state into a
// single struct embedded on Agent, and reduceTurnSignal / applyTurnSignal
// provide one state machine that every guardrail feeds into:
//   - One turnSignal input and turnOutcome verdict type.
//   - One reducer that decides continue, cut, escalate, or halt.
//   - One executor that applies watchdog scrubbing (#1086), halt-kind
//     labelling (#818), logging/event emission, and in-turn cancellation.

package agent

import (
	"fmt"
	"log"
	"sync"
	"sync/atomic"

	"github.com/go-steer/core-agent/v3/pkg/attach/wire"
	"github.com/go-steer/core-agent/v3/pkg/watchdog"
)

// guardrailState holds the runtime latches, streak counters, preflight books,
// durability queue, and wake-fence state shared by the agent's guardrails.
// Embedded on Agent so existing field accesses promote transparently while
// keeping the state machine's footprint defined in one place (#1389).
type guardrailState struct {
	// pendingRefusalStorm carries the suppressed-repeat count that cut
	// this turn (#1081); zero means no cut. Atomic: set by the in-turn
	// arm and drained in post-turn cleanup once the runner's session
	// handle is released (#565). Doubles as the once-per-turn guard.
	pendingRefusalStorm atomic.Int64

	// contextBudgetCut latches the in-turn context backstop (#975) for
	// the duration of one turn.
	contextBudgetCut bool

	// Cost-ceiling runtime state (#145, #643, #1049, #1299).
	turnStartCost              float64
	costCeilingExceeded        bool
	costCeilingReason          string
	turnStartCostSet           bool
	turnCeilingTripped         bool
	turnCeilingStreak          int
	turnPreflightReservedUSD   float64
	turnPreflightEstimates     []costPreflightEstimate
	turnPreflightBaseTurns     map[string]int
	turnPreflightSettled       map[string]int
	turnPreflightGen           uint64
	testPreflightBeforeReserve func()
	testPreflightBeforeTrip    func()
	preflightOwner             *preflightOwner
	unpricedWarned             map[string]bool

	// Durable guardrail and out-of-band event state (#643, #1258, #1439).
	pendingOutOfBandEvents []queuedOutOfBandEvent
	guardrailRestored      bool
	guardrailRestoreMu     sync.Mutex
	outOfBandDrainMu       sync.Mutex

	// guardrailHaltKind names the guardrail that cut the turn in flight
	// (a wire.TurnError* kind; empty = none), consumed in post-turn
	// cleanup to record cut_by (#1258) and label the invocation metric (#818).
	guardrailHaltKind string

	// Wake-fence state while a session halt stands (#1040).
	wakeFenced      bool
	fencedLogReason string

	// Watchdog runtime state (#159, #623, #1090).
	watchdogTripped       bool
	watchdogReason        string
	watchdogPending       []watchdog.Alert
	watchdogTurnCut       bool
	watchdogTurnCutStreak int
}

// turnAction is the decision the turn-outcome reducer reaches for a guardrail
// signal (#1389).
type turnAction string

const (
	// turnActionContinue means no new cut or halt occurs (already latched
	// for this turn/session, or a stale preflight generation).
	turnActionContinue turnAction = "continue"
	// turnActionCut stops the current turn without halting the session.
	turnActionCut turnAction = "cut"
	// turnActionEscalate promotes a consecutive run of turn-scoped cuts into
	// a durable session halt (#1049, #1090).
	turnActionEscalate turnAction = "escalate"
	// turnActionHalt halts the session directly on a session-scoped bound.
	turnActionHalt turnAction = "halt"
)

// turnSignalKind identifies which guardrail arm is feeding the reducer.
type turnSignalKind string

const (
	signalCostCeilingTurn      turnSignalKind = "cost_ceiling_turn"
	signalCostCeilingPreflight turnSignalKind = "cost_ceiling_preflight"
	signalCostCeilingSession   turnSignalKind = "cost_ceiling_session"
	signalWatchdogTurn         turnSignalKind = "watchdog_turn"
	signalWatchdogSession      turnSignalKind = "watchdog_session"
	signalRefusalStorm         turnSignalKind = "refusal_storm"
	signalContextBudget        turnSignalKind = "context_budget"
)

// turnSignal is the input a guardrail arm feeds into reduceTurnSignal.
type turnSignal struct {
	kind           turnSignalKind
	inTurn         bool
	gen            uint64
	turnCost       float64
	inputCost      float64
	ceilingUSD     float64
	sessionCost    float64
	watchdogSignal string
	rawReason      string
	repeats        int
	contextUsed    int
	contextSize    int
}

// turnOutcome is the verdict produced by reduceTurnSignal and executed by
// applyTurnSignal.
type turnOutcome struct {
	action        turnAction
	guardrail     string
	haltKind      string
	reason        string
	streak        int
	inTurn        bool
	emitTrip      bool
	scrubWatchdog bool
	degradedKind  string
	stale         bool
}

func (o turnOutcome) stoppedTurn() bool {
	return o.action != "" && o.action != turnActionContinue
}

func (o turnOutcome) haltedSession() bool {
	return o.action == turnActionEscalate || o.action == turnActionHalt
}

// reduceTurnSignal transitions the agent's guardrailState for sig and returns
// the resulting turnOutcome without performing external side effects (logging,
// event emission, watchdog Reset, or Interrupt).
func (a *Agent) reduceTurnSignal(sig turnSignal) turnOutcome {
	if a == nil {
		return turnOutcome{action: turnActionContinue}
	}
	switch sig.kind {
	case signalCostCeilingTurn:
		return a.reduceCostCeilingTurn(sig)
	case signalCostCeilingPreflight:
		return a.reduceCostCeilingPreflight(sig)
	case signalCostCeilingSession:
		return a.reduceCostCeilingSession(sig)
	case signalWatchdogTurn:
		return a.reduceWatchdogTurn(sig)
	case signalWatchdogSession:
		return a.reduceWatchdogSession(sig)
	case signalRefusalStorm:
		return a.reduceRefusalStorm(sig)
	case signalContextBudget:
		return a.reduceContextBudget(sig)
	default:
		return turnOutcome{action: turnActionContinue}
	}
}

func (a *Agent) reduceCostCeilingTurn(sig turnSignal) turnOutcome {
	streak, tripped, _ := a.recordTurnCeilingTrip(0)
	if !tripped {
		return turnOutcome{action: turnActionContinue}
	}
	if streak >= maxConsecutiveTurnCeilingTrips {
		reason := fmt.Sprintf(
			"per-turn cost ceiling halted the session: %d turns in a row each hit the $%.4f per-turn ceiling (the last cost $%.4f). Every one was inside its own bound, so no per-session ceiling caught the pattern. Agent will refuse new turns until the operator resets it (/guardrail reset, or POST /sessions/{id}/guardrails/reset).",
			streak, sig.ceilingUSD, sig.turnCost,
		)
		a.mu.Lock()
		a.costCeilingExceeded = true
		a.costCeilingReason = reason
		a.mu.Unlock()
		return turnOutcome{
			action:    turnActionEscalate,
			guardrail: wire.GuardrailCostCeiling,
			haltKind:  wire.TurnErrorCostCeiling,
			reason:    reason,
			streak:    streak,
			inTurn:    sig.inTurn,
			emitTrip:  true,
		}
	}
	reason := fmt.Sprintf(
		"per-turn cost ceiling exceeded: this turn cost $%.4f, ceiling is $%.4f. "+
			"The turn was stopped with its work unfinished; the session is NOT halted, "+
			"and a further turn would start from a fresh per-turn budget. "+turnCutNoNextTurn+
			" %d in a row now — at %d the session halts and needs an operator reset.",
		sig.turnCost, sig.ceilingUSD, streak, maxConsecutiveTurnCeilingTrips,
	)
	return turnOutcome{
		action:    turnActionCut,
		guardrail: wire.GuardrailCostCeiling,
		haltKind:  wire.TurnErrorCostCeiling,
		reason:    reason,
		streak:    streak,
		inTurn:    sig.inTurn,
		emitTrip:  true,
	}
}

func (a *Agent) reduceCostCeilingPreflight(sig turnSignal) turnOutcome {
	streak, tripped, stale := a.recordTurnCeilingTrip(sig.gen)
	if stale {
		return turnOutcome{action: turnActionContinue, stale: true}
	}
	if !tripped {
		return turnOutcome{action: turnActionContinue}
	}
	reason := sig.rawReason
	action := turnActionCut
	if streak >= maxConsecutiveTurnCeilingTrips {
		action = turnActionEscalate
		reason = fmt.Sprintf(
			"per-turn cost ceiling halted the session: %d turns in a row each hit the $%.4f per-turn ceiling (the last request was refused after $%.4f was spent with an estimated $%.4f input cost; reason: %s). Every one was inside its own bound, so no per-session ceiling caught the pattern. Agent will refuse new turns until the operator resets it (/guardrail reset, or POST /sessions/{id}/guardrails/reset).",
			streak, sig.ceilingUSD, sig.turnCost, sig.inputCost, sig.rawReason,
		)
		a.mu.Lock()
		a.costCeilingExceeded = true
		a.costCeilingReason = reason
		a.mu.Unlock()
	}
	return turnOutcome{
		action:    action,
		guardrail: wire.GuardrailCostCeiling,
		haltKind:  wire.TurnErrorCostCeiling,
		reason:    reason,
		streak:    streak,
		inTurn:    sig.inTurn,
		emitTrip:  true,
	}
}

func (a *Agent) reduceCostCeilingSession(sig turnSignal) turnOutcome {
	reason := fmt.Sprintf(
		"per-session cost ceiling exceeded: session has cost $%.4f, ceiling is $%.4f. Agent will refuse new turns until the operator resets it WITH additional budget (/guardrail reset +N, or POST /sessions/{id}/guardrails/reset with additional_budget_usd) — a bare reset would re-trip on the next turn.",
		sig.sessionCost, sig.ceilingUSD,
	)
	a.mu.Lock()
	if a.costCeilingExceeded {
		a.mu.Unlock()
		return turnOutcome{action: turnActionContinue}
	}
	a.costCeilingExceeded = true
	a.costCeilingReason = reason
	a.mu.Unlock()
	return turnOutcome{
		action:    turnActionHalt,
		guardrail: wire.GuardrailCostCeiling,
		haltKind:  wire.TurnErrorCostCeiling,
		reason:    reason,
		inTurn:    sig.inTurn,
		emitTrip:  true,
	}
}

func (a *Agent) reduceWatchdogTurn(sig turnSignal) turnOutcome {
	a.mu.Lock()
	if a.watchdogTripped || a.watchdogTurnCut {
		a.mu.Unlock()
		return turnOutcome{action: turnActionContinue}
	}
	a.watchdogTurnCut = true
	a.watchdogTurnCutStreak++
	streak := a.watchdogTurnCutStreak
	if streak >= maxConsecutiveWatchdogTurnCuts {
		reason := fmt.Sprintf(
			"watchdog halted the agent (%s): %d turns in a row were cut for this signal. The last one: %s Ending the turn stopped each of them and the next turn started the same behavior again, which is the pattern a turn-scoped cut cannot answer on its own. Agent will refuse new turns until the operator resets it (/guardrail reset, or POST /sessions/{id}/guardrails/reset).",
			sig.watchdogSignal, streak, sig.rawReason,
		)
		a.watchdogTripped = true
		a.watchdogReason = reason
		a.mu.Unlock()
		return turnOutcome{
			action:    turnActionEscalate,
			guardrail: wire.GuardrailWatchdog,
			haltKind:  wire.TurnErrorWatchdog,
			reason:    reason,
			streak:    streak,
			inTurn:    sig.inTurn,
			emitTrip:  true,
		}
	}
	a.mu.Unlock()
	reason := fmt.Sprintf(
		"watchdog cut the turn (%s): %s "+
			"The turn was stopped with its work unfinished; the session is NOT halted, "+
			"and a further turn would start clean. "+turnCutNoNextTurn+
			" %d in a row now — at %d the session halts and needs an operator reset.",
		sig.watchdogSignal, sig.rawReason, streak, maxConsecutiveWatchdogTurnCuts,
	)
	// Scrub the signals' accumulated evidence on a turn-scoped cut (#1086):
	// the turn it was about is over, and signals that latch one alert per
	// streak (NoOpStreakSignal) must be able to alert again on the next
	// turn so the consecutive-turn escalation can fire.
	return turnOutcome{
		action:        turnActionCut,
		guardrail:     wire.GuardrailWatchdog,
		haltKind:      wire.TurnErrorWatchdog,
		reason:        reason,
		streak:        streak,
		inTurn:        sig.inTurn,
		emitTrip:      true,
		scrubWatchdog: true,
	}
}

func (a *Agent) reduceWatchdogSession(sig turnSignal) turnOutcome {
	reason := fmt.Sprintf(
		"watchdog halted the agent (%s): %s Agent will refuse new turns until the operator resets it (/guardrail reset, or POST /sessions/{id}/guardrails/reset).",
		sig.watchdogSignal, sig.rawReason,
	)
	a.mu.Lock()
	if a.watchdogTripped || a.watchdogTurnCut {
		a.mu.Unlock()
		return turnOutcome{action: turnActionContinue}
	}
	a.watchdogTripped = true
	a.watchdogReason = reason
	a.mu.Unlock()
	return turnOutcome{
		action:    turnActionHalt,
		guardrail: wire.GuardrailWatchdog,
		haltKind:  wire.TurnErrorWatchdog,
		reason:    reason,
		inTurn:    sig.inTurn,
		emitTrip:  true,
	}
}

func (a *Agent) reduceRefusalStorm(sig turnSignal) turnOutcome {
	if !a.pendingRefusalStorm.CompareAndSwap(0, int64(sig.repeats)) {
		return turnOutcome{action: turnActionContinue}
	}
	reason := fmt.Sprintf(
		"the approval gate refused %d tool calls in this turn and the model kept "+
			"re-issuing them. The turn was stopped with its work unfinished; nothing "+
			"is tripped, no operator reset is needed, and a further turn would start "+
			"with an empty refusal map. "+turnCutNoNextTurn,
		sig.repeats,
	)
	// A refusal-storm cut disposes of the repeated tool-call run for this
	// turn, so scrub the watchdog's accumulated evidence (#1086) to prevent
	// the next turn's first identical call from halting the session.
	return turnOutcome{
		action:        turnActionCut,
		guardrail:     wire.TurnErrorRefusalStorm,
		haltKind:      wire.TurnErrorRefusalStorm,
		reason:        reason,
		inTurn:        sig.inTurn,
		scrubWatchdog: true,
	}
}

func (a *Agent) reduceContextBudget(sig turnSignal) turnOutcome {
	a.mu.Lock()
	if a.contextBudgetCut {
		a.mu.Unlock()
		return turnOutcome{action: turnActionContinue}
	}
	a.contextBudgetCut = true
	a.compactionPending = true
	var pending int
	if a.tracker != nil {
		pending = a.tracker.PendingContextBytes()
	}
	a.mu.Unlock()
	reason := fmt.Sprintf(
		"a tool result took the estimated context to %d of %d tokens (%.0f%%, including %d unmeasured bytes); "+
			"cut the turn before building a request the provider would reject, and compaction runs first on any further turn.",
		sig.contextUsed, sig.contextSize, 100*float64(sig.contextUsed)/float64(sig.contextSize), pending,
	)
	return turnOutcome{
		action:       turnActionCut,
		guardrail:    wire.ContextReductionTurnCut,
		reason:       reason,
		inTurn:       sig.inTurn,
		degradedKind: wire.ContextReductionTurnCut,
	}
}

// applyTurnSignal runs the turn-outcome reducer for sig and executes its side
// effects in one place: watchdog signal scrubbing (#1086), logging and durable
// event emission (#891, #1131, #1258), metric halt-kind marking (#818), and
// in-turn context cancellation.
func (a *Agent) applyTurnSignal(sig turnSignal) turnOutcome {
	outcome := a.reduceTurnSignal(sig)
	if !outcome.stoppedTurn() {
		return outcome
	}
	if outcome.scrubWatchdog {
		a.resetWatchdogSignals()
	}
	switch {
	case outcome.emitTrip:
		a.emitGuardrailTrip(outcome.guardrail, outcome.reason, outcome.inTurn, outcome.haltedSession())
	case outcome.degradedKind != "":
		log.Printf("agent:%s %s %s", a.logSessionSuffix(), outcome.reason, turnCutNoNextTurn)
		a.recordContextReductionDegraded(outcome.degradedKind, outcome.reason)
	default:
		a.logGuardrailCut(outcome.guardrail, outcome.reason)
	}
	if outcome.inTurn {
		if outcome.haltKind != "" {
			a.markGuardrailHalt(outcome.haltKind)
		}
		a.Interrupt()
	}
	return outcome
}
