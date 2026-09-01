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

// One terminal frame for a guardrail halt (#818 part 1).
//
// A guardrail that trips DURING a turn does two things: it emits its own
// `turn-error` carrying the operator-facing reason (`cost_ceiling` /
// `watchdog`), and it calls Interrupt to cut the turn short. The second
// half then unwinds through the ordinary cancelled-context path, and
// Run's cleanup — which has no idea why runCtx died — classified the
// cancellation and emitted a SECOND terminal frame, `canceled`. Two
// terminal frames for one turn: a protocol violation (spec v1.x: exactly
// one turn-complete OR turn-error per turn), and on screen a redundant
// "⚠ canceled · turn canceled" block stacked under the warning that
// actually says what happened. Any consumer counting turn outcomes
// double-counts, and one that finalizes on the first terminal frame sees
// a frame arrive after the turn it already closed.
//
// The fix is a per-turn marker rather than an inspection of the error:
// `context.Canceled` looks identical whether the operator hit /interrupt,
// the daemon is shutting down, or a guardrail pulled the plug, so only
// the site that pulled it can say. The two in-turn enforcement arms
// (enforceCostCeilingInTurn, enforceWatchdogInTurn) set it immediately
// before their Interrupt call.
//
// Deliberately NOT set at the emit sites (maybeEnforceCostCeiling,
// maybeTripWatchdog). Both also run as post-turn hooks from Run's own
// cleanup, which is the other shape #818 describes: a guardrail that
// trips at the turn BOUNDARY emits its turn-error and is then followed by
// this turn's `turn-complete`, because the turn did finish and did
// produce an answer. That pairing is odd but not wrong in the same way —
// nothing is duplicated and nothing is contentless — and suppressing
// either half would be worse than the pairing (drop the turn-complete and
// a consumer waits forever for a turn that ended; drop the guardrail
// frame and the reason the agent is about to start refusing turns never
// reaches the operator). Modelling a guardrail trip as what it actually
// is — a non-terminal notification rather than a turn outcome — is the
// v3.0 protocol fix (`guardrail-trip`, tracked separately); marking only
// the in-flight sites keeps this change to the half that is unambiguously
// a defect.
//
// Suppression is also conditional on the classified kind being
// `canceled`. If a marked turn ends in some other error — the model call
// failed on its way out, say — that error is real, unreported elsewhere,
// and gets its frame.
//
// The marker carries WHICH guardrail rather than a bare bool, because
// the turn's metric point needs the same answer: the turn error is a
// bare context.Canceled, so `gen_ai.agent.invocation.duration` would
// otherwise label a runaway halt `canceled` while the client that
// watched it happen was told `watchdog`. Same defect as #818's part 2,
// one turn earlier in the sequence.

package agent

import "github.com/go-steer/core-agent/v2/pkg/attach"

// markGuardrailHalt records that a guardrail is cutting the turn that is
// currently in flight, so Run's cleanup can recognise the cancellation it
// is about to classify as one the guardrail already reported. Called
// immediately before Interrupt by the in-turn enforcement arms; kind is
// the turn-error kind that guardrail emitted (attach.TurnErrorCostCeiling
// or attach.TurnErrorWatchdog).
func (a *Agent) markGuardrailHalt(kind string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.guardrailHaltKind = kind
	a.mu.Unlock()
}

// clearGuardrailHalt resets the marker at turn start. Belt-and-braces
// against a stale flag suppressing a later, legitimate `canceled`: the
// consume path below already clears it on every terminal frame, but a
// turn that never reaches its cleanup (a panic unwinding past it, an
// abandoned iterator) would otherwise leave it armed for the next one.
func (a *Agent) clearGuardrailHalt() {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.guardrailHaltKind = ""
	a.mu.Unlock()
}

// consumeGuardrailHalt returns the guardrail kind that halted this turn
// — meaning the terminal `turn-error` must be suppressed, because that
// guardrail already emitted the turn's one terminal frame, and the
// turn's metric point should carry this kind rather than `canceled`.
// Empty means the ordinary path: emit and classify as usual. Always
// clears the marker, whatever it answers — it describes one turn and
// must not outlive it.
func (a *Agent) consumeGuardrailHalt(turnErr error) string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	kind := a.guardrailHaltKind
	a.guardrailHaltKind = ""
	a.mu.Unlock()
	if kind == "" || turnErr == nil {
		return ""
	}
	if attach.ClassifyTurnError(turnErr).Kind != attach.TurnErrorCanceled {
		return ""
	}
	return kind
}
