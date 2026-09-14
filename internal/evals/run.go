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

package evals

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// DefaultTimeout bounds one case run.
//
// Wall clock is a design constraint from case one, not a thing to tune
// later: a suite that takes hours gets batched around, and a feedback
// loop measured in half-days is one nobody runs before pushing. A case
// that cannot answer in five minutes is too big to be case one.
const DefaultTimeout = 5 * time.Minute

// waitDelay is the grace period after the agent exits — or after the
// timeout cancels it — before Go's exec package force-closes the
// inherited stdout/stderr pipes and kills whatever is still holding
// them.
//
// Without it, Timeout does not bound anything. The agent under test has
// a bash tool, so a run can leave a backgrounded grandchild holding the
// pipe write-ends; cmd.Wait then blocks on its pipe-copy goroutine long
// after the context cancelled the agent itself, and the eval reports
// the wall clock of the orphan. pkg/tools/bash.go carries the same
// remedy for the same reason, one layer down.
const waitDelay = 5 * time.Second

// A Runner executes one case against one fixture.
type Runner struct {
	// Binary is the core-agent executable under test.
	Binary string

	// Args are prepended to the agent's own arguments — this is where
	// the provider selection lives, because which provider the eval
	// runs against is the caller's business and not the corpus's.
	Args []string

	// Timeout bounds the agent process. Zero means DefaultTimeout.
	Timeout time.Duration

	// KeepWorld leaves the materialized world on disk and reports its
	// path, for reading a failure afterwards.
	KeepWorld bool

	// Log, when set, receives progress lines.
	Log func(format string, args ...any)
}

// A RunArtifacts bundle is what one execution left behind.
type RunArtifacts struct {
	// WorldRoot is the materialized fixture. Removed unless KeepWorld.
	WorldRoot string

	// Stdout is the agent's output; Stderr is kept separately so a
	// provider error does not land in the graded text.
	Stdout string
	Stderr string

	// ExitErr is the process's error, if any.
	ExitErr error
}

// Run executes one case at one tier and grades it.
//
// The two tiers differ by exactly one flag. That is deliberate: if the
// baseline run differed from the real one in prompt, fixture or checks,
// a baseline of zero would prove something about the baseline's setup
// rather than about the check.
func (r *Runner) Run(ctx context.Context, c *Case, f *Fixture, tier Tier) (Result, error) {
	bound, err := c.Bind(f)
	if err != nil {
		return Result{}, err
	}

	res := Result{
		CaseID:        bound.ID,
		Fixture:       f.Name,
		PlantedDefect: bound.PlantedDefect,
		Tier:          tier,
	}

	worldRoot, err := os.MkdirTemp("", "core-agent-eval-"+sanitize(bound.ID)+"-"+sanitize(string(tier))+"-")
	if err != nil {
		return res, fmt.Errorf("evals: temp world: %w", err)
	}
	keep := r.KeepWorld
	defer func() {
		if !keep {
			_ = os.RemoveAll(worldRoot)
		}
	}()

	world, err := f.Materialize(worldRoot)
	if err != nil {
		keep = true
		return res, fmt.Errorf("%w (world kept at %s)", err, worldRoot)
	}
	r.logf("world materialized at %s", worldRoot)

	art := r.exec(ctx, bound, world, tier)
	res.Answer = art.Stdout
	if art.ExitErr != nil {
		// Not fatal to grading. A run that crashed after doing the
		// work still leaves witnesses, and a report that says "the
		// checks say X and the process also exited non-zero" is more
		// use than a bare exit code. The verdict can never be a pass.
		res.RunError = summarizeExit(art.ExitErr, art.Stderr)
	}

	for _, ck := range bound.Checks {
		res.Checks = append(res.Checks, ck.Verify(sourceFor(ck, world, art)))
	}
	// A vacuous result is the one a reader most needs the world for: it
	// means a witness was never written, and the question is why.
	// Keeping it costs a temp directory; not keeping it costs the re-run
	// that would have answered the question.
	//
	// Except in the baseline, where an unwritten witness is the point.
	// Keeping that world every time would fill /tmp with copies of the
	// case that behaved exactly as designed, and print a line that reads
	// like a problem beside two checks that are working.
	if res.Vacuous() && tier != TierNoAccess {
		keep = true
		r.logf("result is indeterminate; keeping the world at %s", worldRoot)
	}
	return res, nil
}

func sourceFor(ck Check, world *World, art RunArtifacts) Source {
	if name, ok := ck.WitnessName(); ok {
		text, present := world.ReadWitness(name)
		return Source{Text: text, Present: present}
	}
	// The answer is always "present" even when empty: the process ran
	// and said nothing, which is a finding rather than an absence.
	return Source{Text: art.Stdout, Present: true}
}

func (r *Runner) exec(ctx context.Context, c *Case, world *World, tier Tier) RunArtifacts {
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := append([]string{}, r.Args...)
	// --agentic-tools=false is carried by BOTH tiers, which is the only
	// reason it is here rather than in the no-access branch. The wrappers
	// refuse to register when they have nothing to wrap — with the
	// builtin suite off, `--agentic-tools` is a startup error, not a
	// degraded mode (pkg/compose/agentic.go). Turning them off only for
	// the baseline would buy a tier that starts at the price of a second
	// difference between the runs, and then a baseline of zero would be
	// evidence about the baseline's tool wiring rather than about the
	// fixture being out of reach. Off on both sides, the one flag below
	// remains the whole of the difference.
	args = append(args, "--agentic-tools=false", "--yolo", "-p", c.Prompt)
	if tier == TierNoAccess {
		// The whole baseline, in one flag. Same binary, same prompt,
		// same fixture on disk — the agent simply has no way to reach
		// any of it.
		args = append(args, "--no-builtin-tools")
	}

	// #nosec G204 — launching an operator-named binary with
	// operator-named arguments is this type's entire job; Binary comes
	// from --binary and Args from --agent-args, both typed by whoever is
	// running the eval.
	cmd := exec.CommandContext(ctx, r.Binary, args...)
	cmd.Dir = world.Workdir
	cmd.Env = buildEnv(world, tier)
	cmd.WaitDelay = waitDelay

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	r.logf("running %s (tier=%s, timeout=%s)", filepath.Base(r.Binary), tier, timeout)
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		err = fmt.Errorf("agent did not finish within %s: %w", timeout, ctx.Err())
	}
	return RunArtifacts{
		WorldRoot: world.Root,
		Stdout:    stdout.String(),
		Stderr:    stderr.String(),
		ExitErr:   err,
	}
}

func buildEnv(world *World, tier Tier) []string {
	env := os.Environ()
	for k, v := range world.Env {
		env = append(env, k+"="+v)
	}
	if world.PathDir != "" && tier != TierNoAccess {
		// Only the tools tier gets the fixture on PATH. Leaving it
		// there for the baseline would be harmless today — the agent
		// has no bash to reach it with — but it would make the
		// baseline's zero depend on the tool suite staying off rather
		// than on the fixture being unreachable, and those are
		// different guarantees.
		env = append(env, "PATH="+world.PathDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	return env
}

func summarizeExit(err error, stderr string) string {
	msg := err.Error()
	trimmed := strings.TrimSpace(stderr)
	if trimmed == "" {
		return msg
	}
	lines := strings.Split(trimmed, "\n")
	if len(lines) > 5 {
		lines = lines[len(lines)-5:]
	}
	return msg + ": " + strings.Join(lines, " | ")
}

func (r *Runner) logf(format string, args ...any) {
	if r.Log != nil {
		r.Log(format, args...)
	}
}

func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + 32)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}
