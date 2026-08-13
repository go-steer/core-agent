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
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	adktool "google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
)

// This file ports bouncer's `sandbox_run_command`
// (shared_tools/tools.py) — the single most important reason this
// port is a *library* consumer rather than a config/recipe on the
// shipped binary.
//
// core-agent ships no execution sandbox: pkg/tools/bash.go runs
// /bin/sh -c with the daemon's own uid, and the permissions gate
// delegates containment to whatever wraps the process. bouncer's
// threat model is the opposite way round — the model writes the
// kubectl commands, so the shell must be a jail. As a library
// consumer that is a non-issue: the built-in bash tool is simply
// never registered, and the only shell the model can reach is this
// one, which drops privileges to `agent-runner` and re-execs under
// bubblewrap with a read-only root.
//
// Mode "none" runs the command directly with no jail. It exists so
// the hermetic tests and a laptop demo can run without bwrap/sudo
// installed; it is NOT a supported production mode and main.go says
// so on stderr at boot.

const (
	sandboxModeBwrap = "bwrap"
	sandboxModeNone  = "none"

	// sandboxRunAsUser matches bouncer's unprivileged sandbox uid.
	sandboxRunAsUser = "agent-runner"

	// maxSandboxOutput matches bouncer's 8000-byte clamp on each of
	// stdout and stderr — a runaway `kubectl get` would otherwise
	// blow the context window in one tool call.
	maxSandboxOutput = 8000

	// defaultSandboxTimeout bounds a single command. bouncer's
	// subprocess.run has no timeout at all; a hung `kubectl logs -f`
	// wedges the agent forever. Five minutes is generous for the
	// kubectl calls the prompts describe.
	defaultSandboxTimeout = 5 * time.Minute

	// sandboxWaitDelay mirrors pkg/tools/bash.go: force-close
	// inherited pipes shortly after the direct child exits so a
	// backgrounded process can't wedge cmd.Wait.
	sandboxWaitDelay = 5 * time.Second
)

// serviceAccountDir is the in-cluster projected token mount. bouncer
// bind-mounts it read-only so kubectl inside the jail authenticates
// as the pod's service account.
const serviceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount"

// sandbox executes model-authored shell commands under bubblewrap.
type sandbox struct {
	// Mode is sandboxModeBwrap or sandboxModeNone.
	Mode string
	// Workspace is the one writable path inside the jail; it is
	// bind-mounted at /workspace and is the working directory.
	Workspace string
	// RunAsUser is the uid to drop to via sudo. Empty skips the drop
	// (bouncer does the same when the user doesn't exist).
	RunAsUser string
	// Timeout bounds one command; zero means defaultSandboxTimeout.
	Timeout time.Duration
	// exists reports whether a path is present; injectable so the
	// argv tests don't depend on the host having /var/run/secrets.
	exists func(string) bool
}

func newSandbox(mode, workspace string) *sandbox {
	return &sandbox{
		Mode:      mode,
		Workspace: workspace,
		RunAsUser: sandboxRunAsUser,
		Timeout:   defaultSandboxTimeout,
	}
}

func (s *sandbox) pathExists(p string) bool {
	if s.exists != nil {
		return s.exists(p)
	}
	_, err := os.Stat(p)
	return err == nil
}

// argv builds the exact command line for one sandboxed shell command.
// Split out from run so the jail flags are unit-testable without
// bwrap installed — the flags ARE the security boundary, so a silent
// drop of --unshare-user must fail a test, not a pentest.
func (s *sandbox) argv(command string) []string {
	if s.Mode != sandboxModeBwrap {
		return []string{"/bin/sh", "-c", command}
	}
	var argv []string
	if s.RunAsUser != "" {
		// -n: never prompt (there is no tty); -E: keep the env so
		// KUBERNETES_SERVICE_HOST et al. survive the uid drop.
		argv = append(argv, "sudo", "-n", "-E", "-u", s.RunAsUser)
	}
	argv = append(argv,
		"bwrap",
		"--unshare-user",
		"--unshare-pid",
		"--unshare-ipc",
		"--unshare-cgroup",
		"--ro-bind", "/", "/",
		"--proc", "/proc",
		"--dev", "/dev",
		"--tmpfs", "/tmp",
		"--bind", s.Workspace, "/workspace",
		"--chdir", "/workspace",
	)
	if s.pathExists(serviceAccountDir) {
		argv = append(argv, "--ro-bind", serviceAccountDir, serviceAccountDir)
	}
	return append(argv, "bash", "-c", command)
}

// sandboxOutcome is what the model sees back from a command.
type sandboxOutcome struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout,omitempty"`
	Stderr   string `json:"stderr,omitempty"`
	TimedOut bool   `json:"timed_out,omitempty"`
}

