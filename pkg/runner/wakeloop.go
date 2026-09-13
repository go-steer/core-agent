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

package runner

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/go-steer/core-agent/v2/pkg/agent"
	"github.com/go-steer/core-agent/v2/pkg/attach"
	"github.com/go-steer/core-agent/v2/pkg/usage"
)

// Failure rate-limiting defaults (#978). A wake loop that fails every
// turn used to burn one turn per arriving event, forever, at whatever
// rate the cluster produced them.
//
// The first consecutive failure is free — a single blip must not add
// latency to the operator's next message — and every one after it waits
// base * 2^(n-2), capped. At these numbers a permanently broken session
// costs at most a dozen turns an hour instead of one per event.
const (
	defaultFailureBackoff    = 5 * time.Second
	defaultMaxFailureBackoff = 5 * time.Minute
)

// WakeLoopOptions configures WakeLoop. The zero value is usable:
// no usage accounting, turn errors to stderr, no debug tracing.
type WakeLoopOptions struct {
	// Tracker receives one AppendUsage per completed turn (keyed by
	// Model, priced by Pricing) via the usage.TurnTap discipline —
	// overwrite-per-event, commit exactly once on TurnComplete.
	// Nil disables accounting.
	Tracker *usage.Tracker
	Model   string
	Pricing usage.Pricing

	// OnTurnError is invoked for each error the turn iterator
	// yields. The loop always keeps running — one bad turn must not
	// kill an attach-only daemon or a hosted session. Nil writes
	// "core-agent: session <sid> turn: <err>" to stderr, matching
	// the historical inline loops.
	OnTurnError func(error)

	// Health, when non-nil, is updated with this loop's state after
	// every turn so GET /healthz can tell "idle, waiting for events"
	// from "awake and failing repeatedly" (#978). Register it with the
	// daemon's LoopHealthSet to have it counted in the aggregate check.
	// Nil disables health reporting and changes nothing else.
	Health *LoopHealth

	// FailureBackoff is the base wait after a SECOND consecutive failed
	// turn; each further failure doubles it up to MaxFailureBackoff.
	// Zero means defaultFailureBackoff. Negative disables the backoff
	// entirely, which is only sensible in a test.
	FailureBackoff time.Duration

	// MaxFailureBackoff caps the doubling. Zero means
	// defaultMaxFailureBackoff.
	MaxFailureBackoff time.Duration

	// Test seams, mirroring models.RetryPolicy so there is one timing
	// convention in the tree rather than two. Nil means real time.
	sleep func(context.Context, time.Duration) bool

	// Debugf, when non-nil, receives trace-level lifecycle lines
	// (loop start/stop, wake fired, turn finished). Wire the host's
	// debug logger; nil is silent.
	Debugf func(format string, args ...any)
}

