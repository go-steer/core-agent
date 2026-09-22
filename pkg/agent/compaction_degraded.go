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

// Keeping context bounded when the usual machinery cannot (#974).
//
// Compaction has two dependencies that a long unattended run can lose,
// and it used to lose both silently:
//
//  1. It needs to know the model's context window. Pin a model the
//     pricing catalogue has never heard of and ContextWindowSize()
//     returns 0, which ShouldCompact read as "not full yet" — so
//     compaction never fired, nothing said so, and the run proceeded
//     normally until the provider hard-failed on an oversized request
//     several thousand turns later. That is the worst shape a safety
//     mechanism can have: absent, silent, and observable only through an
//     unrelated downstream failure.
//
//  2. It needs the model itself, because it summarizes by calling it. So
//     the same 429 storm that is breaking turns breaks compaction, the
//     failure backoff grows to 32 turns, and history keeps growing the
//     whole time. The only strategy depended on the resource that was
//     already failing.
//
// Either alone is survivable. Together they describe a long unattended
// run that quietly loses its only defence against context growth at
// exactly the moment it needs it, and cannot tell anyone.
//
// The answers here are deliberately unglamorous: assume a small window
// when the real one is unknown, and truncate mechanically when the
// summarizer will not answer. Both are worse than the real thing. Both
// are enormously better than unbounded growth, and both say so out loud
// — on the eventlog, where an attached operator can see them, not only
// on the daemon's stderr.

package agent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"

	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/pkg/attach"
)

// AssumedContextWindowSize is the window DefaultCompactor.ShouldCompact
// applies when the session's model is not in usage's context-window
// table. 128K tokens.
//
// The number is chosen to be wrong in the safe direction. Guessing too
// large is a hard failure — the threshold lands past a wall the provider
// enforces, and the session dies on a context-length error, which is
// precisely the #974 symptom. Guessing too small costs extra
// compactions: real work, real money, but the run survives and every one
// of them is visible. 128K is at or below the window of every model
// either supported provider currently serves, so the guess is
// conservative for the realistic case (an operator pinning a Vertex
// publication name, a proxy id, or a model newer than our catalogue) and
// merely wasteful for the unrealistic one.
//
// Note this is NOT pushed down into usage.ContextWindowSizeFor. There,
// 0 means "unknown" and the /context surface renders it as unknown,
// which is honest; a tracker that reports a fabricated window would make
// every consumer display a made-up percentage as though it were
// measured. The assumption belongs to the component that has to act
// without the answer.
const AssumedContextWindowSize = 128_000

// MechanicalCompactionAfterFailures is how many consecutive summarizer
// failures it takes before compaction falls back to mechanical
// truncation.
//
// Two, not one: a single failure is frequently a transient the very next
// attempt clears (models.RetryPolicy already absorbs the common 429),
// and spending a whole compaction window on a mechanical truncation that
// a retry would have summarized properly is a real loss. Two consecutive
// failures against a threshold that keeps re-arming is a summarizer that
// is not coming back on this turn. The old behaviour — pure exponential
// backoff — reached 32-turn cooldowns while history grew the entire
// time, so the bound this replaces was no bound at all.
const MechanicalCompactionAfterFailures = 2

// MechanicalCompactionKey marks a boundary event whose summary was
// produced by truncation rather than by the summarizer, stored on the
// event's CustomMetadata alongside CompactionMetadataKey. Exported so an
// audit-log reader can tell a mechanical boundary from a real one
// without parsing the summary prose — the two are not equally
// trustworthy and a post-mortem should not have to guess which it has.
const MechanicalCompactionKey = "compaction_mechanical"

// mechanicalDigestBudget caps the characters the mechanical summary
// spends on carried-forward conversation text. The whole point of the
// fallback is a bounded artifact, so the bound is explicit rather than
// emergent.
const mechanicalDigestBudget = 6000

// mechanicalToolLedgerLimit caps the tool-call lines the digest keeps.
// Most recent first — a long unattended run's oldest calls are the ones
// its next turn is least likely to need, and #1014's lesson is that a
// record of what was RUN is the part a parent cannot reconstruct.
const mechanicalToolLedgerLimit = 40

