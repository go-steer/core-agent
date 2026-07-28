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

// Example: build a headless attach-mode daemon from the library —
// the canonical post-#388 embedding shape. An echo-mock agent is
// wrapped in attachadapter.New (which carries the optional attach
// capabilities: memory + skills providers here), registered with an
// attach.SessionRegistry, and served over HTTP by attach.NewServer on
// a loopback port. runner.WakeLoop drains the inbox into real turns.
//
// The example then demonstrates itself over plain net/http, the same
// wire surface core-agent-tui and curl speak: list sessions, read
// status/memory/skills, inject a message (which wakes the loop and
// runs an echo turn), and tail a few SSE frames from /events —
// asserting the protocol's capabilities frame arrives first.
//
//	go run ./examples/attach-daemon
//
// Everything is hermetic: mock provider, loopback listener, temp
// dirs. No credentials, no network beyond 127.0.0.1.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/glebarez/sqlite"

	"github.com/go-steer/core-agent/v2/pkg/agent"
	"github.com/go-steer/core-agent/v2/pkg/attach"
	"github.com/go-steer/core-agent/v2/pkg/attachadapter"
	"github.com/go-steer/core-agent/v2/pkg/eventlog"
	"github.com/go-steer/core-agent/v2/pkg/models/mock"
	"github.com/go-steer/core-agent/v2/pkg/runner"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

