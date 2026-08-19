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
	"time"

	"github.com/go-steer/core-agent/v2/pkg/attach"
)

// wakeSignal multiplexes an arbitrary number of "wake the loop"
// triggers into a single buffered channel the autonomous driver's
// scheduler can select on. Buffer 1 with non-blocking send means
// multiple wakes between consumer drains coalesce into one pending
// notification — the consumer treats it as "something happened,
// re-check state" rather than "process exactly N events."
//
// Used as the seam between in-process out-of-band signals (operator
// input via Inject, background alerts via BackgroundAgentManager,
// future attach-mode wake from a remote operator) and the sleeping
// SleepScheduler that needs to be interrupted.
type wakeSignal struct {
	ch chan struct{}
}

func newWakeSignal() *wakeSignal {
	return &wakeSignal{ch: make(chan struct{}, 1)}
}

// fire is non-blocking: if a wake is already pending, drop the
// additional fire on the floor (coalesced semantics — the consumer
// will see the existing pending one and re-check).
func (w *wakeSignal) fire() {
	if w == nil {
		return
	}
	select {
	case w.ch <- struct{}{}:
	default:
	}
}

// channel returns the receive end. Nil-safe so callers can plumb the
// channel through context without nil-checking at every layer; a nil
// channel in a select blocks forever, which is exactly the right
// behavior when no wake source is wired.
func (w *wakeSignal) channel() <-chan struct{} {
	if w == nil {
		return nil
	}
	return w.ch
}

// RequestWake fires the agent's wake signal AND publishes a `wake`
// event to connected operators. Callers, as the tree actually stands:
//
//   - The attach-mode `POST /sessions/<id>/wake` endpoint, when an
//     operator outside the process wants an immediate rescan. This is
//     the only caller in cmd/ or pkg/ that a shipped binary reaches.
//   - autonomous.Handle.RequestWake, the host-facing door. Nothing
//     in-tree wires it to anything; the intended shape is a driver-side
//     goroutine on BackgroundAgentManager.Alerts() so a child alert
//     wakes a sleeping supervisor instead of waiting for its next
//     scheduled wake. dev/uat/scheduled-monitor does exactly that and
//     is the worked example.
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
// only way an operator surface can learn about a wake at all: the wake
// CHANNEL is single-consumer with a buffer of one, so a second
// subscriber would steal wakes from the autonomous scheduler that the
// channel exists to interrupt.
//
// emit is a no-op with no operator transport wired, and the attach
// broadcaster's fan-out is non-blocking per subscriber, so this stays
// safe to call from a hot path. It is also safe to call while the loop
// is running: nothing here takes the agent's state lock.
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

// WakeRequested returns a channel that fires whenever RequestWake (or
// Inject, which fires the same signal internally) is invoked. The
// autonomous driver attaches this channel to the context it passes to
// Scheduler.BeforeNextTurn so SleepScheduler can select on it
// alongside its sleep timer and ctx.Done. Buffered(1) coalesced
// semantics: multiple wakes between consumer drains land as one
// notification.
func (a *Agent) WakeRequested() <-chan struct{} {
	if a == nil {
		return nil
	}
	return a.wake.channel()
}
