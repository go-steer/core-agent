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
//
// The last two are each re-fired in-lifetime by AutoContinueRetryLoop
// (#575 defect B) via AutoContinueRetryScan / AutoContinueStartupRetry:
// the same pass under the same guards, logged under its own name so an
// operator does not read a self-heal on a long-lived daemon as a
// reboot (#1066).

package compose

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"time"

	"google.golang.org/adk/v2/session"

	"github.com/go-steer/core-agent/v3/internal/effects"
	"github.com/go-steer/core-agent/v3/pkg/agent"
	"github.com/go-steer/core-agent/v3/pkg/attachadapter"
	"github.com/go-steer/core-agent/v3/pkg/auth"
	"github.com/go-steer/core-agent/v3/pkg/eventlog"
)

// AutoContinueConfig bundles the eventlog handle, freshness window,
// ambiguous-effect classification predicates, and scheduling-lease loss
// signal for auto-continue passes (#1405).
type AutoContinueConfig struct {
	EventLog      *eventlog.Handle
	Freshness     time.Duration
	IsReadOnly    effects.ReadOnlyPredicate
	SubAgentNames map[string]bool
	LeaseLost     <-chan struct{}
}

func (deps SessionFactoryDeps) autoContinueConfig() AutoContinueConfig {
	return AutoContinueConfig{
		EventLog:      deps.EventlogHandle,
		Freshness:     deps.AutoContinueFreshness,
		IsReadOnly:    deps.IsReadOnly,
		SubAgentNames: deps.SubAgentNames,
		LeaseLost:     deps.LeaseLost,
	}
}

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
// Lock staging note: agent_run_lock is held across detection +
// injection only, not across the continuation turn itself (the turn
// runs asynchronously in the wake loop; holding a lease across it
// needs a turn-end hook this path doesn't have). The residual window
// that left — two shared-DB daemons both resuming the same session
// after one's injection but before its turn commits — is closed by the
// durable continuation claim (#977) rather than by a longer lease; see
// lockClassifyInject and pkg/eventlog/claim.go.
func maybeAutoContinue(deps SessionFactoryDeps, caller auth.Caller, sid string, ag *agent.Agent) {
	lockClassifyInject(deps.DaemonCtx, deps.autoContinueConfig(), ag, "core-agent", caller.Identity, sid)
}

// autoContinueOutcome reports what lockClassifyInject did, so the boot
// scan can keep its attempt accounting outcome-aware: only acInjected
// means the session actually got a continuation turn queued and should
// count against the per-session cap. Every other outcome is a
// synchronous skip BEFORE any turn ran — charging it would burn the cap
// on daemons that never got a fair shot (the fleet run-lock case, #575).
type autoContinueOutcome int

