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

// The post-turn cleanup pipeline, as data rather than as statement order (#1389).
//
// Like preTurnSteps in preturn.go (#659), everything Run's deferred cleanup
// does after the runner's event stream has drained is a sequence of small steps
// whose order encodes specific correctness and concurrency invariants:
//   - releasing runCtx before any out-of-band session write (#359, #565)
//   - retiring the turn's cost-preflight binding before background subagents
//     keep billing under the parent's context (#1299)
//   - closing the OTel turn span before detached post-turn bookkeeping runs
//   - promoting checkpoint requests before checking compaction thresholds
//   - running boundary cost-ceiling and watchdog drains before flushing durable
//     out-of-band rows (#643)
//   - queuing interrupt (#565), refusal-storm (#1081), approver (#1175), and
//     turn-error (#1258) rows while cancelInFlight still holds off mid-turn
//     Compact/Checkpoint calls and prevents premature inline drains (#355, #1360)
//   - consuming guardrailHalt before both recordTurnError (which stamps cut_by,
//     #1258) and recordInvocationKind (which labels error.type, #818)
//   - clearing cancelInFlight immediately before the single drainOutOfBandEvents
//     flush so every durable row lands in the eventlog ahead of the terminal
//     SSE frame that names its EventID (#1258, #1360).
//
// postturn_test.go permutes postTurnSteps with transpose() to verify that
// reordering these steps reproduces the historical failure modes.

package agent

import (
	"context"
	"time"

	"github.com/go-steer/core-agent/v3/pkg/attach/wire"
)

// turnCleanup holds the per-turn state read and written by postTurnSteps.
// One value per Run cleanup, owned exclusively by the turn goroutine.
type turnCleanup struct {
	cancel           context.CancelFunc
	endPreflight     func()
	endTurnSpan      func(error)
	approverTurn     *turnApprover
	cancelGen        uint64
	promptID         string
	started          time.Time
	promptTokens     int
	completionTokens int
	turnErr          error
	guardrailHalt    string
	turnError        wire.TurnError
}

// postTurnStep is one ordered stage of the post-turn cleanup pipeline.
type postTurnStep struct {
	name     string
	enforces string
	run      func(a *Agent, tc *turnCleanup)
}

