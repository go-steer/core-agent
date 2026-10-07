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
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// FileSet is a set of credential files, matched by path and by inode
// (#1201). It is the file-shaped counterpart of the withheld env set:
// files the daemon reads a caller credential from, which nothing the
// agent drives may read.
//
// A path matches an entry when its lexical absolute form equals the
// entry's (or the entry's symlink-resolved form at Add), or when it
// names the same file the entry names NOW: os.Stat on both (which
// follows symlinks) and os.SameFile. Comparing the current inode is what
// keeps a Kubernetes Secret volume covered after an update: kubelet
// re-points users.json's symlink chain at a fresh directory, so any path
// or inode recorded at boot goes stale, and a caller that resolves
// symlinks before asking — as the file tools do — would otherwise walk
// straight past. It also catches a symlink or a hard link to the entry
// under another name, with no symlink resolution of its own.
//
// No inode is remembered from Add. A remembered inode outlives its file:
// once the entry is replaced or deleted, the kernel reuses the number,
// and an unrelated new file would be refused. (Observed in this
// package's own test run before it was removed.)
//
// The zero value is an empty, usable set. Safe for concurrent use.
type FileSet struct {
	mu    sync.RWMutex
	files []heldFile
}

type heldFile struct {
	abs      string
	resolved string // symlink-resolved at Add; abs when unresolvable
}

// Add puts paths in the set. Empty paths are ignored, so callers can
// pass optional config fields straight through; relative paths resolve
// against the process working directory, the one the daemon opens them
// from. There is no way to remove a path.
func (s *FileSet) Add(paths ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range paths {
		if p == "" {
			continue
		}
		abs, err := filepath.Abs(p)
		if err != nil {
			continue
		}
		if s.hasLocked(abs) {
			continue
		}
		h := heldFile{abs: abs, resolved: abs}
		if r, err := resolveLenient(abs); err == nil {
			h.resolved = r
		}
		s.files = append(s.files, h)
	}
}

func (s *FileSet) hasLocked(abs string) bool {
	for _, f := range s.files {
		if f.abs == abs {
			return true
		}
	}
	return false
}

// Paths returns the entries' absolute paths, sorted.
func (s *FileSet) Paths() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.files))
	for _, f := range s.files {
		out = append(out, f.abs)
	}
	sort.Strings(out)
	return out
}

// Spellings returns, for each entry, the ways a shell command run in wd
// can name it directly: its absolute path, its symlink-resolved path,
// and its path relative to wd when it sits under wd. Keyed by the
// entry's absolute path.
func (s *FileSet) Spellings(wd string) map[string][]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string][]string, len(s.files))
	for _, f := range s.files {
		paths := []string{f.abs}
		if f.resolved != f.abs {
			paths = append(paths, f.resolved)
		}
		forms := append([]string(nil), paths...)
		for _, p := range paths {
			if wd == "" {
				break
			}
			rel, err := filepath.Rel(wd, p)
			if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				continue
			}
			forms = append(forms, rel)
		}
		out[f.abs] = forms
	}
	return out
}

// Match reports whether path names an entry, and which (by its absolute
// path). The file I/O happens outside the lock.
func (s *FileSet) Match(path string) (string, bool) {
	s.mu.RLock()
	files := make([]heldFile, len(s.files))
	copy(files, s.files)
	s.mu.RUnlock()
	if len(files) == 0 {
		return "", false
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", false
	}
	for _, f := range files {
		if abs == f.abs || abs == f.resolved {
			return f.abs, true
		}
	}
	info, err := os.Stat(abs)
	if err != nil {
		// The path names nothing yet — a write that would create the
		// file. No inode to compare, so compare where it WOULD land:
		// both sides resolved through their deepest existing ancestor,
		// the way permissions.ResolvePath hands paths to the gate. A
		// table that is absent at boot under a symlinked parent (macOS
		// /tmp, a symlinked $PWD) would otherwise let write_file create
		// it, yolo or not.
		want, rerr := resolveLenient(abs)
		if rerr != nil {
			return "", false
		}
		for _, f := range files {
			if now, err := resolveLenient(f.abs); err == nil && now == want {
				return f.abs, true
			}
		}
		return "", false
	}
	for _, f := range files {
		if now, err := os.Stat(f.abs); err == nil && os.SameFile(info, now) {
			return f.abs, true
		}
	}
	return "", false
}

// resolveLenient resolves symlinks in abs through its deepest existing
// ancestor, re-appending the non-existent tail. It is the algorithm of
// permissions.ResolvePath, which calls it: the gate and this matcher
// must agree on where a not-yet-existing path lands, so there is one
// copy, here, because pkg/permissions imports this package.
//
// Not handled: a case-insensitive filesystem for a file that does not
// exist yet. An existing file is matched by inode whatever its case; an
// absent one is compared as a string, so "USERS.json" in the same
// directory is not refused on macOS until the table exists.
func resolveLenient(abs string) (string, error) {
	abs = filepath.Clean(abs)
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
			return "", err
		}
		remainder = filepath.Join(filepath.Base(cur), remainder)
		cur = parent
	}
}

// ResolveLenient is resolveLenient for pkg/permissions' ResolvePath.
func ResolveLenient(abs string) (string, error) { return resolveLenient(abs) }

// daemonFiles is this process's credential files, for readers that hold
// no permission gate — the instruction loader's @include, which would
// otherwise splice the bearer table into a session's system prompt on
// the strength of a single write_file to AGENTS.md.
var daemonFiles FileSet

// WithholdFiles adds paths to the process-wide credential-file set.
func WithholdFiles(paths ...string) { daemonFiles.Add(paths...) }

// WithheldFile reports whether path names a file in the process-wide
// credential-file set.
func WithheldFile(path string) bool {
	_, ok := daemonFiles.Match(path)
	return ok
}

// WithheldFiles returns the process-wide credential files, sorted.
func WithheldFiles() []string { return daemonFiles.Paths() }
