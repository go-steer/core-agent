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

// RequestWake fires the agent's wake signal. Used by:
//
//   - BackgroundAgentManager (via a driver-side goroutine) to wake a
//     sleeping supervisor as soon as a child alert arrives, instead of
//     waiting for the supervisor's next scheduled wake.
//   - The future attach-mode `POST /sessions/<id>/wake` endpoint, when
//     an operator outside the process wants an immediate rescan.
//   - Operator input via Agent.Inject — Inject calls RequestWake
//     internally so a typed command also pierces an active sleep.
//
// No-op when the agent has no wake signal (defensive: hand-constructed
// Agent structs used in tests don't necessarily wire one up).
func (a *Agent) RequestWake() {
	if a == nil {
		return
	}
	a.wake.fire()
}

// WakeRequested returns a channel that fires whenever RequestWake (or
// Inject, which calls RequestWake internally) is invoked. The
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
