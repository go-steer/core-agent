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

package skills

import (
	"errors"
	"io/fs"
)

// overlayFS composes two fs.FS instances so the skilltoolset sees a
// single virtual root with primary entries shadowing fallback ones.
//
// Lookup semantics:
//
//   - Open(name): try primary first; on fs.ErrNotExist fall through
//     to fallback. Any other error from primary bubbles up.
//   - ReadDir(name): merge entries from both, primary wins on name
//     collision so a project-scoped skill named "cli-setup" hides
//     the user-global one of the same name.
//
// Designed for the project-scoped vs user-global skill discovery
// case (Load + LoadAll) — small two-source overlay, not a general-
// purpose union-mount.
type overlayFS struct {
	primary, fallback fs.FS
}

// Open implements fs.FS.
func (o *overlayFS) Open(name string) (fs.File, error) {
	f, err := o.primary.Open(name)
	if err == nil {
		return f, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	return o.fallback.Open(name)
}

// ReadDir implements fs.ReadDirFS. The skilltoolset's filesystem
// source calls fs.ReadDir(rootFS, ".") to enumerate top-level skill
// directories; if the FS implements ReadDirFS the call uses this
// method directly instead of opening + casting.
//
// Entries from primary appear first (alphabetically); fallback
// entries with names not in primary are appended at the end. The
// frontmatter parser doesn't care about order, but keeping primary
// first matches the "project wins" intuition for any future
// position-sensitive caller.
func (o *overlayFS) ReadDir(name string) ([]fs.DirEntry, error) {
	primary, primaryErr := fs.ReadDir(o.primary, name)
	fallback, fallbackErr := fs.ReadDir(o.fallback, name)

	// Both failed → return primary's error (more informative for the
	// "missing directory" case which is what we expect).
	if primaryErr != nil && fallbackErr != nil {
		return nil, primaryErr
	}

	// Only one succeeded → use it directly.
	if primaryErr != nil {
		return fallback, nil
	}
	if fallbackErr != nil {
		return primary, nil
	}

	// Both succeeded → merge. Primary entries first; fallback
	// entries with names not in primary appended after.
	seen := make(map[string]bool, len(primary))
	for _, e := range primary {
		seen[e.Name()] = true
	}
	merged := append([]fs.DirEntry(nil), primary...)
	for _, e := range fallback {
		if !seen[e.Name()] {
			merged = append(merged, e)
		}
	}
	return merged, nil
}
