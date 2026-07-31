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

package compose

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"time"

	"google.golang.org/adk/session"

	"github.com/go-steer/core-agent/v2/pkg/agent"
	"github.com/go-steer/core-agent/v2/pkg/auth"
	"github.com/go-steer/core-agent/v2/pkg/eventlog"
)

// maybeAutoContinue implements the lazy-resume trigger of
// docs/auto-continue-design.md (#539 PR 1): called from
// ReproduceAgent for origin=="resumed" sessions when the feature is
// enabled. It classifies the committed tail, applies the freshness
// window, takes the session run lock for fleet mutual exclusion, and
// queues the synthesized continuation into the agent's inbox — the
// wake loop (started by the caller right after) drains it as the
// session's first turn.
//
// Lock staging note: v1 holds agent_run_lock across detection +
// injection only, not across the continuation turn itself (the turn
// runs asynchronously in the wake loop; holding a lease across it
// needs a turn-end hook this path doesn't have). The residual window
// — two shared-DB daemons both lazily resuming the same session
// after one's injection but before its turn commits — narrows with
// the boot scan's synchronous driving in PR 2. The design doc's
// implementation notes record this deviation.
//
// Every skip path returns silently or with one stderr line; resume
// itself must never fail because auto-continue couldn't run.

// autoContinueScanWindow bounds the tail read. A window this size is
// classification-safe: the interrupted tail is by definition the last
// conversational event, and only annotation rows (audit, checkpoints,
// notes) can trail it — never dozens of them.
const autoContinueScanWindow = 128

// Crash-loop guard thresholds (design doc §breaker). Constants in v1;
// knobs wait for a demonstrated need.
//
//   - breaker: >= breakerBootThreshold boots inside breakerWindow that
//     attempted continuations → this boot's scan stands down. Mostly a
//     fleet/multi-session rate limiter (the per-session guards below
//     handle the canonical single-poisoned-session loop; a rolling
//     fleet restart with legitimate continuations can trip it — the
//     fail-safe direction, cost one stderr line and one quiet boot).
//   - single retry: a session attempted inside breakerWindow is
//     skipped this boot.
//   - cumulative cap: a session attempted maxAttemptsPerSession times
//     inside attemptLookback is skipped until the log ages. This is
//     the bound the committed-note rule can't provide when a poisoned
//     continuation makes partial progress before killing the daemon —
//     each attempt commits fresh events, renewing both the freshness
//     window and the note-rule's tail, so only attempt COUNTING
//     terminates the sequence.
const (
	breakerBootThreshold  = 3
	breakerWindow         = 10 * time.Minute
	attemptLookback       = time.Hour
	maxAttemptsPerSession = 3
)

