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

package agent

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"google.golang.org/adk/v2/session"

	"github.com/go-steer/core-agent/v3/pkg/agent/internal/subsession"
	"github.com/go-steer/core-agent/v3/pkg/models"
	"github.com/go-steer/core-agent/v3/pkg/permissions"
)

type queuedServiceEvent struct {
	ctx      context.Context
	svc      session.Service
	ev       *session.Event
	attempts int
}

type subagentSessionEventDeferrer func(writeCtx context.Context, svc session.Service, appName, userID, sessionID string, ev *session.Event) bool

type (
	subagentSessionEventsKey struct{}
	currentTurnAgentKey      struct{}
)

// withTurnAgent installs a as the root running agent when none is present yet
// (WithRunningAgent is write-once so nested background subagents keep billing
// side usage to the root session) and unconditionally records a as the
// innermost turn's *Agent so EnqueueOrAppendSessionEvent matches nested
// (*Agent).Run invocations such as background subagents (#1571).
func withTurnAgent(ctx context.Context, a *Agent) context.Context {
	if a == nil {
		return ctx
	}
	ctx = WithRunningAgent(ctx, a)
	return context.WithValue(ctx, currentTurnAgentKey{}, a)
}

func currentTurnAgentFrom(ctx context.Context) *Agent {
	if ctx == nil {
		return nil
	}
	if a, ok := ctx.Value(currentTurnAgentKey{}).(*Agent); ok && a != nil {
		return a
	}
	return FromContext(ctx)
}

func subagentSessionEventsFrom(ctx context.Context) subagentSessionEventDeferrer {
	if ctx == nil {
		return nil
	}
	fn, _ := ctx.Value(subagentSessionEventsKey{}).(subagentSessionEventDeferrer)
	return fn
}

func detachWriteContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return context.WithoutCancel(ctx)
}

// EnqueueOrAppendSessionEvent records an out-of-band audit or effect event on
// (appName, userID, sessionID) in svc without racing an in-flight ADK
// runner.Run turn on that session (#565, #1571).
//
// Database-backed session.Service implementations check optimistic concurrency
// against the session handle ADK's Runner captured before dispatching tool
// calls: an inline Get-then-AppendEvent from inside a tool handler advances
// storage UpdateTime and causes the Runner's subsequent FunctionResponse append
// to fail with a stale-session error. When ctx belongs to an active (*Agent).Run,
// (*Agent).RunWithContents, synchronous NewSubagentTool, or (*Agent).RunSubtask
// turn for the same (appName, userID, sessionID), the event is queued and
// flushed once the turn's runner stream has unwound. Cancellation is detached
// via context.WithoutCancel so caller cancellation cannot drop a completed
// side-effect record while context values (auth.Caller, auth.ProxyBy, eventlog
// entry metadata) survive into EventlogMetadataExtractor.
func EnqueueOrAppendSessionEvent(
	ctx context.Context,
	svc session.Service,
	appName, userID, sessionID string,
	ev *session.Event,
) error {
	if svc == nil || ev == nil || sessionID == "" {
		return nil
	}
	if appName == "" {
		appName = DefaultAppName
	}
	if userID == "" {
		userID = defaultUserID
	}
	writeCtx := detachWriteContext(ctx)
	if fn := subagentSessionEventsFrom(ctx); fn != nil && fn(writeCtx, svc, appName, userID, sessionID, ev) {
		return nil
	}
	if a := currentTurnAgentFrom(ctx); a != nil && a.matchesSession(appName, userID, sessionID) {
		target := a.resolveServiceEventTarget(svc)
		if target == svc && a.eventLog != nil && a.eventLog.Service == svc {
			a.queueOutOfBandEventWithContext(writeCtx, ev)
			return nil
		}
		a.queueServiceEvent(writeCtx, target, ev)
		return nil
	}
	return appendSessionAudit(writeCtx, svc, appName, userID, sessionID, ev)
}

func (a *Agent) matchesSession(appName, userID, sessionID string) bool {
	return a != nil && a.appName == appName && a.userID == userID && a.sessionID == sessionID
}

// resolveServiceEventTarget routes out-of-band events through a.sessionService
// when it wraps svc via subsession.BranchInjectingService (as background
// subagents do in pkg/agent/background), so deferred audit events pick up the
// subagent's Branch label on flush.
func (a *Agent) resolveServiceEventTarget(svc session.Service) session.Service {
	if a == nil || svc == nil || a.sessionService == nil || a.sessionService == svc {
		return svc
	}
	for cur := a.sessionService; cur != nil; {
		bis, ok := cur.(*subsession.BranchInjectingService)
		if !ok {
			return svc
		}
		if bis.Inner == svc {
			return a.sessionService
		}
		cur = bis.Inner
	}
	return svc
}

func (a *Agent) queueServiceEvent(ctx context.Context, svc session.Service, ev *session.Event) {
	if a == nil || svc == nil || ev == nil {
		return
	}
	a.mu.Lock()
	a.pendingServiceEvents = append(a.pendingServiceEvents, queuedServiceEvent{
		ctx: detachWriteContext(ctx),
		svc: svc,
		ev:  ev,
	})
	a.mu.Unlock()
	if !a.turnInFlight() {
		a.drainServiceEvents()
	}
}

