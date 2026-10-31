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

// Command ax-agent-entry runs a one-shot core-agent turn as an AX task command.
//
// AX's runner starts the task command on every boot of the sandbox, and
// Agent Substrate also runs it once in the template's golden boot, where
// egress is denied and /workspace is empty. This wrapper makes a core-agent
// turn safe under those rules:
//
//   - With --wait-for-egress it blocks until a TLS handshake to that address
//     succeeds, so no turn starts inside the golden boot.
//   - It skips the run when --state-dir holds a done marker from an earlier
//     boot, so a resumed task does not repeat a finished turn. The marker is
//     checked after the egress wait, because /workspace is only populated
//     once the real actor is running.
//   - It copies core-agent's stdout to result.txt in --state-dir, since AX
//     has no channel for reading a command's output back.
//
// Everything after "--" is the core-agent command line, run as given.
//
//	ax-agent-entry --wait-for-egress=generativelanguage.googleapis.com:443 -- \
//	  core-agent --task=review --session-db --session-db-path=/workspace/.core-agent/sessions.db -p "..."
package main

import (
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

func main() {
	stateDir := flag.String("state-dir", "/workspace/.core-agent", "durable directory for the done marker and result.txt")
	waitFor := flag.String("wait-for-egress", "", "host:port that must accept a TLS handshake before core-agent starts; empty skips the wait")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	os.Exit(entryMain(logger, *stateDir, *waitFor, flag.Args(), nil))
}

func entryMain(logger *slog.Logger, stateDir, waitFor string, args []string, tlsCfg *tls.Config) int {
	if len(args) == 0 {
		logger.Error("no agent command given after --")
		return 2
	}

	if waitFor != "" {
		if err := waitForEgress(logger, waitFor, tlsCfg); err != nil {
			logger.Error("invalid --wait-for-egress", "addr", waitFor, "error", err)
			return 2
		}
	}

	donePath := filepath.Join(stateDir, "done")
	if b, err := os.ReadFile(donePath); err == nil {
		logger.Info("turn already completed on an earlier boot; not running it again", "marker", donePath, "completed", string(b))
		return 0
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		logger.Error("creating state dir", "error", err)
		return 1
	}

	code, err := run(logger, args, filepath.Join(stateDir, "result.txt"))
	if err != nil {
		logger.Error("agent turn did not complete", "exitCode", code, "error", err)
		return code
	}
	if err := os.WriteFile(donePath, []byte(time.Now().UTC().Format(time.RFC3339)), 0o600); err != nil {
		logger.Error("writing done marker", "error", err)
		return 1
	}
	logger.Info("agent turn completed", "marker", donePath)
	return 0
}

// waitForEgress blocks until a TLS handshake with addr succeeds. A bare TCP
// connect is not enough: Substrate's egress gateway accepts the connection
// and then drops it for an actor without an egress policy.
func waitForEgress(logger *slog.Logger, addr string, baseTLS *tls.Config) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	cfg := &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
	if baseTLS != nil {
		cfg = baseTLS.Clone()
		if cfg.ServerName == "" {
			cfg.ServerName = host
		}
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	start, lastLog := time.Now(), time.Time{}
	for {
		conn, err := tls.DialWithDialer(dialer, "tcp", addr, cfg)
		if err == nil {
			_ = conn.Close()
			logger.Info("egress available", "addr", addr, "waited", time.Since(start).Round(time.Second))
			return nil
		}
		if time.Since(lastLog) > time.Minute {
			logger.Info("waiting for egress", "addr", addr, "error", err)
			lastLog = time.Now()
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// run starts the agent command, tees its stdout into resultPath, and returns
// its exit code. SIGTERM and SIGINT are forwarded so core-agent can shut down
// cleanly; under AX the runner signals the whole process group, so core-agent
// may see them twice, which it tolerates.
func run(logger *slog.Logger, args []string, resultPath string) (int, error) {
	result, err := os.Create(resultPath) // #nosec G304 -- resultPath is constructed from the operator-configured --state-dir
	if err != nil {
		return 1, fmt.Errorf("creating %s: %w", resultPath, err)
	}
	defer func() { _ = result.Close() }()

	cmd := exec.Command(args[0], args[1:]...) // #nosec G204 -- runs the command this wrapper was given, by design
	cmd.Stdin = os.Stdin
	cmd.Stdout = io.MultiWriter(os.Stdout, result)
	cmd.Stderr = os.Stderr

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sigs)

	if err := cmd.Start(); err != nil {
		return 1, fmt.Errorf("starting %v: %w", args, err)
	}
	logger.Info("started agent", "pid", cmd.Process.Pid, "command", args)

	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	for {
		select {
		case sig := <-sigs:
			logger.Info("forwarding signal to agent", "signal", sig)
			_ = cmd.Process.Signal(sig)
		case err := <-exited:
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				if code := exitErr.ExitCode(); code > 0 {
					return code, err
				}
				return 1, err // killed by a signal
			}
			if err != nil {
				return 1, err
			}
			return 0, nil
		}
	}
}
