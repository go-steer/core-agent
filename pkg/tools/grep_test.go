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

package tools

import (
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/adk/tool"

	"github.com/go-steer/core-agent/pkg/config"
)

func TestGrep_RequiresPattern(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fn := grepFunc(permissiveGate(t, dir), config.DefaultConfig())
	_, err := fn(tool.Context(nil), grepArgs{Path: dir})
	if err == nil || !strings.Contains(err.Error(), "pattern is required") {
		t.Errorf("err = %v, want pattern-required", err)
	}
}

func TestGrep_InvalidRegexRejected(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fn := grepFunc(permissiveGate(t, dir), config.DefaultConfig())
	_, err := fn(tool.Context(nil), grepArgs{Path: dir, Pattern: "(unclosed"})
	if err == nil || !strings.Contains(err.Error(), "invalid pattern") {
		t.Errorf("err = %v, want invalid-pattern", err)
	}
}

func TestGrep_RecursiveOverDirectory(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "a.go", "// TODO: fix me\npackage a\n")
	writeFile(t, dir, "sub/b.go", "package b\n// TODO: also fix\n")
	writeFile(t, dir, "c.txt", "no todo here\n")
	fn := grepFunc(permissiveGate(t, dir), config.DefaultConfig())
	res, err := fn(tool.Context(nil), grepArgs{Path: dir, Pattern: "TODO"})
	if err != nil {
		t.Fatalf("grep: %v", err)
	}
	if len(res.Matches) != 2 {
		t.Errorf("got %d matches, want 2: %+v", len(res.Matches), res.Matches)
	}
	for _, m := range res.Matches {
		if !strings.Contains(m.Text, "TODO") {
			t.Errorf("match text doesn't contain TODO: %q", m.Text)
		}
		if m.Line == 0 {
			t.Errorf("line number should be 1-based, got 0 for %q", m.Path)
		}
	}
}

func TestGrep_SingleFileMode(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeFile(t, dir, "a.go", "line one\nline two\nline three\n")
	fn := grepFunc(permissiveGate(t, dir), config.DefaultConfig())
	res, err := fn(tool.Context(nil), grepArgs{Path: path, Pattern: "two"})
	if err != nil {
		t.Fatalf("grep: %v", err)
	}
	if len(res.Matches) != 1 || res.Matches[0].Line != 2 || !strings.Contains(res.Matches[0].Text, "two") {
		t.Errorf("single-file match wrong: %+v", res.Matches)
	}
}

func TestGrep_SkipsHiddenDirs(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "kept.go", "// TODO\n")
	writeFile(t, dir, ".git/HEAD.go", "// TODO\n")
	writeFile(t, dir, "node_modules/lib.go", "// TODO\n")
	fn := grepFunc(permissiveGate(t, dir), config.DefaultConfig())
	res, err := fn(tool.Context(nil), grepArgs{Path: dir, Pattern: "TODO"})
	if err != nil {
		t.Fatalf("grep: %v", err)
	}
	if len(res.Matches) != 1 || filepath.Base(res.Matches[0].Path) != "kept.go" {
		t.Errorf("hidden/vendored matches leaked: %+v", res.Matches)
	}
}

func TestGrep_TruncatedAtLineCap(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// 50 lines, all matching.
	var b strings.Builder
	for i := 0; i < 50; i++ {
		b.WriteString("match\n")
	}
	path := writeFile(t, dir, "many.txt", b.String())
	cfg := config.DefaultConfig()
	cfg.ToolOutput.PerTool = map[string]config.ToolOutputPerToolCaps{
		"grep": {MaxLines: 5, MaxBytes: 0},
	}
	fn := grepFunc(permissiveGate(t, dir), cfg)
	res, err := fn(tool.Context(nil), grepArgs{Path: path, Pattern: "match"})
	if err != nil {
		t.Fatalf("grep: %v", err)
	}
	if !res.Truncated {
		t.Errorf("Truncated should be true; got %d matches", len(res.Matches))
	}
	if len(res.Matches) != 5 {
		t.Errorf("Matches length = %d, want 5", len(res.Matches))
	}
}

func TestGrep_GateDeniesPathOutsideScope(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	other := t.TempDir()
	writeFile(t, other, "secret.go", "TODO\n")
	fn := grepFunc(scopedGate(t, dir), config.DefaultConfig())
	_, err := fn(tool.Context(nil), grepArgs{Path: other, Pattern: "TODO"})
	if err == nil {
		t.Errorf("expected gate to deny path outside scope, got nil")
	}
}

func TestGrep_RegexGroupsAndAnchors(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", "FOOBAR\nfoobar\nbaz\n")
	fn := grepFunc(permissiveGate(t, dir), config.DefaultConfig())
	// Case-sensitive RE2: only the first line matches.
	res, err := fn(tool.Context(nil), grepArgs{Path: dir, Pattern: "^FOO"})
	if err != nil {
		t.Fatalf("grep: %v", err)
	}
	if len(res.Matches) != 1 || res.Matches[0].Text != "FOOBAR" {
		t.Errorf("anchor regex wrong: %+v", res.Matches)
	}
}