func (a *Agent) drainServiceEvents() {
	if a == nil {
		return
	}
	a.outOfBandDrainMu.Lock()
	defer a.outOfBandDrainMu.Unlock()
	a.mu.Lock()
	pending := a.pendingServiceEvents
	a.pendingServiceEvents = nil
	a.mu.Unlock()
	if len(pending) == 0 {
		return
	}
	for i := range pending {
		err := appendSessionAudit(detachWriteContext(pending[i].ctx), pending[i].svc, a.appName, a.userID, a.sessionID, pending[i].ev)
		if err == nil || isDuplicateADKEventError(err) {
			continue
		}
		pending[i].attempts++
		if pending[i].attempts < maxOutOfBandEventAttempts {
			a.requeueServiceEvents(pending[i:])
			slog.Warn("core-agent: out-of-band session event not written; will retry", "err", err, "session", a.sessionID)
			return
		}
		slog.Error("core-agent: out-of-band session event dropped after retries", "err", err, "author", pending[i].ev.Author, "session", a.sessionID)
	}
}

func (a *Agent) requeueServiceEvents(failed []queuedServiceEvent) {
	if a == nil || len(failed) == 0 {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.pendingServiceEvents) == 0 {
		a.pendingServiceEvents = append([]queuedServiceEvent(nil), failed...)
		return
	}
	merged := make([]queuedServiceEvent, 0, len(failed)+len(a.pendingServiceEvents))
	merged = append(merged, failed...)
	merged = append(merged, a.pendingServiceEvents...)
	a.pendingServiceEvents = merged
}

func (a *Agent) prepareSubagentTurn(
	ctx context.Context,
	svc session.Service,
	branch, appName, userID, sessionID, reqText string,
	gate *permissions.Gate,
) (context.Context, string, func(error)) {
	ctx, flushEvents := withSubagentSessionEvents(ctx, branch, appName, userID, sessionID)
	ctx, reqText, finishMem := a.prepareSubagentMemory(ctx, svc, appName, userID, sessionID, reqText, gate)
	return ctx, reqText, func(turnErr error) {
		flushEvents()
		finishMem(turnErr)
	}
}

func prepareSubtaskContext(
	ctx context.Context,
	maxWallclock time.Duration,
	branch, appName, userID, sessionID string,
) (context.Context, *models.PriorSuccess, func()) {
	subCtx, cancel := context.WithTimeout(ctx, maxWallclock)
	served := models.NewPriorSuccess()
	subCtx = models.WithPriorSuccess(subCtx, served)
	subCtx, flush := withSubagentSessionEvents(subCtx, branch, appName, userID, sessionID)
	return subCtx, served, func() {
		flush()
		cancel()
	}
}

func (a *Agent) prepareRunWithContentsContext(
	runCtx context.Context,
	preflight costPreflightCheck,
	sessionID string,
) (context.Context, func()) {
	runCtx = withCostPreflight(runCtx, preflight)
	runCtx = withTurnStepGate(runCtx, a.agentName, a.maxSteps, a.finalReport)
	return withSubagentSessionEvents(runCtx, "", a.appName, a.userID, sessionID)
}

// advanceEventTimestampAfter ensures an out-of-band event flushed after a turn
// lands strictly after last at microsecond resolution (matching ADK database
// session optimistic-concurrency and DATETIME(6) storage precision). Clamping
// to last instead of last+1us gives multiple queued events identical timestamps
// so ADK's "ORDER BY timestamp DESC, id DESC" permutes FIFO order by UUID.
func advanceEventTimestampAfter(ev *session.Event, last time.Time) {
	if ev == nil || last.IsZero() {
		return
	}
	if ev.Timestamp.UnixMicro() <= last.UnixMicro() {
		ev.Timestamp = last.Add(time.Microsecond)
	}
}

type pendingSubagentEvent struct {
	ctx context.Context
	svc session.Service
	ev  *session.Event
}

func withSubagentSessionEvents(
	ctx context.Context,
	branch, appName, userID, sessionID string,
) (context.Context, func()) {
	prev := subagentSessionEventsFrom(ctx)
	var (
		drainMu sync.Mutex
		mu      sync.Mutex
		closed  bool
		pending []pendingSubagentEvent
	)
	fn := func(writeCtx context.Context, svc session.Service, aName, uID, sID string, ev *session.Event) bool {
		if aName != appName || uID != userID || sID != sessionID {
			if prev != nil {
				return prev(writeCtx, svc, aName, uID, sID, ev)
			}
			return false
		}
		if ev != nil && ev.Branch == "" && branch != "" {
			ev.Branch = branch
		}
		wctx := detachWriteContext(writeCtx)
		mu.Lock()
		if !closed {
			pending = append(pending, pendingSubagentEvent{ctx: wctx, svc: svc, ev: ev})
			mu.Unlock()
			return true
		}
		mu.Unlock()
		drainMu.Lock()
		defer drainMu.Unlock()
		if err := appendSessionAudit(wctx, svc, appName, userID, sessionID, ev); err != nil && !isDuplicateADKEventError(err) {
			slog.Warn("core-agent: late subagent session event failed", "err", err, "session", sessionID)
		}
		return true
	}
	flush := func() {
		drainMu.Lock()
		defer drainMu.Unlock()
		mu.Lock()
		closed = true
		batch := pending
		pending = nil
		mu.Unlock()
		for _, item := range batch {
			if err := appendSessionAudit(item.ctx, item.svc, appName, userID, sessionID, item.ev); err != nil && !isDuplicateADKEventError(err) {
				slog.Warn("core-agent: subagent session event failed", "err", err, "session", sessionID)
			}
		}
	}
	return context.WithValue(ctx, subagentSessionEventsKey{}, subagentSessionEventDeferrer(fn)), flush
}