// compactionWindowSize resolves the context window ShouldCompact should
// measure against, and reports whether the answer is known or assumed.
//
// known=false with size>0 is the degraded case: the model is not in the
// table and AssumedContextWindowSize is standing in. size==0 means no
// turn has landed yet, which is not degraded and not actionable — there
// is genuinely nothing to compact.
//
// Emitting the operator notice from inside what is otherwise a pure
// lookup is a deliberate trade. The alternative is to make every caller
// of ShouldCompact remember to announce it, and the caller that forgets
// is the one running unattended at 3am. The write is latched to once per
// process and is a queue append, so it stays cheap enough to sit in a
// per-turn predicate.
func (a *Agent) compactionWindowSize() (size int, known bool) {
	if a == nil || a.tracker == nil {
		return 0, false
	}
	if n := a.tracker.ContextWindowSize(); n > 0 {
		return n, true
	}
	last, ok := a.tracker.Last()
	if !ok {
		// No turn has landed. Not a degraded session — an empty one.
		return 0, false
	}
	a.noteAssumedContextWindow(last.Model)
	return AssumedContextWindowSize, false
}

// noteAssumedContextWindow announces, once per process, that compaction
// is running against a guess. Says the model id and the number assumed,
// because the operator's fix is either to pin a catalogued model or to
// accept the guess, and neither decision can be made from "compaction is
// degraded" alone.
func (a *Agent) noteAssumedContextWindow(modelID string) {
	a.mu.Lock()
	if a.warnedUnknownWindow {
		a.mu.Unlock()
		return
	}
	a.warnedUnknownWindow = true
	a.mu.Unlock()

	if modelID == "" {
		modelID = "(unreported)"
	}
	detail := fmt.Sprintf(
		"model %q is not in the context-window table; compacting against an assumed %d-token window. "+
			"Pin a catalogued model id for an exact threshold.",
		modelID, AssumedContextWindowSize)
	// The id rides the log line and stays out of detail, which is also
	// the durable degraded row's text and is already stored against its
	// session (#1136's split, #1137's sweep).
	log.Printf("agent:%s %s", a.logSessionSuffix(), detail)
	a.recordContextReductionDegraded(attach.ContextReductionWindowUnknown, detail)
}

// errNothingToTruncate reports a mechanical compaction that found no
// history to drop. Not a failure worth announcing: it means the window
// is already empty, so the escalation had nothing to do.
var errNothingToTruncate = errors.New("agent: mechanical compaction: no history in the current window")

// mechanicalCompact bounds the context without calling the model.
//
// It writes an ordinary compaction boundary event — same tag, same
// slicing path, same framing on the next turn — whose text is built
// locally from the history it is about to drop. Reusing the boundary
// mechanism rather than inventing a second reduction path is most of
// why this is small: there is nothing new on the read side, and a
// mechanical boundary is indistinguishable from a real one to every
// consumer that does not go looking for MechanicalCompactionKey.
//
// What survives is chosen by what a resuming turn cannot rebuild: the
// ordered ledger of tool calls (a model can re-read a file; it cannot
// know it already did), and the tail of the conversation's prose up to a
// fixed budget. What is dropped is the oldest tool RESULTS, which are
// both the bulk of the tokens and the most re-derivable thing in the
// window. This is lossy and the summary says so in its first line — a
// model that is told it is working from a truncation behaves differently
// from one silently handed a thinner history.
func (a *Agent) mechanicalCompact(ctx context.Context) (CompactionResult, error) {
	if a == nil {
		return CompactionResult{}, errors.New("agent: mechanicalCompact: nil receiver")
	}
	if a.sessionService == nil {
		return CompactionResult{}, errors.New("agent: mechanicalCompact: no session.Service wired")
	}
	if a.turnInFlight() {
		return CompactionResult{}, fmt.Errorf("agent: mechanicalCompact: %w", ErrTurnInFlight)
	}
	history, err := a.summarizerHistory(ctx)
	if err != nil {
		return CompactionResult{}, fmt.Errorf("agent: mechanicalCompact: load history: %w", err)
	}
	if len(history) == 0 {
		return CompactionResult{}, errNothingToTruncate
	}
	summary := mechanicalSummary(history)
	id, err := a.appendBoundaryEvent(ctx, summary, summarizerSpec{
		operation: "MechanicalCompact",
		tag:       CompactionEventTag,
		extraMetadata: map[string]any{
			MechanicalCompactionKey: true,
		},
	})
	if err != nil {
		return CompactionResult{}, fmt.Errorf("agent: mechanicalCompact: persist: %w", err)
	}
	a.mu.Lock()
	a.compactionPending = false
	a.mu.Unlock()
	a.compactionsDone.Add(1)
	return CompactionResult{SummaryEventID: id, SummaryText: summary}, nil
}