// AutoContinueBootScan is the boot-time trigger of
// docs/auto-continue-design.md (#539 PR 2): one pass over the
// persisted ACL rows that continues fresh interrupted sessions nobody
// re-touches (channel sessions). Candidates are found with bounded
// tail reads only — no agent construction for sessions that don't
// need it; the actual continuation happens by driving the registry's
// normal Lookup → resume path, so the PR 1 machinery (run lock,
// re-classification under the lock, injection) is shared verbatim.
//
// Guards, in order:
//   - crash-loop breaker: >= breakerBootThreshold recent boots with
//     attempted continuations → stand down this boot, loudly. A boot
//     whose continuations survive ages out of the window naturally.
//   - per-session single retry + cumulative cap: a session attempted
//     inside breakerWindow is skipped this boot, and one attempted
//     maxAttemptsPerSession times inside attemptLookback is skipped
//     until the log ages — see the constants block for why counting
//     is the only terminating bound.
//   - maxPerBoot (config agent.auto_continue.max_per_boot, default
//     10): oldest interruption first; the remainder is logged, not
//     silently dropped, and lazy touch still covers it.
//
// Runs synchronously; callers launch it on a goroutine after the
// attach listener is up. Every failure path logs and returns — a
// broken scan must never take the daemon down.
func AutoContinueBootScan(deps SessionFactoryDeps, maxPerBoot int) {
	// The scan drives full agent construction (resume) in this
	// goroutine; on the lazy path the same work runs under net/http's
	// per-connection recover. A panic on one poisoned session must
	// not take the daemon down — and must not bypass the write-ahead
	// boot record below.
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "core-agent: auto-continue boot scan: recovered from panic: %v\n", r)
		}
	}()
	h := deps.EventlogHandle
	if h == nil || h.Service == nil || deps.Registry == nil || deps.ACLStore == nil || !deps.AutoContinueEnabled {
		return
	}
	ctx := deps.DaemonCtx
	if maxPerBoot <= 0 {
		maxPerBoot = 10
	}

	boots, err := h.RecentBoots(ctx, time.Now().Add(-attemptLookback))
	if err != nil {
		fmt.Fprintf(os.Stderr, "core-agent: auto-continue boot scan: read boot log: %v\n", err)
		return
	}
	attemptedRecently := map[string]bool{} // inside breakerWindow: single-retry skip
	attemptCount := map[string]int{}       // inside attemptLookback: cumulative cap
	bootsWithAttempts := 0
	breakerCutoff := time.Now().Add(-breakerWindow)
	for _, b := range boots {
		for _, sid := range b.Attempted {
			attemptCount[sid]++
		}
		if !b.BootAt.Before(breakerCutoff) {
			if len(b.Attempted) > 0 {
				bootsWithAttempts++
			}
			for _, sid := range b.Attempted {
				attemptedRecently[sid] = true
			}
		}
	}
	if bootsWithAttempts >= breakerBootThreshold {
		fmt.Fprintf(os.Stderr, "core-agent: auto-continue BREAKER TRIPPED: %d boots attempted continuations within %s — standing down this boot (a continuation turn may be killing the daemon; sessions resume normally on touch)\n",
			bootsWithAttempts, breakerWindow)
		return
	}

	// Admin caller sees every ACL row; the identity is the scan's own,
	// so any Touch/audit side effects attribute correctly.
	rows, err := deps.ACLStore.ListVisibleTo(ctx, auth.Caller{Identity: agent.AutoContinueOriginator, Admin: true})
	if err != nil {
		fmt.Fprintf(os.Stderr, "core-agent: auto-continue boot scan: list sessions: %v\n", err)
		return
	}

	type candidate struct {
		app, sid      string
		interruptedAt time.Time
	}
	var candidates []candidate
	for _, row := range rows {
		if ctx.Err() != nil {
			return // daemon shutting down mid-scan
		}
		if attemptedRecently[row.SessionID] {
			continue
		}
		if attemptCount[row.SessionID] >= maxAttemptsPerSession {
			// Self-renewing poison: each attempt commits fresh events,
			// so freshness and the note rule never terminate it. Only
			// counting does. The session resumes normally on touch.
			continue
		}
		resp, err := h.Service.Get(ctx, &session.GetRequest{
			AppName:         row.AppName,
			UserID:          row.UserID,
			SessionID:       row.SessionID,
			NumRecentEvents: autoContinueScanWindow,
		})
		if err != nil || resp == nil || resp.Session == nil {
			continue
		}
		var events []*session.Event
		for ev := range resp.Session.Events().All() {
			events = append(events, ev)
		}
		interruptedAt, interrupted := agent.ClassifyInterruptedTail(events)
		if !interrupted {
			continue
		}
		if deps.AutoContinueFreshness > 0 && time.Since(interruptedAt) > deps.AutoContinueFreshness {
			continue
		}
		candidates = append(candidates, candidate{app: row.AppName, sid: row.SessionID, interruptedAt: interruptedAt})
	}
	if len(candidates) == 0 {
		if err := h.RecordBoot(ctx, time.Now(), nil); err != nil {
			fmt.Fprintf(os.Stderr, "core-agent: auto-continue boot scan: record boot: %v\n", err)
		}
		return
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].interruptedAt.Before(candidates[j].interruptedAt) })
	if len(candidates) > maxPerBoot {
		fmt.Fprintf(os.Stderr, "core-agent: auto-continue boot scan: %d interrupted sessions, continuing the %d oldest (max_per_boot); the rest resume on touch\n",
			len(candidates), maxPerBoot)
		candidates = candidates[:maxPerBoot]
	}

	// WRITE-AHEAD intent record, before any resume is driven: a
	// continuation (or resume-time agent construction) that kills the
	// daemon mid-loop must still count against the breaker and the
	// per-session guards on the next boot — a write-behind record
	// would leave both guards blind in exactly the fast-kill
	// scenarios they exist for. Failing to record intent aborts the
	// scan: never run unguarded.
	attempted := make([]string, 0, len(candidates))
	for _, c := range candidates {
		attempted = append(attempted, c.sid)
	}
	if err := h.RecordBoot(ctx, time.Now(), attempted); err != nil {
		fmt.Fprintf(os.Stderr, "core-agent: auto-continue boot scan: record boot intent: %v — aborting scan (guards must not be blind)\n", err)
		return
	}
	queued := 0
	for _, c := range candidates {
		// Lookup drives the normal miss → resume path; ReproduceAgent's
		// resumed-origin hook re-classifies under the run lock and
		// injects the continuation. Already-in-memory sessions (raced
		// by an operator touch) just hit the registry and are handled
		// by that touch's own resume.
		if _, err := deps.Registry.Lookup(ctx, c.app, c.sid); err != nil {
			fmt.Fprintf(os.Stderr, "core-agent: auto-continue boot scan: resume session %s: %v\n", c.sid, err)
			continue
		}
		queued++
	}
	if queued > 0 {
		fmt.Fprintf(os.Stderr, "core-agent: auto-continue boot scan: queued continuations for %d session(s)\n", queued)
	}
}

