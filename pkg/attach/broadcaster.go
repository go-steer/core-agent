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
	"log"
	"sync"

	"google.golang.org/adk/session"

	"github.com/go-steer/core-agent/pkg/eventlog"
)

// Frame is one item the SSE stream emits. Carries the eventlog seq
// (so reconnecting clients can resume via ?since=N) plus the underlying
// ADK event. Marshaled to JSON before going out the wire.
type Frame struct {
	Seq   int64          `json:"seq"`
	Event *session.Event `json:"event"`
}

// subscriberBufferSize is the per-subscriber channel capacity. Slow
// subscribers that fall behind get dropped (their channel is closed)
// rather than stalling the publisher — the design's "we don't share
// one buffered channel across subscribers" decision in action.
//
// 256 frames is generous: at 10 events/sec sustained, that's ~25s of
// catch-up room before a subscriber is declared slow.
const subscriberBufferSize = 256

// Broadcaster owns one goroutine per session that pumps events from
// eventlog.Stream.Watch into N subscriber channels. Subscribers can
// join any time; replay-then-tail is handled via the since parameter.
//
// One Broadcaster per Entry. Lazily created on first Subscribe and
// torn down when the last subscriber leaves (refcount).
type Broadcaster struct {
	entry  *Entry
	stream eventlog.Stream
	query  []eventlog.QueryOption // ForSession(...) for this entry

	mu        sync.Mutex
	subs      map[*subscriber]struct{}
	cancel    context.CancelFunc // cancels the pump goroutine
	startedAt int64              // last seq the pump has yielded
}

type subscriber struct {
	ch     chan Frame
	closed bool
}

// NewBroadcaster constructs a broadcaster for one registered session.
// The pump goroutine is NOT started until the first Subscribe — we
// don't want background goroutines for sessions nobody's watching.
func NewBroadcaster(entry *Entry) (*Broadcaster, error) {
	if entry == nil {
		return nil, errors.New("attach: NewBroadcaster: nil entry")
	}
	if entry.Agent == nil {
		return nil, errors.New("attach: NewBroadcaster: entry has nil Agent")
	}
	h := entry.Agent.EventLog()
	if h == nil {
		return nil, errors.New("attach: NewBroadcaster: agent has no eventlog (attach-mode requires --session-db)")
	}
	return &Broadcaster{
		entry:  entry,
		stream: h.Stream,
		query: []eventlog.QueryOption{
			eventlog.ForSession(entry.AppName, entry.UserID, entry.SessionID),
		},
		subs: make(map[*subscriber]struct{}),
	}, nil
}

// Subscribe adds a new client and returns its frame channel. Replays
// every frame with seq > since before switching to live-tail (which is
// invisible to the caller; same channel).
//
// The returned channel is closed when:
//   - ctx is cancelled (typical: HTTP request ends), OR
//   - the subscriber falls behind subscriberBufferSize frames (the
//     drop-the-subscriber decision; better than stalling everyone).
//
// Caller MUST drain the channel until close to release goroutine
// resources.
func (b *Broadcaster) Subscribe(ctx context.Context, since int64) <-chan Frame {
	sub := &subscriber{ch: make(chan Frame, subscriberBufferSize)}

	b.mu.Lock()
	b.subs[sub] = struct{}{}
	// Lazy pump start on first subscriber.
	if b.cancel == nil {
		pumpCtx, cancel := context.WithCancel(context.Background())
		b.cancel = cancel
		// startedAt is set to the lowest "since" we've ever seen so
		// the pump pulls from far enough back to satisfy this
		// subscriber. Subsequent subscribers either find their
		// since >= startedAt (already in flight) or get a fresh
		// scan via the replay loop below.
		b.startedAt = since
		go b.pump(pumpCtx)
	}
	b.mu.Unlock()

	// Replay loop runs in its own goroutine so Subscribe returns
	// immediately. The same channel carries both replayed and live
	// frames — the client doesn't distinguish.
	go b.replayThenTail(ctx, sub, since)

	return sub.ch
}

