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
	"fmt"
	"strings"
	"time"

	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
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
type markTaskDoneArgs struct {
	Detail string `json:"detail" jsonschema:"one-paragraph completion summary: what was the task and what's the outcome. The checkpointer will fold this into a richer handover record on the next turn."`
}

type markTaskDoneResult struct {
	Status string `json:"status"`
}

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
// wired via WithCheckpointer.
func NewMarkTaskDoneTool(getter func() *Agent) tool.Tool {
	handler := func(_ tool.Context, args markTaskDoneArgs) (markTaskDoneResult, error) {
		a := getter()
		if a == nil {
			// Pre-registration race or test stub. Don't error —
			// the model interpreting the tool's contract should
			// still see the tool as successful.
			return markTaskDoneResult{Status: "acknowledged (no-op: agent not yet bound)"}, nil
		}
		a.mu.Lock()
		a.checkpointRequested = true
		a.pendingCheckpointNote = args.Detail
		a.mu.Unlock()
		return markTaskDoneResult{Status: "acknowledged"}, nil
	}
	t, err := functiontool.New(functiontool.Config{
		Name:        "mark_task_done",
		Description: "Call this when you have completed a coherent task and the conversation is about to shift to a new task (or end). The detail argument is your one-paragraph summary of what was done. After this turn finishes, the runtime will fold this detail into a richer handover record and slice the prior conversation from future turns — so the next task starts with a clean context window. Use this generously at natural task boundaries (after shipping a feature, finishing a code review, completing a debugging session). Do NOT call this mid-task or for partial progress.",
	}, handler)
	if err != nil {
		panic("agent: NewMarkTaskDoneTool: " + err.Error())
	}
	return t
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
		// Don't fail the turn — the operator can /done manually
		// if it persistently fails. The flag is already cleared
		// above so we don't loop.
		_ = err
		// Best-effort log so the failure isn't completely silent.
		fmt.Fprintf(devNullForLog{}, "agent: pending checkpoint failed: %v\n", err)
	}
}

// devNullForLog is a placeholder writer the failed-checkpoint log
// goes to today. Wiring a real *log.Logger or eventlog write
// would be a follow-up; for now we don't want to litter stderr
// with mid-turn diagnostic noise that the operator can't act on.
// Replace with the agent-level logger when one lands.
type devNullForLog struct{}

func (devNullForLog) Write(p []byte) (int, error) { return len(p), nil }
