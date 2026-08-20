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
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/go-steer/core-agent/v2/pkg/attach"
)

func TestWakeSignal_FireAndDrain(t *testing.T) {
	t.Parallel()
	w := newWakeSignal()
	w.fire()
	select {
	case <-w.channel():
	case <-time.After(50 * time.Millisecond):
		t.Fatalf("wake channel did not fire after fire()")
	}
}

func TestWakeSignal_CoalescesMultipleFires(t *testing.T) {
	t.Parallel()
	w := newWakeSignal()
	w.fire()
	w.fire()
	w.fire()
	// Should be exactly one pending notification.
	select {
	case <-w.channel():
	default:
		t.Fatalf("expected one pending wake")
	}
	select {
	case <-w.channel():
		t.Errorf("multiple fires should coalesce to one pending notification")
	default:
	}
}

func TestWakeSignal_NilSafe(t *testing.T) {
	t.Parallel()
	var w *wakeSignal
	w.fire() // should not panic
	if ch := w.channel(); ch != nil {
		t.Errorf("nil wakeSignal should return nil channel, got %v", ch)
	}
}

func TestAgent_RequestWakeFiresChannel(t *testing.T) {
	t.Parallel()
	a := &Agent{wake: newWakeSignal()}
	a.RequestWake()
	select {
	case <-a.WakeRequested():
	case <-time.After(50 * time.Millisecond):
		t.Fatalf("WakeRequested did not fire after RequestWake")
	}
}

func TestAgent_RequestWakeNilSafe(t *testing.T) {
	t.Parallel()
	var a *Agent
	a.RequestWake() // should not panic
	if ch := a.WakeRequested(); ch != nil {
		t.Errorf("nil Agent should return nil wake channel")
	}
	a = &Agent{} // no wake field initialized
	a.RequestWake()
	if ch := a.WakeRequested(); ch != nil {
		t.Errorf("Agent without wake signal should return nil channel")
	}
}

func TestAgent_InjectAlsoFiresWake(t *testing.T) {
	t.Parallel()
	a := &Agent{wake: newWakeSignal(), inbox: newInbox()}
	if err := a.Inject("hello"); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	select {
	case <-a.WakeRequested():
	case <-time.After(50 * time.Millisecond):
		t.Fatalf("Inject should fire wake signal so operator input pierces sleep")
	}
}

// recordedEmits collects the typed operator events an agent publishes,
// which is the only view an attached client has of the agent.
type recordedEmits struct {
	mu    sync.Mutex
	types []string
	last  any
}

func (r *recordedEmits) install(a *Agent) {
	a.SetOperatorEventEmitter(func(eventType string, payload any) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.types = append(r.types, eventType)
		r.last = payload
	})
}

func (r *recordedEmits) count(eventType string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, t := range r.types {
		if t == eventType {
			n++
		}
	}
	return n
}

// TestAgent_RequestWakePublishesWakeEvent pins the daemon half of #802:
// a wake has to reach operators who are not in this process. Firing the
// channel alone reaches only in-process subscribers, and an attached
// TUI is at the far end of an SSE stream — #813's fan-out gave
// in-process observers a way to subscribe without stealing the driver's
// wakes, but it cannot put a channel in another process.
func TestAgent_RequestWakePublishesWakeEvent(t *testing.T) {
	t.Parallel()
	a := &Agent{wake: newWakeSignal()}
	var rec recordedEmits
	rec.install(a)

	a.RequestWake()

	if got := rec.count(attach.EventWake); got != 1 {
		t.Fatalf("wake events emitted = %d, want 1 — nothing outside this process can see the wake", got)
	}
	ev, ok := rec.last.(attach.WakeEvent)
	if !ok {
		t.Fatalf("payload = %T, want attach.WakeEvent", rec.last)
	}
	if ev.At.IsZero() {
		t.Error("WakeEvent.At is zero; consumers stamp the notice with it")
	}
}

// TestAgent_InjectDoesNotPublishWakeEvent pins the exclusion. Inject
// fires the same signal internally, but it already published an
// inbox/queued event describing itself; a wake on top would make every
// operator-typed prompt raise an attention notice about the operator's
// own typing.
func TestAgent_InjectDoesNotPublishWakeEvent(t *testing.T) {
	t.Parallel()
	a := &Agent{wake: newWakeSignal(), inbox: newInbox()}
	var rec recordedEmits
	rec.install(a)

	if err := a.Inject("hello"); err != nil {
		t.Fatalf("Inject: %v", err)
	}

	if got := rec.count(attach.EventInbox); got != 1 {
		t.Errorf("inbox events = %d, want 1 (the inject's own frame)", got)
	}
	if got := rec.count(attach.EventWake); got != 0 {
		t.Errorf("wake events = %d, want 0 — inject is reported by its inbox frame, not by a wake", got)
	}
	// The signal itself still fires: the local scheduler must still be
	// interrupted by operator input.
	select {
	case <-a.WakeRequested():
	case <-time.After(50 * time.Millisecond):
		t.Error("Inject no longer pierces an active sleep")
	}
}

