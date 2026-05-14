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
	s.AddAlwaysAllow(other + "/...")
	in, _ := s.Contains(filepath.Join(other, "x.txt"))
	if !in {
		t.Errorf("AddAlwaysAllow did not extend scope")
	}
}
