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
	"sync/atomic"
	"testing"
	"time"
)

func TestEvictBefore_RemovesIdleEntries(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	if _, err := reg.Register(&stubRegistrant{app: "core-agent", user: "u", sid: "sess-idle"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	// Backdate the entry's lastTouchedNs so the cutoff catches it.
	reg.mu.RLock()
	for _, e := range reg.byTriple {
		e.lastTouchedNs.Store(time.Now().Add(-2 * time.Hour).UnixNano())
	}
	reg.mu.RUnlock()

	n := reg.EvictBefore(time.Now().Add(-1 * time.Hour))
	if n != 1 {
		t.Errorf("EvictBefore returned %d, want 1", n)
	}
	if got := reg.Len(); got != 0 {
		t.Errorf("registry.Len after evict: got %d, want 0", got)
	}
}

func TestEvictBefore_SkipsFreshEntries(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	if _, err := reg.Register(&stubRegistrant{app: "core-agent", user: "u", sid: "sess-fresh"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	// Cutoff in the past — a just-registered entry (touched at
	// registration time = now) is newer than the cutoff.
	n := reg.EvictBefore(time.Now().Add(-1 * time.Hour))
	if n != 0 {
		t.Errorf("fresh entry should not be evicted; got EvictBefore = %d", n)
	}
	if got := reg.Len(); got != 1 {
		t.Errorf("registry.Len after no-op sweep: got %d, want 1", got)
	}
}

func TestEvictBefore_FiresCancelOnEvict(t *testing.T) {
	t.Parallel()
	// Cancel-on-evict is what stops the per-session wake loop.
	// Verify the func is called before the entry disappears.
	reg := NewSessionRegistry()
	var cancelFired atomic.Bool
	cancel := func() { cancelFired.Store(true) }
	if _, err := reg.RegisterOwnedWithCancel(&stubRegistrant{app: "core-agent", user: "u", sid: "sess-cancel"}, "alice@example.com", cancel); err != nil {
		t.Fatalf("RegisterOwnedWithCancel: %v", err)
	}
	// Backdate so the sweep catches it.
	reg.mu.RLock()
	for _, e := range reg.byTriple {
		e.lastTouchedNs.Store(time.Now().Add(-2 * time.Hour).UnixNano())
	}
	reg.mu.RUnlock()

	reg.EvictBefore(time.Now().Add(-1 * time.Hour))
	if !cancelFired.Load() {
		t.Error("cancelOnEvict should have fired during eviction")
	}
}

func TestEvictBefore_MixedFreshAndIdleEntries(t *testing.T) {
	t.Parallel()
	// Two entries, one idle, one fresh — only the idle one is evicted.
	reg := NewSessionRegistry()
	for _, sid := range []string{"sess-a", "sess-b"} {
		if _, err := reg.Register(&stubRegistrant{app: "core-agent", user: "u", sid: sid}); err != nil {
			t.Fatalf("Register %s: %v", sid, err)
		}
	}
	// Backdate only sess-a.
	reg.mu.RLock()
	for k, e := range reg.byTriple {
		if k.SID == "sess-a" {
			e.lastTouchedNs.Store(time.Now().Add(-2 * time.Hour).UnixNano())
		}
	}
	reg.mu.RUnlock()

	n := reg.EvictBefore(time.Now().Add(-1 * time.Hour))
	if n != 1 {
		t.Errorf("EvictBefore returned %d, want 1", n)
	}
	// Only sess-b should remain.
	if got := reg.Len(); got != 1 {
		t.Errorf("registry.Len: got %d, want 1", got)
	}
	if _, err := reg.Lookup(context.Background(), "core-agent", "sess-b"); err != nil {
		t.Errorf("sess-b (fresh) should still be present: %v", err)
	}
}

func TestLookup_TouchesEntry(t *testing.T) {
	t.Parallel()
	// A memory-hit Lookup must bump lastTouchedNs — otherwise
	// active sessions still get evicted despite regular polls.
	reg := NewSessionRegistry()
	if _, err := reg.Register(&stubRegistrant{app: "core-agent", user: "u", sid: "sess-poll"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	// Backdate.
	oldTs := time.Now().Add(-2 * time.Hour).UnixNano()
	reg.mu.RLock()
	for _, e := range reg.byTriple {
		e.lastTouchedNs.Store(oldTs)
	}
	reg.mu.RUnlock()

	if _, err := reg.Lookup(context.Background(), "core-agent", "sess-poll"); err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	// After the hit, lastTouchedNs must be > oldTs.
	reg.mu.RLock()
	var afterTs int64
	for _, e := range reg.byTriple {
		afterTs = e.lastTouchedNs.Load()
	}
	reg.mu.RUnlock()
	if afterTs <= oldTs {
		t.Errorf("Lookup didn't touch entry: before=%d after=%d", oldTs, afterTs)
	}
}

func TestEntry_TouchAndLastTouchedAt(t *testing.T) {
	t.Parallel()
	// Public LastTouchedAt() should reflect the internal touch()
	// (used by the broadcaster + registry).
	e := &Entry{AppName: "a", SessionID: "s"}
	if !e.LastTouchedAt().IsZero() {
		t.Errorf("uninitialized: want zero, got %v", e.LastTouchedAt())
	}
	before := time.Now()
	e.touch()
	got := e.LastTouchedAt()
	if got.Before(before) {
		t.Errorf("touch didn't bump timestamp: before=%v got=%v", before, got)
	}
}

func TestSweepIdle_DisabledOnZero(t *testing.T) {
	t.Parallel()
	// idleAfter <= 0 must return immediately — the config's "0s
	// disables the sweep" contract. If SweepIdle blocked on the
	// ticker despite the zero, this test would time out.
	reg := NewSessionRegistry()
	done := make(chan struct{})
	go func() {
		reg.SweepIdle(context.Background(), 0)
		close(done)
	}()
	select {
	case <-done:
		// Expected.
	case <-time.After(100 * time.Millisecond):
		t.Fatal("SweepIdle(0) did not return; the zero-timeout contract is broken")
	}
}

func TestSweepIdle_ExitsOnContextCancel(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		// idleAfter > 0 keeps the ticker alive; cancel is the exit path.
		reg.SweepIdle(ctx, time.Hour)
		close(done)
	}()
	cancel()
	select {
	case <-done:
		// Expected.
	case <-time.After(500 * time.Millisecond):
		t.Fatal("SweepIdle did not exit on ctx cancel")
	}
}

func TestUnregister_FiresCancelOnEvict(t *testing.T) {
	t.Parallel()
	// Unregister is the explicit-removal path (agent shutdown,
	// future DELETE /sessions). It should also fire the cancel.
	reg := NewSessionRegistry()
	var cancelFired atomic.Bool
	cancel := func() { cancelFired.Store(true) }
	if _, err := reg.RegisterOwnedWithCancel(&stubRegistrant{app: "core-agent", user: "u", sid: "sess-un"}, "alice@example.com", cancel); err != nil {
		t.Fatalf("RegisterOwnedWithCancel: %v", err)
	}
	reg.Unregister("core-agent", "u", "sess-un")
	if !cancelFired.Load() {
		t.Error("cancelOnEvict should fire on Unregister")
	}
}

func TestTouchEntry_NoopOnMiss(t *testing.T) {
	t.Parallel()
	// Silent no-op — the entry may have been evicted between the
	// caller's Lookup and Touch. Must not panic.
	reg := NewSessionRegistry()
	reg.TouchEntry("core-agent", "no-such-sid")
}

func TestTouchEntry_BumpsLastTouched(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	if _, err := reg.Register(&stubRegistrant{app: "core-agent", user: "u", sid: "sess-touch"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	oldTs := time.Now().Add(-1 * time.Hour).UnixNano()
	reg.mu.RLock()
	for _, e := range reg.byTriple {
		e.lastTouchedNs.Store(oldTs)
	}
	reg.mu.RUnlock()

	reg.TouchEntry("core-agent", "sess-touch")

	reg.mu.RLock()
	var afterTs int64
	for _, e := range reg.byTriple {
		afterTs = e.lastTouchedNs.Load()
	}
	reg.mu.RUnlock()
	if afterTs <= oldTs {
		t.Errorf("TouchEntry didn't bump: before=%d after=%d", oldTs, afterTs)
	}
}
