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

func TestReadManyFiles_RequiresPathsOrPattern(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fn := readManyFilesFunc(permissiveGate(t, dir), config.DefaultConfig())
	_, err := fn(tool.Context(nil), readManyFilesArgs{})
	if err == nil || !strings.Contains(err.Error(), "provide paths or pattern") {
		t.Errorf("err = %v, want paths-or-pattern", err)
	}
}

func TestReadManyFiles_ExplicitPaths(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a := writeFile(t, dir, "a.txt", "alpha")
	b := writeFile(t, dir, "b.txt", "bravo")

	fn := readManyFilesFunc(permissiveGate(t, dir), config.DefaultConfig())
	res, err := fn(tool.Context(nil), readManyFilesArgs{Paths: []string{a, b}})
	if err != nil {
		t.Fatalf("read_many_files: %v", err)
	}
	if len(res.Files) != 2 {
		t.Fatalf("expected 2 files, got %d (%+v)", len(res.Files), res.Files)
	}
	got := map[string]string{}
	for _, f := range res.Files {
		got[f.Path] = f.Content
	}
	if got[a] != "alpha" || got[b] != "bravo" {
		t.Errorf("content mismatch: %+v", got)
	}
}

func TestReadManyFiles_PreservesExplicitOrder(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Reverse-alphabetical to prove order comes from the model, not
	// the filesystem.
	z := writeFile(t, dir, "z.txt", "z")
	a := writeFile(t, dir, "a.txt", "a")
	m := writeFile(t, dir, "m.txt", "m")

	fn := readManyFilesFunc(permissiveGate(t, dir), config.DefaultConfig())
	res, err := fn(tool.Context(nil), readManyFilesArgs{Paths: []string{z, a, m}})
	if err != nil {
		t.Fatalf("read_many_files: %v", err)
	}
	wantOrder := []string{z, a, m}
	if len(res.Files) != len(wantOrder) {
		t.Fatalf("expected %d files, got %d", len(wantOrder), len(res.Files))
	}
	for i, want := range wantOrder {
		if res.Files[i].Path != want {
			t.Errorf("Files[%d].Path = %q, want %q", i, res.Files[i].Path, want)
		}
	}
}

func TestReadManyFiles_PatternWalk(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "a.go", "package a")
	writeFile(t, dir, "b.go", "package b")
	writeFile(t, dir, "README.md", "ignore me")

	fn := readManyFilesFunc(permissiveGate(t, dir), config.DefaultConfig())
	res, err := fn(tool.Context(nil), readManyFilesArgs{Pattern: "*.go", Path: dir})
	if err != nil {
		t.Fatalf("read_many_files: %v", err)
	}
	if len(res.Files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(res.Files))
	}
	for _, f := range res.Files {
		if !strings.HasSuffix(f.Path, ".go") {
			t.Errorf("non-.go file in results: %s", f.Path)
		}
		if f.Skipped != "" {
			t.Errorf("file %s should not be skipped: %s", f.Path, f.Skipped)
		}
	}
}

func TestReadManyFiles_PathsAndPatternUnioned(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	goA := writeFile(t, dir, "a.go", "package a")
	goB := writeFile(t, dir, "b.go", "package b")
	md := writeFile(t, dir, "README.md", "readme")

	fn := readManyFilesFunc(permissiveGate(t, dir), config.DefaultConfig())
	// Explicit README + pattern *.go should produce 3 files; goA
	// should not appear twice even if the model also lists it
	// explicitly.
	res, err := fn(tool.Context(nil), readManyFilesArgs{
		Paths:   []string{md, goA},
		Pattern: "*.go",
		Path:    dir,
	})
	if err != nil {
		t.Fatalf("read_many_files: %v", err)
	}
	paths := map[string]int{}
	for _, f := range res.Files {
		paths[f.Path]++
	}
	if paths[md] != 1 || paths[goA] != 1 || paths[goB] != 1 {
		t.Errorf("expected each path once; got %+v", paths)
	}
	if len(res.Files) != 3 {
		t.Errorf("expected 3 files total, got %d", len(res.Files))
	}
}

func TestReadManyFiles_SkipsHiddenAndVendored(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "real.go", "real")
	writeFile(t, dir, ".git/HEAD", "ref: refs/heads/main")
	writeFile(t, dir, "vendor/foo/bar.go", "vendored")
	writeFile(t, dir, "node_modules/baz/qux.go", "node")

	fn := readManyFilesFunc(permissiveGate(t, dir), config.DefaultConfig())
	res, err := fn(tool.Context(nil), readManyFilesArgs{Pattern: "*.go", Path: dir})
	if err != nil {
		t.Fatalf("read_many_files: %v", err)
	}
	if len(res.Files) != 1 {
		t.Fatalf("expected only real.go, got %d files (%+v)", len(res.Files), res.Files)
	}
	if !strings.HasSuffix(res.Files[0].Path, "real.go") {
		t.Errorf("unexpected file: %s", res.Files[0].Path)
	}
}