// fallBackToMechanicalCompaction is the escalation runPendingCompaction
// takes once the summarizer has failed enough times in a row to stop
// being worth waiting for. summarizerErr is the failure that triggered
// it, carried only so the operator notice can name the cause.
//
// Clears the exponential cooldown on success. The cooldown exists to
// stop us re-paying for a doomed summarizer call every turn; once the
// context is bounded by other means there is nothing left for it to
// protect, and leaving it armed would skip the NEXT compaction — real
// or mechanical — for up to 32 turns while history regrew. The
// consecutive-failure count is deliberately NOT cleared: it is what
// makes the next over-threshold event escalate immediately instead of
// serving another backoff. That leaves exactly one summarizer attempt
// per refill cycle, which is both the cost and the recovery probe.
//
// Every outcome is swallowed. This runs on the operator's turn and is
// already the fallback path; a fallback that can fail the turn it was
// added to protect is not a fallback.
func (a *Agent) fallBackToMechanicalCompaction(ctx context.Context, summarizerErr error) {
	res, err := a.mechanicalCompact(ctx)
	switch {
	case errors.Is(err, errNothingToTruncate):
		// The window is already empty, so there was nothing for the
		// escalation to do. Not worth an operator's attention.
		return
	case err != nil:
		// Both strategies are down. This is the one case with no
		// remaining defence against context growth, so it is reported
		// as a failure rather than as a degradation.
		log.Printf("agent:%s mechanical compaction fallback also failed: %v", a.logSessionSuffix(), err)
		a.recordContextReductionFailure(attach.ContextReductionCompaction, err, 0, 0)
		return
	}
	a.mu.Lock()
	a.compactionCooldown = 0
	a.mu.Unlock()
	a.noteMechanicalCompaction(fmt.Sprintf(
		"summarizer unavailable (%v) — context was bounded by mechanical truncation instead, writing a %d-character literal record in place of a handover summary. Detail has been lost.",
		summarizerErr, len(res.SummaryText)))
}

// noteMechanicalCompaction announces, once per process, that compaction
// has stopped being summarization. Once rather than per-occurrence
// because a summarizer that is down stays down for a while, and a row
// per truncation would bury the failure rows that say why.
func (a *Agent) noteMechanicalCompaction(detail string) {
	a.mu.Lock()
	if a.warnedMechanical {
		a.mu.Unlock()
		return
	}
	a.warnedMechanical = true
	a.mu.Unlock()
	// Log line only; detail is the durable row's text. See
	// noteAssumedContextWindow.
	log.Printf("agent:%s %s", a.logSessionSuffix(), detail)
	a.recordContextReductionDegraded(attach.ContextReductionMechanical, detail)
}

