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
	checked := 0

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
		checked += checkCommentGroup(t, fset, rel, file.Doc)
		ast.Inspect(file, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.GenDecl:
				checked += checkCommentGroup(t, fset, rel, x.Doc)
			case *ast.FuncDecl:
				checked += checkCommentGroup(t, fset, rel, x.Doc)
			case *ast.TypeSpec:
				checked += checkCommentGroup(t, fset, rel, x.Doc)
				checked += checkCommentGroup(t, fset, rel, x.Comment)
			case *ast.ValueSpec:
				checked += checkCommentGroup(t, fset, rel, x.Doc)
				checked += checkCommentGroup(t, fset, rel, x.Comment)
			case *ast.Field:
				checked += checkCommentGroup(t, fset, rel, x.Doc)
				checked += checkCommentGroup(t, fset, rel, x.Comment)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walking repo: %v", err)
	}
	if checked == 0 {
		t.Fatal("TestDeprecationMarkersNameTheirRemoval checked 0 Deprecated: paragraphs under pkg/; walk is broken or all deprecations were removed without updating this test")
	}
}

func checkCommentGroup(t *testing.T, fset *token.FileSet, rel string, cg *ast.CommentGroup) int {
	t.Helper()
	if cg == nil {
		return 0
	}
	found, bad := missingRemovalTarget(cg.Text())
	for _, para := range bad {
		pos := fset.Position(cg.Pos())
		t.Errorf("%s:%d: Deprecated marker missing trailing 'Removed in v<N>.<M>.': %q (see docs/compatibility-policy.md)", rel, pos.Line, para)
	}
	return found
}

// missingRemovalTarget returns the count of `Deprecated:` paragraphs in text
// and every `Deprecated:` paragraph that does not end with `Removed in v<N>.<M>.`.
func missingRemovalTarget(text string) (int, []string) {
	var found int
	var bad []string
	for _, para := range strings.Split(text, "\n\n") {
		trimmed := strings.TrimSpace(para)
		for _, line := range strings.Split(trimmed, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "Deprecated:") {
				found++
				flat := strings.Join(strings.Fields(trimmed), " ")
				if !removalPattern.MatchString(flat) {
					bad = append(bad, flat)
				}
				break
			}
		}
	}
	return found, bad
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
			found, got := missingRemovalTarget(tc.doc)
			if found != 1 {
				t.Fatalf("missingRemovalTarget(%q) found = %d, want 1", tc.doc, found)
			}
			if len(got) != tc.want {
				t.Fatalf("missingRemovalTarget(%q) = %v (len %d), want len %d", tc.doc, got, len(got), tc.want)
			}
		})
	}
}
