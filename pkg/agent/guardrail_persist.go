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

// Durable guardrail halt state (#643) — the agent side of the eventlog
// rows defined in pkg/attach/guardrail_events.go.
//
// Before this, a watchdog trip or a cost-ceiling halt lived only in the
// Agent struct. A crash, an OOM kill, or a pod roll started a fresh
// process with the backstop disarmed, so the runaway-loop → crash →
// restart cycle the #623–#627 train exists to break could repeat
// forever, each restart handing the loop a clean budget. #642 made both
// backstops default-on for unattended runs, which is exactly the
// deployment shape that gets restarted by a supervisor.
//
// Three moving parts:
//
//   - Writes. Trip and reset sites queue a row; drainOutOfBandEvents
//     flushes it. Queuing rather than writing inline is the #565 lesson:
//     an out-of-band Get-then-AppendEvent while the runner holds the
//     session bumps last_update_time and trips ADK's optimistic-
//     concurrency check, surfacing as an opaque stale-session error on
//     the operator's turn. The queue drains in windows where no turn is
//     in flight.
//
//   - Restore. Lazily, once per agent, at the top of the first Run —
//     NOT wired per construction site. Every path that can build an
//     agent over an existing session (multi-session resume, a daemon
//     restart against --session-db, an embedder) gets the property
//     without remembering to ask for it. A wiring step an embedder can
//     forget is the same shape of unenforced safety claim this
//     milestone is about.
//
//   - Config still wins. A restored halt is only applied where the
//     current process is actually configured to enforce it: no watchdog
//     halt under --watchdog=warn, no cost halt with no ceiling
//     configured. Restoring a backstop the operator has since turned
//     off would be the durable state overruling live config.
//
// Persistence requires an eventlog (WithEventLog). Without one there is
// nowhere to write and nothing to restore, and the in-memory behavior
// is exactly what it was before.

package agent

import (
	"context"
	"iter"
	"log/slog"

	"google.golang.org/adk/v2/session"

	"github.com/go-steer/core-agent/v3/pkg/attach"
	"github.com/go-steer/core-agent/v3/pkg/eventlog"
	"github.com/go-steer/core-agent/v3/pkg/watchdog"
)

// queueOutOfBandEvent enqueues a durable row the agent writes ABOUT a
// session rather than as part of a turn, and flushes it immediately when
// no turn is in flight — which is the common case for every writer:
// guardrail trips fire from the post-turn cleanup (after the stream
// drained and the run context was cancelled) or from the top-of-Run
// enforcement pass, resets arrive from an operator while the session is
// halted, and context-reduction failures (#908) come from the pre-turn
// drains, which run before the turn's cancel is registered. The queue is
// the fallback for the racy windows, such as a pre-emptive budget raise
// landing mid-turn.
//
// Named for the write window rather than for guardrails: the mechanism
// is "append a row without racing the runner's session handle", and the
// guardrail rows were only its first callers.
func (a *Agent) queueOutOfBandEvent(ev *session.Event) {
	if a == nil || ev == nil || a.eventLog == nil {
		return
	}
	a.mu.Lock()
	a.pendingOutOfBandEvents = append(a.pendingOutOfBandEvents, ev)
	a.mu.Unlock()
	if !a.turnInFlight() {
		a.drainOutOfBandEvents()
	}
}

// drainOutOfBandEvents writes every queued row (#643, #1360).
//
// Requests NumRecentEvents: 1 on Service.Get because AppendEvent only
// reads the session identity, updatedAt, and state map — never its
// hydrated event history. When Get or AppendEvent fails, any unwritten
// rows are prepended back onto pendingOutOfBandEvents and logged so the
// next drain window retries them rather than silently dropping a
// durable guardrail trip or audit row (#1360).
//
// Uses context.Background rather than a turn context: the post-turn
// cleanup runs with runCtx already cancelled, and a reset arrives on an
// HTTP request context whose client may hang up before the write lands.
// A row this one is written FOR must not be droppable by the caller it
// records.
//
// Serialized by outOfBandDrainMu across the take AND the writes: with
// only the take under a.mu, two concurrent drains could each take a
// batch and append them in the reverse of the order they were queued.
func (a *Agent) drainOutOfBandEvents() {
	if a == nil || a.eventLog == nil || a.eventLog.Service == nil {
		return
	}
	a.outOfBandDrainMu.Lock()
	defer a.outOfBandDrainMu.Unlock()
	a.mu.Lock()
	pending := a.pendingOutOfBandEvents
	a.pendingOutOfBandEvents = nil
	a.mu.Unlock()
	if len(pending) == 0 {
		return
	}
	ctx := context.Background()
	getResp, err := a.eventLog.Service.Get(ctx, &session.GetRequest{
		AppName:         a.appName,
		UserID:          a.userID,
		SessionID:       a.sessionID,
		NumRecentEvents: 1,
	})
	if err != nil || getResp == nil || getResp.Session == nil {
		a.requeueOutOfBandEvents(pending)
		a.logOutOfBandDrainError("get session", err)
		return
	}
	for i, ev := range pending {
		if err := a.eventLog.Service.AppendEvent(ctx, getResp.Session, ev); err != nil {
			a.requeueOutOfBandEvents(pending[i:])
			a.logOutOfBandDrainError("append event", err)
			return
		}
	}
}

