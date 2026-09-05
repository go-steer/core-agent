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
	"github.com/go-steer/core-agent/v2/pkg/attachadapter"
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

// autoContinueOutcome reports what lockClassifyInject did, so the boot
// scan can keep its attempt accounting outcome-aware: only acInjected
// means the session actually got a continuation turn queued and should
// count against the per-session cap. Every other outcome is a
// synchronous skip BEFORE any turn ran — charging it would burn the cap
// on daemons that never got a fair shot (the fleet run-lock case, #575).
type autoContinueOutcome int

const (
	acInjected              autoContinueOutcome = iota // note queued: a real attempt
	acSkippedLocked                                    // run lock held by another daemon/run
	acSkippedLockErr                                   // AcquireLock failed for another reason
	acSkippedNotInterrupted                            // tail re-classified clean under the lock
	acSkippedStale                                     // interruption older than the freshness window
	acSkippedTurnInFlight                              // a local turn is generating the answer right now (#796)
	acSkippedOperatorInput                             // operator input already queued — it drives the next turn (#624)
	acSkippedPaused                                    // operator parked the loop; resume drives the next turn
	acSkippedInjectErr                                 // inject itself failed
)

// injected reports whether the outcome represents a queued continuation.
// Used only for the human-facing "queued N continuations" log line.
func (o autoContinueOutcome) injected() bool { return o == acInjected }

// refundable reports whether the write-ahead attempt charge should be
// REFUNDED (narrowed out of the boot record) for this outcome. Only the
// benign/contended skips qualify: a peer daemon holding the run lock
// (the #575 fleet case — we made no attempt, another daemon will), the
// tail having gone clean or stale between the unlocked candidate scan
// and the locked re-classification, a local turn already generating the
// answer (#796 — no note was injected and the turn will commit its own
// reply), or an operator having queued input
// that will drive the next turn itself (#624 — no note was injected),
// or the session being parked by an operator (no note was injected and
// the operator's resume drives the next turn).
// Everything else stays charged:
// a queued note is a real attempt, and a failed resume/inject or an
// unexpected lock error must stay counted because a PERSISTENT such
// failure is a crash-loop vector that only the per-session cap can
// bound. Sessions the scan never reaches (ctx cancelled, resume error)
// are likewise left charged — over-counting only makes the guards more
// conservative, which is the safe direction.
func (o autoContinueOutcome) refundable() bool {
	switch o {
	case acSkippedLocked, acSkippedNotInterrupted, acSkippedStale, acSkippedTurnInFlight, acSkippedOperatorInput, acSkippedPaused:
		return true
	default:
		return false
	}
}

// deferInjectKey marks a resume ctx so sessionResumer.Resume defers the
// inline continuation inject to the boot scan. Unexported: only the
// scan (setter) and the resumer (reader) — both in this package — use
// it; the registry threads the ctx through opaquely.
type deferInjectKey struct{}

func withDeferAutoContinueInject(ctx context.Context) context.Context {
	return context.WithValue(ctx, deferInjectKey{}, true)
}

func deferAutoContinueInject(ctx context.Context) bool {
	v, _ := ctx.Value(deferInjectKey{}).(bool)
	return v
}

