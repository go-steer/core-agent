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
	"strings"
	"time"

	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"

	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
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
	// Notice carries advisory feedback about the command itself,
	// distinct from anything the command printed. Currently only the
	// search gate in "warn" mode (#158) sets it: the command ran, and
	// this says there was a better tool for it. Omitted when empty so
	// the common result shape is unchanged.
	Notice string `json:"notice,omitempty"`
}

// bashDescription renders the model-facing description, which has to
// match what the gate will actually do. "Prefer the structured tools"
// is accurate in warn/allow mode and an understatement in enforce mode,
// where a `grep -rn` is refused outright — and a model that learns the
// rule from a refusal has already spent a turn learning it (#158).
func bashDescription(gate *permissions.Gate) string {
	const opening = "Execute a shell command via /bin/sh -c with a timeout."
	const closing = " Use this tool for actions the structured tools cannot perform: builds, " +
		"tests, git, formatters, package managers, and other shell-native workflows."

	// Name only the structured tools this build registered. The
	// redirect is the whole point of the sentence, so pointing at a
	// tool that isn't in the catalog is worse than saying nothing —
	// same rule the enforce branch below already follows for the
	// native search tools.
	var structured []string
	for _, n := range []string{"read_file", "grep", "glob", "list_dir"} {
		if gate.HasTool(n) {
			structured = append(structured, "`"+n+"`")
		}
	}
	base := opening
	if len(structured) > 0 {
		base += " For code investigation (reading files, searching source, listing directories), " +
			"prefer the structured " + strings.Join(structured, ", ") +
			" tools — they honor the permission gate and per-tool output caps."
	}
	base += closing
	if gate == nil || gate.BashSearchGate() != config.BashSearchGateEnforce {
		return base
	}
	gated := gate.ActiveSearchBinaries()
	if len(gated) == 0 {
		// Enforce with no native tool to redirect to: the gate refuses
		// nothing, so the description must not threaten a refusal.
		return base
	}
	// Name only the natives this build registered: a description that
	// says "use `glob`" when glob was disabled sends the model at a
	// tool that isn't there.
	natives := gate.ActiveNativeSearchTools()
	for i, n := range natives {
		natives[i] = "`" + n + "`"
	}
	nativePhrase := strings.Join(natives, " / ") + " tool"
	if len(natives) > 1 {
		nativePhrase += "s"
	}
	return base + " Search-shaped commands are REFUSED here: a command whose verb is " +
		strings.Join(gated, "/") +
		" must go through the native " + nativePhrase +
		" instead. Piping into one of those " +
		"(`go test ./... | grep -v ok`) is fine — that filters a stream rather than searching a tree."
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
	return func(ctx tool.Context, in bashArgs) (bashResult, error) {
		if in.Command == "" {
			return bashResult{}, fmt.Errorf("bash: command is required")
		}
		if err := gate.CheckBash(ctx, in.Command); err != nil {
			return bashResult{}, err
		}
		// Resolved before the command runs so a timeout or a non-zero
		// exit still carries the advice — those are exactly the
		// outcomes a bash-as-grep call produces (#158's session opened
		// with a grep that matched nothing and a find that exited 123).
		notice := gate.BashSearchNotice(ctx, in.Command)
		timeout := defaultBashTimeout
		if in.TimeoutSeconds > 0 {
			timeout = time.Duration(in.TimeoutSeconds) * time.Second
		}
		// Parent the exec context to the inbound tool ctx (not
		// context.Background) so the turn-level cancel signal — the
		// operator hitting /interrupt, the wake loop's daemonCtx
		// expiring on SIGTERM, agent.Run's cleanup — propagates to
		// the shell. Without this, a hung command (gcloud waiting
		// on auth, kubectl waiting on slow API, etc.) ignores
		// every cancel until the bash timeout fires and the
		// operator can't kill it via /interrupt at all.
		//
		// tool.Context is an interface; some tests pass nil. Fall
		// back to Background in that case so context.WithTimeout
		// doesn't panic on "nil parent."
		parent := context.Context(ctx)
		if parent == nil {
			parent = context.Background()
		}
		execCtx, cancel := context.WithTimeout(parent, timeout)
		defer cancel()

		// Running an arbitrary user-supplied command is the whole point
		// of the bash tool; gating happens via the permission gate, not
		// at the exec call site.
		cmd := exec.CommandContext(execCtx, "/bin/sh", "-c", in.Command) // #nosec G204
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
		timedOut := execCtx.Err() == context.DeadlineExceeded

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
			Notice:   notice,
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
