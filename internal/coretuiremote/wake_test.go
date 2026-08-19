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

package coretuiremote

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/glebarez/sqlite"

	coretui "github.com/go-steer/core-tui/tui"

	"github.com/go-steer/core-agent/v2/internal/attachclient"
	"github.com/go-steer/core-agent/v2/pkg/agent"
	"github.com/go-steer/core-agent/v2/pkg/attach"
	"github.com/go-steer/core-agent/v2/pkg/attachadapter"
	"github.com/go-steer/core-agent/v2/pkg/eventlog"
	"github.com/go-steer/core-agent/v2/pkg/models/mock"
)

// TestWake_EndToEnd_DaemonAgentReachesTUIChannel is the regression pin
// for #802: a wake raised on the daemon's agent has to arrive on the
// channel core-tui subscribes to in attach mode.
//
// It deliberately runs the WHOLE chain rather than any one link —
// *agent.Agent.RequestWake → the attach broadcaster → a real HTTP SSE
// stream → attachclient's frame parser → *Adapter.WakeRequested. The
// bug it guards against was not a broken link; it was a chain with no
// link at all in the middle, behind a method whose name and doc comment
// claimed the capability was wired. A test of the adapter alone (feed
// it a frame, watch the channel) passes just as happily against a
// daemon that never sends the frame, which is exactly the state this
// repo shipped in for every release since the capability was added.
//
// Fails before the fix in both of its stages: without the adapter half
// it does not compile (there is no WakeRequested), and with ONLY the
// adapter half — the rename the issue originally proposed — it compiles,
// satisfies the compile guard, and times out here, because no `wake`
// frame exists in the attach protocol for it to receive.
func TestWake_EndToEnd_DaemonAgentReachesTUIChannel(t *testing.T) {
	t.Parallel()

	ag := newAttachedAgent(t)
	base := startAttachServer(t, ag)

	parsed, err := attachclient.ParseURL(base + "/sessions/" + ag.AppName() + "/" + ag.SessionID())
	if err != nil {
		t.Fatalf("ParseURL: %v", err)
	}
	client := attachclient.New(parsed, "", 0)
	a := New(client, "/sessions/"+ag.AppName()+"/"+ag.SessionID())

	// The capability has to be discoverable the way core-tui discovers
	// it — by type assertion on the value the host hands to the TUI —
	// not just present as a method on the concrete type.
	waker, ok := any(a).(coretui.WakeRequester)
	if !ok {
		t.Fatal("*Adapter does not satisfy coretui.WakeRequester; attach-mode wake is dead again (#802)")
	}
	wakes := waker.WakeRequested()
	if wakes == nil {
		t.Fatal("WakeRequested() returned nil; core-tui declines the subscription on nil")
	}

	ctx, cancel := context.WithCancel(context.Background())

	// Drain the observer stream the way core-tui does. The first event
	// it yields is the broadcaster's boot status-update snapshot, which
	// is the synchronization point we need: the broadcaster wires the
	// agent's operator-event emitter as part of Subscribe, and Subscribe
	// has provably returned by the time its boot frames reach us. Wake
	// after that and there is no "emitted before anyone was listening"
	// race to paper over with retries.
	connected := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	// Joined before the test returns so the read loop cannot outlive it
	// and race the harness on a later t.Log or a shared fixture.
	defer wg.Wait()
	defer cancel()
	go func() {
		defer wg.Done()
		closed := false
		for range a.Events(ctx) {
			if !closed {
				closed = true
				close(connected)
			}
		}
	}()
	select {
	case <-connected:
	case <-time.After(10 * time.Second):
		t.Fatal("observer stream never delivered its boot frames")
	}

	// One wake. Not a loop of them: a capability that only works when
	// you fire it repeatedly is not a working capability.
	ag.RequestWake()

	select {
	case <-wakes:
	case <-time.After(10 * time.Second):
		t.Fatal("wake never arrived on the adapter's channel — the daemon-side signal does not reach an attached TUI (#802)")
	}
}

