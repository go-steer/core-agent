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
	"iter"
	"sync"
	"testing"

	"google.golang.org/adk/session"

	"github.com/go-steer/core-agent/v2/pkg/eventlog"
)

// floodStream is a test eventlog.Stream whose Since and Watch both
// yield a dense, unbounded-until-n run of entries starting just past
// fromSeq. It lets the broadcaster's two delivery sources — the
// per-subscriber replayThenTail (Since) and the shared pump (Watch) —
// race over the same subscriber channel/map without needing a real DB.
type floodStream struct {
	n int // entries to yield from each of Since/Watch
}

func (floodStream) Append(context.Context, session.Session, *session.Event) (int64, error) {
	return 0, nil
}

func (s floodStream) Since(ctx context.Context, fromSeq int64, _ ...eventlog.QueryOption) iter.Seq2[eventlog.Entry, error] {
	return func(yield func(eventlog.Entry, error) bool) {
		for i := int64(1); i <= int64(s.n); i++ {
			select {
			case <-ctx.Done():
				return
			default:
			}
			if !yield(eventlog.Entry{Seq: fromSeq + i, Event: &session.Event{}}, nil) {
				return
			}
		}
	}
}

func (s floodStream) Watch(ctx context.Context, fromSeq int64, _ ...eventlog.QueryOption) iter.Seq2[eventlog.Entry, error] {
	return func(yield func(eventlog.Entry, error) bool) {
		for i := int64(1); i <= int64(s.n); i++ {
			select {
			case <-ctx.Done():
				return
			default:
			}
			if !yield(eventlog.Entry{Seq: fromSeq + i, Event: &session.Event{}}, nil) {
				return
			}
		}
		<-ctx.Done()
	}
}

func (floodStream) Close() error { return nil }

// TestBroadcaster_DualSourceSend_NoRace pins the fix for #377: the
// per-subscriber replayThenTail goroutine used to call send() WITHOUT
// holding b.mu, while the shared pump goroutine iterated b.subs and
// sent under b.mu. A slow SSE consumer (full buffer) during replay made
// both goroutines call detachLocked() — delete(b.subs) + close(sub.ch)
// — concurrently with the pump's locked send/iterate, producing "send
// on closed channel", "concurrent map writes", or "concurrent map
// iteration and map write" and crashing the whole daemon.
//
// The subscriber channel is deliberately tiny (cap 1) and never
// drained, so the buffer fills immediately and detach fires from both
// the replay and pump paths at once. Run under `go test -race`: before
// the fix this reliably trips the race detector (or panics); after it,
// both sources funnel through b.mu and it stays clean.
func TestBroadcaster_DualSourceSend_NoRace(t *testing.T) {
	t.Parallel()

	const iterations = 300

	for run := 0; run < iterations; run++ {
		b := &broadcaster{
			entry:  &Entry{AppName: "core-agent", UserID: "u", SessionID: "test"},
			stream: floodStream{n: 64},
			query:  nil,
		}
		// Tiny, undrained buffer → the very next send after the first
		// fills it, forcing detachLocked from whichever source wins.
		sub := &subscriber{
			ch:       make(chan Frame, 1),
			since:    0,
			lastSent: 0,
		}
		b.subs = map[*subscriber]struct{}{sub: {}}

		ctx, cancel := context.WithCancel(context.Background())

		// Emulate Subscribe's lazy-pump wiring so pump's terminal
		// "no subscribers left" branch can clear b.cancel cleanly.
		b.cancel = cancel

		var wg sync.WaitGroup
		wg.Add(2)
		// Shared pump: locks b.mu, iterates b.subs, sends.
		go func() {
			defer wg.Done()
			b.pump(ctx, b.pumpGen, 0)
		}()
		// Per-subscriber replay+tail: the goroutine that used to send
		// unlocked.
		go func() {
			defer wg.Done()
			b.replayThenTail(ctx, sub, 0)
		}()

		wg.Wait()
		cancel() // no-op if pump already cleared it; releases any tail wait
	}
}

