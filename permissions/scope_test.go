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
	"os"
	"path/filepath"
	"testing"
)

func TestPathScope_RootContainment(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	other := t.TempDir()

	s, err := NewPathScope(root, "", nil)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		path string
		want bool
	}{
		{"root itself", root, true},
		{"file in root", filepath.Join(root, "x.txt"), true},
		{"nested dir", filepath.Join(root, "a", "b", "c.txt"), true},
		{"sibling tempdir", filepath.Join(other, "y.txt"), false},
		{"absolute parent", "/etc/passwd", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := s.Contains(tc.path)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("Contains(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

func TestPathScope_AllowlistTreePattern(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	allowed := t.TempDir()

	s, err := NewPathScope(root, "", []string{allowed + "/..."})
	if err != nil {
		t.Fatal(err)
	}
	in, err := s.Contains(filepath.Join(allowed, "deep", "file.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !in {
		t.Errorf("expected allowed-tree path to be in scope")
	}
	out, _ := s.Contains("/etc/passwd")
	if out {
		t.Errorf("/etc/passwd unexpectedly in scope")
	}
}

func TestPathScope_AllowlistExactPath(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	exact := filepath.Join(t.TempDir(), "single.json")

	s, err := NewPathScope(root, "", []string{exact})
	if err != nil {
		t.Fatal(err)
	}
	in, _ := s.Contains(exact)
	if !in {
		t.Errorf("exact allowlist entry not honored")
	}
	siblingInDir := filepath.Join(filepath.Dir(exact), "sibling.json")
	if ok, _ := s.Contains(siblingInDir); ok {
		t.Errorf("exact allowlist leaked to sibling")
	}
}

func TestPathScope_TildeExpansion(t *testing.T) {
	t.Parallel()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home: %v", err)
	}
	s, err := NewPathScope("", home, nil)
	if err != nil {
		t.Fatal(err)
	}
	in, err := s.Contains("~/.core-agent/sessions/x.json")
	if err != nil {
		t.Fatal(err)
	}
	if !in {
		t.Errorf("tilde-expanded path not recognized as in-scope")
	}
}

func TestPathScope_AddAlwaysAllow(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	other := t.TempDir()
	s, err := NewPathScope(root, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if in, _ := s.Contains(other); in {
		t.Fatal("baseline: sibling should NOT be in scope")
	}
	s.AddAlwaysAllow(other+"/...", AccessReadWrite)
	in, _ := s.Contains(filepath.Join(other, "x.txt"))
	if !in {
		t.Errorf("AddAlwaysAllow did not extend scope")
	}
}

func TestParseAccess(t *testing.T) {
	t.Parallel()
	cases := map[string]Access{
		"r":          AccessRead,
		"R":          AccessRead,
		"read":       AccessRead,
		"  read  ":   AccessRead,
		"w":          AccessWrite,
		"write":      AccessWrite,
		"rw":         AccessReadWrite,
		"wr":         AccessReadWrite,
		"readwrite":  AccessReadWrite,
		"READWRITE":  AccessReadWrite,
		"read+write": AccessReadWrite,
	}
	for in, want := range cases {
		got, err := ParseAccess(in)
		if err != nil {
			t.Errorf("ParseAccess(%q) errored: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseAccess(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestParseAccess_Invalid(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"", "x", "rwx", "rwwr", "none", "1", "true"} {
		if _, err := ParseAccess(in); err == nil {
			t.Errorf("ParseAccess(%q) should have failed", in)
		}
	}
}

func TestAccess_Allows(t *testing.T) {
	t.Parallel()
	if !AccessReadWrite.Allows(AccessRead) {
		t.Error("rw should allow read")
	}
	if !AccessReadWrite.Allows(AccessWrite) {
		t.Error("rw should allow write")
	}
	if AccessRead.Allows(AccessWrite) {
		t.Error("read-only should not allow write")
	}
	if AccessWrite.Allows(AccessRead) {
		t.Error("write-only should not allow read")
	}
	if AccessNone.Allows(AccessRead) {
		t.Error("none should not allow read")
	}
}

func TestAccess_StringRoundTrip(t *testing.T) {
	t.Parallel()
	for _, a := range []Access{AccessRead, AccessWrite, AccessReadWrite} {
		s := a.String()
		got, err := ParseAccess(s)
		if err != nil {
			t.Errorf("round-trip ParseAccess(%q): %v", s, err)
		}
		if got != a {
			t.Errorf("round-trip mismatch: %v.String()=%q → ParseAccess=%v", a, s, got)
		}
	}
}

func TestPathScope_AccessFor_ReadOnlyEntry(t *testing.T) {
	t.Parallel()
	tree := t.TempDir()
	s, err := NewPathScopeFromEntries("", "", []pathEntry{
		{Pattern: tree + "/...", Access: AccessRead},
	})
	if err != nil {
		t.Fatal(err)
	}
	acc, _ := s.AccessFor(filepath.Join(tree, "x.txt"))
	if acc != AccessRead {
		t.Errorf("expected AccessRead, got %v", acc)
	}
	if acc.Allows(AccessWrite) {
		t.Error("read-only entry should NOT allow write")
	}
}

func TestPathScope_AccessFor_LongestPrefixWins(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	a := filepath.Join(root, "a")
	ab := filepath.Join(root, "a", "b")
	s, err := NewPathScopeFromEntries("", "", []pathEntry{
		{Pattern: a + "/...", Access: AccessRead},
		{Pattern: ab + "/...", Access: AccessReadWrite},
	})
	if err != nil {
		t.Fatal(err)
	}
	// /a/x.txt is covered only by the broader r entry.
	got, _ := s.AccessFor(filepath.Join(a, "x.txt"))
	if got != AccessRead {
		t.Errorf("/a/x.txt: expected AccessRead, got %v", got)
	}
	// /a/b/y.txt matches both — the narrower rw should win.
	got, _ = s.AccessFor(filepath.Join(ab, "y.txt"))
	if got != AccessReadWrite {
		t.Errorf("/a/b/y.txt: expected AccessReadWrite (longest prefix), got %v", got)
	}
}

func TestPathScope_AccessFor_NoMatch(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	s, err := NewPathScopeFromEntries(root, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := s.AccessFor(filepath.Join(t.TempDir(), "x.txt"))
	if got != AccessNone {
		t.Errorf("sibling tempdir: expected AccessNone, got %v", got)
	}
}

func TestPathScope_AccessFor_RootIsRW(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	s, err := NewPathScopeFromEntries(root, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := s.AccessFor(filepath.Join(root, "nested", "x.txt"))
	if got != AccessReadWrite {
		t.Errorf("path inside project root: expected AccessReadWrite, got %v", got)
	}
}

func TestPathScope_LegacyAllowList_GrantsRW(t *testing.T) {
	t.Parallel()
	tree := t.TempDir()
	// Legacy untyped allow list — every entry should map to rw for
	// backward compatibility with pre-Access configs.
	s, err := NewPathScope("", "", []string{tree + "/..."})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := s.AccessFor(filepath.Join(tree, "x.txt"))
	if got != AccessReadWrite {
		t.Errorf("legacy allow entry: expected AccessReadWrite, got %v", got)
	}
}

func TestPathScope_AddAlwaysAllow_PersistsAccess(t *testing.T) {
	t.Parallel()
	tree := t.TempDir()
	s, err := NewPathScope("", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	s.AddAlwaysAllow(tree+"/...", AccessRead)
	got, _ := s.AccessFor(filepath.Join(tree, "x.txt"))
	if got != AccessRead {
		t.Errorf("AddAlwaysAllow(AccessRead): got %v, want AccessRead", got)
	}
}