// lockClassifyInject is the shared core: take the session run lock
// (fleet mutual exclusion — skip on ErrSessionLocked), classify the
// committed tail under it, apply the freshness window, stand down on
// live local agent state the tail cannot show (a turn already
// generating — #796, queued operator input — #624, an operator pause),
// then inject the synthesized note. Every skip path returns silently or with one stderr
// line; callers must never fail because auto-continue couldn't run. The
// returned outcome lets the boot scan account only real attempts.
func lockClassifyInject(ctx context.Context, h *eventlog.Handle, ag *agent.Agent, app, user, sid string, freshness time.Duration) autoContinueOutcome {
	// Lock first — one cheap DB write, and classifying under it means
	// two daemons can't both read the same interrupted tail.
	lock, err := h.AcquireLock(ctx, app, user, sid)
	if err != nil {
		if errors.Is(err, eventlog.ErrSessionLocked) {
			// Another daemon (or an autonomous run) owns the session
			// right now. Logged because on the startup/boot-scan paths
			// the write-ahead intent record has already been charged —
			// a silent skip here would make a burned attempt
			// undiagnosable. The boot scan refunds this via the
			// acSkippedLocked outcome so the cap is not burned.
			fmt.Fprintf(os.Stderr, "core-agent: session %s: auto-continue: skipped, run lock held elsewhere\n", sid)
			return acSkippedLocked
		}
		fmt.Fprintf(os.Stderr, "core-agent: session %s: auto-continue: acquire run lock: %v\n", sid, err)
		return acSkippedLockErr
	}
	defer func() { _ = lock.Release() }()

	verdict := classifyTail(ctx, h, app, user, sid)
	if !verdict.Interrupted {
		// Most declines are a completed or deliberately-killed turn and
		// say nothing worth a line. The classifier populates a reason
		// only for the stand-down an operator cannot see in the
		// transcript — the transient-error budget (#969), where the
		// session just goes quiet.
		if verdict.DeclineReason != "" {
			fmt.Fprintf(os.Stderr, "core-agent: session %s: auto-continue: standing down: %s\n", sid, verdict.DeclineReason)
		}
		return acSkippedNotInterrupted
	}
	interruptedAt := verdict.InterruptedAt
	if freshness > 0 && time.Since(interruptedAt) > freshness {
		fmt.Fprintf(os.Stderr, "core-agent: session %s: interrupted turn is %s old (> freshness %s); waiting for the next message\n",
			sid, time.Since(interruptedAt).Round(time.Second), freshness)
		return acSkippedStale
	}
	// The "interrupted" tail may be a turn that is still generating
	// (#796). ClassifyInterruptedTail reads committed history, where an
	// unanswered user message looks the same whether the turn died or is
	// twenty seconds into a long answer — so on the boot path, where
	// nothing can be in flight, the verdict is sound, and on the
	// in-lifetime retry path it is the common case: the retry driver
	// re-runs this pass on a timer, so any turn a tick happens to land
	// inside got a continuation note queued behind the answer it was
	// still producing, which then ran as a second reply. Exposure scales
	// with turn duration over the interval, not with being slower than
	// it: the reported case was a ~22-second generation, well inside any
	// plausible interval, that a tick simply landed in.
	//
	// The run lock above does not cover this. It is fleet mutual
	// exclusion, and the only local caller that takes it is
	// autonomous.Resume — an ordinary attach-driven or wake-loop turn
	// holds no lease, so a session busy answering a message acquires the
	// lock here without contention.
	//
	// Agent.TurnInFlight is the fact the lock isn't: the agent driving
	// the turn is this process's own object, and registration brackets
	// every event the runner commits for the turn. Checked AFTER
	// classification deliberately — a turn that starts between the two
	// reads is then caught by this check rather than injected against,
	// where the reverse order would clear the check and classify the
	// user message the new turn just committed.
	//
	// In-process only, and that is the honest scope: a peer daemon's turn
	// against a shared eventlog reports false here. Nothing regresses
	// versus today (that case is already unguarded, and the fleet run
	// lock only covers the auto-continue/autonomous callers that take
	// it), and the bug reported is single-daemon. A durable in-flight
	// signal is the fix if the cross-daemon shape ever appears in the
	// wild.
	if ag.TurnInFlight() {
		fmt.Fprintf(os.Stderr, "core-agent: session %s: auto-continue: a turn is already running; standing down (the tail is mid-turn, not interrupted)\n", sid)
		return acSkippedTurnInFlight
	}
	// An operator already queued input while the turn was interrupted
	// (the canonical case: they typed `stop`, then `/interrupt`). That
	// input must drive the next turn on its own — injecting a "continue
	// the task" note into the same drained batch would let the note
	// outrank the operator's stop (#624). Stand down; the wake loop runs
	// the operator's message as the next turn.
	if ag.HasPendingOperatorInput() {
		fmt.Fprintf(os.Stderr, "core-agent: session %s: auto-continue: operator input already queued; standing down so it drives the next turn\n", sid)
		return acSkippedOperatorInput
	}
	// An operator parked the loop (POST /interrupt holds by default, and
	// POST /pause holds outright — docs/operator-interrupt-design.md).
	// Paused means "start nothing until I say so". No inject un-parks a
	// loop any more (#878), so this is no longer the guard that keeps
	// auto-continue off the gate — but standing down is still right,
	// because InjectAs would otherwise queue a note the operator's
	// eventual resume drains alongside their steer, arguing with them
	// one turn late. Stand down entirely — resume synthesizes its own
	// framing.
	if ag.Paused() {
		fmt.Fprintf(os.Stderr, "core-agent: session %s: auto-continue: session is paused; standing down until the operator resumes\n", sid)
		return acSkippedPaused
	}
	if err := ag.InjectAs(agent.AutoContinueNoteFor(interruptedAt, verdict.InterruptedCalls), auth.Caller{Identity: agent.AutoContinueOriginator}); err != nil {
		fmt.Fprintf(os.Stderr, "core-agent: session %s: auto-continue: inject: %v\n", sid, err)
		return acSkippedInjectErr
	}
	fmt.Fprintf(os.Stderr, "core-agent: session %s: auto-continue queued (turn interrupted %s ago)\n",
		sid, time.Since(interruptedAt).Round(time.Second))
	return acInjected
}

