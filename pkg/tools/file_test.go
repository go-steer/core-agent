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

	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
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
		t.Fatalf("expected ambiguity error, got %v", err)
	}
	// The refusal has to name the way out. A model whose only visible
	// options are "widen the snippet" or "give up" will not discover
	// replace_all, and a capability it cannot see is one it does not
	// have (#759/#762, pointed the other way).
	if !strings.Contains(err.Error(), "replace_all") {
		t.Errorf("ambiguity error does not mention replace_all: %v", err)
	}
	if body, _ := os.ReadFile(path); string(body) != "foo foo foo" {
		t.Errorf("a refused edit still touched the file: %q", string(body))
	}
}

func TestEditFile_ReplaceAllChangesEveryOccurrence(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "edit.txt")
	if err := os.WriteFile(path, []byte("foo bar foo baz foo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gate := gateFor(t, dir)
	fn := editFileFunc(gate)
	res, err := fn(tool.Context(nil), editFileArgs{
		Path: path, OldString: "foo", NewString: "qux", ReplaceAll: true,
	})
	if err != nil {
		t.Fatalf("edit_file: %v", err)
	}
	if res.Replacements != 3 {
		t.Errorf("replacements = %d, want 3", res.Replacements)
	}
	// The count the caller could not know before asking belongs where a
	// model reliably reads it, not only in a sibling field.
	if !strings.Contains(res.Status, "3 occurrences") {
		t.Errorf("status = %q, want it to name the occurrence count", res.Status)
	}
	body, _ := os.ReadFile(path)
	if string(body) != "qux bar qux baz qux\n" {
		t.Errorf("after edit = %q", string(body))
	}
}

// replace_all is an opt-out of the uniqueness CHECK, not a mode switch:
// a unique match must behave exactly as it does without the flag, so a
// model that sets it defensively on every edit gets no surprises.
func TestEditFile_ReplaceAllOnAUniqueMatchIsAnOrdinaryEdit(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "edit.txt")
	if err := os.WriteFile(path, []byte("alpha BETA gamma\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gate := gateFor(t, dir)
	fn := editFileFunc(gate)
	res, err := fn(tool.Context(nil), editFileArgs{
		Path: path, OldString: "BETA", NewString: "delta", ReplaceAll: true,
	})
	if err != nil {
		t.Fatalf("edit_file: %v", err)
	}
	if res.Replacements != 1 {
		t.Errorf("replacements = %d, want 1", res.Replacements)
	}
	if res.Status != "edited "+path {
		t.Errorf("status = %q, want the plain single-edit status", res.Status)
	}
	body, _ := os.ReadFile(path)
	if string(body) != "alpha delta gamma\n" {
		t.Errorf("after edit = %q", string(body))
	}
}

// A missing string is an error in both modes. replace_all relaxes "must
// occur exactly once" to "must occur at least once" — it does not turn
// a no-op into a success, which would let a rename report done while
// changing nothing.
func TestEditFile_ReplaceAllStillFailsOnNoMatch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "edit.txt")
	if err := os.WriteFile(path, []byte("alpha beta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gate := gateFor(t, dir)
	fn := editFileFunc(gate)
	_, err := fn(tool.Context(nil), editFileArgs{
		Path: path, OldString: "gamma", NewString: "delta", ReplaceAll: true,
	})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected a not-found error, got %v", err)
	}
	if body, _ := os.ReadFile(path); string(body) != "alpha beta\n" {
		t.Errorf("a failed edit still touched the file: %q", string(body))
	}
}

// Overlapping matches are consumed left to right by strings.Replace, so
// "aa" in "aaaa" is two replacements and not three. Pinned because it
// is the one place the count in the result could plausibly disagree
// with what the caller expects, and the result is what they will act on.
func TestEditFile_ReplaceAllCountsNonOverlappingMatches(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "edit.txt")
	if err := os.WriteFile(path, []byte("aaaa"), 0o644); err != nil {
		t.Fatal(err)
	}
	gate := gateFor(t, dir)
	fn := editFileFunc(gate)
	res, err := fn(tool.Context(nil), editFileArgs{
		Path: path, OldString: "aa", NewString: "b", ReplaceAll: true,
	})
	if err != nil {
		t.Fatalf("edit_file: %v", err)
	}
	if res.Replacements != 2 {
		t.Errorf("replacements = %d, want 2", res.Replacements)
	}
	if body, _ := os.ReadFile(path); string(body) != "bb" {
		t.Errorf("after edit = %q, want %q", string(body), "bb")
	}
}

