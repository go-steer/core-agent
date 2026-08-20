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

// Auto-continue support (#539, docs/auto-continue-design.md): pure
// detection + prompt-synthesis primitives. The wiring that decides
// WHETHER to continue (config, freshness, run lock) lives with the
// resume paths in pkg/compose; everything here is side-effect-free so
// both the lazy-resume trigger (PR 1) and the boot scan (PR 2) share
// one classifier.

package agent

import (
	"fmt"
	"strings"
	"time"

	"google.golang.org/adk/session"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/pkg/eventlog"
	"github.com/go-steer/core-agent/v2/pkg/tools"
)

// AutoContinueOriginator is the turn-originator identity stamped on
// synthesized continuation turns (via Agent.InjectAs), so eventlog
// metadata and audit logs distinguish them from human turns. (When
// the note drains into one batch with a concurrently-injected human
// message, the last-message-caller rule makes the human the turn
// originator; the note text itself still marks the turn.)
const AutoContinueOriginator = "core-agent/auto-continue"

// autoContinueMarker is the note substring the classifier uses to
// recognize a PRIOR continuation attempt in committed history. If a
// continuation turn itself died after its note committed, the tail is
// an unanswered user event containing this marker — and continuing
// AGAIN would loop: crash → resume → note → crash. One committed note
// = no further automatic attempts; a human message resets the state.
// This is the lazy-path loop bound until the boot scan's full breaker
// lands (design PR 2).
//
// The marker is a neutral, cause-agnostic phrase (#615): the note must
// not assert a "daemon restart" it cannot verify — the interruption is
// inferred purely from tail shape and fires from in-lifetime retries
// too, not just boots. It is deliberately decoupled from any wording
// that names a cause so the human text can evolve without breaking the
// loop-breaker. Kept lowercase (no leading article) so it matches as a
// case-sensitive substring of the rendered, sentence-cased note.
const autoContinueMarker = "previous turn did not complete"

// legacyAutoContinueMarker is the pre-#615 marker phrasing. Recognized
// by the classifier (never emitted) so a continuation note committed by
// an older binary and still in flight across an upgrade is not
// re-continued into a loop. Safe to drop once no such notes can remain
// (past the freshness window + per-session cap after every daemon has
// upgraded).
const legacyAutoContinueMarker = "interrupted by a daemon restart"

// interruptAuditAuthor is the Author pkg/attach stamps on the
// contentless audit row appendInterruptAudit writes when an operator
// interrupts a turn (mirrored here because attach imports would be
// fine but the value is wire-stable audit state, not API).
const interruptAuditAuthor = "attach/interrupt"

// AutoContinueNote renders the synthesized continuation prompt. It is
// a system note, not impersonated user text: the model is told what was
// DETECTED and asked to pick the task back up. It describes what the
// classifier actually observed — an unfinished turn — and does NOT claim
// a cause (e.g. a daemon restart) it cannot verify: the same note fires
// from lazy-resume, boot scan, startup-session, and the in-lifetime
// retry loop, and interruption is inferred from tail shape alone (#615).
// Must contain autoContinueMarker (guarded by test).
func AutoContinueNote(interruptedAt time.Time) string {
	// "The " + autoContinueMarker keeps the marker an exact case-sensitive
	// substring of the rendered sentence, so the classifier's loop-breaker
	// grep stays correct as the surrounding wording evolves.
	return fmt.Sprintf("[system note] The %s (last committed at %s). "+
		"The last committed events are in your history; any interrupted tool call has been answered with an interruption notice. "+
		"Continue the task: re-issue interrupted tool calls if their results are still needed, then answer the user's outstanding message. "+
		"If nothing remains to do, reply briefly to confirm.",
		autoContinueMarker, interruptedAt.UTC().Format(time.RFC3339))
}

// autoContinueNoteReadOnly renders the continuation note for a tail
// interrupted mid-tool where EVERY interrupted call is a read-only
// introspection tool (#624). It drops the default note's "re-issue
// interrupted tool calls" imperative: reflexively re-running a
// side-effect-free read is exactly what turned an operator's
// stop+interrupt into a list_agents loop. The model is told the
// interrupted work had no side effects and to re-query only if it still
// needs that data — so a genuine information need is still met, while a
// polling call is not blindly re-issued. Still contains autoContinueMarker
// (the loop-breaker grep) and, like the default, asserts no unverifiable
// cause (#615).
func autoContinueNoteReadOnly(interruptedAt time.Time) string {
	return fmt.Sprintf("[system note] The %s (last committed at %s). "+
		"The last committed events are in your history; any interrupted tool call has been answered with an interruption notice. "+
		"The interrupted call(s) were read-only introspection with no side effects — do not reflexively re-run them; query again only if you still need that information to answer the operator's outstanding message. "+
		"Answer the operator's outstanding message. If nothing remains to do, reply briefly to confirm.",
		autoContinueMarker, interruptedAt.UTC().Format(time.RFC3339))
}

