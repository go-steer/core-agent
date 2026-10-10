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

// Per-turn failures as eventlog rows (#1258).
//
// A guardrail trip that did not halt the session, and the turn error a
// turn ends with, used to exist in exactly two places: a daemon log
// line, and a typed SSE frame delivered to whoever was attached at that
// instant. Neither is a transcript. Replaying the session from seq 0
// showed a turn that simply stopped, and box A2 (#1042) — "no failure
// class has more entries in the daemon log than in the transcripts" —
// failed in its plainest form: 7 cuts and 7 turn errors in the log, 0
// and 0 on replay.
//
// The fix is the one guardrail_events.go and context_reduction_events.go
// already use: an eventlog append. The broadcaster tails the eventlog,
// so the row reaches every attached client on the back-compat `agent`
// frame, and being durable it is still there for a client that attaches
// late and for a post-mortem. See docs/durable-turn-failures-design.md.
//
// Three properties every row here keeps, because each is something a
// reader elsewhere in the tree keys on:
//
//   - No Content. ADK's contents processor skips an event with no
//     content, so the row never enters the model's history, and every
//     conversation reader in pkg/agent (compaction, tail repair, the
//     approver's context, FinalText) skips it for the same reason.
//   - No LLMResponse error fields. The turn error's code lives in
//     CustomMetadata, never in Event.ErrorCode: auto-continue's tail
//     classifier reads ErrorCode BEFORE it skips content-less rows, and
//     a row carrying one would be read as an in-band model error.
//   - Its own author. FoldGuardrailEvents matches on the author, so a
//     per-turn trip row can never restore as a halt — not in this
//     binary, and not in an older one rolled back over the same
//     eventlog, which a shared author plus a "scope" key would not
//     have guaranteed.

package attach

import (
	"context"

	"google.golang.org/adk/v2/session"
)

// Event names and authors for the per-turn failure rows.
const (
	// GuardrailTurnTripEventName is the event name of the row recording
	// a guardrail trip that did NOT halt the session: a per-turn cost
	// ceiling trip (#1049) or a turn-scoped watchdog cut (#1090). A trip
	// that halts the session is recorded by the halt row
	// (GuardrailTripEventName) instead, never by both.
	GuardrailTurnTripEventName = "guardrail-turn-trip"
	// GuardrailTurnTripEventAuthor authors the per-turn trip row.
	// Deliberately not GuardrailTripEventAuthor: the halt fold keys on
	// that author, and a per-turn trip must not restore as a halt.
	GuardrailTurnTripEventAuthor = "agent/guardrail-turn-trip"

	// TurnErrorEventName is the event name of the durable turn-error row.
	TurnErrorEventName = "turn-error"
	// TurnErrorEventAuthor authors the turn-error row.
	TurnErrorEventAuthor = "agent/turn-error"
)

// Metadata keys on the per-turn failure rows. The guardrail row reuses
// the halt row's `source` / `guardrail` / `reason` keys so a reader that
// already renders a halt renders this the same way.
const (
	turnTripMetaHaltedTurn = "halted_turn"

	turnErrMetaKind      = "kind"
	turnErrMetaCode      = "code"
	turnErrMetaMessage   = "message"
	turnErrMetaRetryable = "retryable"
	turnErrMetaHint      = "hint"
	turnErrMetaPromptID  = "prompt_id"
	turnErrMetaCutBy     = "cut_by"
)

// NewGuardrailTurnTripEvent builds the durable row recording a guardrail
// trip that left the session running. guardrail is GuardrailWatchdog or
// GuardrailCostCeiling; reason is the same operator-facing sentence the
// log line and the typed frame carry, verbatim. haltedTurn mirrors the
// typed frame's field: true when the trip cut the turn in flight (a
// `canceled` turn error follows), false when it fired at the boundary.
func NewGuardrailTurnTripEvent(guardrail, reason string, haltedTurn bool) *session.Event {
	ev := session.NewEvent(context.Background(), GuardrailTurnTripEventName)
	ev.Author = GuardrailTurnTripEventAuthor
	ev.CustomMetadata = map[string]any{
		guardrailMetaSource:    "agent",
		guardrailMetaGuardrail: guardrail,
		guardrailMetaReason:    reason,
		turnTripMetaHaltedTurn: haltedTurn,
	}
	return ev
}

