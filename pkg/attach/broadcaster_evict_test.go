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
	"net/http"
	"testing"
	"time"

	"github.com/go-steer/core-agent/v2/pkg/eventlog"
)

// The #486 fixes: the broadcaster pool keys by (app, user, sid), so
// after an idle-evict + lazy resume (or an Unregister + re-register)
// the fresh *Entry shares the triple and pool.For handed back the
// OLD broadcaster — boot snapshots and SetAttachEmitter bound to the
// dead agent, and a pump whose touch() kept refreshing the stale
// entry so the resumed session looked idle to the sweep while
// actively streaming. Two layers fix it: the registry evict hook
// retires the pool entry eagerly, and pool.For validates Entry
// identity as a backstop for re-registration paths that bypass the
// hook.

// evictFixtureEntry builds an Entry whose agent carries a
// flakyStream-backed eventlog — enough for the pool/broadcaster
// machinery without real SQLite. regSeq mirrors what the registry
// stamps at registration: later re-registrations get higher values.
func evictFixtureEntry(sid string, regSeq uint64) *Entry {
	return &Entry{
		AppName:   "core-agent",
		UserID:    "u",
		SessionID: sid,
		regSeq:    regSeq,
		Agent: &eventfulRegistrant{
			stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: sid},
			handle:         &eventlog.Handle{Stream: newFlakyStream()},
		},
	}
}

func TestPool_ForReplacesStaleBroadcaster(t *testing.T) {
	t.Parallel()
	pool := newBroadcasterPool()

	e1 := evictFixtureEntry("swap", 1)
	b1, err := pool.For(e1)
	if err != nil {
		t.Fatalf("For(e1): %v", err)
	}
	ch1 := b1.Subscribe(context.Background(), 0)

	// Same triple, NEWER Entry — the evict+resume shape.
	e2 := evictFixtureEntry("swap", 2)
	b2, err := pool.For(e2)
	if err != nil {
		t.Fatalf("For(e2): %v", err)
	}
	if b2 == b1 {
		t.Fatal("pool.For returned the stale broadcaster for a re-registered Entry — snapshots/emitter/touch all bind to the dead agent")
	}
	if b2.entry != e2 {
		t.Fatalf("replacement broadcaster bound to %p, want the resumed Entry %p", b2.entry, e2)
	}
	// The stale broadcaster was closed: its subscriber gets EOF (and
	// reconnects onto the fresh one in production).
	drainUntilClosed(t, ch1, "stale broadcaster's subscriber")

	// A repeat For for the SAME Entry is a plain hit.
	b2again, err := pool.For(e2)
	if err != nil || b2again != b2 {
		t.Fatalf("For(e2) again = (%p, %v), want cache hit %p", b2again, err, b2)
	}
	b2.Close()
}

// TestPool_ForStaleCallerCannotUnseatCurrent pins the DIRECTION of
// the staleness check (found by adversarial review of the initial
// #486 fix): handlers resolve their *Entry before calling For, so a
// preempted request can arrive holding an OLDER Entry than the
// pooled broadcaster's. Swapping on mere pointer inequality would
// let that stale caller close the current broadcaster mid-stream
// (spurious EOF for its clients) and install one bound to the DEAD
// agent — reinstalling the #486 symptom. The stale caller must get
// the current broadcaster instead.
func TestPool_ForStaleCallerCannotUnseatCurrent(t *testing.T) {
	t.Parallel()
	pool := newBroadcasterPool()

	old := evictFixtureEntry("inv", 1)     // resolved by a slow request, then evicted/deleted
	current := evictFixtureEntry("inv", 2) // the resumed/recreated registration
	b2, err := pool.For(current)
	if err != nil {
		t.Fatalf("For(current): %v", err)
	}
	defer b2.Close()
	ch2 := b2.Subscribe(context.Background(), 0)

	got, err := pool.For(old)
	if err != nil {
		t.Fatalf("For(old): %v", err)
	}
	if got != b2 {
		t.Fatalf("For(stale entry) = %p bound to %p, want the current broadcaster %p — a stale caller must never unseat it", got, got.entry, b2)
	}
	// The current subscriber was not hung up.
	select {
	case _, ok := <-ch2:
		// Boot frames are fine; a CLOSED channel is the failure.
		if !ok {
			t.Fatal("current broadcaster's subscriber was closed by a stale For caller")
		}
	default:
	}
	if again, _ := pool.For(current); again != b2 {
		t.Fatal("current broadcaster displaced from the pool by a stale For caller")
	}
}

