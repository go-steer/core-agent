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

// Auto-continue triggers (#539, #558; docs/auto-continue-design.md).
// Three call sites share one lock/classify/inject core and one set of
// boot-log-derived guards:
//
//   - maybeAutoContinue: the lazy-resume touch (ReproduceAgent,
//     origin=="resumed") — multi-session daemons, #539 PR 1.
//   - AutoContinueBootScan: boot-time pass over persisted ACL rows —
//     channel sessions nobody re-touches, #539 PR 2.
//   - AutoContinueStartupSession: the single startup-time session of a
//     headless --no-repl daemon (the examples/gke-deploy shape) —
//     #558. Interactive REPL/TUI modes stay excluded: a human is
//     present there by definition.

package compose

import (
	"context"
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

// autoContinueScanWindow bounds every tail read. A window this size is
// classification-safe: the interrupted tail is by definition the last
// conversational event, and only annotation rows (audit, checkpoints,
// notes) can trail it — never dozens of them.
const autoContinueScanWindow = 128

// Crash-loop guard thresholds (design doc §breaker). Constants in v1;
// knobs wait for a demonstrated need.
//
//   - breaker: >= breakerBootThreshold boots inside breakerWindow that
//     attempted continuations → this boot's triggers stand down.
//     Mostly a fleet/multi-session rate limiter (the per-session
//     guards below handle the canonical single-poisoned-session loop;
//     a rolling fleet restart with legitimate continuations can trip
//     it — the fail-safe direction, cost one stderr line and one
//     quiet boot).
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

// maybeAutoContinue implements the lazy-resume trigger: called from
// ReproduceAgent for origin=="resumed" sessions when the feature is
// enabled. The wake loop (started by the caller right after) drains
// the injected note as the session's first turn.
//
// Lock staging note: v1 holds agent_run_lock across detection +
// injection only, not across the continuation turn itself (the turn
// runs asynchronously in the wake loop; holding a lease across it
// needs a turn-end hook this path doesn't have). The residual window
// — two shared-DB daemons both lazily resuming the same session
// after one's injection but before its turn commits — requires
// near-simultaneous cross-daemon touches inside the freshness window.
// The design doc's implementation notes record this deviation.
func maybeAutoContinue(deps SessionFactoryDeps, caller auth.Caller, sid string, ag *agent.Agent) {
	lockClassifyInject(deps.DaemonCtx, deps.EventlogHandle, ag, "core-agent", caller.Identity, sid, deps.AutoContinueFreshness)
}

// lockClassifyInject is the shared core: take the session run lock
// (fleet mutual exclusion — skip on ErrSessionLocked), classify the
// committed tail under it, apply the freshness window, inject the
// synthesized note. Every skip path returns silently or with one stderr line; callers
// must never fail because auto-continue couldn't run.
func lockClassifyInject(ctx context.Context, h *eventlog.Handle, ag *agent.Agent, app, user, sid string, freshness time.Duration) {
	// Lock first — one cheap DB write, and classifying under it means
	// two daemons can't both read the same interrupted tail.
	lock, err := h.AcquireLock(ctx, app, user, sid)
	if err != nil {
		if errors.Is(err, eventlog.ErrSessionLocked) {
			// Another daemon (or an autonomous run) owns the session
			// right now. Logged because on the startup/boot-scan paths
			// the write-ahead intent record has already been charged —
			// a silent skip here would make a burned attempt
			// undiagnosable.
			fmt.Fprintf(os.Stderr, "core-agent: session %s: auto-continue: skipped, run lock held elsewhere\n", sid)
			return
		}
		fmt.Fprintf(os.Stderr, "core-agent: session %s: auto-continue: acquire run lock: %v\n", sid, err)
		return
	}
	defer func() { _ = lock.Release() }()

	interruptedAt, interrupted := classifyTail(ctx, h, app, user, sid)
	if !interrupted {
		return
	}
	if freshness > 0 && time.Since(interruptedAt) > freshness {
		fmt.Fprintf(os.Stderr, "core-agent: session %s: interrupted turn is %s old (> freshness %s); waiting for the next message\n",
			sid, time.Since(interruptedAt).Round(time.Second), freshness)
		return
	}
	if err := ag.InjectAs(agent.AutoContinueNote(interruptedAt), auth.Caller{Identity: agent.AutoContinueOriginator}); err != nil {
		fmt.Fprintf(os.Stderr, "core-agent: session %s: auto-continue: inject: %v\n", sid, err)
		return
	}
	fmt.Fprintf(os.Stderr, "core-agent: session %s: auto-continue queued (turn interrupted %s ago)\n",
		sid, time.Since(interruptedAt).Round(time.Second))
}

// classifyTail does one bounded tail read + classification. Bounded
// because this runs synchronously on resume/startup paths — a
// full-session scan on a 100k-event session would break the "resume
// stays fast" promise.
func classifyTail(ctx context.Context, h *eventlog.Handle, app, user, sid string) (time.Time, bool) {
	resp, err := h.Service.Get(ctx, &session.GetRequest{
		AppName:         app,
		UserID:          user,
		SessionID:       sid,
		NumRecentEvents: autoContinueScanWindow,
	})
	if err != nil || resp == nil || resp.Session == nil {
		if err != nil {
			fmt.Fprintf(os.Stderr, "core-agent: session %s: auto-continue: read session: %v\n", sid, err)
		}
		return time.Time{}, false
	}
	var events []*session.Event
	for ev := range resp.Session.Events().All() {
		events = append(events, ev)
	}
	return agent.ClassifyInterruptedTail(events)
}

// attemptGuards aggregates the boot-log-derived skip rules shared by
// the boot scan and the startup-session trigger.
type attemptGuards struct {
	attemptedRecently map[string]bool // inside breakerWindow: single-retry skip
	attemptCount      map[string]int  // inside attemptLookback: cumulative cap
	bootsWithAttempts int
}

func loadAttemptGuards(ctx context.Context, h *eventlog.Handle) (attemptGuards, error) {
	g := attemptGuards{attemptedRecently: map[string]bool{}, attemptCount: map[string]int{}}
	boots, err := h.RecentBoots(ctx, time.Now().Add(-attemptLookback))
	if err != nil {
		return g, err
	}
	breakerCutoff := time.Now().Add(-breakerWindow)
	for _, b := range boots {
		for _, sid := range b.Attempted {
			g.attemptCount[sid]++
		}
		if !b.BootAt.Before(breakerCutoff) {
			if len(b.Attempted) > 0 {
				g.bootsWithAttempts++
			}
			for _, sid := range b.Attempted {
				g.attemptedRecently[sid] = true
			}
		}
	}
	return g, nil
}

func (g attemptGuards) breakerTripped() bool { return g.bootsWithAttempts >= breakerBootThreshold }

func (g attemptGuards) allow(sid string) bool {
	return !g.attemptedRecently[sid] && g.attemptCount[sid] < maxAttemptsPerSession
}

// AutoContinueStartupSession is the #558 trigger: the single
// startup-time session of a headless --no-repl daemon. No ACL rows or
// scan exist for it — there is exactly one session, whose triple comes
// from the agent itself — but the boot-log guards matter MORE here
// than on the lazy path: this trigger fires on every boot with no
// human touch gating it, so it is the closest analogue of the boot
// scan. Same discipline: pre-classify (unlocked, read-only), then
// WRITE-AHEAD intent record, then the shared lock/classify/inject
// core. Call before the wake loop starts; the injected note latches
// the wake signal.
func AutoContinueStartupSession(ctx context.Context, h *eventlog.Handle, ag *agent.Agent, freshness time.Duration) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "core-agent: auto-continue startup: recovered from panic: %v\n", r)
		}
	}()
	if h == nil || h.Service == nil || ag == nil {
		return
	}
	app, user, sid := ag.AppName(), ag.UserID(), ag.SessionID()
	guards, err := loadAttemptGuards(ctx, h)
	if err != nil {
		fmt.Fprintf(os.Stderr, "core-agent: auto-continue startup: read boot log: %v\n", err)
		return
	}
	if guards.breakerTripped() {
		fmt.Fprintf(os.Stderr, "core-agent: auto-continue BREAKER TRIPPED: %d boots attempted continuations within %s — standing down this boot\n",
			guards.bootsWithAttempts, breakerWindow)
		return
	}
	if !guards.allow(sid) {
		return
	}
	// Pre-classify before recording intent: this trigger runs every
	// boot, and recording an "attempt" for a clean tail would burn
	// the cumulative cap on healthy restarts, blocking a real
	// interruption later.
	interruptedAt, interrupted := classifyTail(ctx, h, app, user, sid)
	if !interrupted {
		return
	}
	if freshness > 0 && time.Since(interruptedAt) > freshness {
		return
	}
	if err := h.RecordBoot(ctx, time.Now(), []string{sid}); err != nil {
		fmt.Fprintf(os.Stderr, "core-agent: auto-continue startup: record boot intent: %v — skipping (guards must not be blind)\n", err)
		return
	}
	lockClassifyInject(ctx, h, ag, app, user, sid, freshness)
}

// AutoContinueBootScan is the boot-time trigger for multi-session
// daemons: one pass over the persisted ACL rows that continues fresh
// interrupted sessions nobody re-touches (channel sessions).
// Candidates are found with bounded tail reads only — no agent
// construction for sessions that don't need it; the actual
// continuation happens by driving the registry's normal
// Lookup → resume path, so the lazy-path machinery is shared
// verbatim.
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

	guards, err := loadAttemptGuards(ctx, h)
	if err != nil {
		fmt.Fprintf(os.Stderr, "core-agent: auto-continue boot scan: read boot log: %v\n", err)
		return
	}
	if guards.breakerTripped() {
		fmt.Fprintf(os.Stderr, "core-agent: auto-continue BREAKER TRIPPED: %d boots attempted continuations within %s — standing down this boot (a continuation turn may be killing the daemon; sessions resume normally on touch)\n",
			guards.bootsWithAttempts, breakerWindow)
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
		if !guards.allow(row.SessionID) {
			continue
		}
		interruptedAt, interrupted := classifyTail(ctx, h, row.AppName, row.UserID, row.SessionID)
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
