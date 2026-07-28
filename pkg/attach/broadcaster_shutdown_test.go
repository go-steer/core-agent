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
	"time"

	"google.golang.org/adk/session"

	"github.com/go-steer/core-agent/v2/pkg/eventlog"
)

// blockingStream is a test eventlog.Stream that models the real one
// during shutdown: Watch blocks reading the log until ctx is cancelled
// and then does a slice of work *after* the cancel before its generator
// returns — mirroring an in-flight SQLite read still touching the
// -wal/-shm sidecar files. watchExited is closed only once the Watch
// generator has fully returned, so a test can tell whether Close waited
// for it.
type blockingStream struct {
	watchStarted chan struct{}
	watchExited  chan struct{}
	startOnce    sync.Once
	exitOnce     sync.Once
	postCancel   time.Duration
}

func (s *blockingStream) Append(context.Context, session.Session, *session.Event) (int64, error) {
	return 0, nil
}

// Since returns immediately with no catch-up entries — the shutdown
// race we're pinning is about the live-tail pump, not replay.
func (s *blockingStream) Since(context.Context, int64, ...eventlog.QueryOption) iter.Seq2[eventlog.Entry, error] {
	return func(func(eventlog.Entry, error) bool) {}
}

func (s *blockingStream) Watch(ctx context.Context, _ int64, _ ...eventlog.QueryOption) iter.Seq2[eventlog.Entry, error] {
	return func(func(eventlog.Entry, error) bool) {
		s.startOnce.Do(func() { close(s.watchStarted) })
		<-ctx.Done()
		// The window where the pump is still reading the eventlog after
		// the cancel signal — exactly what raced TempDir cleanup and
		// left the SQLite sidecar files behind ("directory not empty").
		time.Sleep(s.postCancel)
		s.exitOnce.Do(func() { close(s.watchExited) })
	}
}

func (s *blockingStream) Close() error { return nil }

// TestBroadcaster_CloseWaitsForPump pins the fix for the flaky attach
// teardown: broadcaster.Close used to cancel the pump goroutine and
// return immediately, so Server.Close (→ pool.Close) could hand back to
// a caller that then closed the eventlog while the pump's Stream.Watch
// was still mid-read. In tests that surfaced as
// "TempDir RemoveAll cleanup: directory not empty" on the SQLite
// -wal/-shm files; in production it's a use-after-close on the handle.
//
// Close must be a quiescence barrier: once it returns, no goroutine the
// broadcaster spawned is still touching the log. This is deterministic
// — the fake Watch sleeps postCancel after the cancel before marking
// itself exited, so a Close that doesn't wait returns first and the
// watchExited latch is observably still open.
func TestBroadcaster_CloseWaitsForPump(t *testing.T) {
	t.Parallel()

	fake := &blockingStream{
		watchStarted: make(chan struct{}),
		watchExited:  make(chan struct{}),
		postCancel:   80 * time.Millisecond,
	}
	b := &broadcaster{
		entry:   &Entry{AppName: "core-agent", UserID: "u", SessionID: "test"},
		stream:  fake,
		subs:    make(map[*subscriber]struct{}),
		closing: make(chan struct{}),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := b.Subscribe(ctx, 0)
	// Drain so boot frames and any live sends never block the
	// broadcaster's goroutines; the range ends when Close closes ch.
	go func() {
		for range ch { //nolint:revive // draining until close is the point
		}
	}()

	// Wait until the pump goroutine is actually inside Stream.Watch, so
	// we know Close has a live goroutine to wait on.
	select {
	case <-fake.watchStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("pump never entered Stream.Watch")
	}

	done := make(chan struct{})
	go func() {
		b.Close()
		close(done)
	}()

	select {
	case <-done:
		// Close returned: the pump's Watch generator MUST have already
		// returned. If not, Close isn't a real quiescence barrier and
		// the shutdown-vs-eventlog-close race is still live.
		select {
		case <-fake.watchExited:
		default:
			t.Fatal("Close returned before the pump goroutine exited Stream.Watch")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close blocked far longer than the pump's post-cancel window; possible deadlock")
	}
}