func TestPool_RetireSparesResumedBroadcaster(t *testing.T) {
	t.Parallel()
	pool := newBroadcasterPool()

	evicted := evictFixtureEntry("late", 1)
	resumed := evictFixtureEntry("late", 2)
	b2, err := pool.For(resumed)
	if err != nil {
		t.Fatalf("For(resumed): %v", err)
	}
	defer b2.Close()

	// A late evict-hook firing for the OLD Entry must be a no-op —
	// the pool holds the resumed session's broadcaster now.
	if pool.Retire(evicted) {
		t.Fatal("Retire(evicted) = true (must not rip out the resumed session's broadcaster)")
	}
	if again, _ := pool.For(resumed); again != b2 {
		t.Fatalf("resumed broadcaster gone from the pool after stale Retire")
	}
	// Matching Entry does retire (and closes).
	if !pool.Retire(resumed) {
		t.Fatal("Retire(resumed) = false, want true")
	}
	if b3, _ := pool.For(resumed); b3 == b2 {
		t.Fatal("retired broadcaster still pooled")
	} else if b3 != nil {
		b3.Close()
	}
}

// TestEvictHook_ClosesPoolBroadcaster exercises the hook exactly as
// NewServer wires it: idle eviction retires the pool entry, hangs up
// its subscribers, and the next For (post-resume) builds a fresh
// broadcaster bound to the new Entry.
func TestEvictHook_ClosesPoolBroadcaster(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	pool := newBroadcasterPool()
	reg.SetEvictHook(func(e *Entry) {
		pool.Retire(e)
	})

	ag := &eventfulRegistrant{
		stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "idle"},
		handle:         &eventlog.Handle{Stream: newFlakyStream()},
	}
	entry, err := reg.Register(ag)
	if err != nil {
		t.Fatal(err)
	}
	b1, err := pool.For(entry)
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	ch := b1.Subscribe(context.Background(), 0)

	if n := reg.EvictBefore(time.Now().Add(time.Hour)); n != 1 {
		t.Fatalf("EvictBefore evicted %d, want 1", n)
	}
	// Hook closed the broadcaster: the quiet SSE client is hung up
	// (EOF → reconnect → lazy resume) instead of silently tailing a
	// dead registration.
	drainUntilClosed(t, ch, "evicted session's subscriber")

	// Simulated resume: fresh Entry, same triple. For must build new.
	resumedAg := &eventfulRegistrant{
		stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "idle"},
		handle:         &eventlog.Handle{Stream: newFlakyStream()},
	}
	resumed, err := reg.Register(resumedAg)
	if err != nil {
		t.Fatal(err)
	}
	b2, err := pool.For(resumed)
	if err != nil {
		t.Fatalf("For(resumed): %v", err)
	}
	defer b2.Close()
	if b2 == b1 || b2.entry != resumed {
		t.Fatalf("post-evict For returned stale state (b2==b1: %v, entry match: %v)", b2 == b1, b2.entry == resumed)
	}
}