// run is extracted from main so deferred cleanup fires even when an
// error short-circuits — log.Fatal calls os.Exit, which skips defers.
func run() error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dir, err := os.MkdirTemp("", "attach-daemon-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()

	// --- daemon side -------------------------------------------------

	// Eventlog is required for /events: the SSE broadcaster replays
	// and tails the durable log, not an in-memory buffer.
	handle, err := eventlog.Open(ctx, sqlite.Open(filepath.Join(dir, "session.db")))
	if err != nil {
		return fmt.Errorf("eventlog.Open: %w", err)
	}
	defer func() { _ = handle.Close() }()

	llm, err := mock.NewEcho().Model(ctx, "echo")
	if err != nil {
		return err
	}

	// Core agent: narrow surface only (post-#388 split). No attach
	// wiring here — that moved to the adapter below.
	a, err := agent.New(llm,
		agent.WithSession("operator", "main"),
		agent.WithEventLog(handle),
		agent.WithInstruction("you are a headless daemon; echo what operators inject"),
	)
	if err != nil {
		return fmt.Errorf("agent.New: %w", err)
	}

	// A small on-disk memory source so the memory provider reports
	// something real (scope/path/size back GET .../memory).
	agentsMD := filepath.Join(dir, "AGENTS.md")
	if err := os.WriteFile(agentsMD, []byte("# demo\nBe terse.\n"), 0o600); err != nil {
		return err
	}

	// Adapter: bridges the agent onto the attach HTTP/SSE surface and
	// carries the optional capability providers.
	ad := attachadapter.New(a,
		attachadapter.WithMemoryProvider(func() []attach.MemorySource {
			st, err := os.Stat(agentsMD)
			if err != nil {
				return nil
			}
			return []attach.MemorySource{{Scope: "project", Path: agentsMD, Size: int(st.Size())}}
		}),
		attachadapter.WithSkillsProvider(func() []attach.SkillInfo {
			return []attach.SkillInfo{
				{Name: "triage", Description: "walk the runbook for a paging alert"},
				{Name: "release-notes", Description: "draft notes from merged PRs"},
			}
		}),
	)

	reg := attach.NewSessionRegistry()
	if _, err := reg.Register(ad); err != nil {
		return fmt.Errorf("register session: %w", err)
	}

	// Loopback + no token is permitted (local-dev posture) but the
	// server logs a warning — expected in this example's output.
	srv, err := attach.NewServer(attach.Options{
		Registry:        reg,
		Addr:            "127.0.0.1:0",
		ShutdownTimeout: 2 * time.Second,
	})
	if err != nil {
		return fmt.Errorf("NewServer: %w", err)
	}
	if err := srv.Bind(); err != nil {
		return fmt.Errorf("bind listener: %w", err)
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve() }()
	defer func() { _ = srv.Close() }()

	// The wake loop is what makes the daemon headless-but-alive:
	// every POST /inject queues a message AND requests a wake; the
	// loop then runs one empty-prompt turn that drains the inbox.
	go runner.WakeLoop(ctx, a, runner.WakeLoopOptions{})

	base := "http://" + srv.Addr()
	fmt.Printf("daemon: listening on %s\n\n", base)

	// --- client side (plain net/http — same wire as curl / the TUI) --

	// 1. Discover sessions.
	var list struct {
		Sessions []struct {
			App         string `json:"app"`
			User        string `json:"user"`
			SessionID   string `json:"sessionID"`
			HasEventLog bool   `json:"has_event_log"`
		} `json:"sessions"`
	}
	if err := getJSON(base+"/sessions", &list); err != nil {
		return err
	}
	if len(list.Sessions) != 1 {
		return fmt.Errorf("GET /sessions: want 1 session, got %d", len(list.Sessions))
	}
	s := list.Sessions[0]
	sessURL := fmt.Sprintf("%s/sessions/%s/%s", base, s.App, s.SessionID)
	fmt.Printf("GET /sessions        -> %s/%s (eventlog=%v)\n", s.App, s.SessionID, s.HasEventLog)

	// 2. Status + the two capability providers we wired.
	var status struct {
		State     string `json:"state"`
		ModelName string `json:"model_name"`
	}
	if err := getJSON(sessURL+"/status", &status); err != nil {
		return err
	}
	fmt.Printf("GET .../status       -> state=%s model=%s\n", status.State, status.ModelName)

	var mem struct {
		Sources []attach.MemorySource `json:"sources"`
	}
	if err := getJSON(sessURL+"/memory", &mem); err != nil {
		return err
	}
	for _, m := range mem.Sources {
		fmt.Printf("GET .../memory       -> [%s] %s (%d bytes)\n", m.Scope, filepath.Base(m.Path), m.Size)
	}

	var sk struct {
		Skills []attach.SkillInfo `json:"skills"`
	}
	if err := getJSON(sessURL+"/skills", &sk); err != nil {
		return err
	}
	for _, k := range sk.Skills {
		fmt.Printf("GET .../skills       -> %s: %s\n", k.Name, k.Description)
	}

	// 3. Inject a message. This queues it on the inbox and fires the
	// wake; the wake loop runs an echo turn against the eventlog.
	body, _ := json.Marshal(map[string]string{"message": "ping from the operator"})
	resp, err := http.Post(sessURL+"/inject", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("POST .../inject: status %d", resp.StatusCode)
	}
	fmt.Printf("POST .../inject      -> %d (message queued, wake fired)\n", resp.StatusCode)

	// 4. Wait for the wake-loop turn to land in the eventlog (the
	// injected user message + the echo model's reply).
	if err := waitForEvents(ctx, handle, ad.AppName(), ad.UserID(), ad.SessionID(), 2, 10*time.Second); err != nil {
		return err
	}
	fmt.Println("wake loop:            turn complete (events persisted)")

	// 5. Tail the SSE stream. Subscribing with since=0 replays the
	// log; per the protocol (v1.4.0) the capabilities frame is always
	// the first thing on the wire.
	fmt.Println("\nGET .../events (SSE):")
	names, err := readSSEFrames(ctx, sessURL+"/events?since=0", 4, 5*time.Second)
	if err != nil {
		return err
	}
	for i, n := range names {
		fmt.Printf("  frame %d: event=%s\n", i+1, n)
	}
	if len(names) == 0 || names[0] != "capabilities" {
		return fmt.Errorf("expected the capabilities frame first, got %v", names)
	}
	fmt.Println("  ok: capabilities frame arrived first")

	// 6. Clean shutdown: stop the wake loop, then the listener.
	cancel()
	if err := srv.Close(); err != nil {
		return fmt.Errorf("close server: %w", err)
	}
	if err := <-serveErr; err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	fmt.Println("\ndaemon: shut down cleanly")
	return nil
}

// getJSON GETs url and decodes the JSON response into out.
func getJSON(url string, out any) error {
	resp, err := http.Get(url) // #nosec G107 -- loopback URL constructed from our own listener's bound address
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// waitForEvents polls the eventlog until the session has at least n
// rows (the injected message + the model's reply), or times out.
func waitForEvents(ctx context.Context, handle *eventlog.Handle, app, user, sid string, n int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		count := 0
		for _, err := range handle.Stream.Since(ctx, 0, eventlog.ForSession(app, user, sid)) {
			if err != nil {
				return err
			}
			count++
		}
		if count >= n {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for %d eventlog rows", n)
}

// readSSEFrames connects to an SSE endpoint and returns the event
// names of up to max frames (stopping early at the deadline).
func readSSEFrames(ctx context.Context, url string, max int, timeout time.Duration) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}
	var names []string
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if name, ok := strings.CutPrefix(line, "event: "); ok {
			names = append(names, name)
			if len(names) >= max {
				return names, nil
			}
		}
	}
	// A deadline-cancelled read is fine — we report what we got.
	if err := sc.Err(); err != nil && ctx.Err() == nil {
		return names, err
	}
	return names, nil
}
