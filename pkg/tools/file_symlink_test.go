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
	"testing"

	"google.golang.org/adk/tool"

	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

// symlinkScopeGate returns a gate in "allow" mode (no prompter) scoped
// to root, so any out-of-scope access — including one laundered
// through a symlink — is denied rather than prompted. resolvedTemp is
// the symlink-resolved temp root the test built its layout under.
func symlinkScopeGate(t *testing.T, root string) *permissions.Gate {
	t.Helper()
	scope, err := permissions.NewPathScope(root, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	return permissions.New(permissions.Options{
		Mode:  permissions.ModeAllow, // no allowlist match + no prompter → deny
		Scope: scope,
	})
}

func resolvedTemp(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestReadFile_SymlinkEscape_Denied is the #374 read-exfil pin: a
// symlink inside the project root pointing at an out-of-scope file
// must not let read_file exfiltrate it.
func TestReadFile_SymlinkEscape_Denied(t *testing.T) {
	t.Parallel()
	dir := resolvedTemp(t)
	proj := filepath.Join(dir, "proj")
	outside := filepath.Join(dir, "outside")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(outside, "creds")
	if err := os.WriteFile(secret, []byte("t0ken"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(proj, "notes.txt")
	if err := os.Symlink(secret, link); err != nil {
		t.Fatal(err)
	}

	gate := symlinkScopeGate(t, proj)
	fn := readFileFunc(gate, config.DefaultConfig())
	if _, err := fn(tool.Context(nil), readFileArgs{Path: link}); err == nil {
		t.Fatal("read_file through escaping symlink should be denied")
	}
}

// TestWriteFile_SymlinkEscape_Denied is the #374 write-overwrite pin:
// a symlink inside the root pointing at an out-of-scope file must not
// let write_file overwrite the target.
func TestWriteFile_SymlinkEscape_Denied(t *testing.T) {
	t.Parallel()
	dir := resolvedTemp(t)
	proj := filepath.Join(dir, "proj")
	outside := filepath.Join(dir, "outside")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(outside, "victim")
	if err := os.WriteFile(target, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(proj, "harmless.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	gate := symlinkScopeGate(t, proj)
	fn := writeFileFunc(gate)
	if _, err := fn(tool.Context(nil), writeFileArgs{Path: link, Content: "pwned"}); err == nil {
		t.Fatal("write_file through escaping symlink should be denied")
	}
	if got, _ := os.ReadFile(target); string(got) != "original" {
		t.Errorf("target was overwritten through symlink: %q", got)
	}
}

// TestWriteFile_NewFileInSymlinkedDir_Denied is the #374 new-file pin:
// creating a not-yet-existing file inside a symlinked directory that
// escapes scope must classify against the directory's real location
// and be denied.
func TestWriteFile_NewFileInSymlinkedDir_Denied(t *testing.T) {
	t.Parallel()
	dir := resolvedTemp(t)
	proj := filepath.Join(dir, "proj")
	outside := filepath.Join(dir, "outside")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	dirlink := filepath.Join(proj, "exports")
	if err := os.Symlink(outside, dirlink); err != nil {
		t.Fatal(err)
	}

	gate := symlinkScopeGate(t, proj)
	fn := writeFileFunc(gate)
	newFile := filepath.Join(dirlink, "planted.txt")
	if _, err := fn(tool.Context(nil), writeFileArgs{Path: newFile, Content: "x"}); err == nil {
		t.Fatal("write_file of new file in escaping symlinked dir should be denied")
	}
	if _, err := os.Stat(filepath.Join(outside, "planted.txt")); err == nil {
		t.Error("file was created in the out-of-scope real directory")
	}
}

// TestDeleteFile_SymlinkEscape_Denied is the #374 delete pin.
func TestDeleteFile_SymlinkEscape_Denied(t *testing.T) {
	t.Parallel()
	dir := resolvedTemp(t)
	proj := filepath.Join(dir, "proj")
	outside := filepath.Join(dir, "outside")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(outside, "keep")
	if err := os.WriteFile(victim, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(proj, "trash.txt")
	if err := os.Symlink(victim, link); err != nil {
		t.Fatal(err)
	}

	gate := symlinkScopeGate(t, proj)
	fn := deleteFileFunc(gate)
	if _, err := fn(tool.Context(nil), deleteFileArgs{Path: link}); err == nil {
		t.Fatal("delete_file through escaping symlink should be denied")
	}
	if _, err := os.Stat(victim); err != nil {
		t.Errorf("out-of-scope target was deleted through symlink: %v", err)
	}
}

// TestReadFile_NormalPathUnaffected pins that ordinary (non-symlink)
// in-scope reads still work after the resolution change.
func TestReadFile_NormalPathUnaffected(t *testing.T) {
	t.Parallel()
	dir := resolvedTemp(t)
	path := filepath.Join(dir, "hello.txt")
	if err := os.WriteFile(path, []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	gate := symlinkScopeGate(t, dir)
	fn := readFileFunc(gate, config.DefaultConfig())
	res, err := fn(tool.Context(nil), readFileArgs{Path: path})
	if err != nil {
		t.Fatalf("in-scope read: %v", err)
	}
	if res.Content != "hi" {
		t.Errorf("content = %q, want %q", res.Content, "hi")
	}
}