const (
	acInjected               autoContinueOutcome = iota // note queued: a real attempt
	acSkippedLocked                                     // run lock held by another daemon/run
	acSkippedLockErr                                    // AcquireLock failed for another reason
	acSkippedNotInterrupted                             // tail re-classified clean under the lock
	acSkippedStale                                      // interruption older than the freshness window
	acSkippedAmbiguousEffect                            // unacknowledged mutating tool call in flight (#1405)
	acSkippedTurnInFlight                               // a local turn is generating the answer right now (#796)
	acSkippedOperatorInput                              // operator input already queued — it drives the next turn (#624)
	acSkippedPaused                                     // operator parked the loop; resume drives the next turn
	acSkippedGuardrailHalt                              // watchdog or cost ceiling is refusing turns (#1040)
	acSkippedClaimed                                    // this interruption was already continued by someone (#977)
	acSkippedClaimErr                                   // the claim write failed; stand down rather than risk a duplicate (#977)
	acSkippedInjectErr                                  // inject itself failed
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
// the operator's resume drives the next turn), or a guardrail refusing
// turns (#1040 — same shape as parked: no note was injected, and the
// reset releases the fenced wake). Charging the halt case instead would
// spend the session's hourly cap on ticks that did nothing, so a
// session reset inside the freshness window would come back with
// auto-continue already exhausted — penalising it for the guardrail
// having worked.
// A claim already held by somebody else (#977) refunds for exactly the
// #575 reason acSkippedLocked does: we made no attempt and another
// daemon did, so charging us would spend the shared per-session cap on
// the losers of a race the winner is already handling.
// Everything else stays charged:
// a queued note is a real attempt, and a failed resume/inject or an
// unexpected lock error must stay counted because a PERSISTENT such
// failure is a crash-loop vector that only the per-session cap can
// bound. Sessions the scan never reaches (ctx cancelled, resume error)
// are likewise left charged — over-counting only makes the guards more
// conservative, which is the safe direction.
func (o autoContinueOutcome) refundable() bool {
	switch o {
	case acSkippedLocked, acSkippedNotInterrupted, acSkippedStale, acSkippedAmbiguousEffect, acSkippedTurnInFlight, acSkippedOperatorInput, acSkippedPaused, acSkippedGuardrailHalt, acSkippedClaimed:
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
// claim the interruption durably (#977 — the lock covers the decision,
// the claim covers the interruption), then inject the synthesized note.
// Every skip path returns silently or with one stderr
// line; callers must never fail because auto-continue couldn't run. The
// returned outcome lets the boot scan account only real attempts.
func lockClassifyInject(ctx context.Context, cfg AutoContinueConfig, ag *agent.Agent, app, user, sid string) autoContinueOutcome {
	h := cfg.EventLog
	freshness := cfg.Freshness
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

	verdict, events := classifyTailWithEvents(ctx, h, app, user, sid)
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
	if outcome, skipped := checkLocalAgentStandDown(ctx, ag, sid); skipped {
		return outcome
	}
	if hasUnackedAmbiguousEffects(ctx, cfg, app, user, sid, events) {
		fmt.Fprintf(os.Stderr, "core-agent: session %s: auto-continue: unacknowledged mutating tool call in flight; standing down until operator acks effects\n", sid)
		return acSkippedAmbiguousEffect
	}
	// Claim this specific interruption before injecting for it (#977).
	//
	// The run lock above is fleet mutual exclusion for the DECISION, and
	// it is released before the continuation turn runs — the turn
	// executes asynchronously in the wake loop and this path has no
	// turn-end hook to hold a lease across. So a peer daemon that
	// acquires the lock a moment later reads a tail that still looks
	// interrupted, because our turn has committed nothing yet, and
	// injects a second continuation into the same session. The window is
	// not "two turns overlap", it is "two daemons read the same
	// interrupted tail and both act on it", and a durable claim is what
	// closes it: a fact that stays true after its writer has moved on,
	// which is precisely what a lease cannot be.
	//
	// Keyed on the interruption instant, not on the session, so a session
	// that goes quiet again later is continued again. See
	// pkg/eventlog/claim.go for why this is not a lock held longer.
	claimed, err := h.ClaimContinuation(ctx, app, user, sid, interruptedAt, lock.Holder())
	if err != nil {
		// Stand down. Injecting unclaimed is the one outcome this exists
		// to prevent, and a DB that cannot take this write is not going
		// to carry the continuation turn either. Stays CHARGED, like
		// acSkippedLockErr: a persistent failure here is a crash-loop
		// vector only the per-session cap can bound.
		fmt.Fprintf(os.Stderr, "core-agent: session %s: auto-continue: claim continuation: %v — standing down\n", sid, err)
		return acSkippedClaimErr
	}
	if !claimed {
		fmt.Fprintf(os.Stderr, "core-agent: session %s: auto-continue: this interruption has already been continued; standing down\n", sid)
		return acSkippedClaimed
	}
	if err := ag.InjectAs(agent.AutoContinueNoteFor(interruptedAt, verdict.InterruptedCalls), auth.Caller{Identity: agent.AutoContinueOriginator}); err != nil {
		fmt.Fprintf(os.Stderr, "core-agent: session %s: auto-continue: inject: %v\n", sid, err)
		// Hand the claim back: no note was queued, so the interruption is
		// still uncontinued and the retry driver or a peer must be able
		// to take it. Logged rather than retried — a claim that is never
		// released costs a continuation, which is the same direction of
		// error as the release never running, and neither produces the
		// duplicate the claim exists to prevent.
		if rerr := h.ReleaseContinuationClaim(ctx, app, user, sid, interruptedAt, lock.Holder()); rerr != nil {
			fmt.Fprintf(os.Stderr, "core-agent: session %s: auto-continue: release claim after a failed inject: %v\n", sid, rerr)
		}
		return acSkippedInjectErr
	}
	fmt.Fprintf(os.Stderr, "core-agent: session %s: auto-continue queued (turn interrupted %s ago)\n",
		sid, time.Since(interruptedAt).Round(time.Second))
	return acInjected
}

func checkLocalAgentStandDown(ctx context.Context, ag *agent.Agent, sid string) (autoContinueOutcome, bool) {
	if ag.TurnInFlight() {
		fmt.Fprintf(os.Stderr, "core-agent: session %s: auto-continue: a turn is already running; standing down (the tail is mid-turn, not interrupted)\n", sid)
		return acSkippedTurnInFlight, true
	}
	if ag.HasPendingOperatorInput() {
		fmt.Fprintf(os.Stderr, "core-agent: session %s: auto-continue: operator input already queued; standing down so it drives the next turn\n", sid)
		return acSkippedOperatorInput, true
	}
	if ag.Paused() {
		fmt.Fprintf(os.Stderr, "core-agent: session %s: auto-continue: session is paused; standing down until the operator resumes\n", sid)
		return acSkippedPaused, true
	}
	_ = ag.RestoreGuardrails(ctx)
	if halted, reason := ag.GuardrailHalted(); halted {
		fmt.Fprintf(os.Stderr, "core-agent: session %s: auto-continue: guardrail is tripped (%s); standing down until it is reset\n", sid, reason)
		return acSkippedGuardrailHalt, true
	}
	return 0, false
}

func hasUnackedAmbiguousEffects(ctx context.Context, cfg AutoContinueConfig, app, user, sid string, events session.Events) bool {
	if events == nil || cfg.EventLog == nil {
		return false
	}
	scan := effects.ScanDangling(events, cfg.IsReadOnly, cfg.SubAgentNames)
	if len(scan.Mutating) == 0 {
		return false
	}
	ackedAt, ok := cfg.EventLog.EffectsAckedAt(ctx, app, user, sid)
	if !ok {
		return true
	}
	return len(scan.Unacked(ackedAt)) > 0
}

// classifyTail does one bounded tail read + classification. Bounded
// because this runs synchronously on resume/startup paths — a
// full-session scan on a 100k-event session would break the "resume
// stays fast" promise.
func classifyTail(ctx context.Context, h *eventlog.Handle, app, user, sid string) agent.TailVerdict {
	v, _ := classifyTailWithEvents(ctx, h, app, user, sid)
	return v
}

func classifyTailWithEvents(ctx context.Context, h *eventlog.Handle, app, user, sid string) (agent.TailVerdict, session.Events) {
	resp, err := h.Service.Get(ctx, &session.GetRequest{
		AppName:         app,
		UserID:          user,
		SessionID:       sid,
		NumRecentEvents: autoContinueScanWindow,
	})
	if err != nil || resp == nil || resp.Session == nil {
		// A session that is not in the log yet is the normal state of
		// a cold boot, not a read failure — and since #973 made a
		// durable eventlog the default for attach mode, this path runs
		// on every daemon's first start. Logging it would make
		// "database error while fetching session" the first thing a
		// new operator sees from a perfectly healthy daemon.
		if err != nil && !eventlog.IsSessionNotFound(err) {
			fmt.Fprintf(os.Stderr, "core-agent: session %s: auto-continue: read session: %v\n", sid, err)
		}
		return agent.TailVerdict{}, nil
	}
	var events []*session.Event
	for ev := range resp.Session.Events().All() {
		events = append(events, ev)
	}
	return agent.ClassifyInterruptedTailVerdict(events), resp.Session.Events()
}

// autoContinueTrigger names the driver that fired a pass, for the
// operator log only. Each pass below is shared by two drivers — the
// boot-time one and the in-lifetime retry loop (#575 defect B) — and
// the audience for a line it writes is someone reconstructing a night
// they did not watch. "boot scan" on a daemon with six hours of uptime
// and zero restarts tells that reader the process rebooted; they then
// hold two facts that cannot both be true and go looking for a crash
// that never happened (#1066). The guards and the agent_boot_log row
// keep their per-boot semantics — this is about what is read, not
// about what is counted.
type autoContinueTrigger struct {
	what  string // fills "core-agent: auto-continue <what>: …"
	stood string // fills "standing down <stood>" when the breaker trips
}

var (
	triggerBoot         = autoContinueTrigger{what: "boot scan", stood: "this boot"}
	triggerRetry        = autoContinueTrigger{what: "retry", stood: "this pass"}
	triggerStartup      = autoContinueTrigger{what: "startup", stood: "this boot"}
	triggerStartupRetry = autoContinueTrigger{what: "startup retry", stood: "this pass"}
)

// logf writes one operator line attributed to this trigger.
func (t autoContinueTrigger) logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "core-agent: auto-continue "+t.what+": "+format+"\n", args...)
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

func isLeaseLost(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
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
	autoContinueStartupPass(ctx, AutoContinueConfig{EventLog: h, Freshness: freshness}, ag, triggerStartup)
}

// AutoContinueStartupSessionWithConfig is AutoContinueStartupSession configured
// via AutoContinueConfig (#1405).
func AutoContinueStartupSessionWithConfig(ctx context.Context, cfg AutoContinueConfig, ag *agent.Agent) {
	autoContinueStartupPass(ctx, cfg, ag, triggerStartup)
}

// AutoContinueStartupRetry is the same pass fired by the in-lifetime
// retry loop rather than by boot (#575 defect B). Identical work,
// different attribution in the log: nothing restarted (#1066).
func AutoContinueStartupRetry(ctx context.Context, h *eventlog.Handle, ag *agent.Agent, freshness time.Duration) {
	autoContinueStartupPass(ctx, AutoContinueConfig{EventLog: h, Freshness: freshness}, ag, triggerStartupRetry)
}

// AutoContinueStartupRetryWithConfig is AutoContinueStartupRetry configured
// via AutoContinueConfig (#1405).
func AutoContinueStartupRetryWithConfig(ctx context.Context, cfg AutoContinueConfig, ag *agent.Agent) {
	autoContinueStartupPass(ctx, cfg, ag, triggerStartupRetry)
}

func autoContinueStartupPass(ctx context.Context, cfg AutoContinueConfig, ag *agent.Agent, trig autoContinueTrigger) {
	defer func() {
		if r := recover(); r != nil {
			trig.logf("recovered from panic: %v", r)
		}
	}()
	h := cfg.EventLog
	freshness := cfg.Freshness
	if h == nil || h.Service == nil || ag == nil || isLeaseLost(cfg.LeaseLost) {
		return
	}
	app, user, sid := ag.AppName(), ag.UserID(), ag.SessionID()
	guards, err := loadAttemptGuards(ctx, h)
	if err != nil {
		trig.logf("read boot log: %v", err)
		return
	}
	if guards.breakerTripped() {
		fmt.Fprintf(os.Stderr, "core-agent: auto-continue BREAKER TRIPPED: %d boots attempted continuations within %s — standing down %s\n",
			guards.bootsWithAttempts, breakerWindow, trig.stood)
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
		trig.logf("record boot intent: %v — skipping (guards must not be blind)", err)
		return
	}
	// Narrow the write-ahead record only if the session skipped for a
	// benign/contended reason (run lock held elsewhere, raced-clean or
	// raced-stale tail), so a synchronous skip does not burn the cap —
	// same accounting the boot scan does. An inject/lock error stays
	// charged: a persistent one is a crash loop the cap must bound.
	// Non-fatal on error: the pessimistic count stands.
	if lockClassifyInject(ctx, cfg, ag, app, user, sid).refundable() {
		if err := h.UpdateBootAttempted(ctx, bootID, nil); err != nil {
			trig.logf("narrow boot record: %v (pessimistic count stands)", err)
		}
	}
}

// AutoContinueRetryLoop re-runs a guarded auto-continue pass on a fixed
// interval until ctx cancels or an optional leaseLost channel closes.
// It runs NO pass immediately — callers keep their existing boot-time
// invocation, so the note-latches-before-wake-loop ordering is unchanged;
// this loop only adds the in-lifetime *re-tries* that let a stable daemon
// self-heal a stranded continuation without waiting for a reboot or a
// human message (#575 defect B).
//
// interval <= 0 disables the loop (returns immediately). Callers launch
// it on a goroutine tracked by a WaitGroup joined before the eventlog
// closes, so a mid-tick pass never races DB teardown.
func AutoContinueRetryLoop(ctx context.Context, interval time.Duration, pass func(), leaseLost ...<-chan struct{}) {
	if interval <= 0 || pass == nil {
		return
	}
	var lost <-chan struct{}
	if len(leaseLost) > 0 {
		lost = leaseLost[0]
	}
	if isLeaseLost(lost) {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-lost:
			return
		case <-t.C:
			if isLeaseLost(lost) {
				return
			}
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
	autoContinueScanPass(deps, maxPerBoot, triggerBoot)
}

// AutoContinueRetryScan is the same pass fired by the in-lifetime retry
// loop rather than by boot (#575 defect B). Identical work, identical
// guards; it differs only in what the log calls it, because on this
// path the daemon has been up the whole time (#1066).
func AutoContinueRetryScan(deps SessionFactoryDeps, maxPerBoot int) {
	autoContinueScanPass(deps, maxPerBoot, triggerRetry)
}

type autoContinueCandidate struct {
	app, user, sid string
	interruptedAt  time.Time
}

func autoContinueScanPass(deps SessionFactoryDeps, maxPerBoot int, trig autoContinueTrigger) {
	// The scan drives full agent construction (resume) in this
	// goroutine; on the lazy path the same work runs under net/http's
	// per-connection recover. A panic on one poisoned session must
	// not take the daemon down — and must not bypass the write-ahead
	// boot record below.
	defer func() {
		if r := recover(); r != nil {
			trig.logf("recovered from panic: %v", r)
		}
	}()
	h := deps.EventlogHandle
	if h == nil || h.Service == nil || deps.Registry == nil || deps.ACLStore == nil || !deps.AutoContinueEnabled || isLeaseLost(deps.LeaseLost) {
		return
	}
	ctx := deps.DaemonCtx
	if maxPerBoot <= 0 {
		maxPerBoot = 10
	}

	guards, err := loadAttemptGuards(ctx, h)
	if err != nil {
		trig.logf("read boot log: %v", err)
		return
	}
	if guards.breakerTripped() {
		fmt.Fprintf(os.Stderr, "core-agent: auto-continue BREAKER TRIPPED: %d boots attempted continuations within %s — standing down %s (a continuation turn may be killing the daemon; sessions resume normally on touch)\n",
			guards.bootsWithAttempts, breakerWindow, trig.stood)
		return
	}

	candidates, ok := collectAutoContinueCandidates(ctx, deps, guards, maxPerBoot, trig)
	if !ok {
		return
	}
	if len(candidates) == 0 {
		if _, err := h.RecordBoot(ctx, time.Now(), nil); err != nil {
			trig.logf("record boot: %v", err)
		}
		return
	}

	attempted := make([]string, 0, len(candidates))
	for _, c := range candidates {
		attempted = append(attempted, c.sid)
	}
	bootID, err := h.RecordBoot(ctx, time.Now(), attempted)
	if err != nil {
		trig.logf("record boot intent: %v — aborting scan (guards must not be blind)", err)
		return
	}
	refunded := make(map[string]bool, len(candidates))
	injectedCount := 0
	acCfg := deps.autoContinueConfig()
	for _, c := range candidates {
		if ctx.Err() != nil || isLeaseLost(deps.LeaseLost) {
			break // daemon shutting down or lost scheduling lease mid-scan
		}
		entry, err := deps.Registry.Lookup(withDeferAutoContinueInject(ctx), c.app, c.sid)
		if err != nil {
			trig.logf("resume session %s: %v", c.sid, err)
			continue
		}
		ad, ok := entry.Agent.(*attachadapter.Adapter)
		if !ok {
			trig.logf("session %s: unexpected registrant type %T; skipping inject", c.sid, entry.Agent)
			continue
		}
		outcome := lockClassifyInject(ctx, acCfg, ad.Agent(), c.app, c.user, c.sid)
		if outcome.injected() {
			injectedCount++
		}
		if outcome.refundable() {
			refunded[c.sid] = true
		}
	}
	narrowRefundedBootRecord(ctx, h, bootID, attempted, refunded, trig)
	if injectedCount > 0 {
		trig.logf("queued continuations for %d session(s)", injectedCount)
	}
}

func collectAutoContinueCandidates(ctx context.Context, deps SessionFactoryDeps, guards attemptGuards, maxPerBoot int, trig autoContinueTrigger) ([]autoContinueCandidate, bool) {
	rows, err := deps.ACLStore.ListVisibleTo(ctx, auth.Caller{Identity: agent.AutoContinueOriginator, Admin: true})
	if err != nil {
		trig.logf("list sessions: %v", err)
		return nil, false
	}
	var candidates []autoContinueCandidate
	for _, row := range rows {
		if ctx.Err() != nil || isLeaseLost(deps.LeaseLost) {
			return nil, false
		}
		if !guards.allow(row.SessionID) {
			continue
		}
		verdict := classifyTail(ctx, deps.EventlogHandle, row.AppName, row.UserID, row.SessionID)
		if !verdict.Interrupted {
			continue
		}
		if deps.AutoContinueFreshness > 0 && time.Since(verdict.InterruptedAt) > deps.AutoContinueFreshness {
			continue
		}
		candidates = append(candidates, autoContinueCandidate{app: row.AppName, user: row.UserID, sid: row.SessionID, interruptedAt: verdict.InterruptedAt})
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].interruptedAt.Before(candidates[j].interruptedAt) })
	if len(candidates) > maxPerBoot {
		trig.logf("%d interrupted sessions, continuing the %d oldest (max_per_boot); the rest resume on touch",
			len(candidates), maxPerBoot)
		candidates = candidates[:maxPerBoot]
	}
	return candidates, true
}

func narrowRefundedBootRecord(ctx context.Context, h *eventlog.Handle, bootID uint, attempted []string, refunded map[string]bool, trig autoContinueTrigger) {
	if len(refunded) == 0 {
		return
	}
	kept := make([]string, 0, len(attempted))
	for _, sid := range attempted {
		if !refunded[sid] {
			kept = append(kept, sid)
		}
	}
	if err := h.UpdateBootAttempted(ctx, bootID, kept); err != nil {
		trig.logf("narrow boot record: %v (pessimistic count stands)", err)
	}
}
