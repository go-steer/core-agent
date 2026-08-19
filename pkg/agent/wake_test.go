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
// channel alone reaches only the local scheduler, and an attached TUI
// cannot subscribe to that channel — it is single-consumer with a
// buffer of one, so a second reader would steal wakes from the loop the
// signal exists to interrupt.
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