func TestReadManyFiles_GateDeniedSurfacesAsSkipped(t *testing.T) {
	t.Parallel()
	scoped := t.TempDir() // gate root
	outside := t.TempDir()
	allowed := writeFile(t, scoped, "in.txt", "inside")
	denied := writeFile(t, outside, "out.txt", "outside")

	fn := readManyFilesFunc(scopedGate(t, scoped), config.DefaultConfig())
	res, err := fn(tool.Context(nil), readManyFilesArgs{Paths: []string{allowed, denied}})
	if err != nil {
		t.Fatalf("read_many_files: %v", err)
	}
	if len(res.Files) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(res.Files))
	}
	byPath := map[string]readManyFile{}
	for _, f := range res.Files {
		byPath[f.Path] = f
	}
	if byPath[allowed].Content != "inside" || byPath[allowed].Skipped != "" {
		t.Errorf("allowed file should be read; got %+v", byPath[allowed])
	}
	if byPath[denied].Skipped == "" {
		t.Errorf("denied file should have Skipped reason; got %+v", byPath[denied])
	}
	if byPath[denied].Content != "" {
		t.Errorf("denied file must not leak content; got %q", byPath[denied].Content)
	}
}

func TestReadManyFiles_MissingFileSkipped(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	exists := writeFile(t, dir, "real.txt", "hi")
	ghost := filepath.Join(dir, "does-not-exist.txt")

	fn := readManyFilesFunc(permissiveGate(t, dir), config.DefaultConfig())
	res, err := fn(tool.Context(nil), readManyFilesArgs{Paths: []string{exists, ghost}})
	if err != nil {
		t.Fatalf("read_many_files: %v", err)
	}
	if len(res.Files) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(res.Files))
	}
	for _, f := range res.Files {
		switch {
		case f.Path == exists && f.Content != "hi":
			t.Errorf("real file content lost: %+v", f)
		case f.Path == ghost && !strings.Contains(f.Skipped, "stat error"):
			t.Errorf("ghost file should have stat-error skip; got %+v", f)
		}
	}
}

func TestReadManyFiles_DirectoryEntrySkipped(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "real.txt", "data")
	subdir := filepath.Join(dir, "sub")

	fn := readManyFilesFunc(permissiveGate(t, dir), config.DefaultConfig())
	res, err := fn(tool.Context(nil), readManyFilesArgs{Paths: []string{subdir}})
	if err != nil {
		t.Fatalf("read_many_files: %v", err)
	}
	if len(res.Files) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(res.Files))
	}
	if res.Files[0].Skipped == "" || !strings.Contains(res.Files[0].Skipped, "directory") {
		t.Errorf("directory entry should be skipped; got %+v", res.Files[0])
	}
}

func TestReadManyFiles_PerFileCapTruncates(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// One file just over the per-file cap; one well under.
	big := strings.Repeat("x", readManyFilesPerFileBytes+1024)
	huge := writeFile(t, dir, "huge.txt", big)
	tiny := writeFile(t, dir, "tiny.txt", "hi")

	fn := readManyFilesFunc(permissiveGate(t, dir), config.DefaultConfig())
	res, err := fn(tool.Context(nil), readManyFilesArgs{Paths: []string{huge, tiny}})
	if err != nil {
		t.Fatalf("read_many_files: %v", err)
	}
	byPath := map[string]readManyFile{}
	for _, f := range res.Files {
		byPath[f.Path] = f
	}
	if !byPath[huge].Truncated {
		t.Errorf("huge file should be marked truncated; got %+v", byPath[huge])
	}
	if len(byPath[huge].Content) > readManyFilesPerFileBytes+200 {
		// +200 for the truncation marker; Truncate() appends a short tag.
		t.Errorf("huge content not capped: got %d bytes", len(byPath[huge].Content))
	}
	if byPath[tiny].Truncated {
		t.Errorf("tiny file should not be truncated; got %+v", byPath[tiny])
	}
}

func TestReadManyFiles_WholeResponseCapDropsTrailing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Make 5 small files; force a tiny whole-response cap so trailing
	// entries get dropped.
	paths := []string{}
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		paths = append(paths, writeFile(t, dir, name+".txt", strings.Repeat(name, 200)))
	}
	cfg := config.DefaultConfig()
	cfg.ToolOutput.PerTool["read_many_files"] = config.ToolOutputPerToolCaps{
		MaxBytes: 500, // very small — should fit at most ~1-2 entries
		MaxLines: 0,
	}
	fn := readManyFilesFunc(permissiveGate(t, dir), cfg)
	res, err := fn(tool.Context(nil), readManyFilesArgs{Paths: paths})
	if err != nil {
		t.Fatalf("read_many_files: %v", err)
	}
	if len(res.Files) == 5 {
		t.Errorf("expected trailing files to be dropped, got all 5")
	}
	if !res.Truncated {
		t.Errorf("Truncated flag should be set when response is capped")
	}
}

func TestReadManyFiles_InvalidPatternError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fn := readManyFilesFunc(permissiveGate(t, dir), config.DefaultConfig())
	_, err := fn(tool.Context(nil), readManyFilesArgs{Pattern: "[bad", Path: dir})
	if err == nil || !strings.Contains(err.Error(), "invalid pattern") {
		t.Errorf("err = %v, want invalid-pattern", err)
	}
}
