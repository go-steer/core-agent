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

package watchdog

import "testing"

// TestArgsEquivalent covers the #649 canonicalization contract from
// both directions: the path spellings that must collapse, and the
// arguments that must stay distinct so a Critical alert (which halts
// the agent under enforce) isn't raised on legitimate work.
func TestArgsEquivalent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		a, b string
		want bool
	}{
		{"identical", `{"path":"a.go"}`, `{"path":"a.go"}`, true},
		{"dot-slash prefix", `{"path":"./a.go"}`, `{"path":"a.go"}`, true},
		{"traversal cleaned", `{"path":"dir/../a.go"}`, `{"path":"a.go"}`, true},
		{"trailing slash on a dir", `{"dir":"pkg/agent/"}`, `{"dir":"pkg/agent"}`, true},
		// The case #144 named and v1 could not catch.
		{"absolute vs relative", `{"path":"/workspace/main.go"}`, `{"path":"main.go"}`, true},
		{"absolute vs partial", `{"path":"/workspace/pkg/x.go"}`, `{"path":"pkg/x.go"}`, true},
		{"file_path spelling", `{"file_path":"/w/a.go"}`, `{"file_path":"a.go"}`, true},

		// Same basename in different directories is the false positive a
		// basename-keyed canonicalization would introduce. Every Go repo
		// has a dozen doc.go files.
		{"same basename, different dirs", `{"path":"a/doc.go"}`, `{"path":"b/doc.go"}`, false},
		{"suffix not on a boundary", `{"path":"/w/xmain.go"}`, `{"path":"main.go"}`, false},
		{"different files", `{"path":"a.go"}`, `{"path":"b.go"}`, false},
		{"extra argument", `{"path":"a.go"}`, `{"path":"a.go","limit":5}`, false},
		{"different non-path value", `{"path":"a.go","limit":5}`, `{"path":"a.go","limit":6}`, false},
		// Non-path keys are never path-normalized: a grep pattern or a
		// shell command that happens to look like a path is not one.
		{"pattern is not a path", `{"pattern":"a/b"}`, `{"pattern":"b"}`, false},
		{"command is not a path", `{"command":"ls /tmp"}`, `{"command":"/tmp"}`, false},
		{"empty args", `{}`, `{}`, true},
		// Unparseable input still compares to itself and nothing else.
		{"non-json identical", `not-json`, `not-json`, true},
		{"non-json differing", `not-json`, `also-not-json`, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := argsEquivalent(tc.a, tc.b); got != tc.want {
				t.Errorf("argsEquivalent(%s, %s) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
			// The relation has to be symmetric — the repeat detector
			// compares in one direction, the tests in the other.
			if got := argsEquivalent(tc.b, tc.a); got != tc.want {
				t.Errorf("argsEquivalent(%s, %s) = %v, want %v (asymmetric)", tc.b, tc.a, got, tc.want)
			}
		})
	}
}

// TestCanonicalArgs pins the hashable-key form the cycle detector uses:
// path values cleaned, everything else untouched, output stable.
func TestCanonicalArgs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{"cleaned", `{"path":"./a.go"}`, `{"path":"a.go"}`},
		{"already canonical is returned verbatim", `{"path":"a.go"}`, `{"path":"a.go"}`},
		{"non-path untouched", `{"pattern":"./a.go"}`, `{"pattern":"./a.go"}`},
		{"empty", ``, ``},
		{"unparseable passes through", `not-json`, `not-json`},
		{"json array passes through", `[1,2]`, `[1,2]`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := canonicalArgs(tc.in); got != tc.want {
				t.Errorf("canonicalArgs(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}

	// Determinism: the same input must produce the same key every call,
	// or the cycle detector's history never matches itself.
	in := `{"path":"./a.go","file":"./b.go","limit":3}`
	first := canonicalArgs(in)
	for i := 0; i < 20; i++ {
		if got := canonicalArgs(in); got != first {
			t.Fatalf("canonicalArgs is not deterministic: %q then %q", first, got)
		}
	}
}

// TestPathSuffixEquivalent covers the boundary rule directly — the
// component-boundary check is the whole reason this is safe.
func TestPathSuffixEquivalent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		a, b string
		want bool
	}{
		{"main.go", "main.go", true},
		{"/w/main.go", "main.go", true},
		{"main.go", "/w/main.go", true},
		{"/w/pkg/a.go", "pkg/a.go", true},
		{"/w/xmain.go", "main.go", false},
		{"a/doc.go", "b/doc.go", false},
		{"", "main.go", false},
		{".", "main.go", false},
	}
	for _, tc := range tests {
		if got := pathSuffixEquivalent(tc.a, tc.b); got != tc.want {
			t.Errorf("pathSuffixEquivalent(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}
