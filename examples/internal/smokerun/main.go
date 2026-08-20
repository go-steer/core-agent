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

// Command examples-smoke builds and runs the example programs
// examples/internal/smokeset marks runnable, and fails on any that
// does not exit 0.
//
// The problem it exists for (#852): `go build ./...` proves the
// examples compile, and nothing proved they still run. parallel-spawn
// had been exiting 1 for some time while its README advertised
// "Exits 0"; it was found by hand, not by a check.
//
// The runnable set, and the reason each excluded example is excluded,
// live in examples/internal/smokeset — with a test there requiring
// every examples/*/main.go to appear in one column or the other, so a
// new example cannot be silently uncovered.
//
// Modes:
//
//	examples-smoke           # build and run; non-zero exit on any failure
//	examples-smoke --print   # dump the disposition table and exit
//
// Run it through dev/tools/examples-smoke, which is what
// dev/ci/presubmits/examples-smoke calls.
//
// It lives under examples/internal rather than beside the other dev
// tools because the manifest it reads is an internal package of the
// examples tree, and Go's internal rule is what keeps that boundary
// honest: examples/internal/smokeset is for the examples, not a
// general-purpose package the rest of the repo can grow to depend on.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/go-steer/core-agent/v2/examples/internal/smokeset"
)

func main() {
	printOnly := flag.Bool("print", false, "dump the disposition table and exit")
	timeout := flag.Duration("timeout", 0, "override the per-example timeout (default: smokeset.DefaultTimeout)")
	flag.Parse()

	if *printOnly {
		printTable(os.Stdout)
		return
	}

	if err := run(*timeout); err != nil {
		fmt.Fprintf(os.Stderr, "examples-smoke: %v\n", err)
		os.Exit(1)
	}
}

func printTable(w *os.File) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "EXAMPLE\tDISPOSITION\tREASON")
	for _, e := range smokeset.Entries {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", e.Name, e.Disposition, e.Reason)
	}
	_ = tw.Flush()
}

func run(timeoutOverride time.Duration) error {
	runnable := smokeset.Runnable()
	if len(runnable) == 0 {
		return errors.New("smokeset.Runnable() is empty; nothing to smoke")
	}

	binDir, err := os.MkdirTemp("", "examples-smoke-")
	if err != nil {
		return fmt.Errorf("temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(binDir) }()

	// One `go build` for the whole set rather than `go run` per
	// example: `go run` relinks every time, and the compile is most of
	// the wall clock.
	args := []string{"build", "-o", binDir + string(os.PathSeparator)}
	for _, e := range runnable {
		args = append(args, "./examples/"+e.Name)
	}
	build := exec.Command("go", args...)
	build.Stderr = os.Stderr
	build.Stdout = os.Stderr
	if err := build.Run(); err != nil {
		return fmt.Errorf("building the runnable set: %w", err)
	}

	before, gitAnswers := worktreeState()

	var failures []string
	start := time.Now()
	for _, e := range runnable {
		bound := e.Bound()
		if timeoutOverride > 0 {
			bound = timeoutOverride
		}
		took, out, err := runOne(filepath.Join(binDir, e.Name), bound)
		if err != nil {
			failures = append(failures, e.Name)
			fmt.Printf("FAIL  %-24s %5.1fs  %v\n", e.Name, took.Seconds(), err)
			for _, line := range tail(out, 20) {
				fmt.Printf("      | %s\n", line)
			}
			continue
		}
		fmt.Printf("ok    %-24s %5.1fs\n", e.Name, took.Seconds())
	}
	fmt.Printf("examples-smoke: %d examples in %.1fs\n", len(runnable), time.Since(start).Seconds())

	// An example that starts writing into the checkout is a defect the
	// exit code would not catch: it passes here and leaves CI with a
	// dirty tree for whatever runs next.
	if after, ok := worktreeState(); gitAnswers && ok && after != before {
		fmt.Fprintf(os.Stderr, "examples-smoke: the run left the worktree dirty; "+
			"an example is writing into the checkout instead of a temp dir\n%s\n", after)
		failures = append(failures, "(worktree dirtied)")
	}

	if len(failures) > 0 {
		return fmt.Errorf("%d failed: %s", len(failures), strings.Join(failures, ", "))
	}
	return nil
}

// runOne runs one built example with no arguments and no inherited
// provider credentials, bounded by timeout.
func runOne(bin string, timeout time.Duration) (time.Duration, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	start := time.Now()
	cmd := exec.CommandContext(ctx, bin)
	cmd.Env = scrubbedEnv()
	out, err := cmd.CombinedOutput()
	took := time.Since(start)

	if ctx.Err() != nil {
		return took, string(out), fmt.Errorf("timed out after %s", timeout)
	}
	if err != nil {
		return took, string(out), err
	}
	return took, string(out), nil
}

// providerKeyPrefixes are dropped from the child environment. Every
// runnable example defaults to the scripted mock provider and opts
// into a real one behind a flag the runner never passes — but a
// maintainer's shell has these set, and a gate whose result depends on
// whose machine it runs on is not a gate.
var providerKeyPrefixes = []string{
	"GEMINI_API_KEY",
	"GOOGLE_API_KEY",
	"GOOGLE_APPLICATION_CREDENTIALS",
	"GOOGLE_GENAI_USE_VERTEXAI",
	"ANTHROPIC_API_KEY",
	"ANTHROPIC_AUTH_TOKEN",
	"OPENAI_API_KEY",
}

func scrubbedEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		drop := false
		for _, p := range providerKeyPrefixes {
			if name == p {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, kv)
		}
	}
	return out
}

// worktreeState returns `git status --porcelain` output. The second
// result is false when git cannot answer — a source tarball, say — in
// which case the cleanliness check is skipped rather than failed.
// A clean tree answers ("", true), which is why this cannot be a bare
// string: "clean" and "no answer" are the two states that must not be
// confused, since clean-before is the normal case.
func worktreeState() (string, bool) {
	out, err := exec.Command("git", "status", "--porcelain").Output()
	if err != nil {
		return "", false
	}
	return string(out), true
}

func tail(s string, n int) []string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}
