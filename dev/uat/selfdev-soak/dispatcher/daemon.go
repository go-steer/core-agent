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

package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/go-steer/core-agent/v2/internal/attachclient"
	"github.com/go-steer/core-agent/v2/pkg/attach"
)

// daemonClient is the dispatcher's view of the core-agent daemon: create
// a session, hand it the task, and watch it until it goes idle.
type daemonClient struct {
	c   *attachclient.Client
	log *slog.Logger

	// settle is how long a session must stay quiet after its last turn
	// ended before it counts as idle. auto_continue can start another
	// turn right after one ends, so "a turn ended" is not "the session is
	// done".
	settle time.Duration
	// startTimeout bounds how long an injected session may sit idle
	// without ever ending a turn. A turn the agent refuses up front (a
	// halted session) puts no frame on the wire at all, so without this
	// the dispatcher would wait for the session timeout instead.
	startTimeout time.Duration
	// reconnect is the pause before re-opening a dropped event stream.
	reconnect time.Duration
}

// createSession opens a fresh session owned by the dispatcher's identity
// and returns its qualified path ("/sessions/<app>/<sid>") and ID.
func (d *daemonClient) createSession(ctx context.Context) (string, string, error) {
	resp, err := d.c.NewSession(ctx)
	if err != nil {
		return "", "", fmt.Errorf("POST /sessions: %w", err)
	}
	if resp.SessionID == "" || resp.AppName == "" {
		return "", "", fmt.Errorf("POST /sessions returned no session (app %q, id %q)", resp.AppName, resp.SessionID)
	}
	return "/sessions/" + url.PathEscape(resp.AppName) + "/" + url.PathEscape(resp.SessionID), resp.SessionID, nil
}

// injectTask delivers the issue's task to the session.
//
// This is the hook for P1's standing-task flag (decision 15). Until the
// attach protocol has it, the task is an ordinary waking inject, which
// decision 19 says is enough for A7: one small issue, unlikely to reach a
// compaction. When P1 lands, the flag is set here and nowhere else. No
// task_bytes: the whole message counts as the task, because the rig's
// instructions after the issue text are part of what the approver should
// judge calls against, and the dispatcher is the listed task_from caller.
func (d *daemonClient) injectTask(ctx context.Context, sessionPath, task string) error {
	return d.c.Inject(ctx, sessionPath, task)
}

// sessionEnd is how a watched session finished.
type sessionEnd struct {
	// Trips are guardrail trips seen in the session, halting or per-turn,
	// as "guardrail: reason". Any trip stops the issue: A7's "ended by
	// work" excludes a ceiling or a halt.
	Trips []string
	// TurnError is set when the session's last turn ended in an error.
	TurnError *attach.TurnError
	// NeverRan is set when the session went idle without ending a turn.
	NeverRan bool
	// TimedOut is set when the session outlived the session timeout.
	TimedOut bool
}

// stopReason renders why the issue stops, or "" when the session ended
// by finishing its work.
func (e sessionEnd) stopReason() string {
	switch {
	case len(e.Trips) > 0:
		return "a guardrail tripped: " + strings.Join(e.Trips, "; ")
	case e.TimedOut:
		return "the session was still running at the dispatcher's session timeout"
	case e.TurnError != nil:
		return fmt.Sprintf("the session's last turn ended in an error (%s): %s", e.TurnError.Kind, e.TurnError.Message)
	case e.NeverRan:
		return "the session went idle without ending a turn (a halted session refuses turns without an event)"
	}
	return ""
}

// watcher folds a session's event stream into what the dispatcher needs:
// did a turn end, how, and did a guardrail trip. Pure, so tests drive it
// frame by frame.
type watcher struct {
	sawTerminal bool
	// sawActivity is set by evidence that a turn ran: a non-idle
	// status-update, or a model-authored row in the eventlog. Unlike
	// turn-complete, the rows survive a reconnect.
	sawActivity bool
	// reconnected is set once the stream has been re-opened (or the
	// watch resumed after a dispatcher restart). The live-only
	// turn-complete may have been missed in the gap, so from then on
	// evidence of activity plus a settled idle status is enough.
	reconnected bool
	// agentsBusy is set while any background subagent of the session is
	// running or deferred. /status reports idle whenever the PARENT has
	// no turn in flight, so an async subagent (the self-recipe's reviewer
	// runs up to 30 minutes) is invisible there; when it finishes, it
	// wakes the parent for another turn that may commit more.
	agentsBusy bool
	// agentsSig fingerprints the subagent list; a change (one finishing)
	// counts as activity, so the parent turn it triggers is waited for.
	agentsSig    string
	lastActivity time.Time
	lastSeq      int64
	end          sessionEnd
	seenTrips    map[string]bool
}

