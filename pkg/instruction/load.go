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

// Package instruction loads project + user "agent memory" files (typically
// AGENTS.md) into the system prompt.
//
// Loader v2 (docs/instruction-loader-v2-design.md) supports two
// composition primitives on top of the v1 single-file shape:
//
//   - @include <relative-path> directive: a line that matches the
//     pattern is replaced in-place by the referenced file's content.
//     Outside fenced code blocks only. Cycle + depth capped.
//   - .agents/AGENTS.d/*.md directory: every top-level .md file is
//     loaded in lexical filename order, appended after the scope's
//     primary file.
//
// Both primitives work at user-global and project scope. A per-Load
// canonical-path visited set deduplicates: the same file reached via
// any path is read once and the first encounter wins.
//
// The project scope's primary entry is resolved via the first-match-
// wins fallback chain AGENTS.md → CLAUDE.md → GEMINI.md (legacy
// compatibility). The user-global scope reads only AGENTS.md.
package instruction

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// maxFileBytes caps how much of any single memory file is loaded.
// 32 KiB keeps a sprawling memory file from eating most of the
// context window before the conversation starts.
const maxFileBytes = 32 * 1024

// maxIncludeDepth caps how deeply @include can nest. A depth greater
// than this errors with "include depth exceeded". The cap exists to
// fail-fast on accidentally-deep chains; real-world include trees
// rarely exceed 2–3 levels.
const maxIncludeDepth = 8

// agentsDirName is the directory under each scope root whose top-level
// *.md files get loaded after the primary file. Linux conf.d style.
const agentsDirName = "AGENTS.d"

// Project memory filename fallback chain. The first file that exists
// becomes the scope's primary entry; the rest are ignored.
var projectMemoryNames = []string{"AGENTS.md", "CLAUDE.md", "GEMINI.md"}

// userMemoryName is the only filename read from the user-global root
// for the primary entry. The directory primitive (AGENTS.d/) is
// supported at both scopes.
const userMemoryName = "AGENTS.md"

// Source records where one piece of loaded memory came from. Used by
// callers (e.g. a /memory slash command) to show provenance. Every
// file loaded — primary, transitively included, or AGENTS.d/ scanned —
// produces one Source entry.
type Source struct {
	Scope     string // "user" | "project"
	Path      string // canonical absolute path
	Bytes     int    // bytes after truncation
	Truncated bool   // true if the on-disk file exceeded maxFileBytes
}

// Loaded is the result of a Load call. Instruction is the assembled
// text suitable for prepending to the agent's system prompt; Sources
// describes what got included (one entry per loaded file); Searched
// lists the primary-file paths that were probed during resolution
// (regardless of whether a file existed at that path). Callers can
// use Searched to produce actionable "no AGENTS.md found — checked
// [...]" diagnostics when Sources is empty; without this the
// operator has no visible signal that the load found nothing (the
// daemon happily runs with an empty system prompt).
type Loaded struct {
	Instruction string
	Sources     []Source
	Searched    []string
}

// Empty reports whether nothing was loaded.
func (l Loaded) Empty() bool { return l.Instruction == "" }

// Option configures a Load / LoadForSession call. All options are
// optional; the zero-options call matches the pre-#322 loader
// behavior exactly.
type Option func(*loadOptions)

type loadOptions struct {
	// interp is applied to each raw file body after UTF-8 validation
	// and before frontmatter stripping. Nil = no interpolation
	// (identity function semantics). Wire from pkg/agentenv via
	// (*agentenv.Resolver).InterpolateFunc().
	interp func(string) string

	// homeAgentsRoot is an additional user-scope root (typically
	// $HOME/.agents/) loaded between the userRoot and projectRoot
	// scopes. Empty = skip. Unlike userRoot / projectRoot, this
	// scope loads via loadScope directly — the root IS already an
	// .agents/ dir, so descending into a nested .agents/ subdir
	// would be nonsensical.
	homeAgentsRoot string
}

// WithInterpolator supplies a string transform applied to every loaded
// file body — used to substitute ${env:VAR} references declared in
// .agents/env.yaml (see pkg/agentenv). Passing nil is legal and equals
// "no interpolation."
func WithInterpolator(fn func(string) string) Option {
	return func(o *loadOptions) { o.interp = fn }
}