// NewGuardrailHaltEvent is NewGuardrailTripEvent plus the halted_turn
// key the per-turn row carries (#1258). The fold ignores the key; a
// client rendering a replayed halt needs it, because a halt that cut a
// turn is followed by a `canceled` turn-error row that core-tui absorbs
// only under a trip that says it cut the turn — exactly as it does
// live, off the typed frame's field.
func NewGuardrailHaltEvent(guardrail, reason string, haltedTurn bool) *session.Event {
	ev := NewGuardrailTripEvent(guardrail, reason)
	ev.CustomMetadata[turnTripMetaHaltedTurn] = haltedTurn
	return ev
}

// GuardrailTurnTrip reads a per-turn trip row back out of an event
// stream. ok=false for any other event, so a consumer can run it over an
// undifferentiated tail. Matches on author AND name, like
// ContextReductionFailure, so a future row sharing the author cannot be
// misread as this one.
func GuardrailTurnTrip(ev *session.Event) (GuardrailTrip, bool) {
	if ev == nil || ev.CustomMetadata == nil ||
		ev.Author != GuardrailTurnTripEventAuthor || ev.InvocationID != GuardrailTurnTripEventName {
		return GuardrailTrip{}, false
	}
	return guardrailTripFromRow(ev), true
}

// GuardrailHaltRow reads the halt row (#643) as the GuardrailTrip a
// client renders. HaltedTurn is read from the row when it carries one
// (written since #1258) and is false for an older row, which never
// recorded it. ok=false for any other event.
func GuardrailHaltRow(ev *session.Event) (GuardrailTrip, bool) {
	if ev == nil || ev.CustomMetadata == nil ||
		ev.Author != GuardrailTripEventAuthor || ev.InvocationID != GuardrailTripEventName {
		return GuardrailTrip{}, false
	}
	return guardrailTripFromRow(ev), true
}

func guardrailTripFromRow(ev *session.Event) GuardrailTrip {
	gt := GuardrailTrip{EventID: ev.ID}
	gt.Guardrail, _ = ev.CustomMetadata[guardrailMetaGuardrail].(string)
	gt.Reason, _ = ev.CustomMetadata[guardrailMetaReason].(string)
	gt.HaltedTurn, _ = ev.CustomMetadata[turnTripMetaHaltedTurn].(bool)
	return gt
}

// NewTurnErrorEvent builds the durable row recording one turn error. te
// is the classified payload — the same value the typed `turn-error`
// frame carries, so the two cannot disagree. promptID correlates the row
// with the turn (empty when the turn never got one: a pre-turn refusal).
// cutBy names the guardrail whose cut produced the error (a
// TurnError* guardrail kind, e.g. TurnErrorCostCeiling or
// TurnErrorRefusalStorm), empty when no guardrail was involved — the
// error's own kind is then `canceled` and only this key says why.
//
// Optional keys (code, hint, prompt_id, cut_by) are omitted when empty,
// for the reason NewContextReductionFailedEvent gives: a key that is
// always present but meaningless half the time is a key consumers read
// wrong.
func NewTurnErrorEvent(te TurnError, promptID, cutBy string) *session.Event {
	ev := session.NewEvent(context.Background(), TurnErrorEventName)
	ev.Author = TurnErrorEventAuthor
	md := map[string]any{
		guardrailMetaSource:  "agent",
		turnErrMetaKind:      te.Kind,
		turnErrMetaMessage:   te.Message,
		turnErrMetaRetryable: te.Retryable,
	}
	for k, v := range map[string]string{
		turnErrMetaCode:     te.Code,
		turnErrMetaHint:     te.Hint,
		turnErrMetaPromptID: promptID,
		turnErrMetaCutBy:    cutBy,
	} {
		if v != "" {
			md[k] = v
		}
	}
	ev.CustomMetadata = md
	return ev
}

// TurnErrorRow reads a turn-error row back as the TurnError payload the
// typed frame would have carried, with EventID set to the row's id, plus
// the guardrail that cut the turn (empty if none). ok=false for any
// other event.
func TurnErrorRow(ev *session.Event) (te TurnError, cutBy string, ok bool) {
	if ev == nil || ev.CustomMetadata == nil ||
		ev.Author != TurnErrorEventAuthor || ev.InvocationID != TurnErrorEventName {
		return TurnError{}, "", false
	}
	md := ev.CustomMetadata
	te.EventID = ev.ID
	te.Kind, _ = md[turnErrMetaKind].(string)
	te.Code, _ = md[turnErrMetaCode].(string)
	te.Message, _ = md[turnErrMetaMessage].(string)
	te.Retryable, _ = md[turnErrMetaRetryable].(bool)
	te.Hint, _ = md[turnErrMetaHint].(string)
	cutBy, _ = md[turnErrMetaCutBy].(string)
	return te, cutBy, true
}
