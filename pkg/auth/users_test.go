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

package auth_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/auth"
)

func writeUsersFile(t *testing.T, body string, mode os.FileMode) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "users.json")
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatalf("write users.json: %v", err)
	}
	// os.WriteFile honors umask, which on most CI systems clobbers
	// 0600 down to 0644-or-whatever. Force the mode we asked for.
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod users.json: %v", err)
	}
	return path
}

func TestLoadUsersFile_HappyPath(t *testing.T) {
	t.Parallel()
	path := writeUsersFile(t, `{
		"version": 1,
		"users": [
			{"identity": "alice@example.com", "token": "tok_alice", "labels": {"team": "platform"}},
			{"identity": "bob@example.com", "token": "tok_bob"}
		]
	}`, 0o600)

	uf, err := auth.LoadUsersFile(path)
	if err != nil {
		t.Fatalf("LoadUsersFile err: %v", err)
	}
	if uf.Version != 1 {
		t.Errorf("Version: got %d, want 1", uf.Version)
	}
	if len(uf.Users) != 2 {
		t.Fatalf("Users len: got %d, want 2", len(uf.Users))
	}
	if uf.Users[0].Labels["team"] != "platform" {
		t.Errorf("Labels not preserved: got %v", uf.Users[0].Labels)
	}
}

func TestLoadUsersFile_RejectsWorldReadable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file-mode check skipped on Windows")
	}
	t.Parallel()
	path := writeUsersFile(t, `{"version": 1, "users": []}`, 0o644)

	_, err := auth.LoadUsersFile(path)
	if err == nil {
		t.Fatal("expected error on world-readable users file; got nil")
	}
	if !strings.Contains(err.Error(), "0600 or stricter") {
		t.Errorf("error should explain file-mode requirement, got: %v", err)
	}
}

func TestLoadUsersFile_RejectsGroupReadable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file-mode check skipped on Windows")
	}
	t.Parallel()
	path := writeUsersFile(t, `{"version": 1, "users": []}`, 0o640)

	_, err := auth.LoadUsersFile(path)
	if err == nil {
		t.Fatal("expected error on group-readable users file; got nil")
	}
}

func TestLoadUsersFile_AcceptsOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file-mode check skipped on Windows")
	}
	t.Parallel()
	// 0400 (owner read-only) is stricter than 0600 and must be
	// accepted — the requirement is "no group/other bits", not
	// "exactly 0600".
	path := writeUsersFile(t, `{"version": 1, "users": []}`, 0o400)
	if _, err := auth.LoadUsersFile(path); err != nil {
		t.Errorf("0400 mode should be accepted (stricter than 0600), got: %v", err)
	}
}

func TestLoadUsersFile_RejectsUnknownVersion(t *testing.T) {
	t.Parallel()
	path := writeUsersFile(t, `{"version": 99, "users": []}`, 0o600)
	_, err := auth.LoadUsersFile(path)
	if err == nil || !strings.Contains(err.Error(), "schema version") {
		t.Errorf("expected schema-version rejection, got: %v", err)
	}
}

func TestLoadUsersFile_RejectsMissingIdentity(t *testing.T) {
	t.Parallel()
	path := writeUsersFile(t, `{
		"version": 1,
		"users": [{"token": "tok_orphan"}]
	}`, 0o600)
	_, err := auth.LoadUsersFile(path)
	if err == nil || !strings.Contains(err.Error(), "identity is required") {
		t.Errorf("expected identity-required error, got: %v", err)
	}
}

func TestLoadUsersFile_RejectsMissingToken(t *testing.T) {
	t.Parallel()
	path := writeUsersFile(t, `{
		"version": 1,
		"users": [{"identity": "alice@example.com"}]
	}`, 0o600)
	_, err := auth.LoadUsersFile(path)
	if err == nil || !strings.Contains(err.Error(), "token is required") {
		t.Errorf("expected token-required error, got: %v", err)
	}
}

func TestLoadUsersFile_RejectsDuplicateToken(t *testing.T) {
	t.Parallel()
	path := writeUsersFile(t, `{
		"version": 1,
		"users": [
			{"identity": "alice@example.com", "token": "shared"},
			{"identity": "bob@example.com",   "token": "shared"}
		]
	}`, 0o600)
	_, err := auth.LoadUsersFile(path)
	if err == nil || !strings.Contains(err.Error(), "token collides") {
		t.Errorf("expected duplicate-token rejection, got: %v", err)
	}
}

func TestLoadUsersFile_RejectsDuplicateIdentity(t *testing.T) {
	t.Parallel()
	path := writeUsersFile(t, `{
		"version": 1,
		"users": [
			{"identity": "alice@example.com", "token": "tok_a"},
			{"identity": "alice@example.com", "token": "tok_b"}
		]
	}`, 0o600)
	_, err := auth.LoadUsersFile(path)
	if err == nil || !strings.Contains(err.Error(), "duplicate identity") {
		t.Errorf("expected duplicate-identity rejection, got: %v", err)
	}
}

func TestLoadUsersFile_RejectsUnknownFields(t *testing.T) {
	t.Parallel()
	// Future-proofing: an operator who hand-edits users.json shouldn't
	// silently lose configuration to a typo'd field name.
	path := writeUsersFile(t, `{
		"version": 1,
		"users": [],
		"oops_typo": "would-be-silent-otherwise"
	}`, 0o600)
	_, err := auth.LoadUsersFile(path)
	if err == nil {
		t.Fatal("expected error on unknown top-level field; got nil")
	}
}

func TestLoadUsersFile_NotFound(t *testing.T) {
	t.Parallel()
	_, err := auth.LoadUsersFile(filepath.Join(t.TempDir(), "nonexistent.json"))
	if err == nil {
		t.Fatal("expected error on missing file; got nil")
	}
}
