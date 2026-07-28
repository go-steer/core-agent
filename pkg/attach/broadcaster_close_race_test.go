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

package attach

import (
	"context"
	"sync"
	"testing"
	"time"
)

// drainUntilClosed consumes frames until the channel closes, failing
// the test if it doesn't close within the deadline (a subscriber that
// never terminates means Close missed it — or b.mu is stuck locked
// and replay/detach can't run).
func drainUntilClosed(t *testing.T, ch <-chan Frame, what string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatalf("%s: channel never closed", what)
		}
	}
}

// TestBroadcaster_SubscribeRacingClose pins the #483 fix. Handlers
// resolve a broadcaster via pool.For BEFORE subscribing, and DELETE
// /sessions runs pool.Remove + Close in between — so Subscribe can
// execute against an already-closed broadcaster. Pre-fix that was
// `panic: assignment to entry in nil map` inside Subscribe while it
// held b.mu with no deferred unlock: net/http recovered the panic and
// the mutex stayed locked forever, wedging every later operation on
// the broadcaster. The near-miss interleaving (registration wins by a
// hair) spawned goroutines after Close's wg.Wait had returned — the
// exact use-after-close on the eventlog that #424 fenced.
//
// Post-fix: Subscribe checks a closed flag under b.mu (deferred
// unlock) and returns an already-closed channel; both wg.Adds happen
// inside the same critical section so Close's wg.Wait can't miss a
// racing joiner. 200 iterations under -race in CI.
func TestBroadcaster_SubscribeRacingClose(t *testing.T) {
	t.Parallel()

	h, cleanupLog := openTestEventLog(t)
	defer cleanupLog()

	reg := NewSessionRegistry()
	ag := &eventfulRegistrant{
		stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "race"},
		handle:         h,
	}
	entry, err := reg.Register(ag)
	if err != nil {
		t.Fatal(err)
	}
	seedTestEvents(t, h, "core-agent", "u", "race", 5)

	for i := 0; i < 200; i++ {
		b, err := newBroadcaster(entry)
		if err != nil {
			t.Fatalf("newBroadcaster: %v", err)
		}

		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			// Either registers (Close hangs it up) or observes closed
			// (immediately-closed channel) — both must terminate.
			drainUntilClosed(t, b.Subscribe(context.Background(), 0), "racing Subscribe")
		}()
		go func() {
			defer wg.Done()
			<-start
			b.Close()
		}()
		close(start)
		wg.Wait()

		// The broadcaster must remain usable-shaped after the race:
		// a post-Close Subscribe returns a closed channel instead of
		// panicking, and b.mu must not be left locked by a recovered
		// panic (pre-fix failure mode) — drain would hang forever.
		drainUntilClosed(t, b.Subscribe(context.Background(), 0), "Subscribe after Close")
		b.Close() // idempotent
	}
}