// mechanicalSummary renders the boundary text for a truncation.
//
// Deterministic by construction: same history in, same bytes out, no
// clock and no model. That is not incidental — it is what makes the
// fallback testable at all, and a fallback nobody can test is one nobody
// finds out is broken until the night it is needed.
func mechanicalSummary(history []*genai.Content) string {
	calls, texts := walkMechanicalHistory(history)

	var b strings.Builder
	b.WriteString(mechanicalHeader)

	b.WriteString("\n\n# Tool calls in the dropped window\n")
	if len(calls) == 0 {
		b.WriteString("(none)\n")
	} else {
		kept := calls
		if len(kept) > mechanicalToolLedgerLimit {
			fmt.Fprintf(&b, "(%d earlier calls omitted)\n", len(kept)-mechanicalToolLedgerLimit)
			kept = kept[len(kept)-mechanicalToolLedgerLimit:]
		}
		for _, c := range kept {
			b.WriteString("- ")
			b.WriteString(c)
			b.WriteString("\n")
		}
	}

	b.WriteString("\n# Most recent conversation, verbatim\n")
	tail := tailWithinBudget(texts, mechanicalDigestBudget)
	if len(tail) == 0 {
		b.WriteString("(no conversational text in the dropped window)\n")
	} else {
		if len(tail) < len(texts) {
			fmt.Fprintf(&b, "(%d earlier messages omitted)\n\n", len(texts)-len(tail))
		}
		b.WriteString(strings.Join(tail, "\n\n"))
		b.WriteString("\n")
	}
	return b.String()
}

// mechanicalHeader opens the boundary text. Addressed to the model,
// because the model is who reads it on the next turn, and written to
// pre-empt the specific wrong inference: a thin history reads as "not
// much has happened" unless something says otherwise.
const mechanicalHeader = "[Automatic summarization was unavailable, so this conversation was truncated MECHANICALLY rather than summarized. " +
	"What follows is a literal record, not a handover: the tool calls that were made and the most recent messages, with everything else dropped. " +
	"Detail HAS been lost — do not assume that something absent here never happened. " +
	"If you need the contents of an earlier tool result, run the tool again rather than guessing, and say that you are re-reading because the history was truncated.]"

// walkMechanicalHistory splits a compaction window into the two things
// the digest carries: one line per tool call, and the conversational
// text in order. Tool RESULTS are deliberately not collected — they are
// the bulk of the tokens and the most reconstructible part of the
// window, which is exactly what makes them the right thing to drop.
func walkMechanicalHistory(history []*genai.Content) (calls, texts []string) {
	for _, c := range history {
		if c == nil {
			continue
		}
		role := c.Role
		if role == "" {
			role = "model"
		}
		var text strings.Builder
		for _, p := range c.Parts {
			switch {
			case p == nil:
			case p.FunctionCall != nil:
				calls = append(calls, formatMechanicalCall(p.FunctionCall))
			case p.FunctionResponse != nil:
				// Dropped on purpose. See the doc comment above.
			case p.Text != "":
				if text.Len() > 0 {
					text.WriteString("\n")
				}
				text.WriteString(p.Text)
			}
		}
		if s := strings.TrimSpace(text.String()); s != "" {
			texts = append(texts, role+": "+s)
		}
	}
	return calls, texts
}

// formatMechanicalCall renders one tool call as `name(k=v, k=v)` with
// arguments sorted by key so the output does not depend on Go's map
// iteration order, and each value clipped so one enormous argument
// cannot blow the ledger's share of the budget.
func formatMechanicalCall(fc *genai.FunctionCall) string {
	if len(fc.Args) == 0 {
		return fc.Name + "()"
	}
	keys := make([]string, 0, len(fc.Args))
	for k := range fc.Args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+clip(fmt.Sprint(fc.Args[k]), 80))
	}
	return fc.Name + "(" + strings.Join(parts, ", ") + ")"
}

// tailWithinBudget returns the longest suffix of msgs whose combined
// length fits budget, keeping at least the final message however long it
// is — a budget that returns nothing is a truncation with no content,
// which is worse than one slightly over.
func tailWithinBudget(msgs []string, budget int) []string {
	if len(msgs) == 0 {
		return nil
	}
	total := 0
	start := len(msgs)
	for i := len(msgs) - 1; i >= 0; i-- {
		total += len(msgs[i]) + 2 // the join separator
		if total > budget && i < len(msgs)-1 {
			break
		}
		start = i
	}
	out := msgs[start:]
	if len(out) == 1 {
		out = []string{clip(out[0], budget)}
	}
	return out
}

// clip truncates s to at most n characters, marking that it did.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return "…"
	}
	return s[:n-1] + "…"
}