// classifyTail does one bounded tail read + classification. Bounded
// because this runs synchronously on resume/startup paths — a
// full-session scan on a 100k-event session would break the "resume
// stays fast" promise.
func classifyTail(ctx context.Context, h *eventlog.Handle, app, user, sid string) agent.TailVerdict {
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
		return agent.TailVerdict{}
	}
	var events []*session.Event
	for ev := range resp.Session.Events().All() {
		events = append(events, ev)
	}
	return agent.ClassifyInterruptedTailVerdict(events)
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
	// Silent on a decline: lockClassifyInject re-classifies under the run
	// lock and logs any reason there, so speaking here would double every
	// line.
	verdict := classifyTail(ctx, h, app, user, sid)
	if !verdict.Interrupted {
		return
	}
	if freshness > 0 && time.Since(verdict.InterruptedAt) > freshness {
		return
	}
	bootID, err := h.RecordBoot(ctx, time.Now(), []string{sid})
	if err != nil {
		fmt.Fprintf(os.Stderr, "core-agent: auto-continue startup: record boot intent: %v — skipping (guards must not be blind)\n", err)
		return
	}
	// Narrow the write-ahead record only if the session skipped for a
	// benign/contended reason (run lock held elsewhere, raced-clean or
	// raced-stale tail), so a synchronous skip does not burn the cap —
	// same accounting the boot scan does. An inject/lock error stays
	// charged: a persistent one is a crash loop the cap must bound.
	// Non-fatal on error: the pessimistic count stands.
	if lockClassifyInject(ctx, h, ag, app, user, sid, freshness).refundable() {
		if err := h.UpdateBootAttempted(ctx, bootID, nil); err != nil {
			fmt.Fprintf(os.Stderr, "core-agent: auto-continue startup: narrow boot record: %v (pessimistic count stands)\n", err)
		}
	}
}

