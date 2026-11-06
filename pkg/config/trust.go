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

package config

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/go-steer/core-agent/v3/pkg/hooks"
)

// TrustStoreFileName is the user-scoped file under ~/.core-agent/
// that records trusted workspace .agents/ directories and their content
// hashes (#1353).
const TrustStoreFileName = "trusted_workspaces.json"

// TrustStoreVersion is the current schema version for trusted_workspaces.json.
const TrustStoreVersion = 1

// TrustEntry records a single trusted .agents/ directory and the
// SHA-256 digest of its configuration files at the time trust was
// granted (#1353). Editing any configuration file inside .agents/
// changes the digest and revokes trust automatically.
type TrustEntry struct {
	Path      string    `json:"path"`
	Hash      string    `json:"hash"`
	TrustedAt time.Time `json:"trusted_at"`
}

// TrustStore is the on-disk schema for ~/.core-agent/trusted_workspaces.json.
type TrustStore struct {
	Version     int                   `json:"version"`
	TrustedDirs []string              `json:"trusted_dirs,omitempty"`
	Workspaces  map[string]TrustEntry `json:"workspaces,omitempty"`
}

// DefaultTrustStorePath returns the path to the user-scoped workspace
// trust store. CORE_AGENT_TRUST_FILE overrides the default location
// (~/.core-agent/trusted_workspaces.json) for tests and hermetic runs.
func DefaultTrustStorePath() (string, error) {
	if env := strings.TrimSpace(os.Getenv("CORE_AGENT_TRUST_FILE")); env != "" {
		return env, nil
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", fmt.Errorf("config: resolve user home for trust store: %w", err)
	}
	return filepath.Join(home, ".core-agent", TrustStoreFileName), nil
}

// LoadTrustStore reads the user-scoped TrustStore from path (or
// DefaultTrustStorePath when path is empty). A missing file returns an
// empty initialized TrustStore without error.
func LoadTrustStore(path string) (*TrustStore, error) {
	if strings.TrimSpace(path) == "" {
		var err error
		path, err = DefaultTrustStorePath()
		if err != nil {
			return &TrustStore{Version: TrustStoreVersion, Workspaces: make(map[string]TrustEntry)}, nil
		}
	}
	store := &TrustStore{
		Version:    TrustStoreVersion,
		Workspaces: make(map[string]TrustEntry),
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return store, nil
		}
		return nil, fmt.Errorf("config: read trust store %q: %w", path, err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return store, nil
	}
	if err := json.Unmarshal(data, store); err != nil {
		return nil, fmt.Errorf("config: parse trust store %q: %w", path, err)
	}
	if store.Version == 0 {
		store.Version = TrustStoreVersion
	}
	if store.Workspaces == nil {
		store.Workspaces = make(map[string]TrustEntry)
	}
	return store, nil
}

