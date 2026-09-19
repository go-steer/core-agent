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

	"github.com/go-steer/core-agent/v2/internal/testutil"
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

// Directories that are not this repo's source — every dot-directory,
// plus node_modules and vendor — are pruned by testutil.PruneWalkDir,
// which is shared with the other source walks in the tree so the rule
// is stated once. dev/coretui-guard-check prunes for the same reason.

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
	swept, registers, err := sweepTree(root)
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

// sweepTree walks root and returns the package directories (relative to
// root) that register a model-facing tool and those that sweep one.
//
// Takes a root rather than finding it, so the pruning rule can be tested
// against a planted tree instead of only against this repo, where a
// directory the walk should not descend into either is not present or is
// somebody's untracked working state.
func sweepTree(root string) (swept, registers map[string]bool, err error) {
	swept, registers = map[string]bool{}, map[string]bool{}
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		if d.IsDir() {
			if testutil.PruneWalkDir(root, path, d.Name()) {
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
	return swept, registers, err
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
	root, err := testutil.RepoRoot(self)
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// A worktree under .claude is a full second checkout, so a sweep that
// descends into one grades another branch's source against this tree's
// sweep files — reporting failures that a clean shallow clone does not
// have (#964). The planted tree reproduces that exactly: a dot-directory
// holding a package that registers a tool and sweeps nothing, which is
// the shape that fails if it is seen.
//
// Planted rather than asserted against this repo, because the property
// is about a directory that must NOT be walked: in this tree such a
// directory is either absent (CI) or untracked working state (a
// developer machine with worktrees), so a test reading the real root
// proves nothing on the machine where it matters and passes vacuously
// on the one where it does not.
func TestSweepDoesNotDescendIntoDotDirectories(t *testing.T) {
	t.Parallel()
	// The root is itself dot-named, which is the one case the rule has
	// to make an exception for: a checkout can live anywhere, and a walk
	// that pruned its own root would find nothing and report it as a
	// clean tree. Naming it this way here means the exception is covered
	// by every assertion below rather than by a case of its own.
	root := filepath.Join(t.TempDir(), ".checkout")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}

	plant := func(dir, file, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, dir, file), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// A real package, swept. Present so the walk has something to find:
	// a test whose only assertion is an absence passes just as well when
	// the walk is broken end to end.
	plant("realpkg", "tools.go", "package realpkg\n\nvar _ = "+registration+")\n")
	plant("realpkg", "description_neutrality_test.go", "package realpkg\n\nvar _ = "+sweepCall+")\n")

	// The same shape inside every directory the walk must not enter.
	// node_modules and vendor are here so the two non-dot exclusions are
	// covered by this test too, rather than only by the absence of a
	// `functiontool.New(` under docs/site/node_modules in the real tree
	// — which is a fact about npm, not about the rule.
	hidden := []string{
		filepath.Join(".claude", "worktrees", "some-branch", "otherpkg"),
		filepath.Join(".fakeworktree", "otherpkg"),
		filepath.Join("node_modules", "otherpkg"),
		filepath.Join("vendor", "otherpkg"),
	}
	for _, dir := range hidden {
		plant(dir, "tools.go", "package otherpkg\n\nvar _ = "+registration+")\n")
	}

	swept, registers, err := sweepTree(root)
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	if !registers["realpkg"] || !swept["realpkg"] {
		t.Fatalf("the planted real package was not seen at all (registers=%v swept=%v) — the walk found nothing, so the absences below prove nothing", registers, swept)
	}
	for _, dir := range hidden {
		if registers[dir] {
			t.Errorf("sweep descended into %s, which is not this repo's source: a dot-directory is untracked working state (and .claude/worktrees holds full second checkouts), node_modules and vendor are somebody else's code", dir)
		}
	}
}
