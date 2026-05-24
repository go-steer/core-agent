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
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// localSpawn holds everything we need to track a `--local` agent
// child process so we can attach to it and clean up cleanly on TUI
// exit. Stored on rootModel when --local mode is active.
type localSpawn struct {
	// cmd is the running agent child process.
	cmd *exec.Cmd
	// socketPath is the unix socket the agent's attach listener
	// bound to; passed to attachclient.ParseURL to create a Client.
	socketPath string
	// token is the bearer secret the agent expects in the
	// Authorization header. Generated once per spawn; held in
	// memory only. Passed to the agent via env (never argv).
	token string
	// keep is true when --no-cleanup was set; on TUI exit we
	// leave the agent + socket in place and print the unix URL
	// so the operator can re-attach.
	keep bool
}

// spawnLocalAgent forks a child agent process configured to bind a
// fresh unix socket attach listener with a one-shot bearer token,
// then polls until the listener is ready. Returns the spawn handle
// (or an error if the child failed to bind within the timeout).
//
// extraArgs are forwarded verbatim to the agent — operators pass
// e.g. ["--provider=anthropic", "--model=claude-opus-4-7"] after a
// "--" separator on the TUI command line.
//
// Caller is responsible for invoking spawn.shutdown(keep) when the
// TUI exits.
func spawnLocalAgent(ctx context.Context, extraArgs []string) (*localSpawn, error) {
	// Find the agent binary. Prefer one alongside our own binary
	// (works when both ship from the same release tarball);
	// fall back to whatever's on PATH.
	bin, err := locateAgentBinary()
	if err != nil {
		return nil, err
	}

	socketPath := filepath.Join(os.TempDir(),
		fmt.Sprintf("core-agent-tui-%d.sock", os.Getpid()))
	// Pre-clean a stale socket from a crashed previous run; the
	// agent will refuse to bind otherwise.
	_ = os.Remove(socketPath)

	token, err := generateToken()
	if err != nil {
		return nil, fmt.Errorf("generate bearer token: %w", err)
	}

	dbPath := filepath.Join(os.TempDir(),
		fmt.Sprintf("core-agent-tui-%d.db", os.Getpid()))
	_ = os.Remove(dbPath)

	args := []string{
		"--session-db",
		"--session-db-path=" + dbPath,
		"--attach-unix-socket=" + socketPath,
		"--attach-token=CORE_AGENT_TUI_LOCAL_TOKEN",
		// --no-repl prevents the spawned agent from reading our
		// /dev/null stdin and exiting on EOF. Without this the
		// REPL scanner.Scan() returns false immediately, the
		// agent process dies, and we race between socket-bind
		// and process-exit (usually losing). With --no-repl, the
		// agent blocks on ctx.Done() and the only surface is
		// attach-mode — exactly what the TUI drives.
		"--no-repl",
	}
	args = append(args, extraArgs...)

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = append(os.Environ(),
		"CORE_AGENT_TUI_LOCAL_TOKEN="+token,
	)
	// Capture stdout + stderr so bind/config failures surface
	// in the TUI error path. Bubble tea's alt-screen hides
	// anything written directly to os.Stderr while the TUI is
	// running, which makes a silent crash look like a "didn't
	// bind in time" timeout. We hold the last 8KB and tee live
	// to /tmp/<sock>.log for post-mortem.
	tail := newTailBuf(8 * 1024)
	logPath := socketPath + ".log"
	logFile, _ := os.Create(logPath)
	var sinks []io.Writer
	sinks = append(sinks, tail)
	if logFile != nil {
		sinks = append(sinks, logFile)
	}
	mw := io.MultiWriter(sinks...)
	cmd.Stdout = mw
	cmd.Stderr = mw

	if err := cmd.Start(); err != nil {
		if logFile != nil {
			_ = logFile.Close()
		}
		return nil, fmt.Errorf("spawn agent: %w", err)
	}

	// Watch for early process exit in parallel with the socket
	// poll so we surface "agent crashed" immediately rather than
	// waiting the full timeout. We expose `alive` (closed when
	// Wait returns) plus a pointer to the error — NOT a channel
	// of error. With a channel, waitForSocketOrExit and the
	// caller would race to drain it, and the loser blocks forever.
	alive := make(chan struct{})
	var waitErr error
	go func() {
		waitErr = cmd.Wait()
		if logFile != nil {
			_ = logFile.Close()
		}
		close(alive)
	}()

	// 15s is generous on purpose: a cold-start agent reads config,
	// opens the session DB, initializes tools/MCP, then starts the
	// attach listener in a goroutine. On a loaded laptop or in CI,
	// 5s is too tight and the operator gets a misleading timeout.
	if err := waitForSocketOrExit(ctx, socketPath, 15*time.Second, alive, &waitErr); err != nil {
		// Kill only if still alive; otherwise the Wait goroutine
		// is already done and an extra Kill+wait would deadlock.
		select {
		case <-alive:
			// Already exited.
		default:
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
				<-alive
			}
		}
		return nil, decorateSpawnErr(err, bin, args, tail.String(), logPath)
	}

	return &localSpawn{
		cmd:        cmd,
		socketPath: socketPath,
		token:      token,
	}, nil
}

