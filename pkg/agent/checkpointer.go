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

// File checkpointer.go implements Mechanism C (task-boundary
// checkpoints) from docs/context-management-design.md. NOT to be
// confused with agent/checkpoint.go — that file's "checkpoint" is
// the autonomous-driver's per-turn resume snapshot (entirely
// different concept). The naming collision is unfortunate but
// the audience for "task-boundary checkpoint" is the operator;
// the audience for "autonomous checkpoint" is the resume
// machinery. Both names are load-bearing in their own contexts.
//
// Checkpoint shares its summarizer + persistence + slicing
// machinery with compactor.go's Compact via runSummarizer +
// appendBoundaryEvent + sliceFromBoundary. The differences:
//
//   - Trigger: model-initiated via the mark_task_done tool call
//     (operator can also fire manually via /done). Compact's
//     trigger is token-utilization threshold.
//   - Metadata tag: "checkpoint" vs "summary". Same key, different
//     value, so a single findLatestBoundary call recognizes both.
//   - Prompt: Compactor's "Produce a teammate-style handover…"
//     vs Checkpointer's "Produce a completion record + handover…"
//     (the leading completion line tells the model the just-
//     finished task is the focal point of the summary).

package agent

import (
	"context"
	"errors"
	"log"
	"strings"
	"time"

	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"

	"github.com/go-steer/core-agent/v2/pkg/attach"
)

// CheckpointEventTag is the value stored under
// session.Event.CustomMetadata["compaction"] for a task-boundary
// checkpoint event. Distinct from CompactionEventTag ("summary")
// so the audit log + telemetry can distinguish "we hit the token
// wall and summarized" from "the model said this task was done."
const CheckpointEventTag = "checkpoint"

// CheckpointNoteKey carries the operator/model-supplied task note
// (the `detail` arg of mark_task_done, or the operator's /done
// argument) on the checkpoint event's CustomMetadata. Parallel to
// CompactionFocusKey for compaction events.
const CheckpointNoteKey = "checkpoint_note"

// Checkpointer decides whether to auto-trigger task-boundary
// checkpoints and produces the summarizer prompt. The
// mark_task_done tool always triggers a checkpoint regardless of
// ShouldCheckpoint — the heuristic is for OPTIONAL post-turn
// auto-fire based on assistant-text patterns ("looks done") and
// is off by default. Consumers customize by implementing
// Checkpointer themselves.
type Checkpointer interface {
	// ShouldCheckpoint is the heuristic gate fired post-turn when
	// the model didn't explicitly call mark_task_done. Default
	// implementation always returns false (heuristic off). Custom
	// implementations could scan the just-completed assistant
	// message for completion patterns.
	ShouldCheckpoint(ctx context.Context, a *Agent) bool

	// CheckpointInstruction returns the system instruction the
	// summarizer LLM call uses. taskNote carries the operator/
	// model-supplied completion detail (the `detail` arg of
	// mark_task_done, or the operator's /done argument).
	CheckpointInstruction(taskNote string) string
}

// DefaultCheckpointer is the package-default Checkpointer.
// Heuristic is off (ShouldCheckpoint always false) — mark_task_done
// + /done are the trigger paths. Prompt mirrors DefaultCompactor's
// five-section handover plus a "Completion record" preamble that
// names the just-finished task as the focal point of the summary.
type DefaultCheckpointer struct{}

// NewDefaultCheckpointer returns a DefaultCheckpointer. Pass
// &DefaultCheckpointer{} directly if you want to assert the type;
// the constructor exists for symmetry with NewDefaultCompactor.
func NewDefaultCheckpointer() Checkpointer { return &DefaultCheckpointer{} }

// ShouldCheckpoint returns false. Heuristic-based auto-checkpoint
// is intentionally off by default — false positives (declaring a
// task done when the operator is mid-thought) are costly. The
// mark_task_done tool gives the model an explicit signal it can
// invoke when it's confident; /done gives the operator the same.
func (c *DefaultCheckpointer) ShouldCheckpoint(_ context.Context, _ *Agent) bool {
	return false
}

// CheckpointInstruction returns the five-section handover prompt
// with a "Completion record" preamble that names the just-
// finished task. The model's summary will frame the conversation
// from "this task is now done" angle rather than the "we're still
// mid-task" angle DefaultCompactor produces.
func (c *DefaultCheckpointer) CheckpointInstruction(taskNote string) string {
	var b strings.Builder
	b.WriteString(defaultCheckpointHeader)
	if strings.TrimSpace(taskNote) != "" {
		b.WriteString("\n\nCompletion note from the operator or the model: ")
		b.WriteString(strings.TrimSpace(taskNote))
	}
	return b.String()
}