// TestWake_EndToEnd_InjectDoesNotWake pins the deliberate exclusion on
// the other side of the same chain: Inject fires the agent's wake
// signal internally, but it must NOT produce a `wake` frame, because
// the operator's own typed prompt already reports itself as an `inbox`
// event. Without the exclusion every message an operator sends raises a
// "something wants your attention" toast about their own typing.
func TestWake_EndToEnd_InjectDoesNotWake(t *testing.T) {
	t.Parallel()

	ag := newAttachedAgent(t)
	base := startAttachServer(t, ag)

	parsed, err := attachclient.ParseURL(base + "/sessions/" + ag.AppName() + "/" + ag.SessionID())
	if err != nil {
		t.Fatalf("ParseURL: %v", err)
	}
	a := New(attachclient.New(parsed, "", 0), "/sessions/"+ag.AppName()+"/"+ag.SessionID())

	ctx, cancel := context.WithCancel(context.Background())

	var wg sync.WaitGroup
	inboxSeen := make(chan struct{}, 4)
	connected := make(chan struct{})
	wg.Add(1)
	defer wg.Wait()
	defer cancel()
	go func() {
		defer wg.Done()
		closed := false
		for ev := range a.Events(ctx) {
			if !closed {
				closed = true
				close(connected)
			}
			if ev.Inbox != nil {
				select {
				case inboxSeen <- struct{}{}:
				default:
				}
			}
		}
	}()
	select {
	case <-connected:
	case <-time.After(10 * time.Second):
		t.Fatal("observer stream never delivered its boot frames")
	}

	// Two injects, then wait for the SECOND inbox frame. That is what
	// makes this a deterministic ordering assertion rather than a
	// "nothing showed up within N milliseconds" guess, and the ordering
	// is the whole trick: injectAs publishes its `inbox` event BEFORE it
	// touches the wake signal, one SSE connection delivers frames in
	// order, and the adapter's read loop calls signalWake synchronously
	// as it walks them. So by the time inbox #2 has been yielded to us,
	// a wake frame belonging to inject #1 would already be sitting in
	// the adapter's buffered-1 channel, and a plain non-blocking receive
	// finds it with no window to tune and nothing to flake on.
	//
	// Two injects rather than one because inject #1's own wake frame
	// trails its inbox frame; #2's inbox frame is the first marker that
	// provably comes after it.
	for i, msg := range []string{"do the thing", "and another"} {
		if err := ag.Inject(msg); err != nil {
			t.Fatalf("Inject %d: %v", i+1, err)
		}
	}
	for i := range 2 {
		select {
		case <-inboxSeen:
		case <-time.After(10 * time.Second):
			t.Fatalf("only saw %d inbox frames; the stream is not carrying these injects at all", i)
		}
	}

	select {
	case <-a.WakeRequested():
		t.Fatal("inject produced a wake frame; every operator prompt would toast about itself")
	default:
	}
}

// newAttachedAgent builds a real *agent.Agent with the eventlog attach
// requires for live-tail, wrapped in nothing — the caller registers it
// through pkg/attachadapter, the same wrapper cmd/core-agent uses.
func newAttachedAgent(t *testing.T) *agent.Agent {
	t.Helper()

	dsn := filepath.Join(t.TempDir(), "session.db")
	h, err := eventlog.Open(context.Background(), sqlite.Open(dsn))
	if err != nil {
		t.Fatalf("eventlog.Open: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })

	m, err := mock.NewEcho().Model(context.Background(), "echo")
	if err != nil {
		t.Fatalf("mock model: %v", err)
	}
	ag, err := agent.New(m, agent.WithEventLog(h))
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	return ag
}

// startAttachServer registers ag on a fresh registry behind a real
// attach HTTP server on an ephemeral port, and returns its base URL.
func startAttachServer(t *testing.T, ag *agent.Agent) string {
	t.Helper()

	reg := attach.NewSessionRegistry()
	if _, err := reg.Register(attachadapter.New(ag)); err != nil {
		t.Fatalf("Register: %v", err)
	}
	srv, err := attach.NewServer(attach.Options{Registry: reg, Addr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	go func() { _ = srv.ListenAndServe() }()
	t.Cleanup(func() { _ = srv.Close() })

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if addr := srv.Addr(); addr != "" {
			return "http://" + addr
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("attach listener never bound")
	return ""
}

// TestAdapter_signalWake_NeverBlocks is the wedge pin. The SSE read
// loop that calls signalWake is the same loop carrying model output, so
// a wake with no consumer — core-tui not subscribed, or its listener
// between re-arms — must cost nothing. A blocking send here would
// freeze the transcript on the first unread wake.
func TestAdapter_signalWake_NeverBlocks(t *testing.T) {
	t.Parallel()

	a := New(nil, "/sessions/s1")

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 100 {
			a.signalWake() // nobody is reading a.wakeCh
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("signalWake blocked with no consumer — a wake can wedge the SSE read loop")
	}

	// Exactly one survives, and it is still deliverable: coalescing is
	// the agent's own wakeSignal semantics (buffered 1, drop on full),
	// and core-tui promises nothing more.
	select {
	case <-a.WakeRequested():
	default:
		t.Fatal("no wake pending after 100 signals; the coalesced one was lost too")
	}
	select {
	case <-a.WakeRequested():
		t.Fatal("more than one wake queued; the buffer is not bounded at 1")
	default:
	}
}

// TestAdapter_WakeRequested_ZeroValueIsNil covers the hand-constructed
// Adapter: core-tui's listener checks for a nil channel and declines
// the subscription rather than parking on it forever.
func TestAdapter_WakeRequested_ZeroValueIsNil(t *testing.T) {
	t.Parallel()

	var a *Adapter
	if ch := a.WakeRequested(); ch != nil {
		t.Errorf("nil Adapter WakeRequested() = %v, want nil", ch)
	}
	if ch := (&Adapter{}).WakeRequested(); ch != nil {
		t.Errorf("zero Adapter WakeRequested() = %v, want nil", ch)
	}
	// A zero-value adapter's signalWake must not panic on the nil
	// channel either — the send falls through to default.
	(&Adapter{}).signalWake()
}