// Save atomically persists the TrustStore to path (or
// DefaultTrustStorePath when path is empty) with 0600 permissions.
func (s *TrustStore) Save(path string) error {
	if strings.TrimSpace(path) == "" {
		var err error
		path, err = DefaultTrustStorePath()
		if err != nil {
			return err
		}
	}
	if s.Version == 0 {
		s.Version = TrustStoreVersion
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("config: create trust store dir %q: %w", dir, err)
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("config: marshal trust store: %w", err)
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(dir, ".trusted_workspaces-*.tmp")
	if err != nil {
		return fmt.Errorf("config: create temp trust store: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("config: chmod temp trust store: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("config: write temp trust store: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("config: close temp trust store: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return fmt.Errorf("config: rename trust store into %q: %w", path, err)
	}
	return nil
}

// runtimeAgentsSubdirs lists subdirectories under .agents/ that store
// runtime artifacts rather than workspace configuration; writing a plan
// or transcript during a session must not revoke workspace trust.
var runtimeAgentsSubdirs = map[string]bool{
	"sessions":    true,
	"transcripts": true,
	"cache":       true,
	"logs":        true,
	"plans":       true,
	"memory":      true,
	".git":        true,
}

func canonicalizeTrustDir(dir string) string {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		return filepath.Clean(resolved)
	}
	if abs, err := filepath.Abs(dir); err == nil {
		return filepath.Clean(abs)
	}
	return filepath.Clean(dir)
}

// HashAgentsDir computes the canonical symlink-resolved path and a
// deterministic SHA-256 digest over all configuration files inside
// agentsDir (#1353). Runtime subdirectories (sessions/, transcripts/,
// plans/, etc.) are excluded so normal agent operation does not
// invalidate a granted trust entry.
func HashAgentsDir(agentsDir string) (string, string, error) {
	if strings.TrimSpace(agentsDir) == "" {
		return "", "", errors.New("config: empty agentsDir")
	}
	canonical := canonicalizeTrustDir(agentsDir)
	info, err := os.Stat(canonical)
	if err != nil {
		return "", "", fmt.Errorf("config: stat agentsDir %q: %w", agentsDir, err)
	}
	if !info.IsDir() {
		return "", "", fmt.Errorf("config: agentsDir %q is not a directory", agentsDir)
	}

	var relPaths []string
	err = filepath.WalkDir(canonical, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == canonical {
			return nil
		}
		rel, err := filepath.Rel(canonical, path)
		if err != nil {
			return err
		}
		relSlash := filepath.ToSlash(rel)
		first := strings.SplitN(relSlash, "/", 2)[0]
		if d.IsDir() {
			if runtimeAgentsSubdirs[first] {
				return filepath.SkipDir
			}
			return nil
		}
		if runtimeAgentsSubdirs[first] {
			return nil
		}
		relPaths = append(relPaths, relSlash)
		return nil
	})
	if err != nil {
		return "", "", fmt.Errorf("config: walk agentsDir %q: %w", agentsDir, err)
	}
	sort.Strings(relPaths)

	h := sha256.New()
	for _, relSlash := range relPaths {
		full := filepath.Join(canonical, filepath.FromSlash(relSlash))
		if err := hashAgentsDirEntry(h, relSlash, full); err != nil {
			return "", "", err
		}
	}
	hashReferencedSubagentRoots(h, canonical)
	sum := h.Sum(nil)
	return canonical, "sha256:" + hex.EncodeToString(sum), nil
}

func hashAgentsDirEntry(h io.Writer, relSlash, full string) error {
	linfo, err := os.Lstat(full)
	if err != nil {
		return fmt.Errorf("config: stat %q: %w", full, err)
	}
	_, _ = fmt.Fprintf(h, "file:%s\n", relSlash)
	if linfo.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(full)
		if err != nil {
			return fmt.Errorf("config: readlink %q: %w", full, err)
		}
		_, _ = fmt.Fprintf(h, "symlink:%s\n", target)
		resolved, err := filepath.EvalSymlinks(full)
		if err != nil {
			_, _ = fmt.Fprintf(h, "broken-symlink\n")
			return nil
		}
		rinfo, err := os.Stat(resolved)
		if err != nil {
			_, _ = fmt.Fprintf(h, "broken-symlink\n")
			return nil
		}
		if rinfo.Mode().IsRegular() {
			content, err := os.ReadFile(resolved)
			if err != nil {
				return fmt.Errorf("config: read symlink target %q: %w", resolved, err)
			}
			_, _ = fmt.Fprintf(h, "len:%d\n", len(content))
			_, _ = h.Write(content)
			_, _ = h.Write([]byte{'\n'})
			return nil
		}
		if rinfo.IsDir() {
			return hashResolvedSymlinkDir(h, relSlash, resolved)
		}
		return nil
	}
	if !linfo.Mode().IsRegular() {
		return nil
	}
	content, err := os.ReadFile(full)
	if err != nil {
		return fmt.Errorf("config: read %q: %w", full, err)
	}
	_, _ = fmt.Fprintf(h, "len:%d\n", len(content))
	_, _ = h.Write(content)
	_, _ = h.Write([]byte{'\n'})
	return nil
}

