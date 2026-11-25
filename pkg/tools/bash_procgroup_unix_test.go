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

//go:build unix

package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	adkagent "google.golang.org/adk/v2/agent"

	"github.com/go-steer/core-agent/v3/pkg/config"
	"github.com/go-steer/core-agent/v3/pkg/permissions"
)

// cancelableToolCtx is a tool context whose cancellation the test
// controls, as an interrupt or the turn's context ending would. Only its
// context.Context methods are used by the bash tool; the embedded
// adkagent.Context is nil, so a call to any other method panics rather
// than passing silently.
type cancelableToolCtx struct {
	adkagent.Context
	ctx context.Context
}

func (c cancelableToolCtx) Deadline() (time.Time, bool) { return c.ctx.Deadline() }
func (c cancelableToolCtx) Done() <-chan struct{}       { return c.ctx.Done() }
func (c cancelableToolCtx) Err() error                  { return c.ctx.Err() }
func (c cancelableToolCtx) Value(key any) any           { return c.ctx.Value(key) }

// processAlive reports whether pid is a live process: one that exists and
// is not a zombie waiting to be reaped.
func processAlive(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
		if i := strings.LastIndexByte(string(b), ')'); i >= 0 && i+2 < len(b) && b[i+2] == 'Z' {
			return false
		}
	}
	return true
}

// TestBash_CancelEndsEverythingTheCommandStarted: when a command is
// cancelled, by the tool timeout or by its context ending, every process it
// started ends too, not only the shell: a nohup'd background child, a
// grandchild that redirected its output away from the tool's pipes, and a
// foreground child the shell was waiting on. Each records its pid, and each
// must be gone shortly after the tool returns.
func TestBash_CancelEndsEverythingTheCommandStarted(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"nohup background child": `nohup sh -c 'echo $$ > %[1]s; exec sleep 60' >/dev/null 2>&1 & sleep 30`,
		"redirected grandchild":  `sh -c 'sh -c "echo \$\$ > %[1]s; exec sleep 60" >/dev/null 2>&1 &'; sleep 30`,
		// The trailing `true` keeps the outer shell from exec'ing the child
		// in its place, so the child really is the shell's foreground child.
		"foreground child":        `sh -c 'echo $$ > %[1]s; exec sleep 60'; true`,
		"child that ignores TERM": `sh -c 'trap "" TERM; echo $$ > %[1]s; while :; do sleep 0.1; done' >/dev/null 2>&1 & sleep 30`,
	}
	for _, how := range []string{"timeout", "context cancelled"} {
		for name, script := range cases {
			t.Run(how+"/"+name, func(t *testing.T) {
				t.Parallel()
				pidFile := filepath.Join(t.TempDir(), "pid")
				fn := bashFunc(permissions.New(permissions.Options{Mode: permissions.ModeYolo}), config.DefaultConfig())
				args := bashArgs{Command: fmt.Sprintf(script, pidFile), TimeoutSeconds: 60}
				var ctx adkagent.Context
				if how == "timeout" {
					args.TimeoutSeconds = 3 // room to record the pid on a loaded runner
				} else {
					c, cancel := context.WithCancel(context.Background())
					ctx = cancelableToolCtx{ctx: c}
					go func() {
						for {
							if b, err := os.ReadFile(pidFile); err == nil && len(b) > 0 {
								break
							}
							time.Sleep(20 * time.Millisecond)
						}
						time.Sleep(200 * time.Millisecond)
						cancel()
					}()
				}
				done := make(chan struct{})
				go func() {
					_, _ = fn(ctx, args)
					close(done)
				}()
				select {
				case <-done:
				case <-time.After(3*time.Second + bashWaitDelay + bashGroupGrace + 5*time.Second):
					t.Fatal("the bash tool did not return after the cancel")
				}
				b, err := os.ReadFile(pidFile)
				if err != nil {
					t.Fatalf("the command never recorded its pid: %v", err)
				}
				pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
				if err != nil || pid <= 0 {
					t.Fatalf("bad pid %q", b)
				}
				t.Cleanup(func() {
					if processAlive(pid) {
						_ = syscall.Kill(pid, syscall.SIGKILL)
					}
				})
				for deadline := time.Now().Add(time.Second); processAlive(pid); time.Sleep(50 * time.Millisecond) {
					if time.Now().After(deadline) {
						t.Fatalf("process %d the command started is still running after the cancel", pid)
					}
				}
			})
		}
	}
}

// TestBash_ALeftoverGetsSIGTERMEvenWhenTheShellExitedFirst: the shell exits
// at once, leaving a background child that holds the tool's stdout, and the
// timeout fires while the tool waits on that pipe. exec runs no Cancel for
// a shell that already exited, so the cleanup itself must send SIGTERM
// before the SIGKILL: the child's TERM trap records it.
func TestBash_ALeftoverGetsSIGTERMEvenWhenTheShellExitedFirst(t *testing.T) {
	t.Parallel()
	got := filepath.Join(t.TempDir(), "got-term")
	fn := bashFunc(permissions.New(permissions.Options{Mode: permissions.ModeYolo}), config.DefaultConfig())
	// The child keeps the tool's stdout; its stderr goes to /dev/null, or the
	// shell's "Terminated" notice for the killed sleep would hit the closed
	// pipe and end it with SIGPIPE before its trap ran.
	cmd := fmt.Sprintf(`sh -c 'trap "echo term > %s; exit 0" TERM; while :; do sleep 0.1; done' 2>/dev/null & exit 0`, got)
	done := make(chan struct{})
	go func() {
		_, _ = fn(adkagent.Context(nil), bashArgs{Command: cmd, TimeoutSeconds: 1})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(bashWaitDelay + bashGroupGrace + 10*time.Second):
		t.Fatal("the bash tool did not return")
	}
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if b, err := os.ReadFile(got); err == nil && strings.TrimSpace(string(b)) == "term" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the leftover was never sent SIGTERM; it was killed without a chance to exit")
		}
	}
}
