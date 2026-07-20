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

package tools

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/adk/tool"

	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

func TestBash_RunsAndCapturesOutput(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultConfig()
	gate := permissions.New(permissions.Options{Mode: permissions.ModeYolo})
	fn := bashFunc(gate, cfg)
	res, err := fn(tool.Context(nil), bashArgs{Command: "printf hello"})
	if err != nil {
		t.Fatalf("bash: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("exit = %d, want 0", res.ExitCode)
	}
	if res.Stdout != "hello" {
		t.Errorf("stdout = %q", res.Stdout)
	}
}

func TestBash_RefusesDenylist(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultConfig()
	gate := permissions.New(permissions.Options{Mode: permissions.ModeYolo}) // even yolo
	fn := bashFunc(gate, cfg)
	_, err := fn(tool.Context(nil), bashArgs{Command: "rm -rf /"})
	if err == nil || !strings.Contains(err.Error(), "filesystem root") {
		t.Errorf("expected denylist refusal, got %v", err)
	}
}

func TestBash_TimesOut(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultConfig()
	gate := permissions.New(permissions.Options{Mode: permissions.ModeYolo})
	fn := bashFunc(gate, cfg)
	_, err := fn(tool.Context(nil), bashArgs{Command: "sleep 5", TimeoutSeconds: 1})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("expected timeout, got %v", err)
	}
}

func TestBash_NonzeroExitNotAnError(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultConfig()
	gate := permissions.New(permissions.Options{Mode: permissions.ModeYolo})
	fn := bashFunc(gate, cfg)
	res, err := fn(tool.Context(nil), bashArgs{Command: "false"})
	if err != nil {
		t.Errorf("non-zero exit should not be a Go error: %v", err)
	}
	if res.ExitCode != 1 {
		t.Errorf("exit = %d, want 1", res.ExitCode)
	}
}

// TestBash_OrphanedBackgroundProcessDoesntHang reproduces the
// classic shell-pipe-inheritance bug: when a bash command spawns a
// background process that inherits stdout/stderr, that orphan keeps
// the pipe write-end open after its parent shell exits. Without
// WaitDelay, cmd.Wait's internal pipe-copy goroutine blocks forever
// reading from a pipe that never sees EOF — defeating the timeout.
//
// The test runs a command that:
//  1. Forks `sleep 30` into the background (would outlive the test)
//  2. The orphan inherits stdout/stderr from the bash tool's shell
//  3. The shell exits immediately
//
// With WaitDelay set, the tool must return within
// shell-exit + bashWaitDelay, NOT wait for sleep 30 to finish.
func TestBash_OrphanedBackgroundProcessDoesntHang(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultConfig()
	gate := permissions.New(permissions.Options{Mode: permissions.ModeYolo})
	fn := bashFunc(gate, cfg)

	// Wall-clock the call. Must finish in well under sleep's
	// duration (30s) — anything over a couple seconds of grace
	// past WaitDelay means we re-introduced the hang.
	start := time.Now()
	done := make(chan struct{})
	var res bashResult
	var err error
	go func() {
		res, err = fn(tool.Context(nil), bashArgs{
			// Background `sleep 30` and let the shell exit immediately.
			// The orphan still holds stdout/stderr; the tool must
			// SIGKILL it via WaitDelay rather than wait 30s.
			Command:        "sleep 30 &",
			TimeoutSeconds: 60, // tool-level timeout — should NOT fire
		})
		close(done)
	}()

	select {
	case <-done:
		// expected — tool returned despite orphan holding pipes
	case <-time.After(bashWaitDelay + 5*time.Second):
		t.Fatalf("bash tool hung past WaitDelay (%v) — orphan likely held stdout/stderr; got elapsed %v",
			bashWaitDelay, time.Since(start))
	}
	elapsed := time.Since(start)

	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if res.TimedOut {
		t.Errorf("TimedOut = true; the *tool* timeout (60s) should not have fired — WaitDelay should reap the orphan first")
	}
	// Sanity: orphan-reap should be FAST (shell exits in <100ms,
	// then WaitDelay grace). Anything past WaitDelay+1s suggests the
	// fix didn't engage.
	if elapsed > bashWaitDelay+2*time.Second {
		t.Errorf("elapsed = %v, expected ≤ %v (shell exit + WaitDelay grace)",
			elapsed, bashWaitDelay+2*time.Second)
	}
}

// TestBash_TimeoutKillsOrphans is the harder variant: the orphan
// outlives both the shell AND the context deadline. Both the
// context-cancel path and the orphan-reap path have to fire for the
// tool to return. Without WaitDelay, context cancel SIGKILLs only
// the immediate shell; the orphan keeps the pipes open and Wait
// hangs anyway.
func TestBash_TimeoutKillsOrphans(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultConfig()
	gate := permissions.New(permissions.Options{Mode: permissions.ModeYolo})
	fn := bashFunc(gate, cfg)

	start := time.Now()
	done := make(chan struct{})
	var err error
	go func() {
		// Foreground sleep keeps the shell alive past TimeoutSeconds,
		// AND a backgrounded sleep orphan inherits the pipes. Both
		// cleanup paths must run.
		_, err = fn(tool.Context(nil), bashArgs{
			Command:        "sleep 60 & sleep 30",
			TimeoutSeconds: 1,
		})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second + bashWaitDelay + 5*time.Second):
		t.Fatalf("bash tool hung past timeout+WaitDelay; got elapsed %v", time.Since(start))
	}
	elapsed := time.Since(start)

	if err == nil {
		t.Errorf("expected a timeout error, got nil")
	}
	if elapsed > time.Second+bashWaitDelay+2*time.Second {
		t.Errorf("elapsed = %v, expected ≤ %v (timeout + WaitDelay grace)",
			elapsed, time.Second+bashWaitDelay+2*time.Second)
	}
}
