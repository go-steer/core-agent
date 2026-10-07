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

package childenv

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
)

func TestFileSetMatchesByPathAndInode(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("link semantics")
	}
	dir := t.TempDir()
	table := filepath.Join(dir, "users.json")
	other := filepath.Join(dir, "other.json")
	for _, p := range []string{table, other} {
		if err := os.WriteFile(p, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	sym := filepath.Join(dir, "sym")
	hard := filepath.Join(dir, "hard")
	if err := os.Symlink(table, sym); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(table, hard); err != nil {
		t.Fatal(err)
	}
	var s FileSet
	s.Add("", table, table)
	if got := s.Paths(); !slices.Equal(got, []string{table}) {
		t.Fatalf("Paths() = %v", got)
	}
	for _, p := range []string{table, filepath.Join(dir, ".", "users.json"), sym, hard} {
		if abs, ok := s.Match(p); !ok || abs != table {
			t.Errorf("Match(%s) = %q, %v; want %s", p, abs, ok, table)
		}
	}
	if _, ok := s.Match(other); ok {
		t.Errorf("an unrelated file matched")
	}
	var empty FileSet
	if _, ok := empty.Match(table); ok {
		t.Errorf("the empty set matched")
	}
}

// A file that does not exist when it is added is still matched once it
// appears — the comparison is against the entry as it is now.
func TestFileSetMatchesAFileCreatedLater(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	table := filepath.Join(dir, "users.json")
	var s FileSet
	s.Add(table)
	if _, ok := s.Match(table); !ok {
		t.Errorf("a not-yet-existing entry does not match its own path")
	}
	if err := os.WriteFile(table, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		return
	}
	hard := filepath.Join(dir, "hard")
	if err := os.Link(table, hard); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Match(hard); !ok {
		t.Errorf("a hard link to an entry created after Add does not match")
	}
}

func TestFileSetSpellings(t *testing.T) {
	t.Parallel()
	var s FileSet
	s.Add("/srv/app/secrets/users.json", "/elsewhere/users.json")
	got := s.Spellings("/srv/app")
	if want := []string{"/srv/app/secrets/users.json", "secrets/users.json"}; !slices.Equal(got["/srv/app/secrets/users.json"], want) {
		t.Errorf("in-tree spellings = %v, want %v", got["/srv/app/secrets/users.json"], want)
	}
	if want := []string{"/elsewhere/users.json"}; !slices.Equal(got["/elsewhere/users.json"], want) {
		t.Errorf("out-of-tree spellings = %v, want %v (no ../ forms)", got["/elsewhere/users.json"], want)
	}
}

// The process-wide set is what the instruction loader consults. Names
// are per-test temp paths, since the set has no reset.
func TestWithheldFile(t *testing.T) {
	t.Parallel()
	table := filepath.Join(t.TempDir(), "users.json")
	if WithheldFile(table) {
		t.Fatal("matched before it was withheld")
	}
	WithholdFiles(table)
	if !WithheldFile(table) || !slices.Contains(WithheldFiles(), table) {
		t.Error("not withheld after WithholdFiles")
	}
}
