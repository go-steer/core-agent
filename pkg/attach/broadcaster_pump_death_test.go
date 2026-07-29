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
	"errors"
	"iter"
	"sync"
	"testing"
	"time"

	"google.golang.org/adk/session"

	"github.com/go-steer/core-agent/v2/pkg/eventlog"
)

// flakyStream is an eventlog.Stream double whose Watch emits frames
// and errors on command. Lets the test kill the pump with a transient
// error at an exact moment — impossible to schedule against real
// SQLite — and count Watch calls to prove a replacement pump started.
type flakyStream struct {
	mu         sync.Mutex
	watchCalls int

	frames chan eventlog.Entry
	errs   chan error

	// returnGate, when non-nil, blocks the FIRST Watch iterator's
	// ctx-cancelled return until closed. Widens the natural window
	// between a pump's iterator ending and its deferred death-sweep
	// acquiring b.mu — the interleaving where a guardless sweep
	// tears down a successor pump generation.
	returnGate chan struct{}
}

func newFlakyStream() *flakyStream {
	return &flakyStream{
		frames: make(chan eventlog.Entry, 16),
		errs:   make(chan error, 1),
	}
}

func (s *flakyStream) WatchCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.watchCalls
}

func (s *flakyStream) Append(context.Context, session.Session, *session.Event) (int64, error) {
	return 0, errors.New("flakyStream: Append not supported")
}

// Since returns an empty iterator — these tests exercise the live
// tail, not replay.
func (s *flakyStream) Since(context.Context, int64, ...eventlog.QueryOption) iter.Seq2[eventlog.Entry, error] {
	return func(func(eventlog.Entry, error) bool) {}
}

func (s *flakyStream) Watch(ctx context.Context, _ int64, _ ...eventlog.QueryOption) iter.Seq2[eventlog.Entry, error] {
	s.mu.Lock()
	s.watchCalls++
	s.mu.Unlock()
	s.mu.Lock()
	gate := s.returnGate
	first := s.watchCalls == 1
	s.mu.Unlock()
	return func(yield func(eventlog.Entry, error) bool) {
		for {
			select {
			case <-ctx.Done():
				if first && gate != nil {
					<-gate
				}
				return
			case err := <-s.errs:
				yield(eventlog.Entry{}, err)
				return
			case e := <-s.frames:
				if !yield(e, nil) {
					return
				}
			}
		}
	}
}

func (s *flakyStream) Close() error { return nil }

var _ eventlog.Stream = (*flakyStream)(nil)

// waitForSeq drains ch until a legacy frame with the wanted seq
// arrives (typed boot frames are skipped), failing on close/timeout.
func waitForSeq(t *testing.T, ch <-chan Frame, want int64) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case f, ok := <-ch:
			if !ok {
				t.Fatalf("channel closed while waiting for seq %d", want)
			}
			if f.Type != "" {
				continue
			}
			if f.Seq == want {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for seq %d", want)
		}
	}
}

