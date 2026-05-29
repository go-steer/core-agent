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
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Access is a bitmask of file operations a PathScope entry grants.
// Splitting read from write lets the allow-list say "agent may read
// this tree but writes still prompt" — closer to what an operator
// usually wants when granting access to a sibling repo. Operations
// outside file I/O (bash, generic tool calls) are gated separately
// via Policy and don't consult Access.
type Access uint8

const (
	AccessNone      Access = 0
	AccessRead      Access = 1 << 0
	AccessWrite     Access = 1 << 1
	AccessReadWrite        = AccessRead | AccessWrite
)

// Allows reports whether a carries every bit in op. Designed to be
// called with one of AccessRead / AccessWrite (single-bit checks)
// from the gate, though the bitmask semantics generalize.
func (a Access) Allows(op Access) bool { return a&op == op }

// String renders Access in the short form the config + CLI accept
// (`r`, `w`, `rw`) so logs / errors round-trip with ParseAccess.
func (a Access) String() string {
	switch a {
	case AccessNone:
		return "none"
	case AccessRead:
		return "r"
	case AccessWrite:
		return "w"
	case AccessReadWrite:
		return "rw"
	default:
		return fmt.Sprintf("access(%d)", a)
	}
}

// ParseAccess parses the access spec used by --allow-path and the
// config file's allow_paths entries. Accepts the short forms (r, w,
// rw) and the long forms (read, write, readwrite / read+write) so
// operators can use whichever reads better in their config. Case
// insensitive. Returns an error for any other input — silent fallback
// to a permissive default would hide typos that quietly broaden
// access.
func ParseAccess(s string) (Access, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "r", "read":
		return AccessRead, nil
	case "w", "write":
		return AccessWrite, nil
	case "rw", "wr", "read+write", "readwrite":
		return AccessReadWrite, nil
	case "":
		return AccessNone, fmt.Errorf("access spec is required (use r, w, or rw)")
	default:
		return AccessNone, fmt.Errorf("unknown access spec %q (want r, w, or rw)", s)
	}
}

// pathEntry pairs an allowlist pattern with the access level it
// grants. Internal-only — config + CLI shapes translate into this
// form when constructing a PathScope.
type pathEntry struct {
	Pattern string // absolute, expanded path or "/.../" subtree
	Access  Access
}

// PathScope restricts file tool access to a defined set of paths:
// the project root, the user-home root, and any explicit pattern in
// path_scope.allow / path_scope.allow_paths. Out-of-scope access
// escalates to a prompt (in interactive mode) or fails immediately
// (in headless without a prompter).
//
// Patterns supported in the allow list:
//   - Exact absolute paths.
//   - Directory trees ending with `/...` (e.g. "/etc/myapp/...").
//   - Standard filepath glob patterns (passed through path/filepath.Match).
//
// Each entry carries its own Access level (r / w / rw). The legacy
// allow-list (string slice) maps to AccessReadWrite for backward
// compatibility — anyone who already wrote to that list expected
// the agent to be able to do anything inside the entry.
type PathScope struct {
	roots []string    // absolute, cleaned; always full-access
	allow []pathEntry // absolute, cleaned; "/..." subtrees expanded as prefixes
}

// NewPathScope constructs a scope from the project root, the user-
// global home, and an extra allowlist of patterns. Every entry in
// allow gets AccessReadWrite — the legacy semantics. Use
// NewPathScopeFromEntries when callers need per-entry access levels.
//
// projectRoot may be empty, in which case only the user root and
// allowlist apply. userRoot may be empty for tests.
func NewPathScope(projectRoot, userRoot string, allow []string) (*PathScope, error) {
	entries := make([]pathEntry, 0, len(allow))
	for _, p := range allow {
		entries = append(entries, pathEntry{Pattern: p, Access: AccessReadWrite})
	}
	return NewPathScopeFromEntries(projectRoot, userRoot, entries)
}