// run executes command and returns its outcome. Like bouncer, a
// non-zero exit is data for the model, not a tool error: the
// generator prompt tells it to read failures and patch the manifest.
func (s *sandbox) run(ctx context.Context, command string) sandboxOutcome {
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = defaultSandboxTimeout
	}
	if ctx == nil {
		ctx = context.Background()
	}
	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	argv := s.argv(command)
	// #nosec G204 -- executing model-authored commands is this
	// tool's entire purpose; containment is the bwrap jail and the
	// uid drop above, not argument filtering.
	cmd := exec.CommandContext(execCtx, argv[0], argv[1:]...)
	cmd.WaitDelay = sandboxWaitDelay
	if s.Mode != sandboxModeBwrap && s.Workspace != "" {
		// bwrap --chdir handles this inside the jail; the plain
		// mode needs it set on the child.
		cmd.Dir = s.Workspace
	}
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	out := sandboxOutcome{
		Stdout: truncate(stdout.String(), maxSandboxOutput),
		Stderr: truncate(stderr.String(), maxSandboxOutput),
	}
	switch {
	case err == nil:
		return out
	case errors.Is(execCtx.Err(), context.DeadlineExceeded):
		out.TimedOut = true
		out.ExitCode = -1
		out.Stderr = truncate(out.Stderr+"\ncommand exceeded "+timeout.String()+" and was killed", maxSandboxOutput)
		return out
	default:
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			out.ExitCode = exitErr.ExitCode()
			return out
		}
		// Couldn't start at all (bwrap/sudo missing, etc.). Report
		// it to the model rather than killing the run.
		out.ExitCode = -1
		out.Stderr = truncate(out.Stderr+"\n"+err.Error(), maxSandboxOutput)
		return out
	}
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := s[:max]
	// Don't hand back half a rune. Upstream slices a Python str, which
	// is rune-indexed and can't split one; slicing Go bytes at 8000 can,
	// and an invalid-UTF-8 tool result is rejected by the model API —
	// turning "your kubectl printed a lot" into a failed turn. Bounded
	// to one rune's worth of bytes so genuinely binary output is
	// truncated rather than eaten.
	for i := 0; i < utf8.UTFMax-1 && len(cut) > 0 && !utf8.ValidString(cut); i++ {
		cut = cut[:len(cut)-1]
	}
	return cut + "\n...[Output Truncated]..."
}

type sandboxArgs struct {
	Command string `json:"command" jsonschema_description:"bash command to run inside the sandbox; kubectl, jq and yq are available and /workspace is the only writable path"`
}

// sandboxTool exposes the jail to the model under bouncer's tool
// name, so the ported prompts read the same as they do upstream.
func sandboxTool(s *sandbox) (adktool.Tool, error) {
	return functiontool.New(
		functiontool.Config{
			Name: "sandbox_run_command",
			Description: "Run a bash command inside the locked-down agent sandbox. " +
				"The sandbox has kubectl, jq and yq, sees the cluster through the pod's " +
				"service account, and can only write to /workspace. Non-zero exits are " +
				"returned to you rather than raised: read the stderr and try again.",
		},
		func(ctx adktool.Context, in sandboxArgs) (sandboxOutcome, error) {
			if strings.TrimSpace(in.Command) == "" {
				return sandboxOutcome{}, errors.New("sandbox_run_command: command is required")
			}
			return s.run(ctx, in.Command), nil
		},
	)
}

// workspacePath maps a file in the session scratch dir to the path
// the model should use for it. Inside the jail the scratch dir is
// bind-mounted at /workspace; uncontained, it stays where it is.
func (s *sandbox) workspacePath(name string) string {
	if s.Mode == sandboxModeBwrap {
		return "/workspace/" + name
	}
	return filepath.Join(s.Workspace, name)
}

// maxWaitSeconds caps a single wait_seconds call at the 900s the
// checker prompt asks for. Upstream's tool is an unbounded
// time.sleep(); a model that asks for 86400 should not be able to
// wedge the run for a day.
const maxWaitSeconds = 900

type waitArgs struct {
	Seconds int `json:"seconds" jsonschema_description:"how long to wait, in seconds (capped at 900)"`
}

type waitResult struct {
	WaitedSeconds int  `json:"waited_seconds"`
	Capped        bool `json:"capped,omitempty"`
}

// waitTool is bouncer's wait_seconds: the poll-and-sleep loop the
// checker uses while a TPU slice waits on quota.
//
// core-agent's own answer to "come back later" is the scheduler
// (schedule_next_turn + a Scheduler on the autonomous loop), which
// releases the process between polls instead of holding it. That is
// the better shape for the generator's outer loop; the checker's
// in-turn wait is faithfully a sleep, so this stays one.
func waitTool() (adktool.Tool, error) {
	return functiontool.New(
		functiontool.Config{
			Name: "wait_seconds",
			Description: "Pause before checking again — use this while a workload is Pending on quota. " +
				"Capped at 900 seconds per call.",
		},
		func(ctx adktool.Context, in waitArgs) (waitResult, error) {
			return waitFunc(stdContext(ctx), in)
		},
	)
}

func waitFunc(ctx context.Context, in waitArgs) (waitResult, error) {
	seconds := in.Seconds
	capped := false
	if seconds > maxWaitSeconds {
		seconds, capped = maxWaitSeconds, true
	}
	if seconds <= 0 {
		return waitResult{}, nil
	}
	if err := sleepCtx(stdContext(ctx), time.Duration(seconds)*time.Second); err != nil {
		return waitResult{}, err
	}
	return waitResult{WaitedSeconds: seconds, Capped: capped}, nil
}

// stdContext unwraps a tool context into a plain one, tolerating the
// nil that unit tests pass in place of a live invocation.
func stdContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

// resolveWorkspace makes the workspace path absolute and ensures it
// exists — bwrap fails with an opaque error if the bind source is
// missing.
func resolveWorkspace(dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", err
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}
