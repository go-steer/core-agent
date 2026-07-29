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
)

// TestServer_CloseDoesNotBurnShutdownTimeoutOnSSE pins the #488
// shutdown-ordering fix. SSE handlers block ranging their subscriber
// channel, which only closes when the broadcaster does — and the old
// Close ran srv.Shutdown BEFORE pool.Close, so with any attached
// client Shutdown could never drain and every daemon stop burned the
// FULL ShutdownTimeout. Close now hangs up the pool first; with a
// live SSE client attached and a deliberately long ShutdownTimeout,
// Close must return in a fraction of it.
func TestServer_CloseDoesNotBurnShutdownTimeoutOnSSE(t *testing.T) {
	t.Parallel()

	h, cleanupLog := openTestEventLog(t)
	defer cleanupLog()

	reg := NewSessionRegistry()
	ag := &eventfulRegistrant{
		stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "sse-hold"},
		handle:         h,
	}
	if _, err := reg.Register(ag); err != nil {
		t.Fatal(err)
	}
	seedTestEvents(t, h, "core-agent", "u", "sse-hold", 1)

	const shutdownTimeout = 20 * time.Second
	srv, err := NewServer(Options{Registry: reg, Addr: "127.0.0.1:0", ShutdownTimeout: shutdownTimeout})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	go func() { _ = srv.ListenAndServe() }()
	t.Cleanup(func() { _ = srv.Close() }) // idempotent; the timed Close below is the real one
	bindDeadline := time.Now().Add(time.Second)
	for time.Now().Before(bindDeadline) && srv.Addr() == "" {
		time.Sleep(5 * time.Millisecond)
	}
	if srv.Addr() == "" {
		t.Fatal("listener never bound")
	}
	base := "http://" + srv.Addr()

	// Attach a client that never hangs up on its own: background ctx,
	// body held open. Wait for the first frame so the SSE handler is
	// provably inside its blocking range loop.
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet,
		base+"/sessions/core-agent/sse-hold/events?since=0", nil)
	resp, doErr := http.DefaultClient.Do(req)
	if doErr != nil {
		t.Fatalf("subscribe: %v", doErr)
	}
	defer resp.Body.Close()
	buf := make([]byte, 1)
	if _, err := resp.Body.Read(buf); err != nil {
		t.Fatalf("first SSE byte never arrived: %v", err)
	}

	start := time.Now()
	if err := srv.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed > shutdownTimeout/4 {
		t.Fatalf("Close took %v with an attached SSE client (ShutdownTimeout=%v) — the stream held shutdown hostage", elapsed, shutdownTimeout)
	}
}

// TestServer_CloseDoesNotBurnShutdownTimeoutOnPermsStream is the
// /perms/stream sibling (adversarial-review catch on the initial
// #488 fix): the prompt feed doesn't ride the broadcaster pool, so
// closing the pool alone left it holding Shutdown hostage exactly
// like /events used to — the remote TUI subscribes to BOTH streams,
// so every daemon stop with a TUI attached still burned the full
// timeout. The handlers' closing latch must end it.
func TestServer_CloseDoesNotBurnShutdownTimeoutOnPermsStream(t *testing.T) {
	t.Parallel()

	broker := NewPromptBroker()
	defer broker.Close()
	reg := NewSessionRegistry()
	ag := &promptRegistrant{
		stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "perms-hold"},
		broker:         broker,
	}
	if _, err := reg.Register(ag); err != nil {
		t.Fatal(err)
	}

	const shutdownTimeout = 20 * time.Second
	srv, err := NewServer(Options{Registry: reg, Addr: "127.0.0.1:0", ShutdownTimeout: shutdownTimeout})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	go func() { _ = srv.ListenAndServe() }()
	t.Cleanup(func() { _ = srv.Close() })
	bindDeadline := time.Now().Add(time.Second)
	for time.Now().Before(bindDeadline) && srv.Addr() == "" {
		time.Sleep(5 * time.Millisecond)
	}
	if srv.Addr() == "" {
		t.Fatal("listener never bound")
	}

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet,
		"http://"+srv.Addr()+"/sessions/core-agent/perms-hold/perms/stream", nil)
	resp, doErr := http.DefaultClient.Do(req)
	if doErr != nil {
		t.Fatalf("subscribe perms/stream: %v", doErr)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("perms/stream status %d", resp.StatusCode)
	}
	// The handler flushes headers immediately, then parks in its
	// select; give it a beat to be provably inside the loop.
	time.Sleep(100 * time.Millisecond)

	start := time.Now()
	if err := srv.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if elapsed := time.Since(start); elapsed > shutdownTimeout/4 {
		t.Fatalf("Close took %v with an attached /perms/stream client (ShutdownTimeout=%v) — the prompt feed held shutdown hostage", elapsed, shutdownTimeout)
	}
}