// WakeLoop is the wake-driven inbox drain every headless agent
// surface runs: block until an attach client's POST /inject (or any
// other Inject caller) fires WakeRequested, run one empty-prompt
// turn so the inbox drains into a real model turn, account its
// usage, repeat. Returns when ctx is cancelled.
//
// The empty prompt means "no user text this turn, just drain the
// inbox" — the same path the REPL uses between submissions. The
// turn's events flow through the eventlog to the attach broadcaster,
// which is what a remote operator's TUI renders.
//
// This consolidates the previously duplicated loops in
// cmd/core-agent (the --no-repl inline loop and the per-session
// multi-session loop) behind pkg/runner, which already owns the
// "drive the agent through a conversation" surface. Extracted as
// part of the pkg/compose work (#386,
// docs/compose-extraction-design.md).
//
// # Repeated failure (#978)
//
// A turn that ends in a fault increments a consecutive-failure count
// and moves opts.Health to LoopFailing; any clean turn resets both.
// From the second consecutive failure the loop holds off for a capped,
// doubling interval before driving another, which is what stops a
// persistent fault — revoked RBAC, a dead credential, an exhausted
// quota — from burning one turn per arriving event forever. Operator
// cancellations and guardrail halts do not count; see countsAsFailure.
//
// What the loop deliberately does NOT do is give up. #978 asks it to
// "stop after a bound", and there is no honest way to do that here:
//
//   - Returning closes the inbox (the defer below), so every later
//     inject fails with ErrInboxClosed — including the operator's
//     message telling the session what to do about the failure. The
//     loop would destroy the only channel through which it could be
//     fixed.
//   - Staying parked while refusing to drive turns is worse in a
//     quieter way. The inbox is bounded and drops the OLDEST message
//     when full, so a producer pointed at a stopped loop silently
//     loses its earliest signals — the #1040 harm, except #1040 is
//     safe to fence precisely because a guardrail halt has a reset
//     verb that releases it. A loop that stopped itself has none.
//
// So the bound is expressed as a RATE, not as a stop: at most one
// failing turn per MaxFailureBackoff, indefinitely, with the condition
// visible on GET /healthz the whole time. An operator who wants it to
// actually stop has the verbs for that already — pause the session, or
// delete it.
//
// The cap is minutes rather than hours for the same drop-oldest reason.
// A hold-off IS a park, just a bounded one, and the inbox keeps filling
// through it; the next turn drains the lot, so nothing is lost as long
// as the wait stays short against how fast a producer can push 256
// messages. Raising MaxFailureBackoff past that trades the rate limit
// for the harm it was avoiding.
func WakeLoop(ctx context.Context, a *agent.Agent, opts WakeLoopOptions) {
	debugf := opts.Debugf
	if debugf == nil {
		debugf = func(string, ...any) {}
	}
	onErr := opts.OnTurnError
	if onErr == nil {
		onErr = func(err error) {
			fmt.Fprintf(os.Stderr, "core-agent: session %s turn: %v\n", a.SessionID(), err)
		}
	}
	// The loop's ctx is cancelled by both daemon shutdown and
	// per-session eviction (compose derives it from DaemonCtx and hands
	// the cancel to the registry as cancelOnEvict). Closing the inbox on
	// exit makes any inject that races past handler-level gating into an
	// evicted/shut-down session fail loudly with ErrInboxClosed at the
	// source, instead of being acknowledged and silently dropped into a
	// mailbox nobody will drain (#566).
	defer a.CloseInbox()
	opts.Health.setSession(a.SessionID())
	defer opts.Health.setState(LoopStopped)
	debugf("wake loop starting (session=%s model=%s)", a.SessionID(), opts.Model)
	for {
		select {
		case <-ctx.Done():
			debugf("wake loop ending (ctx cancelled)")
			return
		case <-a.WakeRequested():
			debugf("wake fired; calling Run")
			opts.Health.setState(LoopWorking)
			var tap usage.TurnTap
			var evCount int
			// failKind latches the LAST counted error of the turn.
			// Counting per yielded error would be wrong: one turn can
			// yield several, and OnTurnError fires for each, so a
			// failure counter driven off the callback would see one bad
			// turn as three and back off at triple rate.
			var failKind string
			// sawObeyed records that the turn yielded an error we do not
			// count — an operator interrupt, a guardrail halt. Such a
			// turn is not a clean one, and the distinction matters:
			// see turnObeyed.
			var sawObeyed bool
			for ev, runErr := range a.Run(ctx, "") {
				evCount++
				tap.Observe(ev)
				if u, ok := tap.Commit(ev); ok && opts.Tracker != nil {
					// Re-resolved per turn, not taken from
					// opts.Pricing: a wake loop outlives any number of
					// /pricing refreshes, and in multi-session mode
					// opts.Pricing was resolved once when the session
					// was constructed (#930).
					opts.Tracker.AppendUsage(opts.Model, u, usage.PriceForRefreshed(opts.Model, opts.Pricing))
				}
				if runErr != nil {
					onErr(runErr)
					if kind := attach.ClassifyTurnError(runErr).Kind; countsAsFailure(kind) {
						failKind = kind
					} else {
						sawObeyed = true
					}
				}
			}
			debugf("Run finished (events=%d)", evCount)

			outcome := turnClean
			switch {
			case failKind != "":
				outcome = turnFailed
			case sawObeyed:
				outcome = turnObeyed
			}
			fails := opts.Health.observeTurn(outcome, failKind)
			if outcome != turnFailed {
				continue
			}
			d := opts.failureBackoff(fails)
			if d <= 0 {
				continue
			}
			debugf("turn failed (%s, %d consecutive); holding off %s before driving another", failKind, fails, d)
			if !opts.wait(ctx, d) {
				debugf("wake loop ending (ctx cancelled during backoff)")
				return
			}
		}
	}
}

// countsAsFailure reports whether a turn-error kind means the loop is
// BROKEN, as opposed to obeying somebody.
//
// Three kinds are excluded and the exclusions are the load-bearing part:
// an operator pressing stop (canceled) and either guardrail halting the
// session (watchdog, cost_ceiling) are the system doing exactly what it
// was told. Counting them would make a daemon report itself unhealthy
// for working correctly, and — worse — would rate-limit the turn that
// runs right after an operator clears the halt. (Since #1040 a halted
// session stops waking the driver at all, so in practice the guardrail
// kinds arrive here only from a scheduler-driven Run; excluding them is
// belt and braces, and cheap.)
func countsAsFailure(kind string) bool {
	switch kind {
	case "", attach.TurnErrorCanceled, attach.TurnErrorWatchdog, attach.TurnErrorCostCeiling:
		return false
	}
	return true
}

// failureBackoff returns how long to hold off before driving another
// turn, given the consecutive-failure count.
//
// The FIRST failure returns zero. A single blip is the common case and
// must not add latency to the operator's next message; only a repeating
// fault earns a rate limit.
func (o WakeLoopOptions) failureBackoff(consecutive int) time.Duration {
	if o.FailureBackoff < 0 || consecutive <= 1 {
		return 0
	}
	base := o.FailureBackoff
	if base == 0 {
		base = defaultFailureBackoff
	}
	ceiling := o.MaxFailureBackoff
	if ceiling <= 0 {
		ceiling = defaultMaxFailureBackoff
	}
	d := base
	for i := 2; i < consecutive; i++ {
		d *= 2
		if d >= ceiling {
			return ceiling
		}
	}
	if d > ceiling {
		return ceiling
	}
	return d
}

// wait sleeps for d, reporting false if ctx ended first. Mirrors
// models.RetryPolicy.wait so the tree has one shape for this.
func (o WakeLoopOptions) wait(ctx context.Context, d time.Duration) bool {
	if o.sleep != nil {
		return o.sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
