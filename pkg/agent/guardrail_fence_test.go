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

// #1040: a watchdog-halted session with queued inbox input re-drives
// and re-refuses forever.
//
// Most of these fail on pre-fix code; three are deliberate both-ways
// guards and are named as such on the tests themselves —
// TestInject_Healthy_StillWakes, TestReset_NothingQueued_DoesNotWake and
// TestQueueAsContext_WhileHalted_NeitherWakesNorFences pin the
// behaviour the fence must NOT change, so passing before and after is
// the point.
//
// What none of them cover: the loop itself. Every test here pokes the
// flags on a hand-built Agent and asserts on the wake channel, rather
// than going through Run. That is a real gap — a new wake-firing path
// added inside Run would not be caught here — and it is left open
// deliberately, because a Run-level test needs a mock model and a
// driver, and the thing it would assert (no re-drive over tens of
// minutes, unattended) is what the GKE drill measures.

package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/attach"
	"github.com/go-steer/core-agent/v2/pkg/auth"
)

// fencedAgent is the minimal Agent a wake/inbox test needs: the two
// fields Inject touches plus the wake signal it fires.
func fencedAgent(t *testing.T) *Agent {
	t.Helper()
	return &Agent{inbox: newInbox(), wake: newWakeSignal()}
}

// pendingAlertManager is a SubagentManager that only answers the one
// question releaseFencedWake asks it. Everything else is a zero value:
// the fence never spawns, lists or drains.
type pendingAlertManager struct{ pending bool }

func (m *pendingAlertManager) AttachParent(*Agent)                  {}
func (m *pendingAlertManager) PrependPendingAlerts(p string) string { return p }
func (m *pendingAlertManager) HasPendingAlerts() bool               { return m.pending }
func (m *pendingAlertManager) ListSubagents() []attach.AgentInfo    { return nil }
func (m *pendingAlertManager) ListSubagentCatalog() []attach.SubagentCatalogInfo {
	return nil
}
func (m *pendingAlertManager) SpawnSubagent(context.Context, attach.SubagentSpec) (attach.SubagentSpawnResponse, error) {
	return attach.SubagentSpawnResponse{}, nil
}
func (m *pendingAlertManager) StopSubagent(string) (attach.StopAgentOutcome, error) {
	return attach.StopAgentOutcome{}, nil
}

// woke reports whether a wake is latched on the default subscription,
// consuming it. Non-blocking: the channel is buffered-1 and every fire
// is a non-blocking send, so a pending wake is readable immediately.
func woke(a *Agent) bool {
	select {
	case <-a.WakeRequested():
		return true
	default:
		return false
	}
}

// TestInject_WhileWatchdogHalted_QueuesWithoutWaking is the core of
// #1040. Pre-fix, injectAs fires the wake unconditionally, so the wake
// loop calls Run, Run's pre-flight refuses before it ever reaches
// drainInboxFull, and the next inject does it again — thirteen times
// over 51 minutes on the live rig.
func TestInject_WhileWatchdogHalted_QueuesWithoutWaking(t *testing.T) {
	t.Parallel()
	a := fencedAgent(t)
	a.watchdogTripped = true
	a.watchdogReason = "watchdog halted the agent (no-op-streak): three inert mark_task_done calls."

	if err := a.Inject("the cluster is still broken"); err != nil {
		t.Fatalf("Inject: %v", err)
	}

	if woke(a) {
		t.Errorf("a halted agent must not wake a driver that can only be refused (#1040)")
	}
	// Fenced, not dropped: the queue is the only thing protecting this
	// message, and draining it on refusal would lose it.
	if got := a.PendingInboxCount(); got != 1 {
		t.Errorf("PendingInboxCount = %d, want 1 — the fence must not discard the message", got)
	}
}

// TestInject_WhileCostCeilingHalted_QueuesWithoutWaking is the other
// arm. The issue was filed against the watchdog, but preflightCostCeiling
// sits on the same side of the same drain (agent.go), so the livelock is
// a property of the refusal, not of the watchdog.
func TestInject_WhileCostCeilingHalted_QueuesWithoutWaking(t *testing.T) {
	t.Parallel()
	a := fencedAgent(t)
	a.costCeilingExceeded = true
	a.costCeilingReason = "per-session cost ceiling exceeded: session has cost $5.0000"

	if err := a.Inject("keep going"); err != nil {
		t.Fatalf("Inject: %v", err)
	}

	if woke(a) {
		t.Errorf("a cost-ceiling halt must fence the wake too (#1040)")
	}
	if got := a.PendingInboxCount(); got != 1 {
		t.Errorf("PendingInboxCount = %d, want 1", got)
	}
}

