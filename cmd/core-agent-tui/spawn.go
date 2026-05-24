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
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
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
	}
	args = append(args, extraArgs...)

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = append(os.Environ(),
		"CORE_AGENT_TUI_LOCAL_TOKEN="+token,
	)
	// Capture the agent's stderr so we can surface bind failures
	// to the operator. Stdout is the agent's REPL output; we
	// don't drive it here — attach-mode SSE is the channel.
	cmd.Stdout = os.Stderr // out of band: agent's REPL banner lands here
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("spawn agent: %w", err)
	}

	// Poll the socket until it accepts connections (or we time out).
	if err := waitForSocket(ctx, socketPath, 5*time.Second); err != nil {
		// Kill the child; otherwise it lingers as a zombie.
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		return nil, fmt.Errorf("agent did not bind %s within 5s: %w", socketPath, err)
	}

	return &localSpawn{
		cmd:        cmd,
		socketPath: socketPath,
		token:      token,
	}, nil
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

// locateAgentBinary picks the core-agent binary to spawn. Prefers
// one in the same directory as core-agent-tui (works when both ship
// from the same release artifact); falls back to PATH.
func locateAgentBinary() (string, error) {
	// Sibling next to our own executable.
	self, err := os.Executable()
	if err == nil {
		sibling := filepath.Join(filepath.Dir(self), "core-agent")
		if _, statErr := os.Stat(sibling); statErr == nil {
			return sibling, nil
		}
	}
	// PATH.
	if path, err := exec.LookPath("core-agent"); err == nil {
		return path, nil
	}
	return "", fmt.Errorf("core-agent binary not found alongside core-agent-tui or on PATH; install it from the same release artifact pair")
}

// waitForSocket polls until a unix socket at path accepts a
// connection or the timeout expires. ctx cancellation aborts
// early. Uses a short Dial timeout per attempt so the poll
// loop reacts quickly when the socket appears.
func waitForSocket(ctx context.Context, path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		conn, err := net.DialTimeout("unix", path, 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("timeout")
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
