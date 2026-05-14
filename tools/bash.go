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

	"github.com/go-steer/core-agent/config"
	"github.com/go-steer/core-agent/permissions"
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