func maybeAutoContinue(deps SessionFactoryDeps, caller auth.Caller, sid string, ag *agent.Agent) {
	// Fleet exclusion first — the lock is one cheap DB write, and
	// classifying under it means two daemons can't both read the
	// same interrupted tail.
	lock, err := deps.EventlogHandle.AcquireLock(deps.DaemonCtx, "core-agent", caller.Identity, sid)
	if err != nil {
		if errors.Is(err, eventlog.ErrSessionLocked) {
			return // another daemon (or an autonomous run) owns the session right now
		}
		fmt.Fprintf(os.Stderr, "core-agent: session %s: auto-continue: acquire run lock: %v\n", sid, err)
		return
	}
	defer func() { _ = lock.Release() }()

	// Bounded read (same window rationale as tail repair): the
	// classifier only needs the last conversational event plus
	// whatever annotations trail it, and this runs synchronously on
	// the resume path while the touching HTTP request waits — a
	// full-session scan on a 100k-event session would break the
	// "resume stays fast" promise.
	resp, err := deps.EventlogHandle.Service.Get(deps.DaemonCtx, &session.GetRequest{
		AppName:         "core-agent",
		UserID:          caller.Identity,
		SessionID:       sid,
		NumRecentEvents: autoContinueScanWindow,
	})
	if err != nil || resp == nil || resp.Session == nil {
		if err != nil {
			fmt.Fprintf(os.Stderr, "core-agent: session %s: auto-continue: read session: %v\n", sid, err)
		}
		return
	}
	var events []*session.Event
	for ev := range resp.Session.Events().All() {
		events = append(events, ev)
	}
	interruptedAt, interrupted := agent.ClassifyInterruptedTail(events)
	if !interrupted {
		return
	}
	if deps.AutoContinueFreshness > 0 && time.Since(interruptedAt) > deps.AutoContinueFreshness {
		fmt.Fprintf(os.Stderr, "core-agent: session %s: interrupted turn is %s old (> freshness %s); waiting for the next message\n",
			sid, time.Since(interruptedAt).Round(time.Second), deps.AutoContinueFreshness)
		return
	}
	if err := ag.InjectAs(agent.AutoContinueNote(interruptedAt), auth.Caller{Identity: agent.AutoContinueOriginator}); err != nil {
		fmt.Fprintf(os.Stderr, "core-agent: session %s: auto-continue: inject: %v\n", sid, err)
		return
	}
	fmt.Fprintf(os.Stderr, "core-agent: session %s: auto-continue queued (turn interrupted %s ago)\n",
		sid, time.Since(interruptedAt).Round(time.Second))
}
