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

// Ending a turn the operator has already answered (#1081).
//
// Three changes have now been aimed at one loop. #1068 told the model a
// refusal was final. #1074 stopped the gate from asking again. Run 11 of
// dev/uat/approval-gate/ measured what the second one bought and the
// answer was five fewer prompts and zero fewer calls: runs 10 and 11
// printed the byte-identical watchdog critical, `repeated-tool-call:
// agent has called alert with identical args 5 times in a row`.
//
// What that run also shows — and what the issue was filed believing was
// missing — is that the turn DID end. enforceWatchdogInTurn (#705) cut
// it 580 microseconds after the critical, and cut leg 3's second loop
// 3.4 milliseconds after its own. Turn termination was never the gap.
//
// The gap is that the only thing reaching for the lever was a
// SESSION-scoped guardrail, counting to five on evidence it considers
// weak. Nine milliseconds after leg 3's cut the daemon logged `turn
// refused (guardrail still tripped, not reset)`, and a minute later
// auto-continue stood down "until it is reset". On an unattended daemon
// that is the #1068 wedge intact: one human "no" and the agent is done
// working until a human comes back. Meanwhile the gate had known since
// the second call that this exact request was already refused.
//
// So the gate ends the turn, on its own evidence, and ends only the
// turn. Four things follow from that sentence and each is a decision:
//
// TURN SCOPE, NOT SESSION SCOPE. Nothing is tripped and nothing needs
// resetting. The next turn starts with the refusal in history and an
// empty refusal map, which is the state pkg/permissions deliberately
// scoped that map for. A model that will not take no for an answer has
// had a bad turn; it does not follow that the session is unsafe to run,
// and treating it as though it were is what makes a gated daemon
// unoperable in the first place.
//
// A LOW THRESHOLD, BECAUSE THE EVIDENCE IS STRONG. repeated-tool-call
// needs five identical calls because a repeated tool name is almost no
// evidence — it cannot tell a loop from a sweep. A suppressed repeat is
// not an inference: the operator refused this exact request, the model
// was told so in a tool result, and it asked again anyway. Three of
// those is not a pattern that needs corroborating. (Run 11: leg 2 was
// one denial plus three suppressed, leg 3 one expiry plus four.)
//
// COUNTED ACROSS REQUESTS, NOT PER REQUEST. See
// Gate.TurnRefusalRepeats. A model alternating between two already-
// refused requests is in the same state as one repeating a single
// request, and both are unambiguous.
//
// NO GUARDRAIL-TRIP EVENT. attach.GuardrailTrip's vocabulary is shared
// with the reset endpoint and the durable trip row, so announcing a cut
// that needs no reset through it would tell a client there is something
// to reset. The turn's terminal frame is the ordinary `canceled` that
// Interrupt produces; the reason is durable in the eventlog as an audit
// row, and the model's own history already carries the refusals that
// caused it. The metric gets attach.TurnErrorRefusalStorm so a backend
// can still tell this stop from an operator pressing stop.
//
// It does, since #1131, write a log line. That is not a reversal of the
// paragraph above: a log line has no reset vocabulary to misuse, and
// this arm is the one that most needs it. The other two at least reach
// an attach client; this one reaches nobody live, so before #1131 an
// operator watching a gated run saw the turn die with a bare
// `context canceled` and no record anywhere but the eventlog.

package agent

import (
	"context"
	"fmt"

	"google.golang.org/adk/session"

	"github.com/go-steer/core-agent/v2/pkg/attach"
)

// refusalStormThreshold is how many calls the gate may refuse without
// asking anybody before the turn is over. See the file docstring for why
// three and not five.
const refusalStormThreshold = 3

// refusalStormAuthor is the audit row's author. Namespaced like
// "attach/interrupt" so a transcript reader can tell at a glance which
// subsystem wrote the row.
const refusalStormAuthor = "gate/refusal-storm"

// hasToolResult reports whether this event carries a tool result, which
// is the only thing that can have moved the gate's refusal count. Cheap
// enough to run on every event in a streaming turn, which is the point:
// the alternative is taking the gate's mutex on every text delta.
//
// A FunctionResponse and not a FunctionCall, because a suppressed
// request produces both and the response is the later of the two — the
// count is already incremented by the time it arrives, so reacting to
// the call would just mean reading a stale number and waiting for the
// next event to notice.
func hasToolResult(ev *session.Event) bool {
	if ev == nil || ev.Content == nil {
		return false
	}
	for _, p := range ev.Content.Parts {
		if p != nil && p.FunctionResponse != nil {
			return true
		}
	}
	return false
}

