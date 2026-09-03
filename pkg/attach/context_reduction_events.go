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

// A failed automatic context reduction, as an eventlog row (#908).
//
// Compaction and checkpointing both run out of band, between turns, and
// both are logged-and-swallowed on failure: they must not fail the
// operator's turn. Until this row existed the ONLY trace was a line on
// the daemon's stderr, which nobody attached to a session can read. That
// matters more for compaction than for checkpointing: a lost checkpoint
// costs a task boundary the operator can re-cut with `/done`, while a
// compaction that never happens means the context window is not being
// reduced when the threshold says it must be, and for a long-lived
// service that is the mechanism keeping the session viable. It also
// fails quietly in the direction that looks fine right up until the
// session wedges against the context wall.
//
// The row follows the shape guardrail_events.go established: an eventlog
// append is the one way an agent-side condition reaches every attach
// consumer without touching the wire protocol. The broadcaster tails the
// eventlog, so the row goes out on the existing back-compat `agent`
// frame and is readable from GET /sessions/{app}/{sid}/events, and being
// durable it is still there for a post-mortem after the client that
// missed it reconnects.
//
// What this deliberately is NOT: a rendered banner in a stock TUI. That
// needs a typed frame, which is a protocol change, and the protocol has
// no non-terminal notification event — the same gap guardrail_halt.go
// records against the v3.0 `guardrail-trip` frame. Reusing `turn-error`
// for it would be worse than the gap: the spec allows exactly one
// terminal frame per turn and this condition is not a turn outcome.
//
// Written for BOTH operations rather than compaction alone. They share
// one summarizer, they fail the same way, and a row that appears for one
// and not the other would misread as "checkpointing is fine" during an
// incident where the shared call is what is broken.

package attach

import "google.golang.org/adk/session"

// Event name and author for the context-reduction failure row. The
// author prefix distinguishes it from model turns in an eventlog tail,
// and matches the `agent/...` convention the guardrail trip row uses for
// things that happen TO a session.
const (
	// ContextReductionFailedEventName is the event name of the row.
	ContextReductionFailedEventName = "context-reduction-failed"
	// ContextReductionFailedEventAuthor authors the row.
	ContextReductionFailedEventAuthor = "agent/context-reduction"
)

// Values carried under the row's `operation` metadata key. Lowercase
// nouns rather than the Go method names so a consumer rendering them is
// not quoting an implementation detail back at an operator.
const (
	// ContextReductionCompaction marks a failed automatic compaction —
	// the threshold-driven summarizer run (Mechanism A).
	ContextReductionCompaction = "compaction"
	// ContextReductionCheckpoint marks a failed automatic task-boundary
	// checkpoint (Mechanism C).
	ContextReductionCheckpoint = "checkpoint"
)

// Metadata keys on the row.
const (
	ctxReductionMetaSource   = "source"
	ctxReductionMetaOp       = "operation"
	ctxReductionMetaReason   = "reason"
	ctxReductionMetaFailures = "consecutive_failures"
	ctxReductionMetaCooldown = "cooldown_turns"
)

// NewContextReductionFailedEvent builds the eventlog row recording that
// an automatic context reduction failed and was swallowed.
//
// operation is ContextReductionCompaction or ContextReductionCheckpoint.
// reason is the error text, carried verbatim — the provider's own
// explanation for a text-less summary ("finish_reason=MAX_TOKENS") is
// the whole diagnostic value of the row, so it is not summarised down to
// a category here.
//
// consecutiveFailures and cooldownTurns describe the backoff the agent
// applied, and are omitted from the metadata when zero: the checkpoint
// path has no backoff to report, and a key that is always present but
// meaningless for half its writers is a key consumers read wrong.
func NewContextReductionFailedEvent(operation, reason string, consecutiveFailures, cooldownTurns int) *session.Event {
	ev := session.NewEvent(ContextReductionFailedEventName)
	ev.Author = ContextReductionFailedEventAuthor
	md := map[string]any{
		ctxReductionMetaSource: "agent",
		ctxReductionMetaOp:     operation,
		ctxReductionMetaReason: reason,
	}
	if consecutiveFailures > 0 {
		md[ctxReductionMetaFailures] = consecutiveFailures
	}
	if cooldownTurns > 0 {
		md[ctxReductionMetaCooldown] = cooldownTurns
	}
	ev.CustomMetadata = md
	return ev
}

// ContextReductionFailure reads a context-reduction failure row back out
// of an eventlog tail. Returns ok=false for any other event, so a
// consumer can run it over an undifferentiated event stream.
//
// Exists so a client (or a test) matches on the row through one function
// rather than re-deriving the key names, which is how the two halves of
// a metadata contract drift apart.
func ContextReductionFailure(ev *session.Event) (operation, reason string, ok bool) {
	if ev == nil || ev.CustomMetadata == nil {
		return "", "", false
	}
	if ev.Author != ContextReductionFailedEventAuthor {
		return "", "", false
	}
	operation, _ = ev.CustomMetadata[ctxReductionMetaOp].(string)
	reason, _ = ev.CustomMetadata[ctxReductionMetaReason].(string)
	return operation, reason, true
}
