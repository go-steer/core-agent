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
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-steer/core-agent/v2/pkg/attach"
)

// wakeSignal multiplexes an arbitrary number of "wake the loop"
// triggers and fans each one out to every subscriber. Every
// subscription is its own buffered-1 channel written with a
// non-blocking send, so multiple wakes between one subscriber's
// drains coalesce into one pending notification for that subscriber
// — the consumer treats it as "something happened, re-check state"
// rather than "process exactly N events." Coalescing is per
// subscriber: a slow reader drops its own redundant wakes and cannot
// starve or delay anyone else's.
//
// Used as the seam between in-process out-of-band signals (operator
// input via Inject, background alerts via BackgroundAgentManager,
// attach-mode wake from a remote operator) and the things that need
// to hear about them: the sleeping SleepScheduler the driver is
// parked in, and any operator surface that wants to report the wake.
//
// Before #813 there was exactly one channel and every caller of
// Agent.WakeRequested shared it, so a second reader consumed wakes
// the first would never see. See SubscribeWake for the shape that
// replaces that aliasing and Agent.WakeRequested for what the
// original method still means.
type wakeSignal struct {
	// def is the default subscription — the one Agent.WakeRequested
	// hands out. Allocated eagerly by newWakeSignal, never removed,
	// and stable for the agent's lifetime, because callers rely on
	// both halves of that: core-tui re-invokes WakeRequested after
	// every wake to re-arm its listener, and the autonomous driver
	// re-invokes it each time it hands the channel to the scheduler
	// between turns, so a method that minted a fresh subscription per
	// call would leak one per call and lose any wake latched into the
	// discarded one. Eager allocation is what makes a fire that lands
	// before the first reader arrives still latch — pkg/compose's
	// auto-continue depends on exactly that ordering (inject first,
	// wake loop second).
	def chan struct{}

	// subs is the full fan-out set, def included, published
	// copy-on-write so fire needs no lock: an atomic load plus N
	// non-blocking sends, which is what keeps it safe to call from
	// the loop's hot path. mu serializes writers only.
	//
	// nil on a zero-value wakeSignal (one not built by
	// newWakeSignal), which makes fire a no-op there rather than a
	// panic. Nothing constructs one — newWakeSignal is the only
	// construction site in the package — so subscribe does not
	// special-case it; on such a value it would register a working
	// subscription alongside a nil def.
	//
	// A *nil* *wakeSignal, by contrast, is reachable (hand-built Agent
	// structs in tests) and every method here handles it.
	mu   sync.Mutex
	subs atomic.Pointer[[]chan struct{}]
}

func newWakeSignal() *wakeSignal {
	w := &wakeSignal{def: make(chan struct{}, 1)}
	subs := []chan struct{}{w.def}
	w.subs.Store(&subs)
	return w
}

