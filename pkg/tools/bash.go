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
	"context"
	"fmt"
	"os/exec"
	"time"

	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"

	"github.com/go-steer/core-agent/pkg/config"
	"github.com/go-steer/core-agent/pkg/permissions"
)

type bashArgs struct {
	Command        string `json:"command" jsonschema:"single shell command to execute via /bin/sh -c"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"max wall time for the command (default 30)"`
}

type bashResult struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	TimedOut bool   `json:"timed_out,omitempty"`
}

const defaultBashTimeout = 30 * time.Second

// bashWaitDelay is the grace period after the immediate child exits
// (or the context cancels) before Go's exec package force-closes any
// inherited stdout/stderr and kills subprocesses still holding the
// pipes open. Required because shell commands often spawn background
// processes that inherit those file descriptors:
//
//	node server.js & SERVER_PID=$! && sleep 1.5 && client && kill $SERVER_PID
//
// Here `kill $SERVER_PID` actually kills the backgrounded subshell
// (the `&` binds at the subshell level), not the orphaned node
// server. Node keeps holding the stdout/stderr write-ends, so
// cmd.Wait blocks on the internal pipe-copy goroutine — defeating
// the timeout entirely. WaitDelay's job is exactly this: SIGKILL
// any subprocess still holding the pipes after the grace window.
//
// 5s is long enough that benign shell trailers (a wait, a final
// print) finish naturally; short enough that real hangs surface
// quickly. Added in Go 1.20; we're on a newer toolchain.
const bashWaitDelay = 5 * time.Second

func bashFunc(gate *permissions.Gate, cfg *config.Config) functiontool.Func[bashArgs, bashResult] {
	return func(_ tool.Context, in bashArgs) (bashResult, error) {
		if in.Command == "" {
			return bashResult{}, fmt.Errorf("bash: command is required")
		}
		if err := gate.CheckBash(context.Background(), in.Command); err != nil {
			return bashResult{}, err
		}
		timeout := defaultBashTimeout
		if in.TimeoutSeconds > 0 {
			timeout = time.Duration(in.TimeoutSeconds) * time.Second
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()

		// Running an arbitrary user-supplied command is the whole point
		// of the bash tool; gating happens via the permission gate, not
		// at the exec call site.
		cmd := exec.CommandContext(ctx, "/bin/sh", "-c", in.Command) // #nosec G204
		// Bound how long we wait on inherited stdout/stderr after the
		// shell exits or the context cancels. See bashWaitDelay docs.
		cmd.WaitDelay = bashWaitDelay
		var stdout, stderr capBuffer
		caps := capsFor(cfg, "bash", 64*1024, 2000)
		stdout.maxBytes = caps.bytes
		stderr.maxBytes = caps.bytes
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr

		runErr := cmd.Run()
		timedOut := ctx.Err() == context.DeadlineExceeded

		exit := 0
		if runErr != nil {
			if ee, ok := runErr.(*exec.ExitError); ok {
				exit = ee.ExitCode()
			} else if !timedOut {
				exit = -1
			}
		}
		out := bashResult{
			ExitCode: exit,
			Stdout:   Truncate(stdout.String(), caps.bytes, caps.lines),
			Stderr:   Truncate(stderr.String(), caps.bytes, caps.lines),
			TimedOut: timedOut,
		}
		if timedOut {
			return out, fmt.Errorf("bash: timed out after %s", timeout)
		}
		return out, nil
	}
}

// capBuffer is a minimal io.Writer with a hard byte cap. Writes past
// the cap are silently dropped to bound memory while still producing
// useful (truncated) output.
type capBuffer struct {
	buf      []byte
	maxBytes int
}

func (c *capBuffer) Write(p []byte) (int, error) {
	written := len(p)
	if c.maxBytes <= 0 || len(c.buf) < c.maxBytes {
		room := c.maxBytes - len(c.buf)
		if c.maxBytes <= 0 || room >= len(p) {
			c.buf = append(c.buf, p...)
		} else {
			c.buf = append(c.buf, p[:room]...)
		}
	}
	return written, nil
}

func (c *capBuffer) String() string { return string(c.buf) }