// enforceRefusalStormInTurn ends the turn when the gate has suppressed
// refusalStormThreshold calls in it. Runs from Run's event tap beside
// the watchdog and cost-ceiling arms, and on the same trigger: a fresh
// tool observation, because the count cannot move without one.
//
// Not gated on any mode flag. The other two in-turn arms are opt-in
// because they act on inference — a spend rate, a repetition — and an
// operator may reasonably prefer to watch rather than halt. This one
// acts on a decision the operator already made: they refused the call.
// A configuration in which the agent keeps re-issuing it anyway is not a
// preference anybody would express, so there is nothing to switch off.
//
// Idempotent: the first cut stores the count and every later call
// returns immediately, which matters because Interrupt cancels runCtx
// and the tap keeps draining events until the stream ends.
func (a *Agent) enforceRefusalStormInTurn(ctx context.Context) {
	if a == nil || a.gate == nil {
		return
	}
	if a.pendingRefusalStorm.Load() > 0 {
		return
	}
	repeats := a.gate.TurnRefusalRepeats(ctx)
	if repeats < refusalStormThreshold {
		return
	}
	// CompareAndSwap rather than Store: two observations from one event
	// (a call and its result) both reach this line, and the loser must
	// not overwrite the winner's count or re-Interrupt a turn that is
	// already unwinding.
	if !a.pendingRefusalStorm.CompareAndSwap(0, int64(repeats)) {
		return
	}
	// Say so in the log before cutting (#1131). This arm emits no
	// operator event by design (see the file docstring), so without this
	// line the metric label below is the only record outside the
	// eventlog and the operator's whole output is the `context canceled`
	// the Interrupt produces. The reason text deliberately states that
	// nothing needs resetting — the thing the event was withheld to
	// avoid implying.
	logGuardrailCut(attach.TurnErrorRefusalStorm, fmt.Sprintf(
		"the approval gate refused %d tool calls in this turn and the model kept "+
			"re-issuing them. The turn was stopped; nothing is tripped, no operator "+
			"reset is needed, and the next turn starts with an empty refusal map.",
		repeats))
	// Label the metric point before cutting, for the reason
	// guardrail_halt.go exists: the turn error is a bare
	// context.Canceled and nothing downstream could otherwise tell this
	// stop from an operator interrupt. This is NOT a guardrail trip —
	// markGuardrailHalt only relabels, it does not trip anything.
	a.markGuardrailHalt(attach.TurnErrorRefusalStorm)
	// Take the repetition off the watchdog's books on the way out
	// (#1086). The calls being cut here are the watchdog's evidence too —
	// it is watching the same stream — and its run length is not
	// turn-scoped, so without this the next turn's first identical call
	// is counted as the fifth in a row and halts the SESSION. That is
	// the defect #1081 set out to fix, relocated one turn later, and the
	// approval-gate drill found it exactly there: leg 2 stopped halting
	// and leg 3 started.
	//
	// Safe because this arm is strictly tighter than the one it is
	// clearing. Three suppressed repeats cut the turn before
	// repeated-tool-call's five can be reached, so a model that keeps
	// looping through the gate is stopped sooner every time, not later.
	// A loop on a tool the gate never sees never gets here, and nothing
	// is reset. The tripped flag is untouched: this arm never sets it,
	// and a watchdog that genuinely halted earlier in the turn stays
	// halted and still needs the operator's reset.
	a.resetWatchdogSignals()
	a.Interrupt()
}

// clearRefusalStorm resets the per-turn marker at turn start, matching
// clearGuardrailHalt's belt-and-braces reasoning: the drain below clears
// it on every ordinary turn, but a turn that never reaches its cleanup
// would otherwise leave it armed and silently disarm the next turn's
// enforcement.
func (a *Agent) clearRefusalStorm() {
	if a == nil {
		return
	}
	a.pendingRefusalStorm.Store(0)
}

// drainRefusalStormAudit appends the audit row for a turn this cut, then
// clears the marker. Runs from the post-turn cleanup next to
// drainInterruptAudit, after the runner's event stream has drained and
// its session handle is released, because an out-of-band write into a
// live session bumps last_update_time and trips ADK's optimistic
// concurrency check — the #565 failure, which surfaces as an opaque
// "stale session error" attributed to whatever the operator did next.
//
// Best-effort and deliberately silent on failure, mirroring the
// interrupt audit: the cut has already happened and the model's history
// already carries the refusals that caused it, so a missing row costs an
// operator an explanation rather than costing anyone the protection.
//
// The row carries no content on purpose. A text part authored here would
// enter the model's history as a message nobody sent, and the next turn
// would open with the runtime narrating at it; what the model needs to
// know is in the tool results it already has.
func (a *Agent) drainRefusalStormAudit() {
	if a == nil {
		return
	}
	repeats := a.pendingRefusalStorm.Swap(0)
	if repeats == 0 || a.eventLog == nil {
		return
	}
	getResp, err := a.eventLog.Service.Get(context.Background(), &session.GetRequest{
		AppName:   a.appName,
		UserID:    a.userID,
		SessionID: a.sessionID,
	})
	if err != nil {
		return
	}
	ev := session.NewEvent("gate-refusal-storm")
	ev.Author = refusalStormAuthor
	ev.CustomMetadata = map[string]any{
		"source":  "gate",
		"repeats": repeats,
		"reason":  "turn ended: the agent re-issued a call the operator had already refused in this turn",
	}
	_ = a.eventLog.Service.AppendEvent(context.Background(), getResp.Session, ev)
}
