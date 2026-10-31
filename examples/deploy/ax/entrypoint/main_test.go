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

package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestEntryMain_RequiresCommand(t *testing.T) {
	t.Parallel()
	code := entryMain(context.Background(), discardLogger(), entryOptions{stateDir: t.TempDir()}, nil)
	if code != 2 {
		t.Fatalf("entryMain(no args) = %d, want 2", code)
	}
}

func TestEntryMain_InvalidWaitForEgress(t *testing.T) {
	t.Parallel()
	o := entryOptions{stateDir: t.TempDir(), waitFor: "not-a-host-port"}
	code := entryMain(context.Background(), discardLogger(), o, []string{"/bin/echo", "hi"})
	if code != 2 {
		t.Fatalf("entryMain(bad --wait-for-egress) = %d, want 2", code)
	}
}

func TestEntryMain_WritesResultAndDoneMarker_AndSkipsOnSecondBoot(t *testing.T) {
	t.Parallel()
	stateDir := filepath.Join(t.TempDir(), ".core-agent")
	counterFile := filepath.Join(t.TempDir(), "runs")

	script := `count=0; [ -f "$1" ] && count=$(cat "$1"); count=$((count + 1)); echo "$count" > "$1"; echo "turn-output-$count"`
	args := []string{"/bin/sh", "-c", script, "sh", counterFile}
	o := entryOptions{stateDir: stateDir}

	if code := entryMain(context.Background(), discardLogger(), o, args); code != 0 {
		t.Fatalf("first boot exit code = %d, want 0", code)
	}

	gotResult, err := os.ReadFile(filepath.Join(stateDir, "result.txt"))
	if err != nil {
		t.Fatalf("read result.txt: %v", err)
	}
	if strings.TrimSpace(string(gotResult)) != "turn-output-1" {
		t.Errorf("result.txt = %q, want %q", string(gotResult), "turn-output-1\n")
	}
	if _, err := os.Stat(filepath.Join(stateDir, "done")); err != nil {
		t.Fatalf("done marker missing after successful turn: %v", err)
	}

	// Second boot with the same stateDir must see the done marker and
	// skip re-running the child command.
	if code := entryMain(context.Background(), discardLogger(), o, args); code != 0 {
		t.Fatalf("second boot exit code = %d, want 0", code)
	}
	runs, err := os.ReadFile(counterFile)
	if err != nil {
		t.Fatalf("read counterFile: %v", err)
	}
	if strings.TrimSpace(string(runs)) != "1" {
		t.Errorf("command ran %s times across two boots, want 1", strings.TrimSpace(string(runs)))
	}
}

func TestEntryMain_FailedCommandLeavesNoDoneMarker(t *testing.T) {
	t.Parallel()
	stateDir := filepath.Join(t.TempDir(), ".core-agent")
	args := []string{"/bin/sh", "-c", "echo partial-output; exit 7"}

	code := entryMain(context.Background(), discardLogger(), entryOptions{stateDir: stateDir}, args)
	if code != 7 {
		t.Fatalf("entryMain exit code = %d, want 7", code)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "done")); !os.IsNotExist(err) {
		t.Errorf("done marker must not be written when command fails, stat err = %v", err)
	}
}

func TestEntryMain_WaitForEgressProbesTLS(t *testing.T) {
	t.Parallel()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("url.Parse: %v", err)
	}
	tlsCfg := &tls.Config{InsecureSkipVerify: true} // #nosec G402 -- test server uses ephemeral self-signed cert
	stateDir := filepath.Join(t.TempDir(), ".core-agent")
	o := entryOptions{stateDir: stateDir, waitFor: u.Host, tlsCfg: tlsCfg}
	code := entryMain(context.Background(), discardLogger(), o, []string{"/bin/echo", "egress-ok"})
	if code != 0 {
		t.Fatalf("entryMain with TLS egress server = %d, want 0", code)
	}
	got, err := os.ReadFile(filepath.Join(stateDir, "result.txt"))
	if err != nil {
		t.Fatalf("read result.txt: %v", err)
	}
	if strings.TrimSpace(string(got)) != "egress-ok" {
		t.Errorf("result.txt = %q, want %q", string(got), "egress-ok")
	}
}

// closedPort returns a loopback host:port that nothing is listening on,
// so every TLS dial to it is refused immediately.
func closedPort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

// With --egress-timeout, a host that never comes up ends the wait with
// exit 1 and no turn, instead of spinning until AX kills the pod (#1419).
func TestEntryMain_EgressTimeoutExitsNonZero(t *testing.T) {
	t.Parallel()
	stateDir := filepath.Join(t.TempDir(), ".core-agent")
	o := entryOptions{stateDir: stateDir, waitFor: closedPort(t), egressTimeout: 300 * time.Millisecond}

	start := time.Now()
	code := entryMain(context.Background(), discardLogger(), o, []string{"/bin/echo", "never"})
	if code != 1 {
		t.Fatalf("entryMain with unreachable egress and a timeout = %d, want 1", code)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("entryMain took %v to give up on a 300ms --egress-timeout", took)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "result.txt")); !os.IsNotExist(err) {
		t.Errorf("the turn must not run when egress never came up; stat err = %v", err)
	}
}

// A SIGTERM-style cancellation during the wait ends it, instead of being
// ignored until the wait completes (#1419).
func TestEntryMain_SignalDuringEgressWaitExits(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	stateDir := filepath.Join(t.TempDir(), ".core-agent")
	o := entryOptions{stateDir: stateDir, waitFor: closedPort(t)}

	done := make(chan int, 1)
	go func() { done <- entryMain(ctx, discardLogger(), o, []string{"/bin/echo", "never"}) }()
	select {
	case code := <-done:
		if code != 1 {
			t.Fatalf("entryMain after cancellation = %d, want 1", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("entryMain did not return within 10s of the context being cancelled; the egress wait ignores signals")
	}
}

// The Info line for a started agent names the binary, not the whole
// argv: the prompt rides in -p and does not belong in every runner log
// (#1419).
func TestRun_DoesNotLogArgvAtInfo(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	const secretish = "prompt-text-that-must-not-appear-in-info-logs"
	args := []string{"/bin/sh", "-c", "exit 0", "sh", "-p", secretish}

	code, err := run(logger, args, filepath.Join(t.TempDir(), "result.txt"))
	if err != nil || code != 0 {
		t.Fatalf("run = (%d, %v), want (0, nil)", code, err)
	}
	if strings.Contains(buf.String(), secretish) {
		t.Fatalf("Info log carries the argv:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "command=/bin/sh") {
		t.Fatalf("Info log should still name the binary:\n%s", buf.String())
	}
}
