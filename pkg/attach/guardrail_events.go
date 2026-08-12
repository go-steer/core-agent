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

// Durable guardrail state (#643).
//
// A halt that a restart clears is not a halt. Before this, a tripped
// watchdog or cost ceiling lived only in agent memory: a crash, an OOM
// kill, or a pod roll re-initialized the process with the backstop
// disarmed — so the exact runaway-loop → crash → restart cycle the
// #623–#627 train was built to stop could repeat indefinitely, each
// restart handing the loop a fresh budget.
//
// The fix is to make the trip a fact in the eventlog rather than a
// field in a struct. Two row kinds are appended as state transitions
// happen, and folding them forward reconstructs the halt on the next
// process:
//
//	guardrail-trip   — a backstop halted the session, and why
//	guardrail-reset  — an operator cleared it, and what runway they added
//
// The reset row doubles as the audit trail #331 asked for, which is why
// there is one row kind and not a separate audit row: two rows meaning
// "the operator reset this" would be two things to keep in agreement.
//
// The fold is deliberately last-writer-wins per guardrail rather than a
// counter — a trip is a latch, not a tally — while added budget
// accumulates, because two resets that each hand over $5 have handed
// over $10.

package attach

import (
	"iter"

	"google.golang.org/adk/session"
)

// Event names and authors for the durable guardrail rows. The author
// prefix distinguishes them from model turns in an eventlog tail.
const (
	// GuardrailTripEventName is the event name of a durable trip row.
	GuardrailTripEventName = "guardrail-trip"
	// GuardrailTripEventAuthor authors a trip row. The agent writes it,
	// not an operator — a trip is something that happened TO the
	// session.
	GuardrailTripEventAuthor = "agent/guardrail-trip"

	// GuardrailResetEventName is the event name of a durable reset row.
	GuardrailResetEventName = "attach-guardrail-reset"
	// GuardrailResetEventAuthor authors a reset row.
	GuardrailResetEventAuthor = "attach/guardrail-reset"
)

// Metadata keys carried on the durable guardrail rows.
const (
	guardrailMetaSource    = "source"
	guardrailMetaGuardrail = "guardrail"
	guardrailMetaReason    = "reason"
	guardrailMetaReset     = "reset"
	guardrailMetaCaller    = "caller"
	guardrailMetaBudget    = "budget_added_usd"
)

// GuardrailPersistedState is the halt state folded out of a session's
// eventlog — what a fresh process must restore to honor a halt that
// happened before it started.
type GuardrailPersistedState struct {
	// WatchdogTripped / CostTripped are latched by a trip row and
	// cleared by a reset row naming that guardrail.
	WatchdogTripped bool
	WatchdogReason  string
	CostTripped     bool
	CostReason      string

	// BudgetAddedUSD is the total runway operators granted across every
	// reset in the session's history. Applied on top of the CONFIGURED
	// per-session ceiling, so an operator who bought $5 of headroom
	// doesn't lose it to a pod roll and re-halt at the old bar.
	BudgetAddedUSD float64
}

// Halted reports whether the folded state leaves the session refusing
// turns.
func (s GuardrailPersistedState) Halted() bool {
	return s.WatchdogTripped || s.CostTripped
}

// NewGuardrailTripEvent builds the durable row recording that a
// guardrail halted the session. guardrail is GuardrailWatchdog or
// GuardrailCostCeiling; reason is the operator-facing explanation,
// carried verbatim so the restored halt says the same thing the
// original did rather than a reconstruction of it.
func NewGuardrailTripEvent(guardrail, reason string) *session.Event {
	ev := session.NewEvent(GuardrailTripEventName)
	ev.Author = GuardrailTripEventAuthor
	ev.CustomMetadata = map[string]any{
		guardrailMetaSource:    "agent",
		guardrailMetaGuardrail: guardrail,
		guardrailMetaReason:    reason,
	}
	return ev
}

// NewGuardrailResetAuditEvent builds the durable row recording an
// operator-initiated guardrail reset (#666). It is both the audit trail
// (#331) and the state transition that keeps the halt cleared across a
// restart (#643): CustomMetadata carries who did it, what was cleared,
// and how much budget was added.
func NewGuardrailResetAuditEvent(identity string, reset []string, budgetUSD float64) *session.Event {
	ev := session.NewEvent(GuardrailResetEventName)
	ev.Author = GuardrailResetEventAuthor
	meta := map[string]any{
		guardrailMetaSource: "operator",
		guardrailMetaReset:  append([]string{}, reset...),
	}
	if identity != "" {
		meta[guardrailMetaCaller] = identity
	}
	if budgetUSD > 0 {
		meta[guardrailMetaBudget] = budgetUSD
	}
	ev.CustomMetadata = meta
	return ev
}

// FoldGuardrailEvents replays a session's events in order and returns
// the guardrail state they imply. Events that aren't guardrail rows are
// skipped, so callers can hand it a whole session tail.
//
// Malformed rows are ignored rather than failing the fold: a row whose
// metadata we can't read is a row we can't act on, and refusing to
// restore ANY state because one row is unreadable would disarm the
// backstop — the opposite of what a durable halt is for.
func FoldGuardrailEvents(events iter.Seq[*session.Event]) GuardrailPersistedState {
	var out GuardrailPersistedState
	for ev := range events {
		if ev == nil || len(ev.CustomMetadata) == 0 {
			continue
		}
		switch ev.Author {
		case GuardrailTripEventAuthor:
			guardrail, _ := ev.CustomMetadata[guardrailMetaGuardrail].(string)
			reason, _ := ev.CustomMetadata[guardrailMetaReason].(string)
			switch guardrail {
			case GuardrailWatchdog:
				out.WatchdogTripped, out.WatchdogReason = true, reason
			case GuardrailCostCeiling:
				out.CostTripped, out.CostReason = true, reason
			}
		case GuardrailResetEventAuthor:
			for _, g := range guardrailResetList(ev.CustomMetadata[guardrailMetaReset]) {
				switch g {
				case GuardrailWatchdog:
					out.WatchdogTripped, out.WatchdogReason = false, ""
				case GuardrailCostCeiling:
					out.CostTripped, out.CostReason = false, ""
				}
			}
			if usd, ok := ev.CustomMetadata[guardrailMetaBudget].(float64); ok && usd > 0 {
				out.BudgetAddedUSD += usd
			}
		}
	}
	return out
}

// guardrailResetList normalizes the "reset" metadata value. In-process
// it is a []string; after a round-trip through the eventlog's JSON
// column it comes back as []any. Handling only one of those would make
// the fold work in tests and fail in production, or the reverse.
func guardrailResetList(v any) []string {
	switch got := v.(type) {
	case []string:
		return got
	case []any:
		out := make([]string, 0, len(got))
		for _, item := range got {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}
