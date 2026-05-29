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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/adk/tool"

	"github.com/go-steer/core-agent/pkg/config"
	"github.com/go-steer/core-agent/pkg/permissions"
)

// permissiveGate returns a yolo-mode gate that allows any path. Used
// by the happy-path tests where we just want the tool to walk freely.
func permissiveGate(t *testing.T, root string) *permissions.Gate {
	t.Helper()
	scope, err := permissions.NewPathScope(root, "", nil)
	if err != nil {
		t.Fatalf("NewPathScope: %v", err)
	}
	return permissions.New(permissions.Options{
		Mode:  permissions.ModeYolo,
		Scope: scope,
	})
}

// scopedGate returns an allow-mode gate restricted to root. Out-of-
// scope reads return an error rather than going through a prompt
// (no Prompter is wired). Used to test gate-denial paths.
func scopedGate(t *testing.T, root string) *permissions.Gate {
	t.Helper()
	scope, err := permissions.NewPathScope(root, "", nil)
	if err != nil {
		t.Fatalf("NewPathScope: %v", err)
	}
	return permissions.New(permissions.Options{
		Mode:  permissions.ModeAllow,
		Scope: scope,
	})
}

// writeFile writes data into dir/name, creating parent directories as
// needed. Returns the absolute path.
func writeFile(t *testing.T, dir, name, data string) string {
	t.Helper()
	full := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(full, []byte(data), 0o644); err != nil {
		t.Fatalf("WriteFile %s: %v", full, err)
	}
	return full
}

func TestGlob_RequiresPattern(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fn := globFunc(permissiveGate(t, dir), config.DefaultConfig())
	_, err := fn(tool.Context(nil), globArgs{Path: dir})
	if err == nil || !strings.Contains(err.Error(), "pattern is required") {
		t.Errorf("err = %v, want pattern-required", err)
	}
}

func TestGlob_MatchesByBasename(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "main.go", "")
	writeFile(t, dir, "main_test.go", "")
	writeFile(t, dir, "README.md", "")
	writeFile(t, dir, "sub/util.go", "")

	fn := globFunc(permissiveGate(t, dir), config.DefaultConfig())
	res, err := fn(tool.Context(nil), globArgs{Path: dir, Pattern: "*.go"})
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	got := basenamesOf(res.Paths)
	want := []string{"main.go", "main_test.go", "util.go"}
	if !equalStringSets(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestGlob_DefaultsToCurrentDir(t *testing.T) {
	// No t.Parallel — t.Chdir mutates process-global state and
	// the testing package refuses to allow it on a parallel test.
	dir := t.TempDir()
	writeFile(t, dir, "x.txt", "")
	t.Chdir(dir)
	fn := globFunc(permissiveGate(t, dir), config.DefaultConfig())
	res, err := fn(tool.Context(nil), globArgs{Pattern: "*.txt"})
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(res.Paths) != 1 {
		t.Errorf("got %d paths, want 1: %v", len(res.Paths), res.Paths)
	}
}

func TestGlob_SkipsHiddenDirs(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "kept.go", "")
	writeFile(t, dir, ".git/HEAD.go", "")
	writeFile(t, dir, "node_modules/lib.go", "")
	writeFile(t, dir, "vendor/dep.go", "")

	fn := globFunc(permissiveGate(t, dir), config.DefaultConfig())
	res, err := fn(tool.Context(nil), globArgs{Path: dir, Pattern: "*.go"})
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	got := basenamesOf(res.Paths)
	if !equalStringSets(got, []string{"kept.go"}) {
		t.Errorf("got %v, want [kept.go] (hidden/vendored dirs should be skipped)", got)
	}
}

func TestGlob_ReturnsSortedPaths(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "c.txt", "")
	writeFile(t, dir, "a.txt", "")
	writeFile(t, dir, "b.txt", "")
	fn := globFunc(permissiveGate(t, dir), config.DefaultConfig())
	res, err := fn(tool.Context(nil), globArgs{Path: dir, Pattern: "*.txt"})
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	for i := 1; i < len(res.Paths); i++ {
		if res.Paths[i-1] > res.Paths[i] {
			t.Errorf("paths not sorted at %d: %v", i, res.Paths)
		}
	}
}

func TestGlob_GateDeniesPathOutsideScope(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	other := t.TempDir()
	writeFile(t, other, "secret.go", "")
	// Scope is restricted to dir; querying other should fail under
	// allow-mode (no prompt path open).
	fn := globFunc(scopedGate(t, dir), config.DefaultConfig())
	_, err := fn(tool.Context(nil), globArgs{Path: other, Pattern: "*.go"})
	if err == nil {
		t.Errorf("expected gate to deny path outside scope, got nil error")
	}
}

func TestGlob_TruncatedAtLineCap(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for i := 0; i < 10; i++ {
		writeFile(t, dir, "f"+string(rune('0'+i))+".txt", "")
	}
	cfg := config.DefaultConfig()
	cfg.ToolOutput.PerTool = map[string]config.ToolOutputPerToolCaps{
		"glob": {MaxLines: 3, MaxBytes: 0},
	}
	fn := globFunc(permissiveGate(t, dir), cfg)
	res, err := fn(tool.Context(nil), globArgs{Path: dir, Pattern: "*.txt"})
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if !res.Truncated {
		t.Errorf("Truncated should be true; got Paths=%v", res.Paths)
	}
	if len(res.Paths) != 3 {
		t.Errorf("Paths length = %d, want 3", len(res.Paths))
	}
}

func TestGlob_InvalidPatternRejected(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fn := globFunc(permissiveGate(t, dir), config.DefaultConfig())
	_, err := fn(tool.Context(nil), globArgs{Path: dir, Pattern: "[unclosed"})
	if err == nil || !strings.Contains(err.Error(), "invalid pattern") {
		t.Errorf("err = %v, want invalid-pattern", err)
	}
}

// basenamesOf strips the directory component off each path so test
// assertions stay readable across temp-dir locations.
func basenamesOf(paths []string) []string {
	out := make([]string, len(paths))
	for i, p := range paths {
		out[i] = filepath.Base(p)
	}
	return out
}

// equalStringSets is order-insensitive set equality for the test
// assertions; both sides are converted to maps.
func equalStringSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]bool{}
	for _, s := range a {
		seen[s] = true
	}
	for _, s := range b {
		if !seen[s] {
			return false
		}
	}
	return true
}