// AutoContinueRetryLoop re-runs a guarded auto-continue pass on a fixed
// interval until ctx cancels. It runs NO pass immediately — callers keep
// their existing boot-time invocation, so the note-latches-before-wake-
// loop ordering is unchanged; this loop only adds the in-lifetime
// *re-tries* that let a stable daemon self-heal a stranded continuation
// without waiting for a reboot or a human message (#575 defect B).
//
// Safety: a continuation turn that kills the daemon kills this loop too,
// so the loop can only ever re-fire failures that did NOT take the
// daemon down (transient run-lock contention, a provider blip, a
// poisoned-but-survivable turn). True crash loops remain bounded by the
// cross-boot breaker, and every pass is still bounded by the per-session
// single-retry guard (breakerWindow) + cumulative cap
// (maxAttemptsPerSession) — so no separate backoff schedule is needed:
// a session attempted within breakerWindow simply no-ops on the next
// tick, and the cap terminates a self-renewing session.
//
// interval <= 0 disables the loop (returns immediately). Callers launch
// it on a goroutine tracked by a WaitGroup joined before the eventlog
// closes, so a mid-tick pass never races DB teardown.
func AutoContinueRetryLoop(ctx context.Context, interval time.Duration, pass func()) {
	if interval <= 0 || pass == nil {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pass()
		}
	}
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
		app, user, sid string
		interruptedAt  time.Time
	}
	var candidates []candidate
	for _, row := range rows {
		if ctx.Err() != nil {
			return // daemon shutting down mid-scan
		}
		if !guards.allow(row.SessionID) {
			continue
		}
		// Silent on a decline: the per-candidate lockClassifyInject below
		// re-classifies under the run lock and logs any reason there.
		verdict := classifyTail(ctx, h, row.AppName, row.UserID, row.SessionID)
		if !verdict.Interrupted {
			continue
		}
		if deps.AutoContinueFreshness > 0 && time.Since(verdict.InterruptedAt) > deps.AutoContinueFreshness {
			continue
		}
		candidates = append(candidates, candidate{app: row.AppName, user: row.UserID, sid: row.SessionID, interruptedAt: verdict.InterruptedAt})
	}
	if len(candidates) == 0 {
		if _, err := h.RecordBoot(ctx, time.Now(), nil); err != nil {
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
	bootID, err := h.RecordBoot(ctx, time.Now(), attempted)
	if err != nil {
		fmt.Fprintf(os.Stderr, "core-agent: auto-continue boot scan: record boot intent: %v — aborting scan (guards must not be blind)\n", err)
		return
	}
	// The write-ahead record above charged every candidate
	// pessimistically (so a daemon killed mid-loop still counts them).
	// We now drive each resume and refund only the benign/contended
	// skips — chiefly the fleet run-lock case where a peer daemon owns
	// the session (#575). refunded holds those sids; anything not in it
	// (a queued note, a failed resume/inject, a candidate we never
	// reached) stays charged, which is the crash-loop-safe direction.
	refunded := make(map[string]bool, len(candidates))
	injectedCount := 0
	for _, c := range candidates {
		if ctx.Err() != nil {
			break // daemon shutting down mid-scan; unprocessed candidates stay charged
		}
		// Lookup drives the normal miss → resume path, but with the
		// inject DEFERRED (withDeferAutoContinueInject): ReproduceAgent
		// constructs + registers + wake-loops the agent without
		// injecting, so the scan can classify+inject itself and observe
		// the outcome. Already-in-memory sessions (raced by an operator
		// touch) return their existing entry; injecting into them is
		// still correct — a run-lock/clean-tail skip just reports the
		// matching refundable outcome.
		entry, err := deps.Registry.Lookup(withDeferAutoContinueInject(ctx), c.app, c.sid)
		if err != nil {
			// Resume failed: leave it charged. A persistent reproduction
			// failure is a crash loop the per-session cap must bound.
			fmt.Fprintf(os.Stderr, "core-agent: auto-continue boot scan: resume session %s: %v\n", c.sid, err)
			continue
		}
		ad, ok := entry.Agent.(*attachadapter.Adapter)
		if !ok {
			fmt.Fprintf(os.Stderr, "core-agent: auto-continue boot scan: session %s: unexpected registrant type %T; skipping inject\n", c.sid, entry.Agent)
			continue
		}
		outcome := lockClassifyInject(ctx, h, ad.Agent(), c.app, c.user, c.sid, deps.AutoContinueFreshness)
		if outcome.injected() {
			injectedCount++
		}
		if outcome.refundable() {
			refunded[c.sid] = true
		}
	}
	// Narrow the write-ahead row by dropping the refunded sids. Non-fatal
	// on error: the pessimistic count simply stands, which fails safe
	// (over-counting only ever makes the guards MORE conservative). Skip
	// the write when nothing was refunded.
	if len(refunded) > 0 {
		kept := make([]string, 0, len(attempted))
		for _, sid := range attempted {
			if !refunded[sid] {
				kept = append(kept, sid)
			}
		}
		if err := h.UpdateBootAttempted(ctx, bootID, kept); err != nil {
			fmt.Fprintf(os.Stderr, "core-agent: auto-continue boot scan: narrow boot record: %v (pessimistic count stands)\n", err)
		}
	}
	if injectedCount > 0 {
		fmt.Fprintf(os.Stderr, "core-agent: auto-continue boot scan: queued continuations for %d session(s)\n", injectedCount)
	}
}