// TestInject_Healthy_StillWakes is the guard against fixing #1040 by
// breaking every unattended daemon: the fence must be invisible when
// nothing is tripped.
func TestInject_Healthy_StillWakes(t *testing.T) {
	t.Parallel()
	a := fencedAgent(t)

	if err := a.Inject("diagnose the failing pod"); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if !woke(a) {
		t.Errorf("an inject into a healthy agent must fire the wake — this is the whole wake-loop contract")
	}
}

// TestResetWatchdog_ReleasesFencedWake: the fenced message has to land
// somewhere. The reset is the door, and without this the operator's
// message sits in the inbox until the next inject happens to arrive —
// or until drop-oldest eats it.
func TestResetWatchdog_ReleasesFencedWake(t *testing.T) {
	t.Parallel()
	a := fencedAgent(t)
	a.watchdogTripped = true
	a.watchdogReason = "halt"

	if err := a.Inject("status?"); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if woke(a) {
		t.Fatalf("precondition: the wake should have been fenced")
	}

	a.ResetWatchdog()

	if !woke(a) {
		t.Errorf("clearing the halt must release the fenced wake so the queued input drives the next turn (#1040)")
	}
	if got := a.PendingInboxCount(); got != 1 {
		t.Errorf("PendingInboxCount = %d, want 1 — the reset drives the message, it does not consume it", got)
	}
}

// TestResetCostCeiling_ReleasesFencedWake is the cost-ceiling arm of
// the release.
func TestResetCostCeiling_ReleasesFencedWake(t *testing.T) {
	t.Parallel()
	a := fencedAgent(t)
	a.costCeilingExceeded = true
	a.costCeilingReason = "trip"

	if err := a.Inject("status?"); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if woke(a) {
		t.Fatalf("precondition: the wake should have been fenced")
	}

	a.ResetCostCeiling()

	if !woke(a) {
		t.Errorf("clearing the ceiling must release the fenced wake (#1040)")
	}
}

// TestReset_OneOfTwoHalts_StaysFenced: resetting the watchdog while the
// cost ceiling is still tripped must not release anything. The next
// turn would be refused by the ceiling instead, which is the same
// livelock with a different label on it.
func TestReset_OneOfTwoHalts_StaysFenced(t *testing.T) {
	t.Parallel()
	a := fencedAgent(t)
	a.watchdogTripped = true
	a.costCeilingExceeded = true

	if err := a.Inject("status?"); err != nil {
		t.Fatalf("Inject: %v", err)
	}

	a.ResetWatchdog()
	if woke(a) {
		t.Errorf("the cost ceiling is still tripped; releasing here just moves which pre-flight refuses")
	}

	a.ResetCostCeiling()
	if !woke(a) {
		t.Errorf("clearing the LAST halt must release the fenced wake")
	}
}

// TestReset_NothingQueued_DoesNotWake: a reset on a quiet session must
// not manufacture a turn. The wake loop runs Run(ctx, "") on every
// wake, so an unsolicited fire here is a real, billable, empty turn.
func TestReset_NothingQueued_DoesNotWake(t *testing.T) {
	t.Parallel()
	a := fencedAgent(t)
	a.watchdogTripped = true

	a.ResetWatchdog()

	if woke(a) {
		t.Errorf("a reset with an empty inbox and no fenced wake must not drive a turn")
	}
}

// TestReset_ReleasesQueuedInputWithNothingFenced covers the ordering
// the fenced flag alone misses: the watchdog trips in the POST-TURN
// hook, so the wake that drove the turn was already consumed and the
// message that arrived with it was queued while nothing was tripped.
// Nothing is fenced, but a turn is still owed.
func TestReset_ReleasesQueuedInputWithNothingFenced(t *testing.T) {
	t.Parallel()
	a := fencedAgent(t)

	// Queued healthy — this fires and the (notional) driver consumes it.
	if err := a.Inject("look at the other namespace too"); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if !woke(a) {
		t.Fatalf("precondition: a healthy inject wakes")
	}
	// ...and the turn it drove trips the watchdog on its way out.
	a.watchdogTripped = true
	a.watchdogReason = "halt"

	a.ResetWatchdog()

	if !woke(a) {
		t.Errorf("input queued before the trip must still be driven by the reset (#1040)")
	}
}