const defaultCheckpointHeader = `You are writing a handover record for a task the conversation just finished. The conversation will CONTINUE from this point — the operator may follow up with related questions, refinements, or a new task — but the prior task's exploration, tool output, and back-and-forth are about to be sliced from the next turn's context. Your job is to make sure nothing important is lost when that slicing happens.

Produce a dense teammate-style record with these SIX sections in order, using these exact headings:

# Task
What was the task? What's the headline outcome? One paragraph max. Do NOT lead with "task complete" or similar terminal language — the conversation continues and the next prompt may still be about this work.

# Files & changes
Files modified (one-line per file describing the change). Files read or analyzed during the task. Files that were considered and explicitly NOT changed (with why).

# Technical context
Architectural decisions made. Patterns adopted. Commands that worked. Commands that failed and why. Anything the next turn will need to know about the environment.

# Strategy & approach
The strategy chosen. Alternatives considered and rejected. Gotchas surfaced. Lessons that should carry forward.

# Verification & next steps
What's been verified (tests pass, manual UAT done). What's known-good but unverified. What follow-up work is queued (if any).

# Where we are
A one-paragraph status framed as "what the operator and I both know right now" — the working context the next prompt picks up from. NOT a closing statement; the next turn may revisit this task, extend it, or ask "recap what we did about X" expecting you to answer from this record.

Be dense and concrete. This record REPLACES the task's conversation history for future turns — anything you omit is effectively gone, and anything you record here is what you (and the operator) will have to work from when they ask a follow-up. Skip social niceties; capture facts.`

// CheckpointResult reports what happened on a Checkpoint call.
// Same fields as CompactionResult plus TaskNote (the detail that
// triggered the checkpoint, surfaced so UI / telemetry can show
// it without re-reading the event metadata).
type CheckpointResult struct {
	CheckpointEventID string
	SummaryText       string
	TaskNote          string
	Duration          time.Duration
	Skipped           bool
}

// ErrNoCheckpointer is returned by Agent.Checkpoint when the agent
// was constructed without WithCheckpointer. Callers should check
// for this sentinel before treating it as a hard failure.
var ErrNoCheckpointer = errors.New("agent: no checkpointer wired (pass WithCheckpointer at agent.New)")

// Checkpoint writes a task-boundary checkpoint event to the
// session and clears any pending checkpoint flag. Like Compact,
// the event becomes the slicing boundary for the next turn's
// model request.
//
// taskNote is the operator/model-supplied detail (the mark_task_done
// `detail` arg, or /done's argument). Empty is fine — the prompt
// still produces a useful summary, just without the leading
// completion note.
//
// Errors:
//   - ErrNoCheckpointer when no checkpointer was wired.
//   - Context cancellation: ctx.Err().
//   - Model errors propagate wrapped so callers can errors.Is on
//     transport vs API failures.
func (a *Agent) Checkpoint(ctx context.Context, taskNote string) (CheckpointResult, error) {
	if a == nil {
		return CheckpointResult{}, errors.New("agent: Checkpoint: nil receiver")
	}
	if a.checkpointer == nil {
		return CheckpointResult{}, ErrNoCheckpointer
	}
	out, err := a.runSummarizer(ctx, summarizerSpec{
		operation:         "Checkpoint",
		systemInstruction: a.checkpointer.CheckpointInstruction(taskNote),
		tag:               CheckpointEventTag,
		noteKey:           CheckpointNoteKey,
		note:              taskNote,
	})
	if err != nil {
		return CheckpointResult{}, err
	}
	if out.Skipped {
		return CheckpointResult{Skipped: true, TaskNote: taskNote}, nil
	}
	// Clear any pending flags — we just wrote a boundary. A
	// pending compaction is also moot now (the checkpoint is also
	// a slicing boundary, so re-running compaction immediately
	// would just summarize the empty post-boundary slice).
	a.mu.Lock()
	a.checkpointPending = false
	a.pendingCheckpointNote = ""
	a.compactionPending = false
	a.mu.Unlock()
	// In-memory metrics counter (#338) — see the matching increment
	// in Compact for why this isn't the eventlog-derived count.
	a.checkpointsDone.Add(1)
	return CheckpointResult{
		CheckpointEventID: out.SummaryEventID,
		SummaryText:       out.SummaryText,
		TaskNote:          taskNote,
		Duration:          out.Duration,
	}, nil
}

// markTaskDoneArgs is the JSON shape the model sees when calling
// mark_task_done. Single arg — no task_name — because the model
// is bad at picking stable task identifiers and the detail string
// is what the checkpoint preamble actually needs.
//
// The schema names a content obligation (what the next turn needs)
// rather than a genre (#905/#909). It used to ask for a "one-paragraph
// completion summary", and that phrase, not the description, is what
// shaped the visible failure: a live daemon answered unrelated operator
// questions with completion reports because a tool schema had asked it
// for one. An arg description is a writing prompt — the model writes
// the genre it is handed.
type markTaskDoneArgs struct {
	Detail string `json:"detail" jsonschema:"the facts a future turn needs in order to pick up from here: the concrete outcome, the state things are now in, and anything left unresolved. A few sentences at most. The runtime folds this into a handover record; a short form may also appear in the operator's UI as a label. This is not a report to the operator and not a recap of the session."`
}