func newWatcher(now time.Time) *watcher {
	return &watcher{lastActivity: now, seenTrips: map[string]bool{}}
}

// observe records one frame at time now.
func (w *watcher) observe(f attach.Frame, now time.Time) {
	switch f.Type {
	case "", attach.EventAgent:
		w.observeRow(f, now)
	case attach.EventTurnComplete:
		w.sawTerminal, w.end.TurnError, w.lastActivity = true, nil, now
	case attach.EventTurnError:
		if te, ok := f.TypedData.(*attach.TurnError); ok {
			w.turnError(*te, now)
		}
	case attach.EventGuardrailTrip:
		if gt, ok := f.TypedData.(*attach.GuardrailTrip); ok {
			w.trip(*gt)
		}
	case attach.EventStatusUpdate:
		if su, ok := f.TypedData.(*attach.StatusUpdate); ok && su.TurnState != attach.TurnStateIdle {
			w.lastActivity, w.sawActivity = now, true
		}
	}
}

// observeRow reads the durable rows (#1258) off a legacy `agent` frame.
// They are what a reconnect replays, where the typed frames are gone.
func (w *watcher) observeRow(f attach.Frame, now time.Time) {
	if f.Seq > w.lastSeq {
		w.lastSeq = f.Seq
	}
	if f.Event == nil {
		return
	}
	if gt, ok := attach.GuardrailHaltRow(f.Event); ok {
		gt.EventID = f.Event.ID
		w.trip(gt)
		return
	}
	if gt, ok := attach.GuardrailTurnTrip(f.Event); ok {
		gt.EventID = f.Event.ID
		w.trip(gt)
		return
	}
	if te, _, ok := attach.TurnErrorRow(f.Event); ok {
		te.EventID = f.Event.ID
		w.turnError(te, now)
		return
	}
	w.lastActivity = now
	if f.Event.Content != nil && f.Event.Author != "user" {
		// The model produced output after any earlier turn error, so that
		// error was not the session's last word. Without this, replaying
		// from seq 0 on a resume would stop an issue over a transient
		// error two turns back that a later turn recovered from.
		w.sawActivity, w.end.TurnError = true, nil
	}
}

func (w *watcher) turnError(te attach.TurnError, now time.Time) {
	w.sawTerminal, w.lastActivity = true, now
	w.end.TurnError = &te
}

// trip records a guardrail trip once, whether it arrived as the typed
// frame, the durable row, or both (they share EventID).
func (w *watcher) trip(gt attach.GuardrailTrip) {
	key := gt.EventID
	if key == "" {
		key = gt.Guardrail + "\x00" + gt.Reason
	}
	if w.seenTrips[key] {
		return
	}
	w.seenTrips[key] = true
	w.end.Trips = append(w.end.Trips, gt.Guardrail+": "+gt.Reason)
}

// done reports whether the session is finished, given its current
// status, at time now. started is when the task was injected.
// noteAgents records the session's background subagents at time now.
func (w *watcher) noteAgents(agents []attach.AgentInfo, now time.Time) {
	busy := false
	var sig strings.Builder
	for _, a := range agents {
		if a.Status == attach.AgentStatusRunning || a.Status == attach.AgentStatusDeferred {
			busy = true
		}
		sig.WriteString(a.ID + "=" + a.Status + ";")
	}
	if busy || sig.String() != w.agentsSig {
		w.lastActivity = now
	}
	w.agentsBusy, w.agentsSig = busy, sig.String()
}