// TestQueueAsContext_WhileHalted_NeitherWakesNorFences: a deferred
// message never wanted a wake, so it must not arm the fence either —
// otherwise a later reset drives a turn nobody asked for.
func TestQueueAsContext_WhileHalted_NeitherWakesNorFences(t *testing.T) {
	t.Parallel()
	a := fencedAgent(t)
	a.watchdogTripped = true

	if err := a.QueueAsContext(context.Background(), "background colour", auth.Caller{}); err != nil {
		t.Fatalf("QueueAsContext: %v", err)
	}

	a.ResetWatchdog()

	if woke(a) {
		t.Errorf("a deferred message must not cause the reset to drive a turn — not waking is the feature (#698)")
	}
}

// TestRequestWake_WhileHalted_FencesButStillEmits: RequestWake's other
// caller is BackgroundAgentManager.pushAlert, a machine path that would
// otherwise re-drive a halted session on every subagent report. The
// `wake` EVENT still goes out: an operator watching a halted session
// needs to see that work is still arriving for it.
func TestRequestWake_WhileHalted_FencesButStillEmits(t *testing.T) {
	t.Parallel()
	a := fencedAgent(t)
	a.watchdogTripped = true
	var kinds []string
	a.SetOperatorEventEmitter(func(kind string, _ any) { kinds = append(kinds, kind) })

	a.RequestWake()

	if woke(a) {
		t.Errorf("RequestWake must be fenced by a halt — pushAlert is a machine caller (#1040)")
	}
	var sawWake bool
	for _, k := range kinds {
		if k == attach.EventWake {
			sawWake = true
		}
	}
	if !sawWake {
		t.Errorf("the wake EVENT must still publish; kinds = %v", kinds)
	}
}

// TestPreflightRefusal_IsNotReadAsAFreshTrip: question 2 of #1040.
// Thirteen `watchdog halted the agent` lines for one halt is a log that
// reads as thirteen runaway detections, and that is why the livelock
// took three passes over the daemon log to spot.
func TestPreflightRefusal_IsNotReadAsAFreshTrip(t *testing.T) {
	t.Parallel()
	trip := "watchdog halted the agent (no-op-streak): three inert calls. Agent will refuse new turns until the operator resets it (/guardrail reset, or POST /sessions/{id}/guardrails/reset)."

	a := &Agent{watchdogTripped: true, watchdogReason: trip}
	err := a.preflightWatchdog()
	if err == nil {
		t.Fatalf("a tripped watchdog must refuse the next turn")
	}
	if !strings.HasPrefix(err.Error(), refusalPrefix) {
		t.Errorf("a refusal must announce itself as one, not repeat the halt verbatim; got %q", err.Error())
	}
	// The halt text still follows, so the daemon-log line stays
	// self-contained. (Log line, not operator frame — a refused turn
	// emits nothing; see preflightWatchdog.)
	if !strings.Contains(err.Error(), "no-op-streak") {
		t.Errorf("the refusal must still carry the halt's reason; got %q", err.Error())
	}
	// And the classification must not have moved — a refused turn that
	// stops being labelled `watchdog` is invisible on a dashboard.
	if got := attach.ClassifyTurnError(err); got.Kind != attach.TurnErrorWatchdog {
		t.Errorf("Kind = %q, want %q", got.Kind, attach.TurnErrorWatchdog)
	}

	c := &Agent{costCeilingExceeded: true, costCeilingReason: "per-turn cost ceiling exceeded: this turn cost $0.2000"}
	cerr := c.preflightCostCeiling()
	if cerr == nil {
		t.Fatalf("a tripped ceiling must refuse the next turn")
	}
	if !strings.HasPrefix(cerr.Error(), refusalPrefix) {
		t.Errorf("the cost-ceiling refusal must announce itself too; got %q", cerr.Error())
	}
	if got := attach.ClassifyTurnError(cerr); got.Kind != attach.TurnErrorCostCeiling {
		t.Errorf("Kind = %q, want %q", got.Kind, attach.TurnErrorCostCeiling)
	}
}

// TestInject_WhileHalted_StillWakesObservers is the finding the
// adversarial pass caught: fencing at wakeSignal.fire would swallow
// EVERY subscription, not just the driver's, which re-creates the #813
// aliasing one halt at a time.
//
// It matters most on exactly the path #1040 is about. injectAs
// deliberately publishes no `wake` event, and the `inbox` frame it does
// publish goes to the attach SSE stream — which a LOCAL --tui session
// does not read. So for an operator sitting at the terminal, the
// SubscribeWake channel is the only in-process signal that a message
// arrived at all. Fence it and the inject becomes invisible.
func TestInject_WhileHalted_StillWakesObservers(t *testing.T) {
	t.Parallel()
	a := fencedAgent(t)
	obs, unsub := a.SubscribeWake()
	defer unsub()
	a.watchdogTripped = true
	a.watchdogReason = "watchdog halted the agent: no-op-streak"

	if err := a.Inject("the cluster is on fire"); err != nil {
		t.Fatalf("Inject: %v", err)
	}

	if woke(a) {
		t.Errorf("the DRIVER must not be woken — the turn could only be refused")
	}
	select {
	case <-obs:
	default:
		t.Errorf("an OBSERVER must still be woken: on a local TUI this is the only signal that input arrived on a halted session")
	}
}