// requeueOutOfBandEvents prepends failed events back onto
// pendingOutOfBandEvents so a transient Get or AppendEvent error
// preserves ordering and retries on the next drain window (#1360).
func (a *Agent) requeueOutOfBandEvents(failed []*session.Event) {
	if a == nil || len(failed) == 0 {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.pendingOutOfBandEvents) == 0 {
		a.pendingOutOfBandEvents = append([]*session.Event(nil), failed...)
		return
	}
	combined := make([]*session.Event, 0, len(failed)+len(a.pendingOutOfBandEvents))
	combined = append(combined, failed...)
	combined = append(combined, a.pendingOutOfBandEvents...)
	a.pendingOutOfBandEvents = combined
}

func (a *Agent) logOutOfBandDrainError(op string, err error) {
	slog.Warn("agent: out-of-band event drain failed; events re-queued for retry",
		"op", op,
		"session_id", a.sessionID,
		"err", err,
	)
}

// RecordGuardrailReset persists an operator's reset (#643) and is the
// audit trail #331 asked for — one row, not two, so there is nothing to
// keep in agreement. reset names the guardrails whose flag was cleared,
// budgetUSD the runway added, caller the authenticated identity (empty
// when the reset came from an unauthenticated in-process surface).
//
// Call it AFTER the state mutations, once per operator action. A reset
// that cleared nothing and added nothing writes nothing: a defensive
// reset against a healthy session is not an event.
func (a *Agent) RecordGuardrailReset(reset []string, budgetUSD float64, caller string) {
	if a == nil || (len(reset) == 0 && budgetUSD <= 0) {
		return
	}
	a.queueOutOfBandEvent(attach.NewGuardrailResetAuditEvent(caller, reset, budgetUSD))
}

// RestoreGuardrails folds this session's durable guardrail rows and
// applies the result: a halt that survived a restart is re-armed, and
// budget an operator granted before the restart is re-applied to the
// per-session ceiling.
//
// Idempotent and cheap to over-call — the fold applies at most once per
// agent, and Run calls it before its first pre-flight, so most callers
// never need it. Exposed for embedders that want the state restored
// (and any error surfaced) before accepting traffic rather than on the
// first turn.
//
// The latch is set on SUCCESS only, so a read that fails is retried on
// the next call rather than leaving the agent permanently unrestored.
// A sync.Once here would mean one transient database error at the wrong
// moment disarms the backstop for the whole life of the process — the
// exact failure this durability work exists to remove.
//
// Returns an error only when the session read fails. A session with no
// guardrail history restores nothing and reports success.
func (a *Agent) RestoreGuardrails(ctx context.Context) error {
	if a == nil || a.eventLog == nil || a.eventLog.Service == nil {
		return nil
	}
	a.mu.Lock()
	done := a.guardrailRestored
	a.mu.Unlock()
	if done {
		return nil
	}
	var st attach.GuardrailPersistedState
	if a.eventLog.Guardrail != nil {
		if _, err := a.eventLog.Service.Get(ctx, &session.GetRequest{
			AppName:         a.appName,
			UserID:          a.userID,
			SessionID:       a.sessionID,
			NumRecentEvents: 1,
		}); err != nil {
			return err
		}
		gs, needsLegacyFold, err := a.eventLog.Guardrail.LoadWithLegacyCheck(ctx, a.appName, a.userID, a.sessionID)
		if err != nil {
			return err
		}
		if needsLegacyFold {
			getResp, err := a.eventLog.Service.Get(ctx, &session.GetRequest{
				AppName:   a.appName,
				UserID:    a.userID,
				SessionID: a.sessionID,
			})
			if err != nil {
				return err
			}
			st = attach.FoldGuardrailEvents(eventSeq(getResp.Session))
			_ = a.eventLog.Guardrail.SaveState(ctx, eventlog.GuardrailState{
				AppName:            a.appName,
				UserID:             a.userID,
				SessionID:          a.sessionID,
				WatchdogTripped:    st.WatchdogTripped,
				WatchdogReason:     st.WatchdogReason,
				CostTripped:        st.CostTripped,
				CostReason:         st.CostReason,
				BudgetExtensionUSD: st.BudgetAddedUSD,
			})
		} else {
			st = attach.GuardrailPersistedState{
				WatchdogTripped: gs.WatchdogTripped,
				WatchdogReason:  gs.WatchdogReason,
				CostTripped:     gs.CostTripped,
				CostReason:      gs.CostReason,
				BudgetAddedUSD:  gs.BudgetExtensionUSD,
			}
		}
	} else {
		getResp, err := a.eventLog.Service.Get(ctx, &session.GetRequest{
			AppName:   a.appName,
			UserID:    a.userID,
			SessionID: a.sessionID,
		})
		if err != nil {
			return err
		}
		st = attach.FoldGuardrailEvents(eventSeq(getResp.Session))
	}

	a.mu.Lock()
	if a.guardrailRestored {
		// A concurrent caller won the race and already applied it.
		// Applying twice would double-count granted budget.
		a.mu.Unlock()
		return nil
	}
	a.guardrailRestored = true
	a.mu.Unlock()

	a.applyGuardrailState(st)
	return nil
}

