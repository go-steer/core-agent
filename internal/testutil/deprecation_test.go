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
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// removalPattern matches the required trailing sentence on every
// `Deprecated:` paragraph in exported doc comments (for example,
// "Deprecated: use Foo instead. Removed in v4.0.").
var removalPattern = regexp.MustCompile(`Removed in v[0-9]+\.[0-9]+(\.[0-9]+)?\.\s*$`)

// TestDeprecationMarkersNameTheirRemoval walks every non-test Go source
// file under pkg/ and requires that every `Deprecated:` doc-comment
// paragraph ends with `Removed in v<N>.<M>.`.
func TestDeprecationMarkersNameTheirRemoval(t *testing.T) {
	t.Parallel()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root, err := RepoRoot(thisFile)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()

	err = filepath.WalkDir(filepath.Join(root, "pkg"), func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if PruneWalkDir(root, path, d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, parseErr := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if parseErr != nil {
			return parseErr
		}
		rel, _ := filepath.Rel(root, path)
		checkCommentGroup(t, fset, rel, file.Doc)
		ast.Inspect(file, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.GenDecl:
				checkCommentGroup(t, fset, rel, x.Doc)
			case *ast.FuncDecl:
				checkCommentGroup(t, fset, rel, x.Doc)
			case *ast.TypeSpec:
				checkCommentGroup(t, fset, rel, x.Doc)
			case *ast.ValueSpec:
				checkCommentGroup(t, fset, rel, x.Doc)
			case *ast.Field:
				checkCommentGroup(t, fset, rel, x.Doc)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walking repo: %v", err)
	}
}

func checkCommentGroup(t *testing.T, fset *token.FileSet, rel string, cg *ast.CommentGroup) {
	t.Helper()
	if cg == nil {
		return
	}
	for _, para := range missingRemovalTarget(cg.Text()) {
		pos := fset.Position(cg.Pos())
		t.Errorf("%s:%d: Deprecated marker missing trailing 'Removed in v<N>.<M>.': %q (see docs/compatibility-policy.md)", rel, pos.Line, para)
	}
}

// missingRemovalTarget returns every `Deprecated:` paragraph in text that
// does not end with `Removed in v<N>.<M>.`.
func missingRemovalTarget(text string) []string {
	var bad []string
	for _, para := range strings.Split(text, "\n\n") {
		trimmed := strings.TrimSpace(para)
		for _, line := range strings.Split(trimmed, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "Deprecated:") {
				flat := strings.Join(strings.Fields(trimmed), " ")
				if !removalPattern.MatchString(flat) {
					bad = append(bad, flat)
				}
				break
			}
		}
	}
	return bad
}

func TestDeprecationCheckerReadsTheMarker(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		doc  string
		want int
	}{
		{
			name: "valid single-line",
			doc:  "Foo does x.\n\nDeprecated: use Bar instead. Removed in v4.0.\n",
			want: 0,
		},
		{
			name: "valid wrapped paragraph",
			doc:  "Foo does x.\n\nDeprecated: use Bar instead.\nRemoved in v4.0.\n",
			want: 0,
		},
		{
			name: "missing removal version",
			doc:  "Foo does x.\n\nDeprecated: use Bar instead.\n",
			want: 1,
		},
		{
			name: "vague removal phrase",
			doc:  "Deprecated: use Bar instead. Removed in the next major.\n",
			want: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := missingRemovalTarget(tc.doc)
			if len(got) != tc.want {
				t.Fatalf("missingRemovalTarget(%q) = %v (len %d), want len %d", tc.doc, got, len(got), tc.want)
			}
		})
	}
}
