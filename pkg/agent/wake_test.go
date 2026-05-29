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
	"testing"
	"time"
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
