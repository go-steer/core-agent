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

package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// forbiddenInGitDir are files that make git read, or run against, a path
// outside the copy. None exists in a fresh clone, and the agent's work —
// edits and commits — never creates one. alternates, http-alternates and
// grafts are read as lists of paths or object names and their content is
// echoed on a parse error; commondir and gitdir redirect the repository
// itself.
var forbiddenInGitDir = []string{
	"objects/info/alternates",
	"objects/info/http-alternates",
	"info/grafts",
	"commondir",
	"gitdir",
}

// maxConfigBytes bounds the read of the copy's config for the include
// check. A real one is a few hundred bytes.
const maxConfigBytes = 64 << 10

// inspectCopy refuses a working copy whose .git could make the fetch read
// a file outside the copy. It is defence in depth behind runQuiet, which
// already keeps git's output (and so any echoed file content) out of every
// message: an agent that reshaped its .git this way has also told us
// something, and the refusal names what it found.
//
// The rules: .git is a real directory (not a gitfile, not a symlink);
// nothing under it is a symlink or anything but a directory or a regular
// file; none of forbiddenInGitDir exists; the config has no include
// section; and the shallow file names only the base the clone was made at.
// Messages carry paths relative to .git, never file content.
func inspectCopy(dir, base string) error {
	gitDir := filepath.Join(dir, ".git")
	fi, err := os.Lstat(gitDir) // #nosec G703 -- the copy the dispatcher made under --worktrees-dir.
	if err != nil {
		return errors.New(".git is missing")
	}
	if !fi.IsDir() {
		return errors.New(".git is not a directory (a gitfile or a symlink)")
	}
	walkErr := filepath.WalkDir(gitDir, func(path string, d fs.DirEntry, err error) error { // #nosec G703 -- the copy the dispatcher made under --worktrees-dir.
		if err != nil {
			return errors.New("part of .git could not be read")
		}
		if t := d.Type(); t&fs.ModeSymlink != 0 || (!t.IsDir() && !t.IsRegular()) {
			rel, _ := filepath.Rel(gitDir, path)
			return fmt.Errorf(".git/%s is a symlink or a special file", truncate(rel, 200))
		}
		return nil
	})
	if walkErr != nil {
		return walkErr
	}
	for _, rel := range forbiddenInGitDir {
		if _, err := os.Lstat(filepath.Join(gitDir, rel)); err == nil { // #nosec G703 -- fixed names under the copy.
			return fmt.Errorf(".git/%s exists; a fresh clone has none and committing never makes one", rel)
		}
	}
	if err := checkConfig(filepath.Join(gitDir, "config"), false); err != nil {
		return err
	}
	if err := checkConfig(filepath.Join(gitDir, "config.worktree"), true); err != nil {
		return err
	}
	return checkShallow(filepath.Join(gitDir, "shallow"), base)
}

// forbiddenInConfig are config fragments a fresh clone never has, matched
// case-insensitively on the config with all whitespace removed (so
// `[ Include ]` or `partialClone = x` can't dodge the match). Matching
// text rather than parsing errs towards refusing: a value that merely
// mentions one of these words refuses the copy too, which only costs a
// stop.
//
//   - "[include": include / includeIf pull in a file outside the copy.
//   - "partialclone", "promisor": a partial clone lazily fetches missing
//     objects from its promisor remote, running that remote's
//     `uploadpack` command (GIT_NO_LAZY_FETCH in gitEnv is the other
//     half of this).
//   - "uploadpack", "receivepack": per-remote commands git would run.
//   - "worktreeconfig": makes git read config.worktree as well; that file
//     is also checked, but nothing the agent does needs it.
var forbiddenInConfig = []string{"[include", "partialclone", "promisor", "uploadpack", "receivepack", "worktreeconfig"}

// checkConfig applies forbiddenInConfig to a config file. The whole file
// is read: a config larger than maxConfigBytes is refused rather than
// checked in part, since an include after the bound would otherwise pass.
// A missing file is fine only when allowMissing.
func checkConfig(path string, allowMissing bool) error {
	name := ".git/" + filepath.Base(path)
	raw, err := readBounded(path, maxConfigBytes+1)
	if allowMissing && errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%s could not be read", name)
	}
	if len(raw) > maxConfigBytes {
		return fmt.Errorf("%s is larger than %d bytes", name, maxConfigBytes)
	}
	flat := strings.ToLower(strings.Join(strings.Fields(raw), ""))
	for _, f := range forbiddenInConfig {
		if strings.Contains(flat, f) {
			return fmt.Errorf("%s sets %q, which a fresh clone never has", name, strings.TrimPrefix(f, "["))
		}
	}
	return nil
}

// checkShallow allows only the shallow root the depth-1 clone was made
// with. An agent that appends its own tip asks the fetch to cut history
// at that commit, hiding whatever came before it from the identity check.
func checkShallow(path, base string) error {
	raw, err := readBounded(path, 4096)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errors.New(".git/shallow could not be read")
	}
	for _, line := range strings.Fields(raw) {
		if line != base {
			return errors.New(".git/shallow names a commit other than the base")
		}
	}
	return nil
}

func readBounded(path string, limit int64) (string, error) {
	f, err := openNoFollow(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, limit))
	return string(raw), err
}

// truncate shortens s to at most n bytes, on a rune boundary, with a
// marker.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "") + " [truncated]"
}