// fire is non-blocking per subscriber: if a wake is already pending
// for one of them, drop the additional fire for that subscriber on
// the floor (coalesced semantics — it will see the existing pending
// one and re-check). Every other subscriber still gets its own.
//
// Takes no lock and never parks, so it stays safe to call while a
// turn is running.
func (w *wakeSignal) fire() {
	if w == nil {
		return
	}
	subs := w.subs.Load()
	if subs == nil {
		return
	}
	for _, ch := range *subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// channel returns the receive end of the default subscription.
// Nil-safe so callers can plumb the channel through context without
// nil-checking at every layer; a nil channel in a select blocks
// forever, which is exactly the right behavior when no wake source
// is wired.
func (w *wakeSignal) channel() <-chan struct{} {
	if w == nil {
		return nil
	}
	return w.def
}

// subscribe registers an independent buffered-1 subscription and
// returns it alongside an idempotent unsubscribe.
//
// The unsubscribe deregisters the channel but deliberately does NOT
// close it. Closing would be a data race against a concurrent fire —
// send on a closed channel panics — which on its own settles it. It
// would also say the wrong thing downstream: a receive on a closed
// channel succeeds immediately and forever, so a caller looping on it
// without checking the ok flag spins, and core-tui, which does check,
// reads it as "this subscription is over, permanently" and stops
// re-arming (tui/agentcmd.go returns a nil msg on !ok). After
// unsubscribing the channel simply goes quiet — the same state a
// caller holding a nil channel is in, which is the semantics the rest
// of this file is already written against. Anything still buffered in
// it stays receivable.
func (w *wakeSignal) subscribe() (<-chan struct{}, func()) {
	if w == nil {
		return nil, func() {}
	}
	ch := make(chan struct{}, 1)

	w.mu.Lock()
	var cur []chan struct{}
	if p := w.subs.Load(); p != nil {
		cur = *p
	}
	next := make([]chan struct{}, len(cur), len(cur)+1)
	copy(next, cur)
	next = append(next, ch)
	w.subs.Store(&next)
	w.mu.Unlock()

	var once sync.Once
	return ch, func() {
		once.Do(func() {
			w.mu.Lock()
			defer w.mu.Unlock()
			var cur []chan struct{}
			if p := w.subs.Load(); p != nil {
				cur = *p
			}
			next := make([]chan struct{}, 0, len(cur))
			for _, c := range cur {
				if c != ch {
					next = append(next, c)
				}
			}
			w.subs.Store(&next)
		})
	}
}

// RequestWake fires the agent's wake signal AND publishes a `wake`
// event to connected operators. Callers, as the tree actually stands:
//
//   - The attach-mode `POST /sessions/<id>/wake` endpoint, when an
//     operator outside the process wants an immediate rescan. This is
//     the only caller in cmd/ or pkg/ that a shipped binary reaches.
//   - BackgroundAgentManager.pushAlert, on every alert a subagent
//     reports. Background alerts are otherwise PULLED at the top of a
//     parent turn, so a child that finishes after the parent's last
//     turn reports into a queue nothing is scheduled to read; the wake
//     is what makes "result will be pushed" true (#780).
//   - autonomous.Handle.RequestWake, the host-facing door, for wakes
//     the host knows about and the runtime doesn't. Nothing in-tree
//     wires it to anything now that the manager wakes for itself;
//     dev/uat/scheduled-monitor is the worked example of a host doing
//     the same thing by hand.
//   - ResumeWith(mode, "", caller) with no message — reachable from a
//     library caller only. `POST /resume` never takes it: attachadapter
//     frames a message for both steer and continue, and a non-empty
//     message short-circuits ResumeWith before the wake.
//
// Operator input via Agent.Inject fires the signal too, but through
// the unexported path — it deliberately does NOT publish the event.
// See injectAs.
//
// The event is published here rather than from the attach handler for
// the same reason emitPause is: an attached operator has to see every
// wake, and the wake paths above are mostly NOT HTTP ones — the host
// wiring and the library call never touch a handler, and putting the
// emit in the handler would make them invisible to exactly the remote
// operator who can't see the process (#802). Publishing is also the
// only way an operator surface OUTSIDE this process can learn about a
// wake: SubscribeWake hands out in-process channels, and an attached
// TUI is at the other end of an SSE stream. (When #802 shipped, the
// event was the only way any operator surface could learn about a
// wake, in-process ones included, because the wake channel was
// single-consumer and a second subscriber would have stolen wakes
// from the autonomous scheduler it exists to interrupt. #813 replaced
// that channel with the fan-out below; the reason to emit here did
// not change.)
//
// emit is a no-op with no operator transport wired, and the attach
// broadcaster's fan-out is non-blocking per subscriber, so this stays
// safe to call from a hot path. The wake fan-out is non-blocking per
// subscriber too and takes no lock at all. It is also safe to call
// while the loop is running: nothing here takes the agent's state
// lock.
//
// No-op when the agent has no wake signal (defensive: hand-constructed
// Agent structs used in tests don't necessarily wire one up) — but the
// event still publishes, because "something asked for a wake" is true
// regardless of whether a scheduler was listening.
func (a *Agent) RequestWake() {
	if a == nil {
		return
	}
	a.wake.fire()
	a.emit(attach.EventWake, attach.WakeEvent{At: time.Now().UTC()})
}

// WakeRequested returns the agent's DEFAULT wake subscription: a
// channel that fires whenever RequestWake (or Inject, which fires the
// same signal internally) is invoked. Buffered(1) coalesced
// semantics: multiple wakes between drains land as one notification.
//
// The same channel every call, for the agent's whole lifetime. Two
// callers rely on that and would break under a per-call
// subscription: core-tui re-invokes it to re-arm its listener after
// every wake, and the autonomous driver re-invokes it on every turn
// that schedules a successor, to attach it to the context it passes
// to Scheduler.BeforeNextTurn, where SleepScheduler selects on it
// alongside its sleep timer and ctx.Done.
//
// Which is also the one caveat: because it is the same channel, two
// concurrent readers of THIS method still race each other for each
// value, exactly as they did before #813. That is fine for its
// callers as the tree stands, because they are the agent's driver —
// pkg/runner's REPL loop, pkg/runner.WakeLoop, and the autonomous
// scheduler — and an agent has exactly one driver at a time. Anything
// that merely wants to OBSERVE wakes alongside a driver must call
// SubscribeWake instead; that is the bug #813 fixed in the local TUI
// adapter, which used to hand this channel to core-tui and take turns
// with the scheduler for each wake.
func (a *Agent) WakeRequested() <-chan struct{} {
	if a == nil {
		return nil
	}
	return a.wake.channel()
}

// SubscribeWake registers an independent wake subscription and
// returns its receive end plus an unsubscribe func. Every subscriber
// sees every wake: RequestWake and Inject fan out to all of them with
// one non-blocking send apiece, so a subscriber that isn't reading
// drops only its own redundant wakes.
//
// Same per-channel semantics as WakeRequested — buffer 1, drop on
// full, "something happened, re-check state" rather than an event
// count. A wake fired after the subscription exists but before the
// caller starts reading is latched, not lost; one fired before it
// exists is not, so subscribe before the agent goes live if that
// window matters. (WakeRequested's channel has no such window: it is
// allocated with the agent.)
//
// This is what an operator surface should use. The local `--tui`
// adapter is the in-tree caller: it holds one subscription for the
// lifetime of the TUI so an operator's toast and the driver's wake no
// longer consume each other (#813).
//
// Lifecycle: the returned func is idempotent and safe to call from
// any goroutine. It deregisters the subscription; it does NOT close
// the channel, because closing races a concurrent fire and because a
// closed channel reads downstream as a permanently-ended
// subscription rather than a quiet one. An unsubscribed channel goes
// silent, which is what a select wants. Call it — a subscription
// nobody releases is retained for the agent's lifetime, which is one
// leaked channel per TUI attach or per host session that forgets.
//
// Nil-safe on both a nil *Agent and a hand-constructed Agent with no
// wake signal wired: the channel comes back nil (a nil channel in a
// select blocks forever, the correct "no wake source" behavior) and
// the unsubscribe is a non-nil no-op, so callers can `defer` it
// unconditionally.
func (a *Agent) SubscribeWake() (<-chan struct{}, func()) {
	if a == nil {
		return nil, func() {}
	}
	return a.wake.subscribe()
}
