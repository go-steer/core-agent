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

package testutil

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// notSource are the two non-dot directory names a source walk must not
// descend into. Neither is this repo's code: vendor/ is somebody else's,
// and docs/site/node_modules is large enough that walking it is a cost
// on its own.
var notSource = map[string]bool{"node_modules": true, "vendor": true}

// PruneWalkDir reports whether a directory met during a walk of this
// repo's source should be skipped entirely. path is the directory's
// full path, name its base; root is the walk root and is never pruned,
// however it happens to be named.
//
// Every dot-directory is pruned, which is a rule about working state
// rather than a list of known offenders. `.git` is the obvious one;
// `.claude` is the one that actually broke a suite, because this repo's
// git worktrees live there and each is a **full second checkout**, so a
// walk that descends into it reports another branch's source as though
// it were this tree's. The 2026-09-04 analysis saw that as two test
// failures on a machine with 26 worktrees present, against a tree that
// passes `go test ./...` from a clean shallow clone (#964).
//
// Pruning the class rather than the names is the point: `.claude` was
// added by name after it broke something, which left the next tool to
// put a checkout under a dot-directory to break it again. A suite that
// fails on the developer's untracked working state teaches people to
// ignore the suite, and that is a worse outcome than any single false
// negative it might now miss — a walk of this repo's own source has no
// business in a dot-directory in the first place, since nothing the
// module builds or ships lives in one.
//
// Callers keep their own additional exclusions (an unswept top-level
// tree, a self-exemption) where those are about what the walk *means*
// rather than about what counts as this repo's source.
func PruneWalkDir(root, path, name string) bool {
	if path == root {
		return false
	}
	return strings.HasPrefix(name, ".") || notSource[name]
}

// RepoRoot walks up from startFile to the directory holding go.mod.
// Callers pass their own compiled-in path from runtime.Caller, so the
// answer is derived from the source tree rather than from the working
// directory, which `go test` sets to the package directory.
func RepoRoot(startFile string) (string, error) {
	dir := filepath.Dir(startFile)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod above %s", filepath.Dir(startFile))
		}
		dir = parent
	}
}