// TestBroadcaster_SubscribeCapabilitiesAlwaysFirst pins the #385 SSE
// boot-frame ordering fix: Subscribe must enqueue the spec-required
// capabilities frame into the new subscriber's channel BEFORE the
// subscriber becomes visible to any live producer. Pre-fix, Subscribe
// registered the subscriber in b.subs (and started the pump) and only
// THEN delivered boot frames — a concurrent Emit (or pump broadcast)
// in that window put a typed live frame ahead of capabilities. The
// test floods typed events from another goroutine while subscribing
// repeatedly and asserts the first frame is always capabilities.
// Run under -race.
func TestBroadcaster_SubscribeCapabilitiesAlwaysFirst(t *testing.T) {
	t.Parallel()

	const iterations = 200

	for run := 0; run < iterations; run++ {
		b := &broadcaster{
			entry:   &Entry{AppName: "core-agent", UserID: "u", SessionID: "boot-order"},
			stream:  floodStream{n: 16},
			subs:    make(map[*subscriber]struct{}),
			closing: make(chan struct{}),
		}

		stop := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				b.Emit(EventStatusUpdate, StatusUpdate{TurnState: TurnStateStreaming})
			}
		}()

		ctx, cancel := context.WithCancel(context.Background())
		ch := b.Subscribe(ctx, 0)
		first, ok := <-ch
		if !ok {
			t.Fatalf("run %d: channel closed before any frame", run)
		}
		if first.Type != EventCapabilities {
			t.Fatalf("run %d: first frame = type=%q seq=%d, want the %q boot frame first",
				run, first.Type, first.Seq, EventCapabilities)
		}

		close(stop)
		wg.Wait()
		cancel()
		b.Close()
		// Drain to release the (now closed) channel cleanly.
		for range ch { //nolint:revive // draining
		}
	}
}

// TestBroadcaster_BootFramesRaceWithPump pins the companion half of
// #377: deliverBootFrames sent the capabilities/status/usage frames via
// sendTyped WITHOUT b.mu, even though the subscriber was already in
// b.subs and the pump could be broadcasting to it concurrently. A full
// buffer during boot then raced detachLocked against the pump. This
// drives deliverBootFrames concurrently with the pump against a tiny
// channel; clean under -race only with the fix.
func TestBroadcaster_BootFramesRaceWithPump(t *testing.T) {
	t.Parallel()

	const iterations = 300

	for run := 0; run < iterations; run++ {
		b := &broadcaster{
			entry:  &Entry{AppName: "core-agent", UserID: "u", SessionID: "test"},
			stream: floodStream{n: 64},
		}
		sub := &subscriber{
			ch:       make(chan Frame, 1),
			since:    0,
			lastSent: 0,
		}
		b.subs = map[*subscriber]struct{}{sub: {}}

		ctx, cancel := context.WithCancel(context.Background())
		b.cancel = cancel

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			b.pump(ctx, b.pumpGen, 0)
		}()
		go func() {
			defer wg.Done()
			b.deliverBootFrames(ctx, sub)
		}()

		wg.Wait()
		cancel()
	}
}

// TestBroadcaster_PumpCursorIsNotShared pins #1075: the cursor a pump
// starts its Watch from used to live on the broadcaster, written under
// b.mu by the register that spawned the pump and read — unlocked — by
// the pump goroutine at the top of its loop. Between those two points
// the last subscriber can detach (which cancels the pump and nils
// b.cancel), and the next Subscribe then registers as a first
// subscriber and writes the field for its own successor pump while the
// previous pump has still not read it.
//
// That is the interleaving the race detector caught in CI on PR #1073,
// in a run of the whole pkg/attach suite that has nothing to do with
// cursors — which is the honest description of how this was found.
// The test reproduces it directly: register, detach, register, against
// a stream whose Watch parks until cancelled, so the first pump is
// still in flight when the second register lands.
//
// Run under `go test -race`. Before the fix this trips the detector;
// after it there is no shared field left to trip, because the cursor
// travels to the goroutine as an argument.
func TestBroadcaster_PumpCursorIsNotShared(t *testing.T) {
	t.Parallel()

	const iterations = 400

	for run := 0; run < iterations; run++ {
		b := &broadcaster{
			entry:   &Entry{AppName: "core-agent", UserID: "u", SessionID: "test"},
			stream:  floodStream{n: 0}, // Watch yields nothing and parks on ctx
			subs:    map[*subscriber]struct{}{},
			closing: make(chan struct{}),
		}

		// Two registrations separated by a detach. The detach empties
		// the subscriber set, so the second registration is a first
		// subscriber again and starts a second pump — with the first
		// pump's goroutine very likely not yet past its own start.
		for i, since := range []int64{int64(run), int64(run) + 1000} {
			sub := &subscriber{ch: make(chan Frame, 1)}
			registered, _ := b.register(sub, since)
			if !registered {
				t.Fatalf("run %d: registration %d refused on an open broadcaster", run, i)
			}
			// register accounts for the caller's replayThenTail slot;
			// this test is that caller and has no such goroutine.
			b.wg.Done()
			b.detach(sub)
		}
		b.Close()
	}
}
