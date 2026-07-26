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

package permissions

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// resolvedTempDir returns a t.TempDir() with symlinks pre-resolved so
// assertions compare like with like even when the OS tempdir itself
// sits behind a symlink (macOS /tmp -> /private/tmp).
func resolvedTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

func TestResolvePath_SymlinkToFile(t *testing.T) {
	t.Parallel()
	dir := resolvedTempDir(t)
	target := filepath.Join(dir, "real.txt")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.txt")
	mustSymlink(t, target, link)

	got, err := ResolvePath(link)
	if err != nil {
		t.Fatal(err)
	}
	if got != target {
		t.Errorf("ResolvePath(%q) = %q, want %q", link, got, target)
	}
}

func TestResolvePath_NonExistentTailUsesAncestor(t *testing.T) {
	t.Parallel()
	dir := resolvedTempDir(t)
	outside := filepath.Join(dir, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	proj := filepath.Join(dir, "proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	// proj/dirlink -> outside; proj/dirlink/sub/new.txt doesn't exist.
	dirlink := filepath.Join(proj, "dirlink")
	mustSymlink(t, outside, dirlink)

	got, err := ResolvePath(filepath.Join(dirlink, "sub", "new.txt"))
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(outside, "sub", "new.txt")
	if got != want {
		t.Errorf("ResolvePath = %q, want %q (deepest existing ancestor resolved)", got, want)
	}
}

func TestResolvePath_PlainPathsUntouched(t *testing.T) {
	t.Parallel()
	dir := resolvedTempDir(t)
	path := filepath.Join(dir, "a", "b.txt")
	got, err := ResolvePath(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != path {
		t.Errorf("ResolvePath(%q) = %q, want unchanged", path, got)
	}
}

func TestResolvePath_SymlinkLoopErrors(t *testing.T) {
	t.Parallel()
	dir := resolvedTempDir(t)
	loop := filepath.Join(dir, "loop")
	mustSymlink(t, loop, loop)
	if _, err := ResolvePath(loop); err == nil {
		t.Fatal("expected error for symlink loop")
	}
}

// --- PathScope behavior (#374) ---

// TestAccessFor_SymlinkEscape pins the core #374 fix: a symlink
// inside the scope root pointing at an out-of-scope file yields
// AccessNone — the gate then treats it exactly like any out-of-scope
// path (prompt in ask mode, deny where prompting is impossible).
func TestAccessFor_SymlinkEscape(t *testing.T) {
	t.Parallel()
	dir := resolvedTempDir(t)
	proj := filepath.Join(dir, "proj")
	outside := filepath.Join(dir, "outside")
	for _, d := range []string{proj, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("s3cret"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(proj, "innocent.txt")
	mustSymlink(t, secret, link)

	scope, err := NewPathScope(proj, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	access, err := scope.AccessFor(link)
	if err != nil {
		t.Fatal(err)
	}
	if access != AccessNone {
		t.Errorf("AccessFor(in-scope symlink -> out-of-scope file) = %v, want AccessNone", access)
	}
	// The real target, addressed directly, is of course also denied.
	if access, _ := scope.AccessFor(secret); access != AccessNone {
		t.Errorf("AccessFor(outside target) = %v, want AccessNone", access)
	}
	// A normal in-scope file is unaffected.
	normal := filepath.Join(proj, "normal.txt")
	if err := os.WriteFile(normal, []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	if access, _ := scope.AccessFor(normal); access != AccessReadWrite {
		t.Errorf("AccessFor(normal in-scope file) = %v, want AccessReadWrite", access)
	}
}

// TestAccessFor_SymlinkedAncestorDir covers the directory variant:
// proj/dir is a symlink to an out-of-scope directory, and both an
// existing file and a not-yet-created file underneath it must
// classify against the real location.
func TestAccessFor_SymlinkedAncestorDir(t *testing.T) {
	t.Parallel()
	dir := resolvedTempDir(t)
	proj := filepath.Join(dir, "proj")
	outside := filepath.Join(dir, "outside")
	for _, d := range []string{proj, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	existing := filepath.Join(outside, "existing.txt")
	if err := os.WriteFile(existing, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	dirlink := filepath.Join(proj, "dirlink")
	mustSymlink(t, outside, dirlink)

	scope, err := NewPathScope(proj, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if access, _ := scope.AccessFor(filepath.Join(dirlink, "existing.txt")); access != AccessNone {
		t.Errorf("AccessFor(file under symlinked dir) = %v, want AccessNone", access)
	}
	// New-file write into the symlinked directory: the tail doesn't
	// exist, so the deepest existing ancestor (the symlinked dir)
	// must be resolved and the write classified out-of-scope.
	if access, _ := scope.AccessFor(filepath.Join(dirlink, "new-file.txt")); access != AccessNone {
		t.Errorf("AccessFor(new file under symlinked dir) = %v, want AccessNone", access)
	}
}

// TestAccessFor_DanglingSymlink pins the dangling-symlink behavior: a
// link whose target doesn't exist resolves to its own (lexical)
// location via the deepest-existing-ancestor rule. Reads through it
// fail at the OS level anyway, and the write path replaces the link
// itself (atomic rename) rather than writing through it.
func TestAccessFor_DanglingSymlink(t *testing.T) {
	t.Parallel()
	dir := resolvedTempDir(t)
	proj := filepath.Join(dir, "proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	dangling := filepath.Join(proj, "dangling.txt")
	mustSymlink(t, filepath.Join(dir, "no-such-target.txt"), dangling)

	scope, err := NewPathScope(proj, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if access, _ := scope.AccessFor(dangling); access != AccessReadWrite {
		t.Errorf("AccessFor(dangling symlink inside scope) = %v, want AccessReadWrite", access)
	}
}

// TestAccessFor_UnresolvablePathFailsClosed pins that resolution
// errors other than not-exist (here: a symlink loop) yield
// AccessNone without an error — the caller escalates to the
// out-of-scope prompt path instead of aborting.
func TestAccessFor_UnresolvablePathFailsClosed(t *testing.T) {
	t.Parallel()
	dir := resolvedTempDir(t)
	proj := filepath.Join(dir, "proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	loop := filepath.Join(proj, "loop")
	mustSymlink(t, loop, loop)

	scope, err := NewPathScope(proj, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	access, err := scope.AccessFor(loop)
	if err != nil {
		t.Fatalf("AccessFor should fail closed, not error: %v", err)
	}
	if access != AccessNone {
		t.Errorf("AccessFor(symlink loop) = %v, want AccessNone", access)
	}
}

// TestNewPathScope_SymlinkedRoot pins that a scope root that itself
// sits behind a symlink still covers its (resolved) contents —
// without root resolution, every in-project path would misclassify
// as out-of-scope on hosts where the workspace path is a symlink.
func TestNewPathScope_SymlinkedRoot(t *testing.T) {
	t.Parallel()
	dir := resolvedTempDir(t)
	real := filepath.Join(dir, "real-proj")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	rootLink := filepath.Join(dir, "proj-link")
	mustSymlink(t, real, rootLink)
	inside := filepath.Join(real, "file.txt")
	if err := os.WriteFile(inside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	scope, err := NewPathScope(rootLink, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{inside, filepath.Join(rootLink, "file.txt")} {
		if access, _ := scope.AccessFor(p); access != AccessReadWrite {
			t.Errorf("AccessFor(%q) with symlinked root = %v, want AccessReadWrite", p, access)
		}
	}
}

// --- Gate-level behavior (#374) ---

// TestGate_SymlinkEscape_PromptsLikeOutOfScope pins the end-to-end
// contract: reads AND writes through an in-scope symlink to an
// out-of-scope target behave exactly like any out-of-scope path —
// prompt in ask mode, deny when the prompter denies (or is absent).
func TestGate_SymlinkEscape_PromptsLikeOutOfScope(t *testing.T) {
	t.Parallel()
	dir := resolvedTempDir(t)
	proj := filepath.Join(dir, "proj")
	outside := filepath.Join(dir, "outside")
	for _, d := range []string{proj, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("s3cret"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(proj, "innocent.txt")
	mustSymlink(t, secret, link)

	scope, err := NewPathScope(proj, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	prompter := &fakePrompter{decision: DecisionDeny}
	g := New(Options{Mode: ModeAsk, Scope: scope, Prompter: prompter})
	ctx := context.Background()

	if err := g.CheckFileRead(ctx, "read_file", link); err == nil {
		t.Error("read through escaping symlink: want denial after prompt, got nil")
	}
	if err := g.CheckFileWrite(ctx, "write_file", link); err == nil {
		t.Error("write through escaping symlink: want denial after prompt, got nil")
	}
	if len(prompter.calls) != 2 {
		t.Errorf("expected 2 out-of-scope prompts, got %d", len(prompter.calls))
	}
	for _, call := range prompter.calls {
		if call.Kind != PromptKindPathScope {
			t.Errorf("prompt kind = %v, want PromptKindPathScope", call.Kind)
		}
	}

	// Normal in-scope traffic is unaffected: read passes silently.
	normal := filepath.Join(proj, "ok.txt")
	if err := os.WriteFile(normal, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := g.CheckFileRead(ctx, "read_file", normal); err != nil {
		t.Errorf("in-scope read: %v", err)
	}
	if len(prompter.calls) != 2 {
		t.Errorf("in-scope read must not prompt; got %d calls", len(prompter.calls))
	}
}
