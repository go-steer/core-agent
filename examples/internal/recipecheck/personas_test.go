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

package recipecheck_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/instruction"
)

// TestRecipePersonaIncludesResolve is the offline version of starting
// every recipe. A `@include builtin:` line that names a persona which
// does not exist is a FATAL load error, by design (#656) — so a typo in
// a shipped recipe is not a slightly-degraded agent, it is a daemon that
// refuses to start, discovered by whoever deployed it.
//
// The check is cheap because builtins resolve out of the binary: no
// cluster, no config, no ConfigMap projection. That is the same property
// that makes them usable from a read-only mount in the first place.
func TestRecipePersonaIncludesResolve(t *testing.T) {
	t.Parallel()

	var checked int
	err := filepath.WalkDir(examplesDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || d.Name() != "AGENTS.md" {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !strings.Contains(string(body), "@include builtin:") {
			return nil
		}
		checked++

		// Expand rather than Load: it applies the same @include handling
		// without needing the file to sit at a scope root, so this works
		// for a recipe whose AGENTS.md lives under deploy/base/config/
		// as well as one under .agents/.
		dir := filepath.Dir(path)
		if _, _, err := instruction.Expand(string(body), dir, dir); err != nil {
			t.Errorf("%s: %v", path, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// A floor on the walk, not on adoption: if the tree moves or the
	// directive is renamed, this test would pass by checking nothing.
	if checked == 0 {
		t.Fatal("no recipe references a builtin persona — either adoption was reverted or this walk no longer finds AGENTS.md files")
	}
	t.Logf("resolved builtin persona includes in %d recipe instruction files", checked)
}
