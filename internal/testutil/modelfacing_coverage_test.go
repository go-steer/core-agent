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

package testutil_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// sweepCall is what makes a package swept. The convention is to put it
// in a description_neutrality_test.go, but the requirement is the call,
// not the filename: pkg/tools/agentic runs its sweep over the same
// table as the test that asserts the OPPOSITE (one requires phrases,
// the other forbids them), and splitting that pair across files to
// satisfy a naming rule would cost more than it buys.
const sweepCall = "testutil.ModelFacingBanViolations("

// registration is the one way a model-facing tool is built in this
// repo. Every tool in every catalog goes through it, including the ones
// composed at registration time, so it is a sound proxy for "this
// package puts text in front of a model".
//
// A proxy, not a proof: a hand-rolled tool.Tool implementation would
// register without matching, and MCP tools come from the SDK and carry
// the server's text, not ours. Nothing in-tree does the former today,
// and the latter is not this repo's prose to hold to a standard.
const registration = "functiontool.New("

// unsweptRoots are the top-level trees a sweep miss does not ship.
//
// examples/ is sample code a reader copies and adapts; its descriptions
// are illustrative and the reader owns the copy. Everything else that
// registers a tool ends up in the binary, where a recipe author can
// neither see the text nor override it — which is the entire premise of
// ModelFacingBans.
var unsweptRoots = []string{"examples"}

// skipDirs are pruned wherever they appear: not source, and one of them
// (docs/site/node_modules) is large enough to be worth not walking.
var skipDirs = map[string]bool{".git": true, "node_modules": true, "vendor": true}

// #909 shipped a ban list, swept four packages, and wrote "the four
// packages that register model-facing tools" in a comment. The comment
// was wrong the day it was written: pkg/agent registers mark_task_done
// — the tool whose description started the whole thread — and
// pkg/tools/peer registers call_peer, and neither was swept (#919).
//
// A prose count cannot notice a new package. This can: it walks the
// source for the one call every model-facing tool is built with and
// requires a sweep file to sit beside it. Adding a tool to a new
// package now fails here until the sweep exists, which is the only
// structural difference between a list that stays true and a list that
// documents what someone once checked.
//
// Deliberately not a check that the sweep file is CORRECT — a package
// can still write a sweep that misses its own conditionally-registered
// tools, which is a second thing #919 found (pkg/tools' helper never
// registered `alert`). That failure mode belongs to the individual
// sweeps, and each one now asserts its own coverage explicitly.
func TestEveryToolRegisteringPackageHasASweep(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	swept := map[string]bool{}
	registers := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			for _, skip := range unsweptRoots {
				if rel == skip {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") {
			return nil
		}
		dir := filepath.Dir(rel)
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if strings.HasSuffix(d.Name(), "_test.go") {
			if strings.Contains(string(body), sweepCall) {
				swept[dir] = true
			}
			return nil
		}
		if strings.Contains(string(body), registration) {
			registers[dir] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	if len(registers) == 0 {
		t.Fatalf("found no package containing %q under %s: the guard would pass vacuously", registration, root)
	}
	for dir := range registers {
		if !swept[dir] {
			t.Errorf("%s registers a model-facing tool but no test in it calls %s — its descriptions and arg schemas are unswept. Add a description_neutrality_test.go (see internal/testutil.ModelFacingBans)", dir, sweepCall)
		}
	}
}

// repoRoot walks up from this source file to the module root. Derived
// from the compiled-in source path rather than the working directory,
// which `go test` sets to the package directory.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed: cannot locate the source tree")
	}
	dir := filepath.Dir(self)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod above %s", filepath.Dir(self))
		}
		dir = parent
	}
}
