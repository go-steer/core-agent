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

package smokeset_test

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/go-steer/core-agent/v3/examples/internal/smokeset"
)

// examplesDir is the examples/ tree, relative to this package.
const examplesDir = "../.."

// TestEveryExampleProgramIsAccountedFor is the half that keeps the
// list from rotting. The smoke runner iterates smokeset.Entries, so an
// example missing from it is not skipped-with-a-reason — it is
// invisible, and the gate's silence reads as coverage.
func TestEveryExampleProgramIsAccountedFor(t *testing.T) {
	found, err := smokeset.Discover(examplesDir)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(found) < 10 {
		t.Fatalf("only %d example programs discovered under %s; the discovery rule drifted "+
			"and this test would pass vacuously", len(found), examplesDir)
	}

	recorded := map[string]bool{}
	for _, e := range smokeset.Entries {
		recorded[e.Name] = true
	}

	onDisk := map[string]bool{}
	for _, name := range found {
		onDisk[name] = true
		if !recorded[name] {
			t.Errorf("examples/%s/main.go exists but smokeset.Entries does not mention it — "+
				"add it as Run, or as Skip with the reason it cannot run unattended", name)
		}
	}
	for _, e := range smokeset.Entries {
		if !onDisk[e.Name] {
			t.Errorf("smokeset.Entries records %q, but examples/%s/main.go does not exist — "+
				"the example was renamed or removed", e.Name, e.Name)
		}
	}
}

// TestEntriesAreWellFormed pins the properties the runner and the
// accounting test both assume: no duplicates, sorted for readable
// diffs, a valid disposition, and — the one that carries meaning — a
// reason on every exclusion and none on any inclusion.
func TestEntriesAreWellFormed(t *testing.T) {
	seen := map[string]bool{}
	seenBase := map[string]string{}
	var names []string
	for _, e := range smokeset.Entries {
		if seen[e.Name] {
			t.Errorf("duplicate entry for %q", e.Name)
		}
		seen[e.Name] = true
		names = append(names, e.Name)

		if e.Disposition == smokeset.Run {
			base := filepath.Base(e.Name)
			if prev, dup := seenBase[base]; dup {
				t.Errorf("runnable entries %q and %q share binary name %q (go build -o <dir>/ would overwrite one)", prev, e.Name, base)
			}
			seenBase[base] = e.Name
		}

		switch e.Disposition {
		case smokeset.Run:
			if e.Reason != "" {
				t.Errorf("%s is Run but carries a reason (%q); a reason on a runnable example "+
					"reads as a caveat nobody has to honour", e.Name, e.Reason)
			}
		case smokeset.Skip:
			if e.Reason == "" {
				t.Errorf("%s is Skip with no reason; an unexplained exclusion is "+
					"indistinguishable from an oversight", e.Name)
			}
		default:
			t.Errorf("%s has disposition %q, which is neither Run nor Skip", e.Name, e.Disposition)
		}

		if e.Timeout < 0 {
			t.Errorf("%s has a negative timeout %v", e.Name, e.Timeout)
		}
	}

	if !sort.StringsAreSorted(names) {
		t.Errorf("smokeset.Entries is not sorted by name: %v", names)
	}

	if n := len(smokeset.Runnable()); n < 5 {
		t.Errorf("only %d runnable examples; the gate has been hollowed out "+
			"(entries can be moved to Skip, but not silently)", n)
	}
}

// TestDiscoverSeesNestedPrograms pins the discovery rule to "any
// directory with a main.go, at any depth, outside the skipped subtrees".
// The first rule looked one level down, grew an `embed/` special case,
// and missed examples/deploy/ax/entrypoint/main.go in between (#1417).
func TestDiscoverSeesNestedPrograms(t *testing.T) {
	root := t.TempDir()
	mk := func(rel string) {
		t.Helper()
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("package main\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Programs at three depths.
	mk("a/main.go")
	mk("embed/b/main.go")
	mk("deploy/c/d/main.go")
	// Not programs: no main.go, or inside a skipped subtree.
	mk("recipe-only/.agents/config.json")
	mk("recipe-only/README.md")
	mk("internal/tool/main.go")
	mk("a/testdata/fixture/main.go")
	mk("deploy/c/.hidden/main.go")
	mk("web/node_modules/pkg/main.go")
	mk("a/helper.go")

	got, err := smokeset.Discover(root)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	want := []string{"a", "deploy/c/d", "embed/b"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Discover = %v, want %v", got, want)
	}
}

// TestDiscoverAccountsForTheAXEntrypoint is the concrete instance: the
// program this rule was missing must be visible to the accounting test
// in the real tree, so that its Skip row is load-bearing.
func TestDiscoverAccountsForTheAXEntrypoint(t *testing.T) {
	found, err := smokeset.Discover(examplesDir)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	for _, n := range found {
		if n == "deploy/ax/entrypoint" {
			return
		}
	}
	t.Fatalf("Discover(%s) = %v; does not include deploy/ax/entrypoint", examplesDir, found)
}