// TestReleaseFencedWake_SeesPendingBackgroundAlerts covers the second
// queue Run drains. Same post-turn-trip ordering as
// TestReset_ReleasesQueuedInputWithNothingFenced, but the waiting input
// is a subagent's alert rather than an operator's message: the alert's
// wake fired while healthy and was consumed by the turn that then
// tripped, so nothing is fenced and the inbox is empty. Reading only
// those two would strand the alert until some unrelated wake arrived.
func TestReleaseFencedWake_SeesPendingBackgroundAlerts(t *testing.T) {
	t.Parallel()
	a := fencedAgent(t)
	a.bgMgr = &pendingAlertManager{pending: true}
	a.watchdogTripped = true

	a.ResetWatchdog()

	if !woke(a) {
		t.Errorf("a reset with an alert still pending must drive a turn to deliver it")
	}
}

// TestReleaseFencedWake_NoPendingAlertsStaysQuiet is the other half:
// the alert check must not become an unconditional wake. A reset with
// all three queues empty still drives nothing, because Run with nothing
// to deliver is a model call for no reason.
func TestReleaseFencedWake_NoPendingAlertsStaysQuiet(t *testing.T) {
	t.Parallel()
	a := fencedAgent(t)
	a.bgMgr = &pendingAlertManager{pending: false}
	a.watchdogTripped = true

	a.ResetWatchdog()

	if woke(a) {
		t.Errorf("a reset with nothing pending anywhere must not drive an empty turn")
	}
}

// TestHasWakingMessages_ClosedInboxIsNotWaiting pins that an evicted
// session does not get a wake for leftovers. inbox.close sets the flag
// without clearing the slice, and WakeLoop closes the inbox on eviction
// and shutdown — so the driver is gone and a wake would promise a turn
// nothing will run.
func TestHasWakingMessages_ClosedInboxIsNotWaiting(t *testing.T) {
	t.Parallel()
	a := fencedAgent(t)
	if err := a.Inject("still here"); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if !a.inbox.hasWakingMessages() {
		t.Fatalf("an open inbox holding an inject must report waiting input")
	}

	a.inbox.close()

	if a.inbox.hasWakingMessages() {
		t.Errorf("a CLOSED inbox has no driver left to wake, however many messages remain in it")
	}
}

// TestGuardrailHalted_AnswersForBothGuardrails pins the predicate
// pkg/compose's auto-continue stands down on. Asking WatchdogTripped
// alone is the bug it replaces: a session halted by the COST CEILING
// answers false there, and auto-continue would queue a note into it.
func TestGuardrailHalted_AnswersForBothGuardrails(t *testing.T) {
	t.Parallel()
	if halted, _ := (&Agent{}).GuardrailHalted(); halted {
		t.Errorf("a healthy agent must report no halt")
	}
	w := &Agent{watchdogTripped: true, watchdogReason: "no-op-streak"}
	halted, reason := w.GuardrailHalted()
	if !halted || reason != "no-op-streak" {
		t.Errorf("watchdog halt: got (%v, %q), want (true, %q)", halted, reason, "no-op-streak")
	}
	c := &Agent{costCeilingExceeded: true, costCeilingReason: "turn cost $0.20"}
	halted, reason = c.GuardrailHalted()
	if !halted || reason != "turn cost $0.20" {
		t.Errorf("ceiling halt: got (%v, %q), want (true, %q)", halted, reason, "turn cost $0.20")
	}
	// Both at once: the reason names the watchdog, because that is the
	// halt an operator has to clear deliberately — clearing the ceiling
	// alone would leave the session still refusing turns, and a message
	// that named the ceiling would have pointed them at the wrong one.
	// The stand-down itself is the same either way; only the prose an
	// operator reads depends on this order.
	both := &Agent{
		watchdogTripped:     true,
		watchdogReason:      "no-op-streak",
		costCeilingExceeded: true,
		costCeilingReason:   "turn cost $0.20",
	}
	halted, reason = both.GuardrailHalted()
	if !halted || reason != "no-op-streak" {
		t.Errorf("both tripped: got (%v, %q), want (true, %q) — the watchdog is named first", halted, reason, "no-op-streak")
	}
}