// AutoContinueNoteFor selects the continuation note appropriate to what
// was interrupted (#624). When the tail died mid-tool and EVERY
// interrupted call classifies read-only (tools.IsReadOnlyToolName), it
// returns the read-only variant that does NOT nudge re-issuing them;
// otherwise (a mutating or unknown interrupted call, or an interrupted
// shape with no calls — a bare user message or a committed tool
// response) it returns the default note, which still tells the model it
// MAY re-issue. Classification is by tool metadata, never a note-local
// name list: an unrecognized name falls to the default (keep the nudge),
// the conservative direction.
func AutoContinueNoteFor(interruptedAt time.Time, interruptedCalls []string) string {
	if allReadOnlyCalls(interruptedCalls) {
		return autoContinueNoteReadOnly(interruptedAt)
	}
	return AutoContinueNote(interruptedAt)
}

// allReadOnlyCalls reports whether names is non-empty and every entry
// classifies read-only. Empty → false, so a no-calls interrupted shape
// keeps the default note.
func allReadOnlyCalls(names []string) bool {
	if len(names) == 0 {
		return false
	}
	for _, n := range names {
		if !tools.IsReadOnlyToolName(n) {
			return false
		}
	}
	return true
}

// ClassifyInterruptedTail reports whether a session's committed
// history ends in an interrupted turn, and when the interruption
// happened (the timestamp of the last committed event of the broken
// turn). Detection is derived entirely from eventlog state — no
// write-ahead marker exists or is needed; per-event persistence means
// the tail shape IS the marker (docs/auto-continue-design.md
// §Detection). The one place tail shape is insufficient is a MAX_TOKENS
// output-cap truncation that still emitted text — indistinguishable from
// a normal completion by shape alone — so the eventlog overlay stamps the
// genai FinishReason into CustomMetadata and the hasText arm consults it
// (#582).
//
// The classifier walks backward to the last *conversational* event on
// the parent branch, skipping annotation rows: subagent branches,
// streaming partials, contentless/role-less events (autonomous
// checkpoints and notes, interrupt-audit rows), and compaction
// summaries. That event then classifies as:
//
//   - user message (no tool parts)            → interrupted: the
//     question was committed, no answer ever came.
//   - carries a functionResponse part         → interrupted: a tool
//     result (possibly a #537 tail-repair synthesis) was committed
//     but no model turn consumed it.
//   - carries a functionCall part             → interrupted mid-tool
//     (the #537 repair target, seen before repair has run) — UNLESS
//     every call in the event is long-running (LongRunningToolIDs):
//     ADK treats a turn ending in only long-running calls as final,
//     with responses legitimately arriving in a later user turn.
//   - anything else (model text)              → completed turn,
//     UNLESS a stamped MAX_TOKENS finish reason marks it truncated
//     mid-task (then: interrupted — see incompleteFinish).
//
// KNOWN LIMITATION — "interrupted" vs "in progress" (#796). Every arm
// above reads committed history and nothing else, so an interrupted turn
// and a turn that is STILL RUNNING are the same tail: a session whose
// user message has committed and whose model reply has not classifies
// interrupted whether the generation died or is thirty seconds into a
// long answer. This is not fixable here — no shape in the eventlog
// distinguishes the two, and a time-based guess ("a user event newer
// than N seconds is probably still running") would be exactly that, a
// guess, in a position where being wrong duplicates a reply. Liveness is
// a fact the running process holds, not one history records, so callers
// that might run concurrently with a turn must consult it: check
// Agent.TurnInFlight before acting on an interrupted verdict (pkg/compose's
// lockClassifyInject does, for all three auto-continue triggers). On the
// boot path the ambiguity cannot arise — after a restart nothing is in
// flight — which is why the in-lifetime retry driver was the caller that
// found this.
//
// Additional terminal shapes (never continued): an operator
// interrupt-audit row anywhere after the tail (deliberate kill), an
// ErrorCode final (the turn ended with an error the user already
// saw), an empty-parts agent-authored final (Gemini streaming can
// close a completed turn with an empty aggregate), a turn parked on
// ADK's tool-confirmation flow, a SkipSummarization response final,
// and a tail that IS a prior committed continuation note (one
// automatic attempt per interruption — the lazy-path crash-loop
// bound).
func ClassifyInterruptedTail(events []*session.Event) (interruptedAt time.Time, interrupted bool) {
	at, interrupted, _ := classifyInterruptedTail(events)
	return at, interrupted
}

// ClassifyInterruptedTailWithCalls is ClassifyInterruptedTail plus the
// tool-call names of the interrupted tail (#624). interruptedCalls is
// populated ONLY for a mid-tool interruption (the functionCall arm) —
// the shape whose continuation note carries a "re-issue interrupted tool
// calls" nudge — and lists every call name in that tail event. It is nil
// for the other interrupted shapes (a bare unanswered user message, or a
// committed functionResponse whose result is already in history), and nil
// when the tail is not interrupted. Callers scope the continuation note
// by classifying these names (see AutoContinueNoteFor).
func ClassifyInterruptedTailWithCalls(events []*session.Event) (interruptedAt time.Time, interrupted bool, interruptedCalls []string) {
	return classifyInterruptedTail(events)
}