// ensureGuardrailsRestored is the Run-path entry point. Swallows the
// read error: a turn must not fail because the durable state couldn't
// be read, and the in-memory backstops are still armed either way.
//
// Note the failure direction. An unreadable eventlog means a halt is
// NOT restored, so this fails OPEN. The alternative — refusing every
// turn when the guardrail history can't be read — turns a transient DB
// hiccup into a dead session, which is a worse outcome than the window
// this covers (a restart that lands between a trip and its read).
func (a *Agent) ensureGuardrailsRestored(ctx context.Context) {
	_ = a.RestoreGuardrails(ctx)
}

// applyGuardrailState installs folded state, subject to the current
// process's configuration. See the package comment: durable state
// restores a halt the operator hasn't cleared, it does not re-enable a
// backstop the operator has since turned off.
func (a *Agent) applyGuardrailState(st attach.GuardrailPersistedState) {
	a.mu.Lock()
	defer a.mu.Unlock()

	// Budget first: it is what makes a restored cost trip evaluable
	// against the bar the operator actually left in place. Only
	// meaningful where a per-session ceiling is configured — adding
	// runway to a disabled bound would silently ARM it, the same
	// refusal AddSessionCostBudget makes.
	if st.BudgetAddedUSD > 0 && a.costCeiling.MaxSessionUSD > 0 {
		a.costCeiling.MaxSessionUSD += st.BudgetAddedUSD
	}
	if st.WatchdogTripped && a.watchdogEnforce && a.watchdog != nil {
		a.watchdogTripped = true
		a.watchdogReason = st.WatchdogReason
		// Feedback (#159) has to survive the restart too, or the
		// durability this file provides is half a property: the halt
		// comes back, the operator clears it, and the fresh process
		// resumes a model that never learned why it was stopped —
		// straight back into the loop. The in-memory queue died with
		// the old process, so synthesize the observation from the
		// persisted reason. Signal name marks it as reconstructed
		// rather than freshly detected.
		if a.watchdogFeedback {
			a.watchdogPending = append(a.watchdogPending, watchdog.Alert{
				Signal:   "restored-watchdog-halt",
				Severity: watchdog.SeverityCritical,
				Guidance: "An earlier turn in this session was halted by the runaway-behavior watchdog, and an operator has since cleared the halt. The halting observation was: " + st.WatchdogReason + " Do not resume the call pattern that caused it — change your approach before calling any tool again.",
			})
		}
	}
	if st.CostTripped && a.costCeiling.active() {
		a.costCeilingExceeded = true
		a.costCeilingReason = st.CostReason
	}
}

// eventSeq adapts an ADK session's event collection to the iterator the
// fold takes.
func eventSeq(sess session.Session) iter.Seq[*session.Event] {
	return func(yield func(*session.Event) bool) {
		if sess == nil {
			return
		}
		for ev := range sess.Events().All() {
			if !yield(ev) {
				return
			}
		}
	}
}
