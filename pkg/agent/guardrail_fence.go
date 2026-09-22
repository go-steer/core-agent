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

// A halted session is inert, not periodically retriggered (#1040).
//
// Both guardrail pre-flights refuse at the very top of Run — before
// drainInboxFull, which is 45 lines further down. That ordering is
// correct and deliberate (a refused turn must not consume the operator's
// message), but it means a queued inbox item is unreachable for as long
// as the halt stands: it cannot be delivered, and nothing discards it.
// Inject fires the wake signal, so every subsequent inject woke a driver
// that could only refuse again.
//
// Observed live, in the 2026-09-13 drill batch
// (dev/uat/gke-drill/runs/2026-09-13-std-simian-test-bc-20run-post935.md):
// three sessions cycling every 3–4 minutes, one of them thirteen times
// over 51 minutes, nine minutes after the last run finished and with no
// operator within reach of the cluster. Invisible in all 59 transcripts,
// obvious in the daemon log.
//
// Two things to notice about what that cycle actually cost, because both
// shape the fix:
//
//   - It cost no model spend. The pre-flights return before any model
//     call; recordInvocation(0, err) is the only work a refused turn
//     does. The issue as filed says "burning a model turn every three
//     minutes"; that part is wrong.
//   - It did lose operator input. The inbox drops the OLDEST message
//     past defaultInboxCap (inbox.go), so a halted session that keeps
//     receiving injects silently discards the earliest ones. That is
//     the real harm, and it grows with how long the halt stands.
//
// So the fix is not to drain the inbox on refusal (that discards the
// message the queue exists to protect) and not to make the halt
// clearable on its own (the watchdog's hard stop was earned from an
// observed live loop, #623–#628). It is to stop WAKING a driver that
// can only be refused: a halted agent still queues an inject, still
// emits its `inbox`/queued frame, still wakes every OBSERVER, and still
// publishes `wake` from the paths that published one — it just does not
// hand the driver a reason to call Run. The fenced wake is released by
// the guardrail reset, so the queued input drives the first post-reset
// turn, which is where it was always meant to land.
//
// Two honest limits on how visible that is. An inject publishes no
// `wake` event by design (see injectAs), so on a halted session the
// observer fan-out and the `inbox` frame are the whole story — which is
// why the fence is applied to the driver's subscription rather than to
// the fan-out. And a refused turn emits no frame at all, so neither the
// old refusal nor the new fence reaches an attached operator; both land
// in the daemon log. Surfacing a standing halt to an operator is #891.
//
// Scope note: this fences the WAKE-DRIVEN shape, which is what the
// daemon in the drill runs and what the live evidence is about. A
// driver that calls Run on its own schedule — the autonomous
// scheduler's sleep timer — still reaches the pre-flight and is still
// refused, cheaply and now legibly. Bounding that one needs the
// scheduler to read halt state, which is a separate change.

package agent

import "log"

// refusalPrefix marks a turn-error produced by a PRE-FLIGHT refusal
// rather than by the trip itself. Exported-by-convention as a constant
// so a log reader — and the A2 count-the-failure-classes check the v3.0
// definition of done asks for — can tell "the guardrail just tripped"
// from "the guardrail is still tripped and refused another turn"
// without parsing the halt prose behind it.
const refusalPrefix = "turn refused (guardrail still tripped, not reset): "

// refusalReason frames a stored trip reason as a refusal. Both
// pre-flights route through it so the two arms cannot drift.
func refusalReason(tripReason string) string {
	return refusalPrefix + tripReason
}