// TestPool_CloseFencesInFlightRetire pins the retiring-group fence
// (adversarial-review finding on the initial #486 fix): a
// broadcaster pulled out of the map by Retire (or a For-swap) is
// invisible to pool.Close's map walk, so without the fence Close
// could return while that broadcaster's pump still reads the
// eventlog — and the caller (Server.Close → daemon defers) then
// closes the eventlog handle under a live Watch, the exact #424
// use-after-close. The gated fake stream holds the retired pump
// mid-drain; pool.Close must block until it's released.
func TestPool_CloseFencesInFlightRetire(t *testing.T) {
	t.Parallel()

	stream := newFlakyStream()
	stream.returnGate = make(chan struct{})
	e := &Entry{
		AppName:   "core-agent",
		UserID:    "u",
		SessionID: "fence",
		regSeq:    1,
		Agent: &eventfulRegistrant{
			stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "fence"},
			handle:         &eventlog.Handle{Stream: stream},
		},
	}
	pool := newBroadcasterPool()
	b, err := pool.For(e)
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	ch := b.Subscribe(context.Background(), 0) // starts the pump (its exit is gated)

	retireDone := make(chan struct{})
	go func() {
		pool.Retire(e)
		close(retireDone)
	}()
	// Wait until Retire has pulled b out of the map (it then blocks
	// in b.Close → wg.Wait on the gated pump) so pool.Close's
	// snapshot below provably cannot see b.
	deadline := time.Now().Add(5 * time.Second)
	for {
		pool.mu.Lock()
		n := len(pool.bcasts)
		pool.mu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Retire never removed the broadcaster from the pool")
		}
		time.Sleep(time.Millisecond)
	}

	closeDone := make(chan struct{})
	go func() {
		pool.Close()
		close(closeDone)
	}()
	select {
	case <-closeDone:
		t.Fatal("pool.Close returned while a retired broadcaster's goroutines were still draining — the #424 eventlog use-after-close window")
	case <-time.After(150 * time.Millisecond):
		// Correctly blocked on the retiring group.
	}

	close(stream.returnGate)
	for what, c := range map[string]chan struct{}{"Retire": retireDone, "pool.Close": closeDone} {
		select {
		case <-c:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s did not complete after the pump drained", what)
		}
	}
	drainUntilClosed(t, ch, "retired broadcaster's subscriber")
}

// TestServer_CloseJoinsIdleSweep pins the shutdown ordering half of
// #486 (adversarial-review finding): Server.Close must JOIN the
// sweep goroutine, not merely cancel its context — a sweep
// mid-EvictBefore (evict hook Closing a broadcaster included) would
// otherwise still be running when the caller closes the eventlog
// handle right after Close returns. This test asserts the join
// cannot deadlock against a parked sweep; the in-flight-hook fence
// itself is structural (sweepWG + the pool's retiring group).
func TestServer_CloseJoinsIdleSweep(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	srv, err := NewServer(Options{Registry: reg, Addr: "127.0.0.1:0", SessionIdleTimeout: time.Hour})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	go func() { _ = srv.ListenAndServe() }()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && srv.Addr() == "" {
		time.Sleep(5 * time.Millisecond)
	}
	if srv.Addr() == "" {
		t.Fatal("listener never bound")
	}

	done := make(chan struct{})
	go func() {
		_ = srv.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Server.Close hung joining the idle sweep")
	}
}

// TestServer_EvictHangsUpSSE is the end-to-end half: NewServer wires
// the evict hook itself, so an idle-evicted session's live /events
// stream must end (EOF) rather than silently tailing a dead
// registration forever.
func TestServer_EvictHangsUpSSE(t *testing.T) {
	t.Parallel()
	h, cleanupLog := openTestEventLog(t)
	defer cleanupLog()

	reg := NewSessionRegistry()
	ag := &eventfulRegistrant{
		stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "sweepme"},
		handle:         h,
	}
	if _, err := reg.Register(ag); err != nil {
		t.Fatal(err)
	}
	seedTestEvents(t, h, "core-agent", "u", "sweepme", 1)

	base, cleanupSrv := startTestServer(t, reg)
	defer cleanupSrv()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		base+"/sessions/core-agent/sweepme/events?since=0", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer resp.Body.Close()
	frames := readSSEFrames(t, resp.Body)

	// Wait for the seeded frame so the subscription is provably live
	// before the sweep fires.
	deadline := time.After(5 * time.Second)
wait:
	for {
		select {
		case f, ok := <-frames:
			if !ok {
				t.Fatal("stream closed before eviction")
			}
			if f.Event == EventAgent {
				break wait
			}
		case <-deadline:
			t.Fatal("seeded frame never arrived")
		}
	}

	if n := reg.EvictBefore(time.Now().Add(time.Hour)); n != 1 {
		t.Fatalf("EvictBefore evicted %d, want 1", n)
	}

	// The server-wired hook must end the SSE stream.
	deadline = time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-frames:
			if !ok {
				return // EOF — client would now reconnect and lazy-resume
			}
		case <-deadline:
			t.Fatal("SSE stream still open after eviction — stale broadcaster kept serving the dead registration")
		}
	}
}
