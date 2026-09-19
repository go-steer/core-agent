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

package attachadapter_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v2/internal/testutil"
)

// #388 phase 4 moved nine provider options off *agent.Agent and into
// this package, dropping "Attach" from each name:
// agent.WithAttachPromptBroker became attachadapter.WithPromptBroker,
// and so on. Not one WithAttach* symbol survives the rename.
//
// The comments were not walked with it. Three doc comments went on
// naming agent.WithAttachPromptBroker as the thing a reader should
// call — including the one on pkg/attach.PromptBroker itself, which is
// what a reader hits when they are working out how to turn the 501'ing
// /perms routes on. They survived eight releases here and were then
// inherited by a port into the sibling mast repo, where the same text
// pointed at a function that does not exist there either (#1103,
// mast#364).
//
// A doc comment naming a symbol is a claim the compiler does not
// check, and #1103 declined a general symbol-resolving doc lint as
// more project than five lines are worth. This is not that lint. It
// checks one mechanical invariant specific to this rename family:
//
//	the string "WithAttach" may appear in a Go comment only as a
//	record of the old name, never as an instruction to call one.
//
// Two spellings are a record. "Formerly agent.WithAttachX." is the
// form this package's own options use nine times, and the bare glob
// "WithAttach*" is how the package doc refers to the family as a
// whole. Anything else is a live instruction, and every live
// instruction naming one of these is unfollowable — the symbol is
// gone.
//
// Deliberately over the whole tree rather than this package: all three
// stale comments were somewhere else, because the package that gets
// renamed is not the package that talks about it.
const (
	renamedMarker = "WithAttach"
	renameRecord  = "ormerly agent." // "Formerly" and "formerly" both
	renameGlob    = "WithAttach*"
)

// Dot-directories and the two non-source names are pruned by the shared
// testutil.PruneWalkDir: `.claude` is where this repo's git worktrees
// live, so walking it would parse a second copy of the whole tree and
// report every finding twice.
//
// CHANGELOG.md:804 is deliberately not in scope. It records what
// shipped in #87 under the name it shipped with, which is what a
// changelog is for. This walks Go comments only, so it never sees it.
func TestNoCommentNamesARenamedAttachOption(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	// This file is the one place that has to spell the banned forms out
	// in prose in order to define them, so it is exempt from itself —
	// the same exemption a lint rule's own fixtures get. Matched by
	// compiled-in path, not by filename, so a rename cannot widen it.
	self := selfPath(t)

	var records, violations int
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if testutil.PruneWalkDir(root, path, d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") || path == self {
			return nil
		}
		fset := token.NewFileSet()
		f, parseErr := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if parseErr != nil {
			// Not a fatal repo condition: testdata holds Go files that
			// are intentionally unparseable. A file we cannot parse has
			// no comments we can judge.
			return nil //nolint:nilerr // see above
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		for _, cg := range f.Comments {
			// Collapse the group to one line first: the convention
			// wraps, and "Formerly\nagent.WithAttachSkillsProvider."
			// is the same record as the unwrapped form.
			text := strings.Join(strings.Fields(cg.Text()), " ")
			for i := 0; ; {
				j := strings.Index(text[i:], renamedMarker)
				if j < 0 {
					break
				}
				at := i + j
				i = at + len(renamedMarker)
				switch {
				case strings.HasPrefix(text[at:], renameGlob):
					continue
				case strings.HasSuffix(text[:at], renameRecord):
					records++
					continue
				}
				violations++
				t.Errorf("%s:%d: comment names a %s* option as something to call, but the whole family was renamed into attachadapter by #388 and no such symbol exists. Say attachadapter.With<Name>, or record the old name as %q if that is what you mean:\n\t%s",
					rel, fset.Position(cg.Pos()).Line, renamedMarker, "Formerly agent."+renamedMarker+"<Name>.", text)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	// Without this the test passes just as well against a tree where
	// the marker never appears at all — including one where the walk
	// silently matched nothing.
	if records == 0 && violations == 0 {
		t.Fatalf("found no %q in any Go comment under %s: the guard is passing vacuously", renamedMarker, root)
	}
}

// selfPath is this test file's own location, for the self-exemption.
func selfPath(t *testing.T) string {
	t.Helper()
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed: cannot locate this file to exempt it")
	}
	return self
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