// postTurnSteps is the post-turn cleanup pipeline, in order.
var postTurnSteps = []postTurnStep{
	{
		name: "cancel-turn-ctx",
		enforces: "#359, #565: first. Always release the per-turn cancellable context once the " +
			"event stream has drained, preventing lostcancel leaks in long-lived daemons and " +
			"ensuring nothing under runCtx races the out-of-band session writes below.",
		run: func(_ *Agent, tc *turnCleanup) {
			if tc.cancel != nil {
				tc.cancel()
			}
		},
	},
	{
		name: "end-cost-preflight",
		enforces: "#1299: ends the turn's cost-preflight generation binding immediately after " +
			"the stream drains so background subagents that inherited the turn's context " +
			"values do not trip the idle parent's per-turn ceiling after the turn is over.",
		run: func(_ *Agent, tc *turnCleanup) {
			if tc.endPreflight != nil {
				tc.endPreflight()
			}
		},
	},
	{
		name: "end-turn-span",
		enforces: "Closes the OTel turn span after every child span ADK and the tool layer " +
			"opened has finished, and before detached post-turn bookkeeping runs.",
		run: func(_ *Agent, tc *turnCleanup) {
			if tc.endTurnSpan != nil {
				tc.endTurnSpan(tc.turnErr)
			}
		},
	},
	{
		name: "mark-checkpoint-pending",
		enforces: "Promotes checkpointRequested to checkpointPending before mark-compaction-pending " +
			"evaluates the context window.",
		run: func(a *Agent, _ *turnCleanup) { a.maybeMarkCheckpointPending() },
	},
	{
		name: "mark-compaction-pending",
		enforces: "Evaluates the compactor threshold after mark-checkpoint-pending so the next " +
			"Run call's pre-turn pipeline sees both flags.",
		run: func(a *Agent, _ *turnCleanup) { a.maybeMarkCompactionPending() },
	},
	{
		name: "enforce-cost-ceiling",
		enforces: "#145, #643: evaluates post-turn spend against per-turn and per-session caps " +
			"BEFORE clear-cancel-in-flight and drain-out-of-band-events, so any durable trip " +
			"row is queued and flushed in this turn's cleanup rather than stranded.",
		run: func(a *Agent, _ *turnCleanup) { a.maybeEnforceCostCeiling(false) },
	},
	{
		name: "drain-watchdog-alerts",
		enforces: "#123, #159, #623, #643: drains watchdog alerts and trips enforce-mode Critical " +
			"signals BEFORE clear-cancel-in-flight and drain-out-of-band-events, so any durable " +
			"halt or turn-trip row is flushed by this turn's cleanup.",
		run: func(a *Agent, _ *turnCleanup) { a.drainWatchdogAlerts(false) },
	},
	{
		name: "drain-interrupt-audit",
		enforces: "#565: queues the operator /interrupt audit row after runCtx is cancelled and " +
			"before drain-out-of-band-events flushes the queue.",
		run: func(a *Agent, _ *turnCleanup) { a.drainInterruptAudit() },
	},
	{
		name: "drain-refusal-storm-audit",
		enforces: "#1081: queues the gate/refusal-storm audit row in the safe post-stream window " +
			"before drain-out-of-band-events flushes the queue.",
		run: func(a *Agent, _ *turnCleanup) { a.drainRefusalStormAudit() },
	},
	{
		name: "drain-approver-audits",
		enforces: "#1175 decision 10: queues the auto-mode approver's audit rows before " +
			"drain-out-of-band-events flushes the queue.",
		run: func(_ *Agent, tc *turnCleanup) { tc.approverTurn.drainAudits() },
	},
	{
		name: "consume-guardrail-halt",
		enforces: "#818, #1258: consumes the per-turn guardrailHaltKind marker BEFORE " +
			"record-turn-error (which stamps cut_by on the durable turn-error row) and " +
			"BEFORE record-invocation-metric (which labels error.type with the guardrail).",
		run: func(a *Agent, tc *turnCleanup) {
			tc.guardrailHalt = a.consumeGuardrailHalt(tc.turnErr)
		},
	},
	{
		name: "record-turn-error",
		enforces: "#1258: after consume-guardrail-halt (which supplies cut_by) and before " +
			"clear-cancel-in-flight and drain-out-of-band-events, so the turn-error row " +
			"is queued while the turn is still marked in-flight and flushed ahead of the " +
			"terminal SSE frame.",
		run: func(a *Agent, tc *turnCleanup) {
			if tc.turnErr != nil {
				tc.turnError = a.recordTurnError(wire.ClassifyTurnError(tc.turnErr), tc.promptID, tc.guardrailHalt)
			}
		},
	},
	{
		name: "clear-cancel-in-flight",
		enforces: "#355, #1360: cleared AFTER every post-turn hook that queues out-of-band " +
			"rows and IMMEDIATELY BEFORE drain-out-of-band-events. Clearing earlier would " +
			"let queueOutOfBandEvent drain inline mid-cleanup and let concurrent Compact/" +
			"Checkpoint calls race post-turn writes; clearing later would leave turnInFlight " +
			"true during onTurnEnd and terminal SSE emission.",
		run: func(a *Agent, tc *turnCleanup) { a.clearCancelInFlight(tc.cancelGen) },
	},
	{
		name: "drain-out-of-band-events",
		enforces: "#643, #1258: flushes all queued guardrail, audit, and turn-error rows to " +
			"the eventlog AFTER every producer above and BEFORE emit-terminal-event, so the " +
			"durable rows are committed before the terminal SSE frame naming EventID goes out.",
		run: func(a *Agent, _ *turnCleanup) { a.drainOutOfBandEvents() },
	},
	{
		name: "run-turn-end-hook",
		enforces: "Runs the optional WithEventHook onTurnEnd callback after all session and " +
			"eventlog writes for the turn have settled, and before the terminal SSE frame.",
		run: func(a *Agent, _ *turnCleanup) {
			if a.onTurnEnd != nil {
				a.onTurnEnd()
			}
		},
	},
	{
		name: "record-invocation-metric",
		enforces: "#338, #818: records gen_ai.agent.invocation.duration using tc.guardrailHalt " +
			"captured by consume-guardrail-halt so a guardrail-cut turn is labelled with the " +
			"guardrail kind rather than bare canceled.",
		run: func(a *Agent, tc *turnCleanup) {
			if tc.guardrailHalt != "" {
				a.recordInvocationKind(time.Since(tc.started).Seconds(), tc.guardrailHalt)
			} else {
				a.recordInvocation(time.Since(tc.started).Seconds(), tc.turnErr)
			}
		},
	},
	{
		name: "emit-terminal-event",
		enforces: "Last: emits the single terminal turn-error or turn-complete frame followed " +
			"by status-update idle AFTER drain-out-of-band-events has persisted every row " +
			"the terminal frame references (#891, #1258).",
		run: func(a *Agent, tc *turnCleanup) {
			switch {
			case tc.turnErr != nil:
				a.emit(wire.EventTurnError, tc.turnError)
			default:
				a.emit(wire.EventTurnComplete, wire.TurnComplete{
					PromptID:  tc.promptID,
					Model:     a.modelName,
					TokensIn:  tc.promptTokens,
					TokensOut: tc.completionTokens,
					LatencyMs: time.Since(tc.started).Milliseconds(),
				})
			}
			a.emit(wire.EventStatusUpdate, wire.StatusUpdate{
				TurnState: wire.TurnStateIdle,
			})
		},
	},
}

// runPostTurn executes steps in order against tc.
//
// steps is a parameter rather than a direct read of postTurnSteps so
// postturn_test.go can run permuted pipelines to verify each ordering
// constraint.
func (a *Agent) runPostTurn(tc *turnCleanup, steps []postTurnStep) {
	for _, step := range steps {
		step.run(a, tc)
	}
}
