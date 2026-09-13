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

// Labelling a turn a guardrail cut (#818 part 2, #891).
//
// A guardrail that trips DURING a turn calls Interrupt to cut it short,
// and that unwinds through the ordinary cancelled-context path. Run's
// cleanup has no idea why runCtx died: `context.Canceled` looks
// identical whether the operator hit /interrupt, the daemon is shutting
// down, or a guardrail pulled the plug. Only the site that pulled it can
// say, so the two in-turn enforcement arms (enforceCostCeilingInTurn,
// enforceWatchdogInTurn) leave a per-turn marker immediately before
// their Interrupt call and Run's cleanup consumes it.
//
// What the marker is FOR has narrowed. Originally (#818 part 1) it did
// two jobs: label the turn's metric point, and SUPPRESS the `canceled`
// turn-error, because the guardrail had already emitted a `turn-error`
// of its own and two terminal frames for one turn is a protocol
// violation (spec v1.x: exactly one turn-complete OR turn-error per
// turn). The second job is gone. #891 stopped modelling a trip as a turn
// outcome at all — it rides a non-terminal `guardrail-trip` event now —
// so the terminal slot is free and `canceled` is simply the accurate
// answer for a turn that was cut. Nothing to suppress.
//
// The labelling job remains, and is the reason the marker carries WHICH
// guardrail rather than a bare bool. The turn error is a bare
// context.Canceled, so `gen_ai.agent.invocation.duration` would
// otherwise label a runaway halt `canceled` while the client that
// watched it happen was told `watchdog` — #818's part 2 defect, one turn
// earlier in the sequence. The wire moving to a separate event does not
// help here: a metrics backend has no `guardrail-trip` series to
// correlate against, only this label.
//
// Still conditional on the classified kind being `canceled`. If a marked
// turn ends in some other error — the model call failed on its way out,
// say — that error is what happened to the turn, and labelling it with
// the guardrail would be a second mislabelling in place of the first.

package agent

import "github.com/go-steer/core-agent/v2/pkg/attach"

// emitGuardrailTrip puts a trip on the attach stream as the
// non-terminal `guardrail-trip` event (#891). One construction site for
// both guardrails, so the two cannot drift — they were already meant to
// be mirror images and the payload is now shared rather than parallel.
//
// haltedTurn tells the consumer which frame to expect next: true means
// the caller is about to Interrupt and a `canceled` turn-error follows;
// false means the turn completed (or never started) and `turn-complete`
// follows, or nothing does.
func (a *Agent) emitGuardrailTrip(guardrail, reason string, haltedTurn bool) {
	a.emit(attach.EventGuardrailTrip, attach.GuardrailTrip{
		Guardrail:  guardrail,
		Reason:     reason,
		HaltedTurn: haltedTurn,
	})
}

// markGuardrailHalt records that a guardrail is cutting the turn that is
// currently in flight, so Run's cleanup can label the turn's metric point
// with the guardrail instead of the bare cancellation it sees. Called
// immediately before Interrupt by the in-turn enforcement arms; kind is
// the guardrail's turn-error kind (attach.TurnErrorCostCeiling or
// attach.TurnErrorWatchdog), which is the vocabulary `error.type` on
// gen_ai.agent.invocation.duration already speaks.
func (a *Agent) markGuardrailHalt(kind string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.guardrailHaltKind = kind
	a.mu.Unlock()
}

// clearGuardrailHalt resets the marker at turn start. Belt-and-braces
// against a stale flag mislabelling a later, legitimate cancellation:
// the consume path below already clears it on every terminal frame, but
// a turn that never reaches its cleanup (a panic unwinding past it, an
// abandoned iterator) would otherwise leave it armed for the next one.
func (a *Agent) clearGuardrailHalt() {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.guardrailHaltKind = ""
	a.mu.Unlock()
}

// consumeGuardrailHalt returns the guardrail kind that halted this turn,
// meaning the turn's metric point should carry this kind rather than
// `canceled`. Empty means the ordinary path: classify as usual. Always
// clears the marker, whatever it answers — it describes one turn and
// must not outlive it.
//
// It no longer has any say over which frame goes on the wire: since #891
// the cut turn emits its `canceled` turn-error unconditionally, because
// the trip that caused it is reported separately and non-terminally.
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