// TestBroadcaster_PumpDeathDetachesAndRecovers pins the #485 fix. A
// transient Watch error (SQLite "database is locked") killed the pump
// with no cleanup: existing subscribers kept open-but-silent channels
// (a frozen stream, no EOF to trigger a client reconnect), and
// because b.cancel stayed set, every future Subscribe saw
// firstSub == false and never started a replacement pump — live-tail
// stayed bricked until every client disconnected at once.
//
// Post-fix, a pump death must (1) close every subscriber channel so
// clients reconnect, (2) clear b.cancel so the next Subscribe starts
// a fresh pump, and (3) actually deliver live frames again on that
// next subscribe.
func TestBroadcaster_PumpDeathDetachesAndRecovers(t *testing.T) {
	t.Parallel()

	stream := newFlakyStream()
	entry := &Entry{
		AppName:   "core-agent",
		UserID:    "u",
		SessionID: "flaky",
		Agent: &eventfulRegistrant{
			stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "flaky"},
			handle:         &eventlog.Handle{Stream: stream},
		},
	}
	b, err := newBroadcaster(entry)
	if err != nil {
		t.Fatalf("newBroadcaster: %v", err)
	}
	defer b.Close()

	// First subscriber: prove the pump is alive, then kill it.
	ch1 := b.Subscribe(context.Background(), 0)
	stream.frames <- eventlog.Entry{Seq: 1, Event: session.NewEvent("e1")}
	waitForSeq(t, ch1, 1)

	stream.errs <- errors.New("database is locked (transient)")

	// (1) The dying pump must hang up its subscribers — an open-but-
	// silent channel is indistinguishable from a quiet session to the
	// client, so EOF is the reconnect signal.
	drainUntilClosed(t, ch1, "subscriber after pump death")

	// (2) The lazy-restart latch must be re-armed.
	b.mu.Lock()
	cancelCleared := b.cancel == nil
	b.mu.Unlock()
	if !cancelCleared {
		t.Fatal("b.cancel still set after pump death — next Subscribe would see firstSub=false and live-tail stays dead")
	}

	// (3) A reconnecting client gets a working live-tail again.
	ch2 := b.Subscribe(context.Background(), 1)
	stream.frames <- eventlog.Entry{Seq: 2, Event: session.NewEvent("e2")}
	waitForSeq(t, ch2, 2)

	if calls := stream.WatchCalls(); calls != 2 {
		t.Errorf("Watch calls = %d, want 2 (a fresh pump per subscribe generation)", calls)
	}
}

// TestBroadcaster_StalePumpSweepSparesSuccessor pins the generation
// guard on the #485 death-sweep. A pump whose iterator has ended can
// be descheduled before its deferred sweep acquires b.mu; in that
// window the last subscriber's detach clears b.cancel and a new
// Subscribe starts a successor pump with its own subscriber. A
// guardless sweep then running late would range the CURRENT subs map
// — hanging up the successor's subscriber — and cancel the
// SUCCESSOR's context (found by adversarial review of the initial
// fix). The gate on the fake stream makes the interleaving
// deterministic.
func TestBroadcaster_StalePumpSweepSparesSuccessor(t *testing.T) {
	t.Parallel()

	stream := newFlakyStream()
	stream.returnGate = make(chan struct{})
	entry := &Entry{
		AppName:   "core-agent",
		UserID:    "u",
		SessionID: "gen",
		Agent: &eventfulRegistrant{
			stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "gen"},
			handle:         &eventlog.Handle{Stream: stream},
		},
	}
	b, err := newBroadcaster(entry)
	if err != nil {
		t.Fatalf("newBroadcaster: %v", err)
	}
	defer b.Close()

	// sub1 starts pump A; prove it's alive.
	ctx1, cancel1 := context.WithCancel(context.Background())
	ch1 := b.Subscribe(ctx1, 0)
	stream.frames <- eventlog.Entry{Seq: 1, Event: session.NewEvent("e1")}
	waitForSeq(t, ch1, 1)

	// sub1 disconnects: detachLocked cancels pump A's ctx and nils
	// b.cancel — but pump A's iterator parks on the return gate, so
	// its deferred sweep has NOT run yet.
	cancel1()
	drainUntilClosed(t, ch1, "sub1 after disconnect")

	// sub2 subscribes into the window: b.cancel == nil ⇒ successor
	// pump B starts, with sub2 as its subscriber. Prove B pumps.
	ch2 := b.Subscribe(context.Background(), 1)
	stream.frames <- eventlog.Entry{Seq: 2, Event: session.NewEvent("e2")}
	waitForSeq(t, ch2, 2)

	// Release pump A: its deferred sweep now runs against state that
	// belongs entirely to generation B. It must touch nothing. The
	// settle sleep gives the unblocked sweep (a mutex acquire + one
	// check) ample time to run BEFORE the assertion below — without
	// it, a buggy sweep could fire after the frame already flowed and
	// the test would pass vacuously.
	close(stream.returnGate)
	time.Sleep(100 * time.Millisecond)

	// sub2's stream stays live: another frame flows and the channel
	// is still open. Without the generation guard the sweep closes
	// ch2 (spurious EOF right after boot) and cancels pump B, so the
	// frame below never arrives.
	stream.frames <- eventlog.Entry{Seq: 3, Event: session.NewEvent("e3")}
	waitForSeq(t, ch2, 3)
}