type markTaskDoneResult struct {
	Status string `json:"status"`

	// NoOp marks a call that accomplished nothing, for
	// watchdog.NoOpStreakSignal to count (#907). Machine-readable on
	// purpose: Status already says the same thing in English, and the
	// English is what gets reworded. Omitted when false so the ordinary
	// result keeps its one-field shape in the model's context.
	NoOp bool `json:"no_op,omitempty"`
}

// markTaskDoneDescription is the model-facing description. A named
// constant rather than an inline literal so the prose regression guard
// in checkpointer_test.go asserts against the exact string the model
// sees — see NewMarkTaskDoneTool for what the rewrite fixed and why the
// banned shapes are banned.
const markTaskDoneDescription = "Signal that a task you were given is finished, so the runtime can slice its conversation history out of future turns and the next task starts with a clean context window. This is a context-management signal, not a way to report to the operator: calling it does not answer a question and does not deliver work — anything the operator needs to read belongs in your reply. Call it only when a task is finished AND you have already delivered what it asked for, and only for work that finished in THIS turn. Do NOT call it mid-task, for partial progress, for work you already reported in an earlier turn, or in place of answering what you were just asked. When in doubt, do not call it: a boundary you missed can still be declared later, but one declared too early throws away context that is still in use."

// NewMarkTaskDoneTool returns the model-facing tool that signals
// task completion. The handler doesn't fire the checkpoint
// inline — that would require synchronous LLM I/O from inside a
// tool call, which ADK's runner doesn't expect. Instead the
// handler stashes the detail on the agent and flips a pending
// flag; Agent.Run's post-turn hook picks it up and fires
// Checkpoint before the next turn.
//
// Takes a getter rather than a *Agent directly because we
// register this tool BEFORE the agent struct is constructed
// (llmagent.New snapshots its tool list at construction time, so
// registration has to happen up front). The getter resolves
// lazily — agent.New sets the agent pointer after llmagent.New
// returns, and the getter walks the closure to find it. A nil
// return from the getter is treated as "registration race not
// yet completed" and the call is a silent no-op (defensive —
// shouldn't happen in practice because the model never sees the
// tool before agent.New returns).
//
// Registered automatically in agent.New when a Checkpointer is
// wired via WithCheckpointer, unless WithoutMarkTaskDoneTool asks
// for the operator-only posture (#905).
//
// The description is prompt text and was rewritten as such (#905/#909).
// The original told the model to "use this generously at natural task
// boundaries (after shipping a feature, finishing a code review,
// completing a debugging session)" — three examples from an
// interactive coding session, plus a frequency instruction. A daemon
// consuming machine signals has no conversation about to shift to a new
// task, so every inbox bundle after a closed incident reads as a
// boundary; one live deployment produced sixteen calls in a single
// session and answered unrelated operator questions with completion
// reports. Two prose layers in the recipe forbade exactly that
// behavior and lost, because a tool description outranks the persona at
// the point of decision and a recipe author cannot edit it.
//
// So: no frequency instruction, no workload examples, and the negations
// name the observed failure rather than only the obvious one
// ("mid-task"). Skipping a boundary is stated as costless because it
// is — compaction reduces context on its own, and the model has no way
// to know that otherwise.
func NewMarkTaskDoneTool(getter func() *Agent) tool.Tool {
	handler := func(_ tool.Context, args markTaskDoneArgs) (markTaskDoneResult, error) {
		return markTaskDone(getter(), args.Detail), nil
	}
	t, err := functiontool.New(functiontool.Config{
		Name:        "mark_task_done",
		Description: markTaskDoneDescription,
	}, handler)
	if err != nil {
		panic("agent: NewMarkTaskDoneTool: " + err.Error())
	}
	return t
}

// markTaskDoneRepeatStatus is what a second (or fifth, or ninth)
// mark_task_done in one turn gets told. Exported to the test as a
// constant rather than asserted by substring because the whole value of
// the string is its content: it has to say the call did nothing, say why
// repeating cannot help, and point at the thing the model has probably
// left undone. A reworded version that drops one of those is a
// regression the test should catch.
const markTaskDoneRepeatStatus = "already recorded for this turn — the checkpoint fires once, after the turn ends, and your latest detail replaced the previous one. Calling this again cannot do anything further. If you have not yet answered what was asked of you, answer it now; if you are done, end the turn with your reply."