// WithHomeAgentsRoot supplies the portable user-scope agents root
// (typically $HOME/.agents/) as a scope loaded between userRoot and
// projectRoot. Its AGENTS.md and AGENTS.d/*.md concatenate into the
// system prompt with scope="user-home"; the visited-set dedupes
// against every other scope. Empty is legal and equals "no
// home-agents scope."
func WithHomeAgentsRoot(dir string) Option {
	return func(o *loadOptions) { o.homeAgentsRoot = dir }
}

// Load resolves the project + user memory files and returns the
// concatenated instruction text. Missing files at primary slots are
// not errors — memory is optional. A missing @include target IS an
// error so operator typos surface immediately.
//
// projectRoot may be empty; in that case only user memory is loaded.
// userRoot may be empty in tests.
func Load(projectRoot, userRoot string, opts ...Option) (Loaded, error) {
	return LoadForSession(projectRoot, userRoot, "", "", opts...)
}

// LoadForSession extends Load with an optional per-caller overlay
// scope used by the multi-session attach layer. It walks (in order):
//
//  1. userRoot/.agents/ + userRoot/ — the daemon-wide user-scope
//     memory (same as Load).
//  2. projectRoot/.agents/ + projectRoot/ — the project-scope memory
//     (same as Load).
//  3. <usersDir>/<callerIdentity>/.agents/ — the per-caller overlay
//     (NEW; multi-session only).
//
// Per-caller overlay rules:
//   - usersDir == "" OR callerIdentity == "" → no overlay applied;
//     behaves identically to Load. This is the single-user / pre-
//     multi-session path.
//   - callerIdentity containing path separators or ".." → returns
//     ErrInvalidCallerIdentity. Defense against a malicious identity
//     (e.g. "../../etc") traversing out of usersDir.
//   - <usersDir>/<callerIdentity>/ doesn't exist → silently skip.
//     Operators may provision overlays for some callers and not
//     others; a missing directory is a legitimate "no overlay" signal.
//
// The visited-set propagates across all three scopes — a file
// referenced by both the project layer and the caller overlay loads
// exactly once, preserving the dedup semantics of Load.
func LoadForSession(projectRoot, userRoot, callerIdentity, usersDir string, opts ...Option) (Loaded, error) {
	var lo loadOptions
	for _, o := range opts {
		o(&lo)
	}

	var loaded Loaded
	var b strings.Builder
	// Single visited set across all scopes — a file reached from
	// any of user / project / caller-overlay counts as the first
	// encounter, subsequent references skip silently. This is what
	// makes the @include-vs-AGENTS.d/ double-load case (and the
	// cross-scope cycle case) safe by construction.
	visited := make(map[string]bool)

	if userRoot != "" {
		if err := loadScopeWithFallback(userRoot, "user", []string{userMemoryName}, &b, &loaded.Sources, &loaded.Searched, visited, lo.interp); err != nil {
			return loaded, err
		}
	}

	if lo.homeAgentsRoot != "" {
		// loadScope (not loadScopeWithFallback): the home-agents root
		// IS already an .agents/ dir, so descending into <root>/.agents/
		// would look for $HOME/.agents/.agents/AGENTS.md, which is
		// nonsensical. Load the primary + AGENTS.d/ directly at the root.
		if info, err := os.Stat(lo.homeAgentsRoot); err == nil && info.IsDir() {
			if err := loadScope(lo.homeAgentsRoot, "user-home", []string{userMemoryName}, &b, &loaded.Sources, &loaded.Searched, visited, lo.interp); err != nil {
				return loaded, err
			}
		} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return loaded, fmt.Errorf("instruction: stat home-agents dir %q: %w", lo.homeAgentsRoot, err)
		}
	}

	if projectRoot != "" {
		if err := loadScopeWithFallback(projectRoot, "project", projectMemoryNames, &b, &loaded.Sources, &loaded.Searched, visited, lo.interp); err != nil {
			return loaded, err
		}
	}

	if usersDir != "" && callerIdentity != "" {
		if err := validateCallerIdentity(callerIdentity); err != nil {
			return loaded, err
		}
		overlayRoot := filepath.Join(usersDir, callerIdentity)
		if info, err := os.Stat(overlayRoot); err == nil && info.IsDir() {
			if err := loadScopeWithFallback(overlayRoot, "caller", []string{userMemoryName}, &b, &loaded.Sources, &loaded.Searched, visited, lo.interp); err != nil {
				return loaded, err
			}
		} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
			// stat failed for a reason other than missing — surface so
			// operator catches e.g. a permissions misconfig immediately
			// instead of silently losing the overlay.
			return loaded, fmt.Errorf("instruction: stat overlay dir %q: %w", overlayRoot, err)
		}
	}

	loaded.Instruction = b.String()
	return loaded, nil
}