// decorateSpawnErr builds a multi-line error string that includes
// the binary path, the args passed, the captured tail of the
// agent's stderr/stdout, and the path to the full log file. The
// goal: when /spawn fails, the operator has every signal needed
// to diagnose without re-running outside the TUI.
func decorateSpawnErr(cause error, bin string, args []string, tail, logPath string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "agent failed to start: %v", cause)
	fmt.Fprintf(&b, "\n  binary: %s", bin)
	fmt.Fprintf(&b, "\n  args:   %s", strings.Join(args, " "))
	if tail = strings.TrimSpace(tail); tail != "" {
		fmt.Fprintf(&b, "\n  agent stderr/stdout (last %d bytes):\n%s",
			len(tail), indentLines(tail, "    "))
	} else {
		fmt.Fprintf(&b, "\n  agent produced no stderr/stdout before exit")
	}
	fmt.Fprintf(&b, "\n  full log: %s", logPath)
	return fmt.Errorf("%s", b.String())
}

func indentLines(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}

// shutdown terminates the spawned agent + cleans up the socket
// and session DB. If keep is true, leaves everything in place and
// prints the unix URL so the operator can re-attach.
func (s *localSpawn) shutdown() {
	if s == nil || s.cmd == nil || s.cmd.Process == nil {
		return
	}
	if s.keep {
		fmt.Fprintf(os.Stderr,
			"\n[core-agent-tui] --no-cleanup: agent still running at unix://%s\n",
			s.socketPath)
		return
	}
	// Polite SIGTERM first; SIGKILL if it doesn't exit in 3s.
	_ = s.cmd.Process.Signal(os.Interrupt)
	done := make(chan struct{})
	go func() {
		_, _ = s.cmd.Process.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		_ = s.cmd.Process.Kill()
		<-done
	}
	_ = os.Remove(s.socketPath)
	// Leave the session DB in place — useful for post-mortem
	// inspection; /tmp gets cleaned by the OS eventually.
}

// url returns the unix:// URL operators / the TUI client uses to
// attach to the spawned agent.
func (s *localSpawn) url() string {
	if s == nil {
		return ""
	}
	return "unix://" + s.socketPath
}

// locateAgentBinary picks the core-agent binary to spawn. Resolution
// order:
//  1. $CORE_AGENT_BIN if set — explicit operator override
//  2. ./core-agent in the current working directory — convenient
//     when running out of a freshly-built project tree
//  3. sibling next to our own executable — release-tarball case
//  4. PATH
//
// Returns a clear error listing every location we tried when none
// match, so /spawn failures don't degrade to "binary not found"
// with no further detail.
func locateAgentBinary() (string, error) {
	var tried []string

	if override := os.Getenv("CORE_AGENT_BIN"); override != "" {
		// gosec G304: the operator explicitly opted in via env var
		// to point us at this binary. It's the same trust boundary
		// as a CLI flag.
		if _, err := os.Stat(override); err == nil { //nolint:gosec // operator-supplied override
			return override, nil
		}
		tried = append(tried, "$CORE_AGENT_BIN="+override+" (not found)")
	}

	if cwd, err := os.Getwd(); err == nil {
		candidate := filepath.Join(cwd, "core-agent")
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
		tried = append(tried, candidate)
	}

	if self, err := os.Executable(); err == nil {
		sibling := filepath.Join(filepath.Dir(self), "core-agent")
		if _, err := os.Stat(sibling); err == nil {
			return sibling, nil
		}
		tried = append(tried, sibling+" (sibling)")
	}

	if path, err := exec.LookPath("core-agent"); err == nil {
		return path, nil
	}
	tried = append(tried, "$PATH")

	return "", fmt.Errorf("core-agent binary not found — tried:\n  - %s\nset $CORE_AGENT_BIN to override, or `go install ./cmd/core-agent`",
		strings.Join(tried, "\n  - "))
}

// waitForSocketOrExit polls the unix socket at path until it
// accepts a connection, OR the agent process exits early (the
// `alive` channel is closed by the caller's Wait goroutine when
// cmd.Wait returns, and `waitErrPtr` is read at that point so
// we can surface the exit error), OR the timeout fires. ctx
// cancellation aborts early.
//
// Note: waitErrPtr is read only after `alive` is closed, which
// guarantees the Wait goroutine has fully written the error
// before we observe it (close is a happens-before).
func waitForSocketOrExit(ctx context.Context, path string, timeout time.Duration, alive <-chan struct{}, waitErrPtr *error) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-alive:
			if waitErrPtr != nil && *waitErrPtr != nil {
				return fmt.Errorf("agent exited before binding socket: %w", *waitErrPtr)
			}
			return fmt.Errorf("agent exited cleanly before binding socket — did it need a --prompt or interactive stdin?")
		default:
		}
		conn, err := net.DialTimeout("unix", path, 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("timed out after %s waiting for %s", timeout, path)
}

// tailBuf is a fixed-size rolling buffer used to capture the
// tail of the spawned agent's combined stdout/stderr — old
// bytes drop off the front as new bytes arrive. The buffer is
// thread-safe so the exec.Cmd writer goroutine can write while
// the main goroutine reads on timeout.
type tailBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
	max int
}

func newTailBuf(max int) *tailBuf {
	return &tailBuf{max: max}
}

func (t *tailBuf) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf.Write(p)
	if extra := t.buf.Len() - t.max; extra > 0 {
		// Drop from the front.
		b := t.buf.Bytes()
		t.buf.Reset()
		t.buf.Write(b[extra:])
	}
	return len(p), nil
}

func (t *tailBuf) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.buf.String()
}

// generateToken returns a hex-encoded 32-byte random bearer token.
// Held in TUI memory + passed to the agent via env var; never
// appears in argv.
func generateToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