// markTaskDone is the mark_task_done tool body, split out from the
// closure in NewMarkTaskDoneTool so it can be exercised without
// standing up an ADK ToolContext.
//
// The repeat branch is the fix for a loop the watchdog structurally
// could not see (session 01a03f1e-e215-7acd-81a9-6e4654d91325): asked a
// question after an incident was already closed, the agent called
// mark_task_done nine times in a single invocation, each with a
// reworded one-paragraph detail about the same finished work, and
// stopped only when an operator interrupted. Every loop detector keys
// on (name, canonicalArgs) and the rewording changed the hash every
// time, so watchdog=enforce reported tripped: false throughout.
//
// The flag is idempotent — the checkpoint fires exactly once between
// turns and the newest detail wins — so the second call genuinely
// accomplished nothing. What kept the loop alive was being told
// "acknowledged" anyway, which reads as progress. Saying what actually
// happened is the fix, and it belongs here rather than in a behavioral
// detector: a tool that cannot do anything the second time in a turn
// should be the one to say so.
//
// Not an error. The model did nothing illegal, and an error would read
// as "the task did not get marked" — an invitation to retry, which is
// the loop again.
//
// A nil agent is the pre-registration race NewMarkTaskDoneTool
// documents; it stays a successful no-op, and deliberately does NOT
// set no_op (#907). The two look alike and are opposites: the repeat
// branch means "nothing needed doing", while an unbound agent means
// something needed doing and silently did not get done. Routing a
// runtime wiring fault into a Critical loop signal would halt the
// agent for the runtime's mistake, and hand the model guidance
// ("stop calling them") that is exactly backwards — no checkpoint was
// ever recorded, and the model is the only party still trying.
func markTaskDone(a *Agent, detail string) markTaskDoneResult {
	if a == nil {
		return markTaskDoneResult{Status: "acknowledged (no-op: agent not yet bound)"}
	}
	a.mu.Lock()
	repeat := a.checkpointRequested
	a.checkpointRequested = true
	a.pendingCheckpointNote = detail
	a.mu.Unlock()
	if repeat {
		return markTaskDoneResult{Status: markTaskDoneRepeatStatus, NoOp: true}
	}
	return markTaskDoneResult{Status: "acknowledged"}
}

// CheckpointIfRequested is the post-turn hook complement to
// runPendingCleanups. Called from wrapWithCleanup after a Run
// iterator drains. Promotes the in-turn flag (set by the
// mark_task_done tool handler) into a pending flag the NEXT Run
// drains; also fires the heuristic check.
//
// We don't fire Checkpoint inline here because the cleanup
// callback runs after the iterator drains but before the caller
// can react — running an LLM call would block the caller for
// seconds without any way to surface "compacting…" feedback.
// Promoting to a pending flag instead lets the caller decide
// when to start the next turn, which is when the host (TUI /
// REPL) can render a "compacting between turns…" indicator.
func (a *Agent) maybeMarkCheckpointPending() {
	if a == nil || a.checkpointer == nil {
		return
	}
	a.mu.Lock()
	if a.checkpointRequested {
		a.checkpointPending = true
		a.checkpointRequested = false
	}
	a.mu.Unlock()
	if a.checkpointer.ShouldCheckpoint(context.Background(), a) {
		a.mu.Lock()
		a.checkpointPending = true
		a.mu.Unlock()
	}
}

// runPendingCheckpoint fires Checkpoint when the prior turn's
// post-hook flagged a checkpoint as pending. Sibling to
// runPendingCompaction — both are pre-turn drains called from
// Agent.Run before the inbox + alert drains. Errors are logged-
// and-swallowed; the flag is cleared either way so we don't
// retry-loop on a persistent failure.
func (a *Agent) runPendingCheckpoint(ctx context.Context) {
	if a == nil || a.checkpointer == nil {
		return
	}
	a.mu.Lock()
	pending := a.checkpointPending
	note := a.pendingCheckpointNote
	a.checkpointPending = false
	a.pendingCheckpointNote = ""
	a.mu.Unlock()
	if !pending {
		return
	}
	if _, err := a.Checkpoint(ctx, note); err != nil {
		// Don't fail the turn — the operator can /done manually if it
		// persistently fails. The flag is already cleared above so we
		// don't loop. Surface the failure so it isn't silent (#356):
		// a checkpoint drops a task boundary the operator expected,
		// so a swallowed failure is materially misleading.
		log.Printf("agent: pending checkpoint failed: %v", err)
		// The daemon log reaches nobody who is attached, so also write
		// the durable row (#908). No backoff counters to report here —
		// the checkpoint path has none; the cleared flag is what stops
		// it looping.
		a.recordContextReductionFailure(attach.ContextReductionCheckpoint, err, 0, 0)
	}
}
