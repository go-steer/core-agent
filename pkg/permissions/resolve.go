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
	"errors"
	"io/fs"
	"path/filepath"
)

// ResolvePath returns the absolute, cleaned, symlink-resolved form of
// path. This is the canonical form every scope/access check compares
// against — checking the lexical path while the OS follows symlinks
// would let an in-scope symlink alias an out-of-scope target (#374).
//
// Paths that don't exist yet (new-file writes) are handled by
// resolving the deepest existing ancestor directory and rejoining the
// non-existing remainder, so `write_file` into a fresh subdirectory
// still classifies against the directory's real location.
//
// Any resolution error other than "does not exist" (permission
// denied on an ancestor, symlink loops, ...) is returned to the
// caller, which is expected to fail closed — treat the path as
// out-of-scope rather than fall back to the lexical form.
func ResolvePath(path string) (string, error) {
	abs, err := filepath.Abs(expandUser(path))
	if err != nil {
		return "", err
	}
	return resolveSymlinks(filepath.Clean(abs))
}

// resolveSymlinks is the EvalSymlinks core of ResolvePath: abs must
// already be absolute and cleaned. Walks up past non-existing path
// components so a not-yet-created tail doesn't defeat resolution of
// the (existing, possibly symlinked) ancestor chain.
func resolveSymlinks(abs string) (string, error) {
	remainder := ""
	cur := abs
	for {
		resolved, err := filepath.EvalSymlinks(cur)
		if err == nil {
			if remainder == "" {
				return resolved, nil
			}
			return filepath.Join(resolved, remainder), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			// Walked to the filesystem root without finding an
			// existing ancestor. Shouldn't happen ("/" exists), but
			// fail closed rather than guess.
			return "", err
		}
		remainder = filepath.Join(filepath.Base(cur), remainder)
		cur = parent
	}
}