// replace_all does not reach past the permission gate. Same guard as
// every other write, asserted here because a new argument on a gated
// tool is exactly where a bypass would hide. The gate is consulted
// before old_string is ever counted, so this holds no matter how many
// occurrences the target has.
//
// Uses 'allow' mode rather than the yolo gate the other tests share:
// yolo deliberately permits out-of-scope writes (see
// Gate.promptForPath), so it cannot tell "the check ran and passed"
// from "the check never ran".
func TestEditFile_ReplaceAllStillHonorsTheGate(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("foo foo"), 0o644); err != nil {
		t.Fatal(err)
	}
	scope, err := permissions.NewPathScope(dir, "", nil) // scoped to dir, not to outside
	if err != nil {
		t.Fatal(err)
	}
	gate := permissions.New(permissions.Options{Mode: permissions.ModeAllow, Scope: scope})
	fn := editFileFunc(gate)
	if _, err := fn(tool.Context(nil), editFileArgs{
		Path: outside, OldString: "foo", NewString: "bar", ReplaceAll: true,
	}); err == nil {
		t.Fatal("edit_file wrote outside the path scope with replace_all set")
	}
	if body, _ := os.ReadFile(outside); string(body) != "foo foo" {
		t.Errorf("out-of-scope file was modified: %q", string(body))
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

func TestDeleteFile_RemovesRegularFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "scratch.txt")
	if err := os.WriteFile(path, []byte("baseline"), 0o644); err != nil {
		t.Fatal(err)
	}
	fn := deleteFileFunc(gateFor(t, dir))
	res, err := fn(tool.Context(nil), deleteFileArgs{Path: path})
	if err != nil {
		t.Fatalf("delete_file: %v", err)
	}
	if !strings.HasPrefix(res.Status, "deleted ") {
		t.Errorf("status = %q, want 'deleted ...'", res.Status)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("file should be gone, got err=%v", err)
	}
}

func TestDeleteFile_MissingIsIdempotent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fn := deleteFileFunc(gateFor(t, dir))
	res, err := fn(tool.Context(nil), deleteFileArgs{Path: filepath.Join(dir, "never-existed")})
	if err != nil {
		t.Fatalf("delete_file: %v", err)
	}
	if !strings.Contains(res.Status, "no-op") {
		t.Errorf("status = %q, want a no-op message", res.Status)
	}
}

func TestDeleteFile_RefusesDirectory(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sub := filepath.Join(dir, "subdir")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	fn := deleteFileFunc(gateFor(t, dir))
	_, err := fn(tool.Context(nil), deleteFileArgs{Path: sub})
	if err == nil || !strings.Contains(err.Error(), "directory") {
		t.Errorf("expected directory-refusal error, got %v", err)
	}
	if _, statErr := os.Stat(sub); statErr != nil {
		t.Errorf("directory should still exist after refusal: %v", statErr)
	}
}

func TestDeleteFile_OutOfScope_Denied(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	other := t.TempDir()
	outside := filepath.Join(other, "x.txt")
	if err := os.WriteFile(outside, []byte("private"), 0o644); err != nil {
		t.Fatal(err)
	}
	scope, _ := permissions.NewPathScope(dir, "", nil)
	gate := permissions.New(permissions.Options{
		Mode:  permissions.ModeAllow, // no allowlist → deny on write
		Scope: scope,
	})
	fn := deleteFileFunc(gate)
	_, err := fn(tool.Context(nil), deleteFileArgs{Path: outside})
	if err == nil {
		t.Fatalf("expected denial for out-of-scope delete")
	}
	if _, statErr := os.Stat(outside); statErr != nil {
		t.Errorf("file should still exist after denial: %v", statErr)
	}
}

func TestStat_ReturnsMetadata(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "thing.txt")
	if err := os.WriteFile(path, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	fn := statFunc(gateFor(t, dir))
	res, err := fn(tool.Context(nil), statArgs{Path: path})
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !res.Exists {
		t.Errorf("Exists = false, want true")
	}
	if res.IsDir {
		t.Errorf("IsDir = true, want false (regular file)")
	}
	if res.Size != 5 {
		t.Errorf("Size = %d, want 5", res.Size)
	}
	if res.ModTime == "" {
		t.Errorf("ModTime should be set")
	}
	if res.Mode == "" {
		t.Errorf("Mode should be set")
	}
}

func TestStat_MissingPathExistsFalse(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fn := statFunc(gateFor(t, dir))
	res, err := fn(tool.Context(nil), statArgs{Path: filepath.Join(dir, "never-existed")})
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if res.Exists {
		t.Errorf("Exists = true, want false for missing path")
	}
	if res.Size != 0 || res.ModTime != "" || res.Mode != "" {
		t.Errorf("missing path should have zero metadata, got %+v", res)
	}
}

func TestStat_DirReportsIsDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fn := statFunc(gateFor(t, dir))
	res, err := fn(tool.Context(nil), statArgs{Path: dir})
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !res.IsDir {
		t.Errorf("IsDir = false, want true for directory")
	}
}