// GuardrailHalted reports whether EITHER guardrail is currently
// refusing turns, and why. It is the question every "should I start
// something?" caller actually has: WatchdogTripped and
// CostCeilingTripped each take a.mu on their own, so asking both means
// two answers from two instants, and a caller that checks one has
// simply not checked the other.
//
// The reason names the watchdog first when both stand, because the
// watchdog halt is the one an operator has to clear deliberately.
//
// The out-of-package caller is pkg/compose's auto-continue, which stands
// down on a halt rather than queueing a continuation note nobody can
// deliver (#1040).
func (a *Agent) GuardrailHalted() (bool, string) {
	if a == nil {
		return false, ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	switch {
	case a.watchdogTripped:
		return true, a.watchdogReason
	case a.costCeilingExceeded:
		return true, a.costCeilingReason
	}
	return false, ""
}

// Both helpers below read watchdogTripped and costCeilingExceeded
// directly under one lock rather than calling
// WatchdogTripped/CostCeilingTripped, which each take a.mu
// independently and would let the two answers come from different
// instants.

// fireWakeFenced is the single door to the wake signal for anything
// that wants to DRIVE a turn. It fires normally, except while a
// guardrail halt stands, where it records the deferral and withholds
// the DRIVER's wake only.
//
// Observers keep theirs (fireExceptDefault), and callers that publish
// the `wake` event still publish it — "something asked for a wake" is
// true whether or not a driver was listening, the same reasoning
// RequestWake already applies to an agent with no wake signal wired.
// Withholding the whole fan-out would blind the local TUI to input
// arriving on a halted session; see fireExceptDefault.
//
// The log line is deliberately one line per DISTINCT HALT rather than
// one per fenced wake — thirteen identical lines for one halt is the
// noise this issue is about. It is keyed on the reason text, not on the
// wakeFenced flag: keying on the flag would go quiet for a SECOND
// guardrail tripping inside a standing fence, and for the half still
// refusing turns after an operator clears only one of two, which are
// the two moments the log most needs to move.
//
// Daemon log, because that is the artifact the drill captures and the
// only place a refused turn was ever visible — a refusal emits no frame
// at all, so nothing here reaches an attached operator. Giving the halt
// an operator-visible event is #891.
func (a *Agent) fireWakeFenced() {
	if a == nil {
		return
	}
	a.mu.Lock()
	halted := a.watchdogTripped || a.costCeilingExceeded
	reason := a.watchdogReason
	if reason == "" {
		reason = a.costCeilingReason
	}
	newHalt := halted && reason != a.fencedLogReason
	if halted {
		a.wakeFenced = true
		a.fencedLogReason = reason
	}
	a.mu.Unlock()
	if !halted {
		a.wake.fire()
		return
	}
	if newHalt {
		log.Printf("agent:%s wake fenced while the guardrail is tripped (%s); input stays queued and will drive the first turn after a reset (#1040)", a.logSessionSuffix(), reason)
	}
	a.wake.fireExceptDefault()
}

// releaseFencedWake fires a wake the halt swallowed, once no halt
// remains. Called from ResetWatchdog and ResetCostCeiling after they
// clear their flag.
//
// Fires when a wake was fenced, OR when either queue Run drains still
// holds something. The queue conditions are not redundant with the
// flag: a turn that trips the watchdog in its post-turn hook leaves the
// wake that drove it already consumed, so whatever arrived alongside is
// queued with nothing fenced, and the flag alone would strand it. Both
// queues need the check for the same reason — the inbox because an
// inject can land in that window, the alert queue because a subagent
// reporting in can. Firing on none of the three is what keeps a reset
// from driving an empty turn, which is not free: Run with nothing to
// deliver is still a model call.
//
// Resetting one guardrail while the other is still tripped releases
// nothing — the next turn would only be refused by the other one.
func (a *Agent) releaseFencedWake() {
	if a == nil {
		return
	}
	a.mu.Lock()
	stillHalted := a.watchdogTripped || a.costCeilingExceeded
	fenced := a.wakeFenced
	if !stillHalted {
		a.wakeFenced = false
		a.fencedLogReason = ""
	}
	bg := a.bgMgr
	a.mu.Unlock()
	if stillHalted {
		return
	}
	if !fenced && !a.inbox.hasWakingMessages() && (bg == nil || !bg.HasPendingAlerts()) {
		return
	}
	a.RequestWake()
}