// replayThenTail does an eventlog.Stream.Since pull for the catch-up
// range, sending each frame to the subscriber, then leaves the
// subscriber attached for the live-tail (pump goroutine handles
// live broadcasts). Honors ctx.Done so disconnects are clean.
func (b *Broadcaster) replayThenTail(ctx context.Context, sub *subscriber, since int64) {
	for entry, err := range b.stream.Since(ctx, since, b.query...) {
		if err != nil {
			// Replay failures close the subscriber; the client sees
			// EOF on its SSE stream and can reconnect.
			b.detach(sub)
			return
		}
		if !b.send(sub, Frame{Seq: entry.Seq, Event: entry.Event}) {
			return // dropped or ctx cancelled
		}
	}
	// Wait for the ctx to fire (live frames are delivered by the
	// pump goroutine into our channel directly).
	<-ctx.Done()
	b.detach(sub)
}

// pump is the single publisher goroutine per Broadcaster. Drains
// eventlog.Stream.Watch and fans out to every subscriber that's
// attached at the time of the broadcast. Exits when no subscribers
// remain (set by detach).
func (b *Broadcaster) pump(ctx context.Context) {
	for entry, err := range b.stream.Watch(ctx, b.startedAt, b.query...) {
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				log.Printf("attach: broadcaster %s/%s pump error: %v",
					b.entry.AppName, b.entry.SessionID, err)
			}
			return
		}
		frame := Frame{Seq: entry.Seq, Event: entry.Event}
		b.mu.Lock()
		for sub := range b.subs {
			b.send(sub, frame)
		}
		// If no subscribers left, shut down the pump.
		if len(b.subs) == 0 {
			b.cancel = nil
			b.mu.Unlock()
			return
		}
		b.mu.Unlock()
	}
}

// send is non-blocking — if the subscriber's buffer is full, the
// subscriber is closed (treated as slow / dead). Returns false when
// the subscriber should be considered detached.
func (b *Broadcaster) send(sub *subscriber, f Frame) bool {
	if sub.closed {
		return false
	}
	select {
	case sub.ch <- f:
		return true
	default:
		// Buffer full → drop the subscriber.
		log.Printf("attach: broadcaster %s/%s dropping slow subscriber (buffer=%d full)",
			b.entry.AppName, b.entry.SessionID, subscriberBufferSize)
		b.detachLocked(sub)
		return false
	}
}

// detach removes the subscriber under the broadcaster's mutex. If
// this was the last subscriber, the pump goroutine is cancelled at
// its next iteration.
func (b *Broadcaster) detach(sub *subscriber) {
	b.mu.Lock()
	b.detachLocked(sub)
	b.mu.Unlock()
}

func (b *Broadcaster) detachLocked(sub *subscriber) {
	if sub.closed {
		return
	}
	sub.closed = true
	close(sub.ch)
	delete(b.subs, sub)
	if len(b.subs) == 0 && b.cancel != nil {
		b.cancel()
		b.cancel = nil
	}
}

// Close cancels the pump goroutine and closes every subscriber
// channel. Idempotent. Called from Server.Close.
func (b *Broadcaster) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for sub := range b.subs {
		if !sub.closed {
			sub.closed = true
			close(sub.ch)
		}
	}
	b.subs = nil
	if b.cancel != nil {
		b.cancel()
		b.cancel = nil
	}
}

// BroadcasterPool lazily constructs and tracks one Broadcaster per
// Entry. Server uses this so multiple SSE clients for the same session
// share one pump goroutine.
type BroadcasterPool struct {
	mu sync.Mutex
	// Keyed by tripleKey so the (app, user, sid) identity matches
	// the registry's.
	bcasts map[tripleKey]*Broadcaster
}

// NewBroadcasterPool returns an empty pool.
func NewBroadcasterPool() *BroadcasterPool {
	return &BroadcasterPool{bcasts: make(map[tripleKey]*Broadcaster)}
}

// For returns a Broadcaster for entry, constructing it on first use.
// Returns an error when the entry's agent has no eventlog (attach
// requires it).
func (p *BroadcasterPool) For(entry *Entry) (*Broadcaster, error) {
	key := tripleKey{App: entry.AppName, User: entry.UserID, SID: entry.SessionID}
	p.mu.Lock()
	defer p.mu.Unlock()
	if b, ok := p.bcasts[key]; ok {
		return b, nil
	}
	b, err := NewBroadcaster(entry)
	if err != nil {
		return nil, err
	}
	p.bcasts[key] = b
	return b, nil
}

// Close stops every broadcaster in the pool. Used by Server.Close.
func (p *BroadcasterPool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, b := range p.bcasts {
		b.Close()
	}
	p.bcasts = nil
}