func classifyInterruptedTail(events []*session.Event) (interruptedAt time.Time, interrupted bool, interruptedCalls []string) {
	for i := len(events) - 1; i >= 0; i-- {
		ev := events[i]
		if ev == nil || ev.Branch != "" || ev.Partial {
			continue
		}
		// Operator interrupt: the audit row postdates whatever tail
		// the cancelled turn left. The operator deliberately killed
		// that work — resurrecting it would be exactly wrong.
		if ev.Author == interruptAuditAuthor {
			return time.Time{}, false, nil
		}
		// In-band model error final (safety block, quota, provider
		// error surfaced as ErrorCode): the turn ENDED and the error
		// was already delivered; there is nothing to continue.
		if ev.ErrorCode != "" {
			return time.Time{}, false, nil
		}
		if ev.Content == nil || ev.Content.Role == "" {
			continue // annotation events (checkpoints, notes, audit rows)
		}
		if _, isSummary := ev.CustomMetadata[CompactionMetadataKey]; isSummary {
			continue // compaction/checkpoint summary — meta, not conversation
		}
		if len(ev.Content.Parts) == 0 {
			// Empty-parts committed model final: Gemini streaming can
			// close a COMPLETED turn with an empty aggregate (empty-
			// candidate STOP, immediate MAX_TOKENS). Walking past it
			// would misread the preceding tool-response event as an
			// interrupted tail. Agent-authored → terminal.
			if ev.Author != "user" {
				return time.Time{}, false, nil
			}
			continue
		}

		hasText, hasCall, hasResp, hasConfirmation := false, false, false, false
		allCallsLongRunning := true
		longRunning := map[string]bool{}
		for _, id := range ev.LongRunningToolIDs {
			longRunning[id] = true
		}
		var text strings.Builder
		var callNames []string
		for _, p := range ev.Content.Parts {
			switch {
			case p == nil:
			case p.FunctionCall != nil:
				hasCall = true
				callNames = append(callNames, p.FunctionCall.Name)
				if p.FunctionCall.Name == confirmationCallName {
					hasConfirmation = true
				}
				if !longRunning[p.FunctionCall.ID] {
					allCallsLongRunning = false
				}
			case p.FunctionResponse != nil:
				hasResp = true
			case p.Text != "":
				hasText = true
				text.WriteString(p.Text)
			}
		}

		// Turn parked on ADK's tool-confirmation flow: waiting for a
		// human decision, not interrupted — and a continuation turn
		// couldn't proceed anyway (tail repair deliberately leaves
		// the parked original call unanswered).
		if hasConfirmation {
			return time.Time{}, false, nil
		}
		// SkipSummarization response = the turn's final event per
		// ADK's Event.IsFinalResponse — a completed turn for library
		// consumers using that flow.
		if hasResp && ev.Actions.SkipSummarization {
			return time.Time{}, false, nil
		}

		switch {
		case hasResp:
			return ev.Timestamp, true, nil
		case hasCall:
			if allCallsLongRunning {
				return time.Time{}, false, nil
			}
			return ev.Timestamp, true, callNames
		case ev.Author == "user":
			// A prior continuation note that committed and then died:
			// do NOT loop — one automatic attempt per interruption;
			// a real human message resets this. Recognize the legacy
			// marker too so a note committed by a pre-#615 binary and
			// still in flight across an upgrade isn't re-continued.
			t := text.String()
			if strings.Contains(t, autoContinueMarker) || strings.Contains(t, legacyAutoContinueMarker) {
				return time.Time{}, false, nil
			}
			return ev.Timestamp, true, nil
		case hasText:
			// Normally a completed model turn — UNLESS the persisted
			// finish reason says the turn stopped WITHOUT finishing (a
			// MAX_TOKENS output-cap truncation that still emitted text).
			// ADK's storage row drops FinishReason, so we read the copy
			// the eventlog overlay stamps into CustomMetadata (#582). STOP
			// and unset (legacy/unstamped) preserve the completed default.
			if r, ok := ev.CustomMetadata[eventlog.FinishReasonMetadataKey].(string); ok && incompleteFinish(r) {
				return ev.Timestamp, true, nil
			}
			return time.Time{}, false, nil // completed model turn
		default:
			// Agent-authored content with neither text nor tool parts
			// (inline data only) — terminal model output.
			return time.Time{}, false, nil
		}
	}
	return time.Time{}, false, nil
}

// incompleteFinish reports whether a persisted genai FinishReason string
// (stamped by the eventlog overlay under eventlog.FinishReasonMetadataKey)
// marks a model turn that stopped WITHOUT finishing its task and can be
// safely re-driven. Only MAX_TOKENS qualifies: the model hit its output
// cap mid-answer, so a continuation resumes exactly where it was cut.
// STOP and unset (legacy/unstamped) → completed. The terminal-block
// reasons (SAFETY, RECITATION, BLOCKLIST, PROHIBITED_CONTENT, SPII,
// MALFORMED_FUNCTION_CALL) are deliberately NOT continued — a retry would
// re-trigger the same block; they belong with ErrorCode as outcomes the
// user already saw. Revisit per-reason if a concrete consumer appears.
func incompleteFinish(reason string) bool {
	return reason == string(genai.FinishReasonMaxTokens)
}