// TestAgent_RequestWakePublishesWithoutWakeSignal covers the
// hand-constructed agent: no scheduler is listening, but "something
// asked for a wake" is still true and still worth reporting.
func TestAgent_RequestWakePublishesWithoutWakeSignal(t *testing.T) {
	t.Parallel()
	a := &Agent{} // no wake signal wired
	var rec recordedEmits
	rec.install(a)

	a.RequestWake() // must not panic

	if got := rec.count(attach.EventWake); got != 1 {
		t.Errorf("wake events = %d, want 1", got)
	}
}

// pendingWake reports whether ch has a wake buffered RIGHT NOW, with
// no timer and no sleep. Every assertion below runs after RequestWake
// has returned, and RequestWake completes its fan-out synchronously
// before returning, so "buffered by now" is an ordering fact rather
// than a scheduling hope. Using a timeout here instead would let a
// regression that delivers to only one subscriber pass whenever the
// runtime happened to be generous — which is exactly the hole #802's
// first negative test shipped with.
func pendingWake(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// TestAgent_WakeFansOutToEverySubscriber is the #813 regression.
//
// The defect: cmd/core-agent's local TUI adapter returned the agent's
// own WakeRequested channel to core-tui, so the TUI and the driver
// (pkg/runner's loops, or the SleepScheduler the autonomous driver
// parks in) were two receivers on a channel that delivers each value
// exactly once. One RequestWake reached one of them, chosen by
// whichever got there first, and the other silently saw nothing.
//
// The assertion is deterministic: one RequestWake, then every
// subscriber must ALREADY hold a buffered token. Against the pre-fix
// aliasing shape — SubscribeWake returning a.wake.channel() for
// everyone — there is one token and three receivers, so the first
// pendingWake below drains it and the other two report the loss. The
// aliasing check above it is t.Error rather than t.Fatal precisely so
// that those behavioural rows still run: the cause and the
// consequence both get reported.
func TestAgent_WakeFansOutToEverySubscriber(t *testing.T) {
	t.Parallel()
	a := &Agent{wake: newWakeSignal()}

	// The driver's channel: what the scheduler / REPL / WakeLoop get.
	driver := a.WakeRequested()
	// Two independent observers, e.g. the local TUI adapter.
	obsA, cancelA := a.SubscribeWake()
	defer cancelA()
	obsB, cancelB := a.SubscribeWake()
	defer cancelB()

	if obsA == driver || obsB == driver || obsA == obsB {
		t.Error("subscriptions alias each other; each subscriber needs its own channel or they take turns consuming wakes")
	}

	a.RequestWake()

	for _, sub := range []struct {
		name string
		ch   <-chan struct{}
	}{
		{"driver (WakeRequested)", driver},
		{"observer A (SubscribeWake)", obsA},
		{"observer B (SubscribeWake)", obsB},
	} {
		if !pendingWake(sub.ch) {
			t.Errorf("%s saw no wake after one RequestWake — a wake reached some subscribers and not others", sub.name)
		}
	}
}

// TestAgent_InjectFansOutToEverySubscriber pins the same property for
// the inject path, which fires the signal directly rather than through
// RequestWake (no `wake` event; see injectAs). Operator input piercing
// a sleep and operator input raising a toast are the same wake.
func TestAgent_InjectFansOutToEverySubscriber(t *testing.T) {
	t.Parallel()
	a := &Agent{wake: newWakeSignal(), inbox: newInbox()}
	driver := a.WakeRequested()
	obs, cancel := a.SubscribeWake()
	defer cancel()

	if err := a.Inject("hello"); err != nil {
		t.Fatalf("Inject: %v", err)
	}

	if !pendingWake(driver) {
		t.Error("driver saw no wake after Inject")
	}
	if !pendingWake(obs) {
		t.Error("subscriber saw no wake after Inject")
	}
}

// TestAgent_SubscribeWakeCoalescesPerSubscriber pins the semantics the
// fan-out must NOT change: buffer 1, drop on full, per subscriber. A
// wake means "re-check state", never "process exactly N events" — and
// one subscriber's backlog must not become another's.
func TestAgent_SubscribeWakeCoalescesPerSubscriber(t *testing.T) {
	t.Parallel()
	a := &Agent{wake: newWakeSignal()}
	obs, cancel := a.SubscribeWake()
	defer cancel()

	a.RequestWake()
	a.RequestWake()
	a.RequestWake()

	if !pendingWake(obs) {
		t.Fatal("subscriber saw no wake at all")
	}
	if pendingWake(obs) {
		t.Error("three fires left more than one pending wake; subscriptions must coalesce like the original signal did")
	}
}

// TestAgent_SubscribeWakeLatchesAWakeFiredBeforeAnyoneReads covers the
// ordering pkg/compose's auto-continue depends on: the inject that
// fires the wake happens BEFORE the loop that drains it starts. A
// subscription that only delivers to an actively-parked reader would
// strand that first turn.
func TestAgent_SubscribeWakeLatchesAWakeFiredBeforeAnyoneReads(t *testing.T) {
	t.Parallel()
	a := &Agent{wake: newWakeSignal()}
	obs, cancel := a.SubscribeWake()
	defer cancel()

	a.RequestWake() // nobody is in a select yet

	if !pendingWake(a.WakeRequested()) {
		t.Error("default subscription did not latch a wake fired before the driver started reading")
	}
	if !pendingWake(obs) {
		t.Error("SubscribeWake channel did not latch a wake fired before the subscriber started reading")
	}
}

// TestAgent_SubscribeWakeUnsubscribeStopsDeliveryAndSparesTheRest is
// the teardown half: a TUI that detaches must stop consuming fan-out
// slots, and must not take anyone else's delivery down with it.
func TestAgent_SubscribeWakeUnsubscribeStopsDeliveryAndSparesTheRest(t *testing.T) {
	t.Parallel()
	a := &Agent{wake: newWakeSignal()}
	gone, cancelGone := a.SubscribeWake()
	stays, cancelStays := a.SubscribeWake()
	defer cancelStays()

	cancelGone()
	cancelGone() // idempotent: must not panic or double-remove

	a.RequestWake()

	if pendingWake(gone) {
		t.Error("an unsubscribed channel still received a wake")
	}
	if !pendingWake(stays) {
		t.Error("unsubscribing one subscriber stopped delivery to another")
	}
	if !pendingWake(a.WakeRequested()) {
		t.Error("unsubscribing an observer stopped delivery to the driver")
	}

	// Unsubscribing does not close: a closed channel would turn a
	// select into a spin and reads downstream as "subscription over,
	// permanently". A deregistered one just goes quiet.
	select {
	case _, ok := <-gone:
		t.Errorf("unsubscribed channel yielded a value (ok=%v); it should be silent, not closed", ok)
	default:
	}
}

// TestAgent_SubscribeWakeNilSafe covers the hand-constructed agents
// that pepper this package's tests (and any embedder's): no wake
// signal wired, so the channel is nil — which blocks forever in a
// select, the correct "no wake source" behavior — and the unsubscribe
// is still non-nil so callers can defer it unconditionally.
func TestAgent_SubscribeWakeNilSafe(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		a    *Agent
	}{
		{"nil agent", nil},
		{"agent with no wake signal", &Agent{}},
	} {
		ch, cancel := tc.a.SubscribeWake()
		if ch != nil {
			t.Errorf("%s: SubscribeWake channel = %v, want nil", tc.name, ch)
		}
		if cancel == nil {
			t.Fatalf("%s: SubscribeWake returned a nil unsubscribe; callers defer it unconditionally", tc.name)
		}
		cancel() // must not panic
		tc.a.RequestWake()
	}
}