// NewPathScopeFromEntries is the access-aware constructor. Each
// entry's access is preserved verbatim; callers (FromConfig, tests)
// build the slice from whatever shape their input has — typed
// AllowPaths entries, parsed --allow-path flags, etc.
func NewPathScopeFromEntries(projectRoot, userRoot string, entries []pathEntry) (*PathScope, error) {
	s := &PathScope{}
	for _, r := range []string{projectRoot, userRoot} {
		if r == "" {
			continue
		}
		abs, err := filepath.Abs(r)
		if err != nil {
			return nil, fmt.Errorf("path scope: %w", err)
		}
		s.roots = append(s.roots, filepath.Clean(abs))
	}
	for _, e := range entries {
		if e.Pattern == "" {
			continue
		}
		s.allow = append(s.allow, pathEntry{
			Pattern: expandUser(e.Pattern),
			Access:  e.Access,
		})
	}
	return s, nil
}

// Roots returns a copy of the configured scope roots.
func (s *PathScope) Roots() []string {
	out := make([]string, len(s.roots))
	copy(out, s.roots)
	return out
}

// AllowList returns a copy of the configured allowlist patterns.
// Access levels are dropped; callers needing the per-pattern level
// should reach for AccessFor on a specific path instead. Kept on
// the surface so debug commands / snapshot serializers that
// pre-dated Access still work.
func (s *PathScope) AllowList() []string {
	out := make([]string, 0, len(s.allow))
	for _, e := range s.allow {
		out = append(out, e.Pattern)
	}
	return out
}

// AccessFor returns the access level granted for path. Paths inside
// any configured root yield AccessReadWrite (roots are trusted by
// definition). Otherwise the allow-list is scanned and the
// longest-prefix match wins — a narrower entry can carve a more-
// permissive exception inside a less-permissive parent. Returns
// AccessNone when nothing covers the path.
//
// The path is resolved to an absolute, cleaned form before
// comparison; symlinks are not followed (we trust the input).
func (s *PathScope) AccessFor(path string) (Access, error) {
	abs, err := filepath.Abs(expandUser(path))
	if err != nil {
		return AccessNone, fmt.Errorf("path scope: %w", err)
	}
	abs = filepath.Clean(abs)

	for _, root := range s.roots {
		if isInside(abs, root) {
			return AccessReadWrite, nil
		}
	}
	// Longest-prefix wins: track the best match's pattern length so a
	// narrower /a/b:rw beats a broader /a:r when resolving /a/b/foo.
	best := AccessNone
	bestLen := -1
	for _, e := range s.allow {
		if !matchesPattern(abs, e.Pattern) {
			continue
		}
		l := matchSpecificity(e.Pattern)
		if l > bestLen {
			best = e.Access
			bestLen = l
		}
	}
	return best, nil
}

// Contains reports whether path is in scope at all (any access).
// Preserved for snapshot / serializer callers that don't care about
// per-op access. Equivalent to AccessFor(path) != AccessNone.
func (s *PathScope) Contains(path string) (bool, error) {
	access, err := s.AccessFor(path)
	if err != nil {
		return false, err
	}
	return access != AccessNone, nil
}

// AddAlwaysAllow appends a pattern to the in-memory allowlist with
// the given access level. The caller is responsible for persisting
// the change to config.json. Used by the gate when an interactive
// prompt resolves to DecisionAllowAlways — the access bit reflects
// the op the prompt was for, not a blanket rw grant.
func (s *PathScope) AddAlwaysAllow(pattern string, access Access) {
	s.allow = append(s.allow, pathEntry{
		Pattern: expandUser(pattern),
		Access:  access,
	})
}

// matchSpecificity returns a comparable "how specific is this
// pattern" score so longest-prefix-wins selection works for both
// "/a/b/..." subtrees and exact paths. Globs return their static
// prefix length (anything past the first `*` is wildcard, doesn't
// add specificity).
func matchSpecificity(pattern string) int {
	p := strings.TrimSuffix(pattern, "/...")
	if i := strings.IndexAny(p, "*?["); i >= 0 {
		return i
	}
	return len(p)
}

func expandUser(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(p, "~"), "/"))
		}
	}
	return p
}

func isInside(path, root string) bool {
	if path == root {
		return true
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return !strings.HasPrefix(rel, "..") && rel != ".."
}

func matchesPattern(path, pattern string) bool {
	if strings.HasSuffix(pattern, "/...") {
		root := strings.TrimSuffix(pattern, "/...")
		return path == root || isInside(path, root)
	}
	if ok, _ := filepath.Match(pattern, path); ok {
		return true
	}
	return path == pattern
}
