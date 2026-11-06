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
	"context"
	"path/filepath"
	"slices"
	"strings"
)

type workDirKey struct{}
type isolatedWorktreeKey struct{}

// WithWorkDir attaches a per-invocation working directory to ctx so
// built-in file and shell tools resolve relative paths and command
// execution against dir without mutating the process-wide cwd (#653).
// Empty dir is a no-op.
func WithWorkDir(ctx context.Context, dir string) context.Context {
	dir = expandUser(strings.TrimSpace(dir))
	if dir == "" {
		return ctx
	}
	if !filepath.IsAbs(dir) {
		if base := WorkDirFromContext(ctx); base != "" {
			dir = filepath.Join(base, dir)
		} else if abs, err := filepath.Abs(dir); err == nil {
			dir = abs
		}
	}
	dir = filepath.Clean(dir)
	return context.WithValue(ctx, workDirKey{}, dir)
}

// WorkDirFromContext returns the per-invocation working directory
// previously attached via WithWorkDir or WithIsolatedWorktree, or ""
// when none is set on ctx.
func WorkDirFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	dir, _ := ctx.Value(workDirKey{}).(string)
	return dir
}

// WithIsolatedWorktree attaches an isolated git worktree directory to
// ctx as both the active working directory (WithWorkDir) and the
// write-isolation boundary (#653). While an isolated worktree is set on
// ctx, CheckFileRead / CheckFileWrite grant in-scope access inside
// worktreeDir and CheckFileWrite refuses writes that target the
// parent's project root outside worktreeDir.
func WithIsolatedWorktree(ctx context.Context, worktreeDir string) context.Context {
	if strings.TrimSpace(worktreeDir) == "" {
		return ctx
	}
	ctx = WithWorkDir(ctx, worktreeDir)
	return context.WithValue(ctx, isolatedWorktreeKey{}, WorkDirFromContext(ctx))
}

func resolveAgainstWorkDir(ctx context.Context, path string) string {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" || strings.HasPrefix(trimmed, "~") || filepath.IsAbs(trimmed) {
		return path
	}
	if wd := WorkDirFromContext(ctx); wd != "" {
		return filepath.Join(wd, trimmed)
	}
	return path
}

// IsolatedWorktreeFromContext returns the isolated worktree directory
// previously attached via WithIsolatedWorktree, or "" when none is set.
func IsolatedWorktreeFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	dir, _ := ctx.Value(isolatedWorktreeKey{}).(string)
	return dir
}

func isInsideWorktree(absPath, worktreeDir string) bool {
	if worktreeDir == "" {
		return false
	}
	if slices.Contains(splitPathComponents(absPath), userCoreAgentDirName) {
		return false
	}
	if isInside(absPath, worktreeDir) {
		return true
	}
	if resolved, err := ResolvePath(worktreeDir); err == nil && resolved != worktreeDir {
		return isInside(absPath, resolved)
	}
	return false
}
