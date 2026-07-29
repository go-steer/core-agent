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

package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestMutate_ErrNoChangeSkipsSave pins the skip-the-save contract:
// a fn reporting ErrNoChange must leave the filesystem untouched —
// no file creation on a fresh dir, no rewrite of an existing file.
func TestMutate_ErrNoChangeSkipsSave(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	if err := Mutate(dir, func(*Config) error { return ErrNoChange }); err != nil {
		t.Fatalf("Mutate(ErrNoChange): %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ConfigFileName)); !os.IsNotExist(err) {
		t.Fatal("ErrNoChange created config.json — the no-op path must not write")
	}

	// All-dup append is the production shape of that path (#492
	// drift disclosure: previously an identical file was rewritten).
	if err := AppendPermissionsAllow(dir, []string{"bash:ls"}); err != nil {
		t.Fatalf("first append: %v", err)
	}
	before, err := os.Stat(filepath.Join(dir, ConfigFileName))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if err := AppendPermissionsAllow(dir, []string{"bash:ls"}); err != nil {
		t.Fatalf("dup append: %v", err)
	}
	after, err := os.Stat(filepath.Join(dir, ConfigFileName))
	if err != nil {
		t.Fatalf("stat after: %v", err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Error("all-dup append rewrote the file — ErrNoChange must skip the save")
	}
}

// TestMutate_FnErrorAbortsWithoutSave pins that a real error from fn
// (e.g. PersistThemeChoice's pre-save validation) aborts the write.
func TestMutate_FnErrorAbortsWithoutSave(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	boom := errors.New("boom")
	if err := Mutate(dir, func(*Config) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("Mutate error = %v, want fn's error", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ConfigFileName)); !os.IsNotExist(err) {
		t.Fatal("failed Mutate wrote config.json")
	}
}