func hashResolvedSymlinkDir(h io.Writer, prefixRel, resolvedDir string) error {
	var subPaths []string
	err := filepath.WalkDir(resolvedDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == resolvedDir || d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(resolvedDir, path)
		if err != nil {
			return err
		}
		subPaths = append(subPaths, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return err
	}
	sort.Strings(subPaths)
	for _, sub := range subPaths {
		content, err := os.ReadFile(filepath.Join(resolvedDir, filepath.FromSlash(sub)))
		if err != nil {
			continue
		}
		_, _ = fmt.Fprintf(h, "symlink-dir-file:%s/%s\nlen:%d\n", prefixRel, sub, len(content))
		_, _ = h.Write(content)
		_, _ = h.Write([]byte{'\n'})
	}
	return nil
}

func hashReferencedSubagentRoots(h io.Writer, canonicalAgentsDir string) {
	cfgPath := filepath.Join(canonicalAgentsDir, "config.json")
	var subagents []SubagentSpec
	if data, err := os.ReadFile(cfgPath); err == nil {
		var parsed struct {
			Subagents []SubagentSpec `json:"subagents"`
		}
		if json.Unmarshal(data, &parsed) == nil {
			subagents = append(subagents, parsed.Subagents...)
		}
	}
	var mdCfg Config
	if mdCfg.loadMarkdownSubagents(canonicalAgentsDir) == nil {
		subagents = append(subagents, mdCfg.Subagents...)
	}
	baseDir := filepath.Dir(canonicalAgentsDir)
	for _, sa := range subagents {
		root := strings.TrimSpace(sa.Root)
		if root == "" {
			continue
		}
		rootAbs := root
		if !filepath.IsAbs(rootAbs) {
			rootAbs = filepath.Join(baseDir, rootAbs)
		}
		rootAbs = filepath.Clean(rootAbs)
		mcpData, err := os.ReadFile(filepath.Join(rootAbs, "mcp.json"))
		if err != nil {
			continue
		}
		_, _ = fmt.Fprintf(h, "subagent-root-mcp:%s:%s\nlen:%d\n", sa.Name, root, len(mcpData))
		_, _ = h.Write(mcpData)
		_, _ = h.Write([]byte{'\n'})
	}
}

// IsTrusted reports whether agentsDir is currently trusted by the
// store's allowlist, the CORE_AGENT_TRUSTED_DIRS environment variable,
// or a matching content hash in s.Workspaces (#1353).
func (s *TrustStore) IsTrusted(agentsDir string) bool {
	if strings.TrimSpace(agentsDir) == "" {
		return true
	}
	canonical := canonicalizeTrustDir(agentsDir)
	workspaceRoot := filepath.Dir(canonical)

	for _, allowed := range s.allowlistDirs() {
		if allowed == canonical || (filepath.Base(canonical) == AgentsDirName && allowed == workspaceRoot) {
			return true
		}
	}
	if s == nil || len(s.Workspaces) == 0 {
		return false
	}
	canonPath, hash, err := HashAgentsDir(agentsDir)
	if err != nil || hash == "" {
		return false
	}
	entry, ok := s.Workspaces[canonPath]
	return ok && entry.Hash == hash
}

func (s *TrustStore) allowlistDirs() []string {
	var raw []string
	if s != nil {
		raw = append(raw, s.TrustedDirs...)
	}
	if env := strings.TrimSpace(os.Getenv("CORE_AGENT_TRUSTED_DIRS")); env != "" {
		for _, part := range strings.FieldsFunc(env, func(r rune) bool {
			return r == os.PathListSeparator || r == ','
		}) {
			if trimmed := strings.TrimSpace(part); trimmed != "" {
				raw = append(raw, trimmed)
			}
		}
	}
	out := make([]string, 0, len(raw))
	for _, d := range raw {
		if c := canonicalizeTrustDir(d); c != "" {
			out = append(out, c)
		}
	}
	return out
}

// Trust records the current content hash of agentsDir in the store.
func (s *TrustStore) Trust(agentsDir string) error {
	canonPath, hash, err := HashAgentsDir(agentsDir)
	if err != nil {
		return err
	}
	if s.Workspaces == nil {
		s.Workspaces = make(map[string]TrustEntry)
	}
	s.Workspaces[canonPath] = TrustEntry{
		Path:      canonPath,
		Hash:      hash,
		TrustedAt: time.Now().UTC(),
	}
	return nil
}

// TrustWorkspace loads the TrustStore at storePath (or
// DefaultTrustStorePath when empty), records trust for agentsDir, and
// saves the updated store atomically.
func TrustWorkspace(storePath, agentsDir string) error {
	store, err := LoadTrustStore(storePath)
	if err != nil {
		return err
	}
	if err := store.Trust(agentsDir); err != nil {
		return err
	}
	return store.Save(storePath)
}

// IsWorkspaceTrusted loads the TrustStore at storePath (or
// DefaultTrustStorePath when empty) and reports whether agentsDir is
// trusted.
func IsWorkspaceTrusted(storePath, agentsDir string) bool {
	store, err := LoadTrustStore(storePath)
	if err != nil {
		return false
	}
	return store.IsTrusted(agentsDir)
}

// NeedsWorkspaceTrust inspects <agentsDir>/mcp.json and cfg for
// privileged repo-supplied capabilities that could execute code or
// weaken permissions before the first prompt (#1353):
//   - stdio MCP servers in <agentsDir>/mcp.json
//   - command hooks in cfg.Hooks
//   - permissions.mode above "ask" ("acceptEdits", "auto", "allow", "yolo")
//   - subagents[].root or content_roots
//
// Plain instructions (AGENTS.md), skills, HTTP MCP servers, and
// read-only settings (model, compaction, ui, permissions.mode="ask" or
// "plan") return nil.
func NeedsWorkspaceTrust(cfg *Config, agentsDir string) []string {
	var reasons []string
	if strings.TrimSpace(agentsDir) != "" {
		reasons = append(reasons, stdioMCPTrustReasons(agentsDir)...)
	}
	if cfg == nil {
		return reasons
	}
	if hookCount := countConfiguredHooks(cfg.Hooks); hookCount > 0 {
		reasons = append(reasons, fmt.Sprintf("command hooks (%d configured in config.json)", hookCount))
	}
	if isModeAboveAsk(cfg.Permissions.Mode) {
		reasons = append(reasons, fmt.Sprintf("permissions.mode=%q (elevated above \"ask\")", cfg.Permissions.Mode))
	}
	if len(cfg.Permissions.Allow) > 0 {
		reasons = append(reasons, fmt.Sprintf("permissions.allow (%d rule(s))", len(cfg.Permissions.Allow)))
	}
	if len(cfg.Permissions.BuiltinAllowExtras) > 0 {
		reasons = append(reasons, fmt.Sprintf("permissions.builtin_allow_extras (%s)", strings.Join(cfg.Permissions.BuiltinAllowExtras, ", ")))
	}
	if allowPaths := len(cfg.PathScope.Allow) + len(cfg.PathScope.AllowPaths); allowPaths > 0 {
		reasons = append(reasons, fmt.Sprintf("path_scope.allow (%d path(s))", allowPaths))
	}
	baseDir := ""
	if strings.TrimSpace(agentsDir) != "" {
		baseDir = filepath.Dir(canonicalizeTrustDir(agentsDir))
	}
	for _, sa := range cfg.Subagents {
		root := strings.TrimSpace(sa.Root)
		if root != "" {
			reasons = append(reasons, fmt.Sprintf("subagent %q with custom root %q", sa.Name, root))
			rootAbs := root
			if !filepath.IsAbs(rootAbs) && baseDir != "" {
				rootAbs = filepath.Join(baseDir, rootAbs)
			}
			reasons = append(reasons, stdioMCPTrustReasons(rootAbs)...)
		}
	}
	if len(cfg.ContentRoots) > 0 {
		reasons = append(reasons, fmt.Sprintf("content_roots (%s)", strings.Join(cfg.ContentRoots, ", ")))
	}
	return reasons
}

func countConfiguredHooks(h hooks.Config) int {
	total := 0
	for _, handlers := range h {
		total += len(handlers)
	}
	return total
}

func isModeAboveAsk(mode string) bool {
	switch strings.TrimSpace(mode) {
	case "acceptEdits", "auto", "allow", "yolo":
		return true
	default:
		return false
	}
}

func stdioMCPTrustReasons(agentsDir string) []string {
	mcpPath := filepath.Join(agentsDir, "mcp.json")
	data, err := os.ReadFile(mcpPath)
	if err != nil {
		return nil
	}
	var parsed struct {
		Servers map[string]struct {
			Transport string   `json:"transport"`
			Command   string   `json:"command"`
			Args      []string `json:"args"`
		} `json:"servers"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil
	}
	var names []string
	for name, spec := range parsed.Servers {
		if spec.Transport == "stdio" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	reasons := make([]string, 0, len(names))
	for _, name := range names {
		spec := parsed.Servers[name]
		cmdStr := strings.TrimSpace(spec.Command + " " + strings.Join(spec.Args, " "))
		reasons = append(reasons, fmt.Sprintf("stdio MCP server %q (%s)", name, cmdStr))
	}
	return reasons
}

// ClampUntrustedWorkspace strips privileged repo-supplied settings from
// cfg when the workspace .agents/ directory is untrusted (#1353):
//   - permissions.mode above "ask" falls back to "ask"
//   - command hooks are cleared
//   - content_roots and rooted subagents (subagents[].root) are stripped
//   - cfg.WorkspaceUntrusted() is set to true so downstream MCP loading
//     skips stdio servers from the untrusted .agents/ directory.
func ClampUntrustedWorkspace(cfg *Config) []string {
	if cfg == nil {
		return nil
	}
	cfg.workspaceUntrusted = true
	var clamped []string
	if isModeAboveAsk(cfg.Permissions.Mode) {
		clamped = append(clamped, fmt.Sprintf("permissions.mode=%q clamped to \"ask\"", cfg.Permissions.Mode))
		cfg.Permissions.Mode = "ask"
	}
	if len(cfg.Permissions.Allow) > 0 {
		clamped = append(clamped, fmt.Sprintf("cleared %d permissions.allow rule(s)", len(cfg.Permissions.Allow)))
		cfg.Permissions.Allow = nil
	}
	if len(cfg.Permissions.BuiltinAllowExtras) > 0 {
		clamped = append(clamped, fmt.Sprintf("cleared permissions.builtin_allow_extras (%s)", strings.Join(cfg.Permissions.BuiltinAllowExtras, ", ")))
		cfg.Permissions.BuiltinAllowExtras = nil
	}
	if allowPaths := len(cfg.PathScope.Allow) + len(cfg.PathScope.AllowPaths); allowPaths > 0 {
		clamped = append(clamped, fmt.Sprintf("cleared %d path_scope.allow path(s)", allowPaths))
		cfg.PathScope.Allow = nil
		cfg.PathScope.AllowPaths = nil
	}
	if hookCount := countConfiguredHooks(cfg.Hooks); hookCount > 0 {
		clamped = append(clamped, fmt.Sprintf("skipped %d command hook(s)", hookCount))
		cfg.Hooks = nil
	}
	var keptSubagents []SubagentSpec
	for _, sa := range cfg.Subagents {
		if strings.TrimSpace(sa.Root) != "" {
			clamped = append(clamped, fmt.Sprintf("skipped rooted subagent %q (root=%q)", sa.Name, sa.Root))
			continue
		}
		keptSubagents = append(keptSubagents, sa)
	}
	cfg.Subagents = keptSubagents
	if len(cfg.ContentRoots) > 0 {
		clamped = append(clamped, fmt.Sprintf("skipped content_roots (%s)", strings.Join(cfg.ContentRoots, ", ")))
		cfg.ContentRoots = nil
	}
	return clamped
}

// WorkspaceUntrusted reports whether this Config came from an untrusted
// workspace .agents/ directory and had its privileged capabilities
// clamped (#1353).
func (c *Config) WorkspaceUntrusted() bool {
	if c == nil {
		return false
	}
	return c.workspaceUntrusted
}

// SetWorkspaceUntrusted marks whether this Config came from an
// untrusted workspace .agents/ directory (#1353).
func (c *Config) SetWorkspaceUntrusted(untrusted bool) {
	if c != nil {
		c.workspaceUntrusted = untrusted
	}
}

// WorkspaceTrustOptions configures ResolveWorkspaceTrust (#1353).
type WorkspaceTrustOptions struct {
	// StorePath overrides DefaultTrustStorePath() when non-empty.
	StorePath string
	// TrustFlag is true when the operator passed --trust on the CLI.
	TrustFlag bool
	// Interactive is true when stdin/stderr are attached to an operator
	// terminal and the session is not headless (-p / --no-repl).
	Interactive bool
	// Prompt asks the operator whether to trust agentsDir for the given
	// privileged capability reasons. When nil and Interactive is true,
	// PromptWorkspaceTrust(os.Stdin, os.Stderr) is used.
	Prompt func(agentsDir string, reasons []string) (bool, error)
}

// ResolveWorkspaceTrust evaluates whether agentsDir requires workspace
// trust and either verifies/records trust in the user-scoped TrustStore
// or clamps privileged settings on cfg (#1353).
//
// Plain instructions (AGENTS.md) and read-only config (model,
// compaction, ui, permissions.mode="ask" or "plan") do not require
// trust unless opts.TrustFlag is explicitly set.
func ResolveWorkspaceTrust(cfg *Config, agentsDir string, opts WorkspaceTrustOptions) (trusted bool, clamped []string, err error) {
	if strings.TrimSpace(agentsDir) == "" {
		return true, nil, nil
	}
	store, err := LoadTrustStore(opts.StorePath)
	if err != nil {
		return false, nil, err
	}
	if opts.TrustFlag {
		if err := store.Trust(agentsDir); err != nil {
			return false, nil, err
		}
		if err := store.Save(opts.StorePath); err != nil {
			return false, nil, err
		}
		if cfg != nil {
			cfg.workspaceUntrusted = false
		}
		return true, nil, nil
	}

	reasons := NeedsWorkspaceTrust(cfg, agentsDir)
	if len(reasons) == 0 {
		return true, nil, nil
	}
	if store.IsTrusted(agentsDir) {
		return true, nil, nil
	}

	if opts.Interactive {
		promptFn := opts.Prompt
		if promptFn == nil {
			promptFn = func(dir string, r []string) (bool, error) {
				return PromptWorkspaceTrust(os.Stdin, os.Stderr, dir, r)
			}
		}
		approved, perr := promptFn(agentsDir, reasons)
		if perr != nil {
			return false, nil, perr
		}
		if approved {
			if err := store.Trust(agentsDir); err != nil {
				return false, nil, err
			}
			if err := store.Save(opts.StorePath); err != nil {
				return false, nil, err
			}
			return true, nil, nil
		}
	}

	clamped = ClampUntrustedWorkspace(cfg)
	for _, r := range stdioMCPTrustReasons(agentsDir) {
		clamped = append(clamped, "skipped "+r)
	}
	return false, clamped, nil
}

// PromptWorkspaceTrust displays the privileged capabilities declared by
// agentsDir on out and reads a single y/N confirmation line from in.
func PromptWorkspaceTrust(in io.Reader, out io.Writer, agentsDir string, reasons []string) (bool, error) {
	if out != nil {
		fmt.Fprintf(out, "core-agent: workspace %s declares privileged settings in %s:\n", filepath.Dir(agentsDir), agentsDir)
		for _, r := range reasons {
			fmt.Fprintf(out, "  - %s\n", r)
		}
		fmt.Fprintf(out, "Trust this workspace and remember its .agents/ configuration? [y/N]: ")
	}
	if in == nil {
		return false, nil
	}
	reader := bufio.NewReader(in)
	line, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	ans := strings.ToLower(strings.TrimSpace(line))
	return ans == "y" || ans == "yes", nil
}
