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

	"github.com/go-steer/core-agent/config"
	"github.com/go-steer/core-agent/permissions"
)

// gateFor builds a permissive (yolo) gate scoped to root for use in
// tool unit tests.
func gateFor(t *testing.T, root string) *permissions.Gate {
	t.Helper()
	scope, err := permissions.NewPathScope(root, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	return permissions.New(permissions.Options{
		Mode:  permissions.ModeYolo,
		Scope: scope,
	})
}

func TestReadFile_RoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "hello.txt")
	if err := os.WriteFile(path, []byte("hi core-agent"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := config.DefaultConfig()
	gate := gateFor(t, dir)
	fn := readFileFunc(gate, cfg)
	res, err := fn(tool.Context(nil), readFileArgs{Path: path})
	if err != nil {
		t.Fatalf("read_file: %v", err)
	}
	if res.Content != "hi core-agent" {
		t.Errorf("content = %q, want %q", res.Content, "hi core-agent")
	}
}

func TestReadFile_OutOfScope_Denied(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	other := t.TempDir()
	outside := filepath.Join(other, "x.txt")
	if err := os.WriteFile(outside, []byte("private"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := config.DefaultConfig()
	scope, _ := permissions.NewPathScope(dir, "", nil)
	gate := permissions.New(permissions.Options{
		Mode:  permissions.ModeAllow, // no prompter, no allowlist match → deny
		Scope: scope,
	})
	fn := readFileFunc(gate, cfg)
	_, err := fn(tool.Context(nil), readFileArgs{Path: outside})
	if err == nil {
		t.Fatalf("expected denial for out-of-scope read")
	}
}

func TestWriteFile_AtomicAndContent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "out.txt")
	gate := gateFor(t, dir)
	fn := writeFileFunc(gate)
	res, err := fn(tool.Context(nil), writeFileArgs{Path: path, Content: "abc\n"})
	if err != nil {
		t.Fatalf("write_file: %v", err)
	}
	if res.Bytes != 4 {
		t.Errorf("bytes = %d, want 4", res.Bytes)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "abc\n" {
		t.Errorf("on-disk = %q, want %q", string(got), "abc\n")
	}
}

func TestEditFile_UniqueReplacement(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "edit.txt")
	if err := os.WriteFile(path, []byte("alpha BETA gamma\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gate := gateFor(t, dir)
	fn := editFileFunc(gate)
	res, err := fn(tool.Context(nil), editFileArgs{Path: path, OldString: "BETA", NewString: "delta"})
	if err != nil {
		t.Fatalf("edit_file: %v", err)
	}
	if res.Replacements != 1 {
		t.Errorf("replacements = %d, want 1", res.Replacements)
	}
	body, _ := os.ReadFile(path)
	if string(body) != "alpha delta gamma\n" {
		t.Errorf("after edit = %q", string(body))
	}
}

func TestEditFile_AmbiguousMatch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "edit.txt")
	if err := os.WriteFile(path, []byte("foo foo foo"), 0o644); err != nil {
		t.Fatal(err)
	}
	gate := gateFor(t, dir)
	fn := editFileFunc(gate)
	_, err := fn(tool.Context(nil), editFileArgs{Path: path, OldString: "foo", NewString: "bar"})
	if err == nil || !strings.Contains(err.Error(), "appears 3 times") {
		t.Errorf("expected ambiguity error, got %v", err)
	}
}

func TestListDir_SortedEntries(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, name := range []string{"b.txt", "a.txt", "c.txt"} {
		os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644)
	}
	cfg := config.DefaultConfig()
	gate := gateFor(t, dir)
	fn := listDirFunc(gate, cfg)
	res, err := fn(tool.Context(nil), listDirArgs{Path: dir})
	if err != nil {
		t.Fatalf("list_dir: %v", err)
	}
	if len(res.Entries) != 3 || res.Entries[0].Name != "a.txt" {
		t.Errorf("entries = %+v", res.Entries)
	}
}