// ErrInvalidCallerIdentity is returned by LoadForSession when the
// callerIdentity argument contains characters that could traverse out
// of usersDir (path separators or ".."). Operators see this as a
// validation error at agent construction time rather than a silent
// load from an unexpected directory.
var ErrInvalidCallerIdentity = errors.New("instruction: caller identity contains path-traversal characters")

// validateCallerIdentity rejects identities that would let a crafted
// Caller traverse out of usersDir. Email-shaped ("alice@example.com")
// and service-account-marker ("sa:slack-bot") identities pass;
// anything containing /, \, or ".." (anywhere — not just leading)
// fails loudly.
func validateCallerIdentity(id string) error {
	if id == "" {
		return nil
	}
	if strings.ContainsAny(id, `/\`) {
		return fmt.Errorf("%w: %q contains a path separator", ErrInvalidCallerIdentity, id)
	}
	if id == ".." || strings.Contains(id, "..") {
		return fmt.Errorf("%w: %q contains %q", ErrInvalidCallerIdentity, id, "..")
	}
	return nil
}

// loadScopeWithFallback runs loadScope twice per scope: first against
// `<root>/.agents/` if that directory exists (the operator's
// preferred "everything agent stuff under .agents/" convention),
// then against `<root>/` itself (the broader-ecosystem convention
// where AGENTS.md lives at the project root, matching Cursor /
// Antigravity / Hermes layouts).
//
// Both load when both exist — operators who legitimately use both
// (e.g., root AGENTS.md as the cross-tool canonical document plus
// .agents/AGENTS.md as core-agent-specific additions) get the union.
// The canonical-path visited-set in the underlying loadFile
// guarantees no file loads twice even if reachable from both.
//
// Order: .agents/ first, then root. Operators who only put files in
// one location see no behavior difference; operators with both get
// .agents/ content prepended.
func loadScopeWithFallback(root, scope string, primaryNames []string, b *strings.Builder, sources *[]Source, searched *[]string, visited map[string]bool, interp func(string) string) error {
	subDir := filepath.Join(root, ".agents")
	if info, err := os.Stat(subDir); err == nil && info.IsDir() {
		if err := loadScope(subDir, scope, primaryNames, b, sources, searched, visited, interp); err != nil {
			return err
		}
	}
	return loadScope(root, scope, primaryNames, b, sources, searched, visited, interp)
}

// loadScope drives one scope's primary-file + AGENTS.d/ load. The
// primary fallback chain is per-scope (just AGENTS.md for user; full
// chain for project). The AGENTS.d/ scan is identical at both scopes.
func loadScope(scopeRoot, scope string, primaryNames []string, b *strings.Builder, sources *[]Source, searched *[]string, visited map[string]bool, interp func(string) string) error {
	// Primary file via fallback chain. First-match-wins.
	for _, name := range primaryNames {
		path := filepath.Join(scopeRoot, name)
		if searched != nil {
			*searched = append(*searched, path)
		}
		if _, err := os.Stat(path); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return fmt.Errorf("instruction: stat %s: %w", path, err)
		}
		body, err := loadFile(path, scope, scopeRoot, 0, visited, sources, interp)
		if err != nil {
			return err
		}
		if body != "" {
			appendBlock(b, scope, mustCanonical(path), body)
		}
		break // first match wins
	}

	// AGENTS.d/ directory scan. Non-existent directory is fine.
	dirPath := filepath.Join(scopeRoot, agentsDirName)
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("instruction: read %s: %w", dirPath, err)
	}
	var mdFiles []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		// Skip hidden files (operator-staging convention).
		if strings.HasPrefix(name, ".") {
			continue
		}
		// .md only — other files (READMEs, scripts) are ignored.
		if !strings.HasSuffix(name, ".md") {
			continue
		}
		mdFiles = append(mdFiles, name)
	}
	sort.Strings(mdFiles) // lexical order; operator picks the prefix convention
	for _, name := range mdFiles {
		path := filepath.Join(dirPath, name)
		body, err := loadFile(path, scope, scopeRoot, 0, visited, sources, interp)
		if err != nil {
			return err
		}
		if body != "" {
			appendBlock(b, scope, mustCanonical(path), body)
		}
	}
	return nil
}

// appendBlock writes the per-file header + body to the assembly
// buffer, with a blank-line separator between blocks. Empty body
// (a deduped file or one that contained only stripped frontmatter)
// is the caller's responsibility to filter — we don't write a
// header for nothing.
func appendBlock(b *strings.Builder, scope, path, body string) {
	if b.Len() > 0 {
		b.WriteByte('\n')
	}
	b.WriteString("# ")
	b.WriteString(scopeLabel(scope))
	b.WriteString(" memory (")
	b.WriteString(path)
	b.WriteString(")\n")
	b.WriteString(body)
	if !strings.HasSuffix(body, "\n") {
		b.WriteByte('\n')
	}
}

// scopeLabel renders the per-block header's scope word. Kept as a
// helper so future scopes (org, workspace, …) don't require changing
// the header format in two places.
func scopeLabel(scope string) string {
	switch scope {
	case "user":
		return "User"
	case "project":
		return "Project"
	default:
		// Unknown scope — should not happen with current callers.
		// Capitalize first byte without pulling in a Unicode caser
		// (English-only scope names by design).
		if scope == "" {
			return ""
		}
		return strings.ToUpper(scope[:1]) + scope[1:]
	}
}

// mustCanonical returns the canonical absolute path or, on error,
// the input unchanged. Used for header rendering where a sub-optimal
// path is preferred over a load failure.
func mustCanonical(p string) string {
	if abs, err := canonicalPath(p); err == nil {
		return abs
	}
	return p
}

// loadFile reads one file (primary, AGENTS.d/-scanned, or @included),
// applies dedup + frontmatter strip + UTF-8 validation + truncation,
// then recursively resolves any @include directives in its body.
//
// Returns the body to insert at the call site. An empty body means
// the file was a dedup-skip (already loaded) — caller should NOT
// emit a header/separator for it. Errors are fatal for the whole
// Load (a missing @include target, a path escape, depth exceeded,
// non-UTF-8 content) — operator typos and config bugs should surface
// loudly rather than silently truncating the system prompt.
func loadFile(path, scope, scopeRoot string, depth int, visited map[string]bool, sources *[]Source, interp func(string) string) (string, error) {
	if depth > maxIncludeDepth {
		return "", fmt.Errorf("instruction: include depth exceeded (max %d) at %s", maxIncludeDepth, path)
	}

	canonPath, err := canonicalPath(path)
	if err != nil {
		// Resolution failed (broken symlink, permission, missing
		// file at the top level). For an @include this is reached
		// only after validateIncludePath confirmed shape, so a stat
		// failure here is genuinely missing-on-disk.
		return "", fmt.Errorf("instruction: resolve %s: %w", path, err)
	}
	if visited[canonPath] {
		return "", nil // first-encounter-wins; subsequent silently skipped
	}
	visited[canonPath] = true

	raw, truncated, err := readUpTo(canonPath, maxFileBytes)
	if err != nil {
		return "", fmt.Errorf("instruction: read %s: %w", canonPath, err)
	}

	// UTF-8 validation. We're going to splice this into the system
	// prompt; a non-UTF-8 file would corrupt downstream encoding and
	// the operator almost certainly meant to point at a text file.
	if !isValidUTF8(raw) {
		return "", fmt.Errorf("instruction: %s contains invalid UTF-8", canonPath)
	}

	// Apply ${env:VAR} interpolation before frontmatter strip. Doing
	// it here (rather than after strip) keeps interpolation-vs-
	// frontmatter interactions simple: any ${env:VAR} inside YAML
	// frontmatter is legal and gets resolved. If nil (bundle without
	// an env.yaml manifest), pass through unchanged — full backwards
	// compat with pre-#322 bundles.
	rawStr := string(raw)
	if interp != nil {
		rawStr = interp(rawStr)
	}
	body := stripFrontmatter(rawStr)
	if truncated {
		// Visible-in-prompt marker so the agent and the operator
		// both know the on-disk file was bigger than the cap. The
		// system prompt is exactly where this needs to surface.
		if !strings.HasSuffix(body, "\n") {
			body += "\n"
		}
		body += "[...truncated by core-agent at " + strconv.Itoa(maxFileBytes) + " bytes...]\n"
	}

	// Record provenance. Note: we record BEFORE @include resolution
	// so the source list reflects the file we actually opened, not
	// the recursive expansion.
	*sources = append(*sources, Source{
		Scope:     scope,
		Path:      canonPath,
		Bytes:     len(raw),
		Truncated: truncated,
	})

	// Recurse: resolve @include directives in the body. The
	// containing file's directory is the base for relative paths.
	fileDir := filepath.Dir(canonPath)
	expanded, err := processIncludes(body, scope, fileDir, scopeRoot, depth+1, visited, sources, interp)
	if err != nil {
		return "", err
	}
	return expanded, nil
}

// processIncludes scans the body line-by-line, replacing each
// @include directive (outside fenced code blocks) with the
// referenced file's content. Recursion happens through loadFile,
// which calls back into processIncludes for the included body —
// so a chain A → B → C resolves correctly.
//
// Code fence tracking is necessary so an @include in a markdown
// example block stays literal rather than being expanded.
func processIncludes(body, scope, fileDir, scopeRoot string, depth int, visited map[string]bool, sources *[]Source, interp func(string) string) (string, error) {
	var out strings.Builder
	out.Grow(len(body))
	inFence := false
	lines := splitLinesKeepNewline(body)
	for _, line := range lines {
		if isCodeFenceMarker(line) {
			inFence = !inFence
			out.WriteString(line)
			continue
		}
		if !inFence {
			if rel, ok := parseIncludeLine(line); ok {
				if err := validateIncludePath(rel); err != nil {
					return "", fmt.Errorf("instruction: @include %q: %w", rel, err)
				}
				target := filepath.Join(fileDir, rel)
				if err := ensureWithinScope(target, scopeRoot); err != nil {
					return "", fmt.Errorf("instruction: @include %q: %w", rel, err)
				}
				if _, err := os.Stat(target); err != nil {
					if errors.Is(err, fs.ErrNotExist) {
						return "", fmt.Errorf("instruction: @include %q: file not found (resolved to %s)", rel, target)
					}
					return "", fmt.Errorf("instruction: @include %q: %w", rel, err)
				}
				included, err := loadFile(target, scope, scopeRoot, depth, visited, sources, interp)
				if err != nil {
					return "", err
				}
				// Empty `included` = dedup-skip; emit nothing in
				// place of the @include line. The line itself is
				// consumed either way.
				out.WriteString(included)
				if included != "" && !strings.HasSuffix(included, "\n") {
					out.WriteByte('\n')
				}
				continue
			}
		}
		out.WriteString(line)
	}
	return out.String(), nil
}

// parseIncludeLine returns (rel, true) when line matches the
// @include directive shape: leading whitespace, "@include", one or
// more spaces/tabs, a relative path, optional trailing whitespace.
// Anything else (including @include inside prose, e.g.
// "see @include for details") returns ("", false).
func parseIncludeLine(line string) (string, bool) {
	// Strip the trailing newline (if any) for matching, since the
	// directive ends at end-of-line.
	trimmed := strings.TrimRight(line, "\r\n")
	rest := strings.TrimLeft(trimmed, " \t")
	const directive = "@include"
	if !strings.HasPrefix(rest, directive) {
		return "", false
	}
	rest = rest[len(directive):]
	// Must be followed by at least one space/tab — "@includefoo"
	// is not a directive.
	if rest == "" || (rest[0] != ' ' && rest[0] != '\t') {
		return "", false
	}
	rel := strings.TrimSpace(rest)
	if rel == "" {
		return "", false
	}
	return rel, true
}

// isCodeFenceMarker reports whether the line is a markdown code
// fence opener/closer. Recognizes both backtick and tilde fences
// with optional leading whitespace. The marker itself is the line's
// only "content" — info-string after the fence (e.g. "```go") is
// allowed.
func isCodeFenceMarker(line string) bool {
	trimmed := strings.TrimLeft(line, " \t")
	return strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~")
}

// stripFrontmatter removes a leading YAML frontmatter block. The
// block must start at the very first line with "---" and end at
// the next "---" on its own line. Anything else (including "---"
// later in the file used as a markdown hr) is left intact.
//
// v2 doesn't parse the frontmatter — we just want it out of the
// system prompt. Future versions may extract tags / metadata.
func stripFrontmatter(s string) string {
	if !strings.HasPrefix(s, "---\n") && !strings.HasPrefix(s, "---\r\n") {
		return s
	}
	// Position after the opening "---" line.
	var start int
	if strings.HasPrefix(s, "---\r\n") {
		start = len("---\r\n")
	} else {
		start = len("---\n")
	}
	// Search for the closing "---" line.
	rest := s[start:]
	for {
		nl := strings.IndexByte(rest, '\n')
		if nl < 0 {
			// No closing fence — not actually frontmatter; return original.
			return s
		}
		line := rest[:nl]
		// Trim trailing \r so "---\r\n" closing matches.
		if strings.TrimRight(line, "\r") == "---" {
			return rest[nl+1:]
		}
		rest = rest[nl+1:]
	}
}

// validateIncludePath rejects shapes that have no business being in
// an @include directive:
//   - absolute paths (POSIX or Windows-style) — relative-only by design
//   - URLs — no network fetches; this is a local-files loader
//   - empty path after trimming
//
// Path-escape checking (../..) is done by ensureWithinScope after
// the join, since it requires the scope root.
func validateIncludePath(rel string) error {
	if rel == "" {
		return errors.New("empty path")
	}
	if filepath.IsAbs(rel) {
		return errors.New("absolute paths not allowed")
	}
	// Windows-drive absolute like "C:\foo" — IsAbs handles this on
	// Windows, but on Linux it doesn't. Reject explicitly so the
	// behavior is portable.
	if len(rel) >= 2 && rel[1] == ':' {
		return errors.New("absolute paths not allowed")
	}
	if strings.HasPrefix(rel, "/") {
		return errors.New("absolute paths not allowed")
	}
	for _, scheme := range []string{"http://", "https://", "file://", "ftp://"} {
		if strings.HasPrefix(rel, scheme) {
			return errors.New("URLs not allowed")
		}
	}
	return nil
}

// ensureWithinScope verifies the resolved target sits under
// scopeRoot. Symlinks within the scope are fine; symlinks pointing
// out of the scope are rejected because they're an exfil vector
// (an operator could be tricked into pointing an @include at a
// crafted symlink that pulls /etc/passwd into the system prompt).
func ensureWithinScope(target, scopeRoot string) error {
	canonScope, err := canonicalPath(scopeRoot)
	if err != nil {
		return fmt.Errorf("resolve scope root: %w", err)
	}
	// We canonicalize the parent dir if the target doesn't exist yet
	// (so the error from os.Stat later is the operator-facing one).
	canonTarget, err := canonicalPathLenient(target)
	if err != nil {
		return fmt.Errorf("resolve target: %w", err)
	}
	rel, err := filepath.Rel(canonScope, canonTarget)
	if err != nil {
		return fmt.Errorf("path-rel: %w", err)
	}
	if strings.HasPrefix(rel, "..") || rel == ".." {
		return fmt.Errorf("path escapes scope root (%s)", canonScope)
	}
	return nil
}

// canonicalPath returns the absolute, symlink-resolved path. Used
// for visited-set keys and per-file Source records — same file
// reached by different routes must produce the same key.
func canonicalPath(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	return resolved, nil
}

// canonicalPathLenient resolves what it can and falls back to
// resolving the parent for paths whose final component doesn't
// exist yet. This lets ensureWithinScope check missing @include
// targets without short-circuiting on a confusing EvalSymlinks
// error — the file-not-found error will surface from the os.Stat
// in processIncludes, which is the operator-facing message.
func canonicalPathLenient(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved, nil
	}
	parent := filepath.Dir(abs)
	resolvedParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		// Parent missing too — give up and return the abs path;
		// ensureWithinScope's filepath.Rel will work on it and the
		// next os.Stat will produce the operator-facing error.
		return abs, nil //nolint:nilerr // intentional graceful degradation
	}
	return filepath.Join(resolvedParent, filepath.Base(abs)), nil
}

// readUpTo reads at most max+1 bytes so we can detect truncation
// without slurping a multi-gigabyte file. Returns the (possibly
// truncated) bytes and a truncated flag.
func readUpTo(path string, max int) ([]byte, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close() //nolint:errcheck // read-only

	data, err := io.ReadAll(io.LimitReader(f, int64(max)+1))
	if err != nil {
		return nil, false, err
	}
	if len(data) > max {
		return data[:max], true, nil
	}
	return data, false, nil
}

// isValidUTF8 wraps utf8.Valid behind a package-local symbol so the
// test file can shadow it if a non-strict mode is ever added.
// Strict by default in v2.
func isValidUTF8(b []byte) bool { return utf8.Valid(b) }

// splitLinesKeepNewline splits s on '\n' but keeps the newline at
// the end of each preceding line, so reconstruction via join is
// lossless. The final element is the post-last-newline tail (often
// empty if the file ends in \n).
func splitLinesKeepNewline(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i+1])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}
