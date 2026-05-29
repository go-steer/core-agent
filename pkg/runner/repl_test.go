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

package runner

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-steer/core-agent/pkg/agent"
	"github.com/go-steer/core-agent/pkg/models/mock"
	"github.com/go-steer/core-agent/pkg/usage"
)

// safeBuf is a goroutine-safe bytes.Buffer. The REPL test goroutine
// reads from stdout while REPLWithAgent's goroutine writes to it;
// the race detector flags the bare bytes.Buffer for that pattern.
type safeBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *safeBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *safeBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// TestREPL_WakeTriggersInboxDrain confirms the bug fix from PR #14's
// UAT: an external Inject (which fires the wake signal) was being
// queued silently in REPL mode because the loop only read stdin.
// The fix selects on (stdin line, wake signal); on wake we run a
// turn with empty prompt so Agent.Run's pre-turn drain processes
// the inbox via formatInboxForPrompt + the model "echoes" it back.
//
// This test wires a real Agent + the echo mock + a stdin pipe that
// produces nothing (until /exit). The test goroutine calls
// a.Inject("ping") and then waits for the model's reply to show up
// in stdout. Without the fix, the goroutine times out and stdout
// remains empty.
func TestREPL_WakeTriggersInboxDrain(t *testing.T) {
	t.Parallel()

	provider := mock.NewEcho()
	m, err := provider.Model(context.Background(), "echo")
	if err != nil {
		t.Fatalf("model: %v", err)
	}
	a, err := agent.New(m)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}

	// Stdin reader feeds /exit after we've verified the inbox was
	// drained so REPLWithAgent returns. We don't write anything
	// until the model reply lands.
	stdinR, stdinW := stringPipe()
	var stdout, stderr safeBuf

	repLDone := make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go func() {
		defer close(repLDone)
		_, _ = REPLWithAgent(ctx, a, m, stdinR, &stdout, &stderr,
			usage.NewTracker(), usage.Pricing{})
	}()

	// Give the REPL a moment to enter its loop before injecting.
	time.Sleep(50 * time.Millisecond)
	if err := a.Inject("ping-from-test"); err != nil {
		t.Fatalf("Inject: %v", err)
	}

	// Wait up to 2s for the inject to show up in stdout — the echo
	// provider should reply with "[Inbox]\n- ping-from-test\n\n---\n\n"
	// (the prepended inbox + empty prompt).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(stdout.String(), "ping-from-test") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(stdout.String(), "ping-from-test") {
		t.Errorf("expected stdout to contain the injected message;\n got stdout=%q\n got stderr=%q",
			stdout.String(), stderr.String())
	}
	// Also confirm the wake banner went to stderr (proves we
	// took the wake branch, not the stdin branch).
	if !strings.Contains(stderr.String(), "[wake]") {
		t.Errorf("expected stderr to contain [wake] banner; got %q", stderr.String())
	}

	// Exit the REPL cleanly.
	stdinW.WriteString("/exit\n")
	stdinW.Close()
	select {
	case <-repLDone:
	case <-time.After(2 * time.Second):
		t.Errorf("REPL did not exit after /exit")
	}
}

// TestREPL_StdinStillWorksAfterWake confirms that the persistent
// stdin reader keeps serving lines after a wake-driven turn. (The
// goroutine is shared across iterations; a buggy implementation
// might consume the wake AND a queued stdin line in the same select
// or leave the reader in a bad state.)
func TestREPL_StdinStillWorksAfterWake(t *testing.T) {
	t.Parallel()

	provider := mock.NewEcho()
	m, err := provider.Model(context.Background(), "echo")
	if err != nil {
		t.Fatalf("model: %v", err)
	}
	a, err := agent.New(m)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}

	stdinR, stdinW := stringPipe()
	var stdout, stderr safeBuf

	repLDone := make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go func() {
		defer close(repLDone)
		_, _ = REPLWithAgent(ctx, a, m, stdinR, &stdout, &stderr,
			usage.NewTracker(), usage.Pricing{})
	}()

	time.Sleep(50 * time.Millisecond)
	// Trigger a wake-driven turn first.
	if err := a.Inject("inject-A"); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	// Wait for the wake-turn to complete (echo emits "inject-A" → stdout).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(stdout.String(), "inject-A") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(stdout.String(), "inject-A") {
		t.Fatalf("wake-driven turn never emitted inject-A: stdout=%q", stdout.String())
	}

	// Now type a normal line and verify the model also sees it.
	stdinW.WriteString("normal-line-B\n")
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(stdout.String(), "normal-line-B") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(stdout.String(), "normal-line-B") {
		t.Errorf("stdin-driven turn after wake did not fire; stdout=%q", stdout.String())
	}

	stdinW.WriteString("/exit\n")
	stdinW.Close()
	select {
	case <-repLDone:
	case <-time.After(2 * time.Second):
		t.Errorf("REPL did not exit after /exit")
	}
}

// stringPipe returns a reader/writer pair backed by an in-memory
// pipe. The writer is goroutine-safe and signals io.EOF on Close().
// Used by the tests above to feed REPL stdin from the test goroutine
// without spawning a real terminal.
type pipeReader struct {
	mu     sync.Mutex
	buf    []byte
	closed bool
	wake   chan struct{}
}

type pipeWriter struct {
	r *pipeReader
}

func stringPipe() (*pipeReader, *pipeWriter) {
	r := &pipeReader{wake: make(chan struct{}, 1)}
	return r, &pipeWriter{r: r}
}

func (r *pipeReader) Read(p []byte) (int, error) {
	for {
		r.mu.Lock()
		if len(r.buf) > 0 {
			n := copy(p, r.buf)
			r.buf = r.buf[n:]
			r.mu.Unlock()
			return n, nil
		}
		closed := r.closed
		r.mu.Unlock()
		if closed {
			return 0, io.EOF
		}
		<-r.wake
	}
}

func (w *pipeWriter) WriteString(s string) {
	w.r.mu.Lock()
	w.r.buf = append(w.r.buf, []byte(s)...)
	w.r.mu.Unlock()
	select {
	case w.r.wake <- struct{}{}:
	default:
	}
}

func (w *pipeWriter) Close() {
	w.r.mu.Lock()
	w.r.closed = true
	w.r.mu.Unlock()
	select {
	case w.r.wake <- struct{}{}:
	default:
	}
}
