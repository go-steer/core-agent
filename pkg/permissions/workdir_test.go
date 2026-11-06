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
	"path/filepath"
	"strings"
	"testing"
)

func TestIsolatedWorktree_AllowsWorktreeWritesAndRefusesParentTreeWrites(t *testing.T) {
	t.Parallel()
	parentDir := t.TempDir()
	worktreeDir := t.TempDir()

	scope, err := NewPathScope(parentDir, "", nil)
	if err != nil {
		t.Fatalf("NewPathScope: %v", err)
	}
	gate := New(Options{
		Mode:  ModeYolo,
		Scope: scope,
	})

	ctx := WithIsolatedWorktree(context.Background(), worktreeDir)
	if got := WorkDirFromContext(ctx); got != filepath.Clean(worktreeDir) {
		t.Fatalf("WorkDirFromContext = %q, want %q", got, filepath.Clean(worktreeDir))
	}
	if got := IsolatedWorktreeFromContext(ctx); got != filepath.Clean(worktreeDir) {
		t.Fatalf("IsolatedWorktreeFromContext = %q, want %q", got, filepath.Clean(worktreeDir))
	}

	// Reading and writing inside the isolated worktree succeeds without
	// needing an explicit allowlist entry for worktreeDir.
	wtFile := filepath.Join(worktreeDir, "sub.txt")
	if err := gate.CheckFileRead(ctx, "read_file", wtFile); err != nil {
		t.Errorf("CheckFileRead inside worktree: %v", err)
	}
	if err := gate.CheckFileWrite(ctx, "write_file", wtFile); err != nil {
		t.Errorf("CheckFileWrite inside worktree: %v", err)
	}

	// Reading from the parent project root still succeeds, but writing to
	// the parent project root via an absolute path is refused.
	parentFile := filepath.Join(parentDir, "shared.txt")
	if err := gate.CheckFileRead(ctx, "read_file", parentFile); err != nil {
		t.Errorf("CheckFileRead on parent tree: %v", err)
	}
	err = gate.CheckFileWrite(ctx, "write_file", parentFile)
	if err == nil || !strings.Contains(err.Error(), "isolated in worktree") {
		t.Errorf("CheckFileWrite on parent tree: got %v, want isolated-worktree refusal", err)
	}
}