func (w *watcher) done(st attach.StatusInfo, now, started time.Time, settle, startTimeout time.Duration) bool {
	idle := st.State == attach.AgentStateIdle && !st.TurnInFlight && !w.agentsBusy
	if !idle {
		return false
	}
	quiet := now.Sub(w.lastActivity)
	switch {
	case len(w.end.Trips) > 0, w.sawTerminal:
		return quiet >= settle
	case w.sawActivity && (w.reconnected || quiet >= startTimeout):
		// A turn ran but its turn-complete never reached us: lost in a
		// reconnect gap, or on a stream that died without closing. The
		// idle status, held for the settle window, decides.
		return quiet >= settle
	case now.Sub(started) >= startTimeout && quiet >= startTimeout:
		w.end.NeverRan = true
		return true
	}
	return false
}

// runTask opens the session's event stream, injects the task (when task
// is non-empty — a resumed watch after a dispatcher restart passes ""),
// and blocks until the session is done or timeout passes.
func (d *daemonClient) runTask(ctx context.Context, sessionPath, task string, timeout time.Duration) (sessionEnd, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	started := time.Now()
	w := newWatcher(started)
	// Subscribe BEFORE the inject: turn-complete is a live-only frame, and
	// a short turn can finish before a stream opened afterwards connects.
	frames, err := d.c.Stream(ctx, sessionPath, 0)
	if err != nil {
		return sessionEnd{}, fmt.Errorf("open event stream: %w", err)
	}
	if task != "" {
		if err := d.injectTask(ctx, sessionPath, task); err != nil {
			return sessionEnd{}, fmt.Errorf("inject task: %w", err)
		}
	} else {
		w.reconnected = true // resumed: the replay and the status poll decide
	}
	return d.watch(ctx, sessionPath, frames, w, started, timeout)
}

func (d *daemonClient) watch(ctx context.Context, sessionPath string, frames <-chan attach.Frame, w *watcher, started time.Time, timeout time.Duration) (sessionEnd, error) {
	tick := time.NewTicker(max(d.settle/2, 10*time.Millisecond))
	defer tick.Stop()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		select {
		case <-ctx.Done():
			return w.end, ctx.Err()
		case <-deadline.C:
			w.end.TimedOut = true
			return w.end, nil
		case f, ok := <-frames:
			if ok {
				w.observe(f, time.Now())
				continue
			}
			w.reconnected = true
			frames = d.reopen(ctx, sessionPath, w.lastSeq)
		case <-tick.C:
			st, err := d.c.Status(ctx, sessionPath)
			if err != nil {
				d.log.Warn("session status poll failed", "session", sessionPath, "err", err)
				continue
			}
			agents, err := d.c.Agents(ctx, sessionPath)
			if err != nil {
				d.log.Warn("session agents poll failed", "session", sessionPath, "err", err)
				continue // unknown counts as busy: never publish on a guess
			}
			w.noteAgents(agents, time.Now())
			if w.done(st, time.Now(), started, d.settle, d.startTimeout) {
				return w.end, nil
			}
		}
	}
}

// reopen re-subscribes after the stream dropped, resuming after the last
// durable row seen so trip and turn-error rows are replayed, not missed.
// While the daemon is unreachable it returns a channel that closes after
// the reconnect delay, so the loop retries without blocking the status
// poll.
func (d *daemonClient) reopen(ctx context.Context, sessionPath string, since int64) <-chan attach.Frame {
	select {
	case <-ctx.Done():
		return nil
	case <-time.After(d.reconnect):
	}
	frames, err := d.c.Stream(ctx, sessionPath, since)
	if err != nil {
		d.log.Warn("event stream reconnect failed", "session", sessionPath, "err", err)
		return d.retryLater(ctx)
	}
	return frames
}

// retryLater returns a channel that closes after the reconnect delay, so
// the watch loop's next receive sees a closed stream and calls reopen
// again — a retry loop driven by the same select, never a blocking one.
func (d *daemonClient) retryLater(ctx context.Context) <-chan attach.Frame {
	ch := make(chan attach.Frame)
	go func() {
		select {
		case <-ctx.Done():
		case <-time.After(d.reconnect):
		}
		close(ch)
	}()
	return ch
}

// interrupt cancels the session's turn and parks it. Best effort: used
// when the dispatcher abandons a session at its timeout.
func (d *daemonClient) interrupt(ctx context.Context, sessionPath string) {
	if _, err := d.c.Interrupt(ctx, sessionPath, true, true); err != nil {
		d.log.Warn("interrupt failed", "session", sessionPath, "err", err)
	}
}