// TestWakeSignal_ConcurrentFireAndSubscribe is the -race guard on the
// copy-on-write publish: fire reads the subscriber slice with an
// atomic load and no lock precisely so it stays safe on the loop's hot
// path, which is only true if every writer republishes a fresh slice
// instead of mutating the live one.
func TestWakeSignal_ConcurrentFireAndSubscribe(t *testing.T) {
	t.Parallel()
	w := newWakeSignal()

	const workers = 8
	stop := make(chan struct{})
	var background, churn sync.WaitGroup

	background.Add(1)
	go func() {
		defer background.Done()
		for {
			select {
			case <-stop:
				return
			default:
				w.fire()
				runtime.Gosched()
			}
		}
	}()
	// Drain the default subscription so fire keeps hitting a live
	// send rather than always taking the drop branch.
	background.Add(1)
	go func() {
		defer background.Done()
		for {
			select {
			case <-stop:
				return
			case <-w.channel():
			}
		}
	}()

	for i := 0; i < workers; i++ {
		churn.Add(1)
		go func() {
			defer churn.Done()
			for j := 0; j < 50; j++ {
				ch, cancel := w.subscribe()
				runtime.Gosched() // give a fire room to interleave
				select {
				case <-ch:
				default:
				}
				cancel()
			}
		}()
	}

	// The churn workers are bounded by their own loop counts; the two
	// endless goroutines stop on the close.
	churn.Wait()
	close(stop)
	background.Wait()

	// Every subscription was cancelled; only the default remains.
	if got := len(*w.subs.Load()); got != 1 {
		t.Errorf("subscriber count after all cancels = %d, want 1 (the default subscription)", got)
	}
}
