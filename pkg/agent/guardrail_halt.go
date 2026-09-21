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

import (
	"log"

	"github.com/go-steer/core-agent/v2/pkg/attach"
)

// emitGuardrailTrip reports a trip: to the log always, and to the attach
// stream as the non-terminal `guardrail-trip` event (#891). One
// construction site for both guardrails, so the two cannot drift — they
// were already meant to be mirror images and the payload is now shared
// rather than parallel.
//
// haltedTurn tells the consumer which frame to expect next: true means
// the caller is about to Interrupt and a `canceled` turn-error follows;
// false means the turn completed (or never started) and `turn-complete`
// follows, or nothing does.
//
// The log half is #1131, and it is here rather than at the two cut sites
// for the reason the rest of this function is: the reason string is
// built inside maybeTripWatchdog and maybeEnforceCostCeiling and does
// not survive out to enforceWatchdogInTurn, and a third guardrail added
// later gets the log for free instead of having to remember it. Logging
// both shapes and not only the cut is deliberate — a session-scoped halt
// at the turn boundary is equally invisible headless, and an operator
// reading "the agent stopped taking turns" wants the same sentence.
func (a *Agent) emitGuardrailTrip(guardrail, reason string, haltedTurn bool) {
	if haltedTurn {
		a.logGuardrailCut(guardrail, reason)
	} else {
		log.Printf("agent: %s guardrail tripped%s: %s", guardrail, a.logSessionSuffix(), reason)
	}
	a.emit(attach.EventGuardrailTrip, attach.GuardrailTrip{
		Guardrail:  guardrail,
		Reason:     reason,
		HaltedTurn: haltedTurn,
	})
}

// turnCutNoNextTurn is the clause every turn-scoped cut ends its
// aftermath sentence with (#1140).
//
// A turn-scoped trip leaves the session available, and both cut
// messages used to report that as "the next turn starts clean" — a
// promise, in the present tense, about a turn that may never exist.
// Nothing inside pkg/agent starts a turn. Under a driver the claim is
// true and useful; in a `-p` one-shot there is no next turn at all, the
// process exits 1, and the operator has just been told the opposite of
// what happened by the only line that explained it. That reader is the
// likely one: a one-shot is the run with no client attached, so this
// log line IS the report.
//
// The fix is wording, not plumbing. Whether a caller will start another
// turn is not knowable here and is deliberately not knowable — a
// per-turn trip does not raise the session fence (#1049), so there is
// no signal to consult and inventing one would couple the guardrail to
// its driver. A sentence true in both worlds costs nothing, and naming
// the two worlds is what turns "the session is not halted" from
// reassurance into something the reader can act on.
//
// Shared rather than duplicated so the two guardrails cannot drift on
// the half that is identical, in the file that already exists because
// they were meant to be mirror images.
const turnCutNoNextTurn = "Nothing starts that turn on its own, though: " +
	"under a driver (TUI, attach, auto-continue) the run goes on, and a " +
	"one-shot (-p) run ends here with exit 1."

// logGuardrailCut writes the one line an unattended operator gets when a
// guardrail cuts the turn in flight (#1131).
//
// Agent.emit is a no-op until an emitter is registered and the only
// non-test registration site in the tree is the attach adapter, so a run
// without --attach-listen had no channel for this at all. What reached
// the operator instead was the downstream consequence — the model
// client's `context canceled`, which reads as a provider failure — and
// that is the shape MOST likely to need the line, because an unattended
// run defaults to watchdog=enforce and a session cost ceiling. Same belt
// as cutTurnForContextBudget, which has logged its own cut alongside the
// event since #975.
//
// The message names the guardrail and disclaims the cancellation ahead
// of time, because the cancellation is what the operator sees first in a
// stream and the log line is what explains it. Wiring the whole typed
// event stream to stderr is the broader fix (#1132); this line stands on
// its own because the refusal-storm arm deliberately emits no event at
// all (see refusal_storm.go) and so is invisible even with one.
//
// It names the session too (#1136), which the one-shot run this was
// written for does not need and a daemon cannot do without: several
// sessions interleave their lines into one log, and the halt that most
// needs an operator — a per-session cost ceiling — is reported by asking
// them to reset it with additional budget, which is a request that takes
// the id.
func (a *Agent) logGuardrailCut(guardrail, reason string) {
	log.Printf("agent: %s guardrail cut the turn in flight%s — the cancellation "+
		"error that follows is this cut, not a provider failure: %s",
		guardrail, a.logSessionSuffix(), reason)
}

// logSessionSuffix names the session a log line is about.
//
// Bracketed, where the daemon's own lines write a bare `session %s:`
// prefix (pkg/compose/auto_continue.go, pkg/runner/wakeloop.go). Those
// lead with it and can punctuate it with a colon; these two splice it
// into the middle of a sentence, where an unbracketed `session s-7`
// would read as prose. A grep for `session <id>` still finds both.
//
// Splicing costs one anchor and saves the other. `agent: <guardrail>
// guardrail cut the turn in flight` survives byte-for-byte, because the
// suffix lands after "in flight"; `<guardrail> guardrail tripped:` does
// NOT, because the suffix lands between the word and the colon. That is
// a real break and it is taken knowingly — #1131 is days old and in no
// tag, and a sweep of the tree found nothing matching on either string.
// Drop the colon from any grep that has one.
//
// It does not special-case the single-session run, even though its id is
// the unglamorous `default` that New fills in when nothing passes
// WithSession. That is a real id and not a placeholder: it is what the
// reset endpoint takes, and it is already what the --no-repl banner
// prints as `session %s`, so a log line saying something else about the
// same run would be the inconsistency, not the noise.
//
// The empty case returns nothing rather than `[session ]` and is only
// reachable through an explicit WithSession(_, ""), which no caller in
// the tree does — a guard, not a mode.
//
// No lock on a.sessionID: no production path reassigns it — New sets it
// from the options and nothing else writes it — and SessionID() has read
// it unlocked since it was added. (One test does rebind it, on an idle
// agent with no turn in flight. A future session-rebind path would have
// to revisit both readers, not just this one.)
func (a *Agent) logSessionSuffix() string {
	if a == nil || a.sessionID == "" {
		return ""
	}
	return " [session " + a.sessionID + "]"
}

// markGuardrailHalt records that a guardrail is cutting the turn that is
// currently in flight, so Run's cleanup can label the turn's metric point
// with the guardrail instead of the bare cancellation it sees. Called
// immediately before Interrupt by the in-turn enforcement arms; kind is
// the guardrail's turn-error kind (attach.TurnErrorCostCeiling,
// attach.TurnErrorWatchdog or attach.TurnErrorRefusalStorm), which is
// the vocabulary `error.type` on gen_ai.agent.invocation.duration
// already speaks.
//
// Marking is not tripping. The name says "halt" because that is what
// the first two callers were doing anyway; the refusal-storm arm (#1081)
// calls it while tripping nothing at all, and is entitled to, because
// all this function does is decide a metric label.
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
