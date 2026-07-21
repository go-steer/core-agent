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

// Package skills loads SKILL.md bundles from .agents/skills/<name>/
// and exposes them as an ADK Toolset the agent can invoke.
//
// The schema mirrors Anthropic's published SKILL.md frontmatter so
// users can drop existing skill bundles directly into a project.
//
// Bodies load lazily on invocation — we keep cold-start fast by
// skipping skill.WithCompletePreloadSource.
package skills

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	adktool "google.golang.org/adk/tool"
	"google.golang.org/adk/tool/skilltoolset"
	"google.golang.org/adk/tool/skilltoolset/skill"
	"gopkg.in/yaml.v3"

	"github.com/go-steer/core-agent/v2/pkg/permissions"
	coretools "github.com/go-steer/core-agent/v2/pkg/tools"
)

// SkillDirName is the project-local directory holding skill bundles.
const SkillDirName = "skills"

// Info is the per-skill metadata surfaced to hosts that want to render
// a /skills view.
type Info struct {
	Name        string
	Description string
}

// Skills bundles the discovered skills' toolset (for agent.WithToolsets)
// alongside the metadata list.
type Skills struct {
	Toolset adktool.Toolset
	Infos   []Info
}

// Empty reports whether no skills were discovered.
func (s Skills) Empty() bool { return s.Toolset == nil }

// Option configures a Load / LoadAll call. All options are optional;
// the zero-options call matches the pre-#322 loader behavior exactly.
type Option func(*loadOptions)

type loadOptions struct {
	// interp is applied to each .md file body opened through the
	// sanitizing filesystem — SKILL.md and everything under a skill's
	// references/. Nil = no interpolation, matching pre-#322 behavior.
	// Wire from pkg/agentenv via (*agentenv.Resolver).InterpolateFunc().
	interp func(string) string

	// homeAgentsSkillsDir is an additional user-scope skills root
	// (typically $HOME/.agents/skills/) layered between the project
	// source and the ~/.core-agent/skills/ source. Empty = skip.
	homeAgentsSkillsDir string
}

// WithInterpolator supplies a string transform applied to every .md
// file loaded from a skill directory — SKILL.md and referenced files
// under references/. Used to substitute ${env:VAR} references declared
// in .agents/env.yaml (see pkg/agentenv). Passing nil is legal and
// equals "no interpolation."
func WithInterpolator(fn func(string) string) Option {
	return func(o *loadOptions) { o.interp = fn }
}

// WithHomeAgentsSkillsDir supplies an extra user-scope skills root
// — typically $HOME/.agents/ (LoadAll appends the "skills" suffix
// itself, same as it does for the positional args). This source
// layers between the project-scoped source and the ~/.core-agent/
// fallback, so precedence is project > home-agents > core-home.
// Empty is legal and equals "no home-agents source."
func WithHomeAgentsSkillsDir(dir string) Option {
	return func(o *loadOptions) { o.homeAgentsSkillsDir = dir }
}

// Load discovers skills under agentsDir/skills/ only. A missing
// directory (or empty agentsDir) yields a zero Skills with no error.
//
// Deprecated since v2.1: use LoadAll to also pick up user-global
// skills from userCoreHome/skills/. Load remains as a one-source
// wrapper around LoadAll for callers that explicitly don't want the
// global path.
//
// gate (optional) wraps the resulting toolset so skill invocations go
// through the permission system. Pass nil to skip gating.
func Load(ctx context.Context, agentsDir string, gate *permissions.Gate, opts ...Option) (Skills, error) {
	return LoadAll(ctx, agentsDir, "", gate, opts...)
}

// LoadAll discovers skills from up to three sources and merges them
// into a single toolset:
//
//  1. projectAgentsDir/skills/ — project-scoped skills, checked in
//     to the repo (or wherever .agents/ lives). Takes precedence on
//     name collision.
//  2. WithHomeAgentsSkillsDir/skills/ — portable user-scope skills
//     (typically $HOME/.agents/skills/), layered under project scope
//     but above the ~/.core-agent/ fallback. Off unless the option
//     is passed. See the note at WithHomeAgentsSkillsDir.
//  3. userCoreHome/skills/ — user-global skills (typically
//     ~/.core-agent/skills/). Bottom layer.
//
// Any path may be "" to skip that source. Missing directories (vs
// missing parent) are silently treated as empty — most operators
// won't have any populated.
//
// Sources are merged via nested overlayFS so the underlying
// skilltoolset sees a single virtual root; higher-precedence entries
// win on name collision. Every source shares the same sanitizingFS
// wrapper so extended-frontmatter properties get filtered the same way.
//
// gate (optional) wraps the resulting toolset so skill invocations
// go through the permission system. Pass nil to skip gating.
func LoadAll(ctx context.Context, projectAgentsDir, userCoreHome string, gate *permissions.Gate, opts ...Option) (Skills, error) {
	var lo loadOptions
	for _, o := range opts {
		o(&lo)
	}

	// Collect the open sources in precedence order (highest first).
	// Any dir that's "" or missing is silently skipped.
	sources := make([]fs.FS, 0, 3)
	if primary, ok := openSkillsDir(projectAgentsDir); ok {
		sources = append(sources, primary)
	}
	if homeAgents, ok := openSkillsDir(lo.homeAgentsSkillsDir); ok {
		sources = append(sources, homeAgents)
	}
	if fallback, ok := openSkillsDir(userCoreHome); ok {
		sources = append(sources, fallback)
	}
	if len(sources) == 0 {
		return Skills{}, nil
	}

	// Right-fold into nested overlays so [A, B, C] becomes
	// overlayFS{A, overlayFS{B, C}} — A wins over B wins over C.
	composed := sources[len(sources)-1]
	for i := len(sources) - 2; i >= 0; i-- {
		composed = &overlayFS{primary: sources[i], fallback: composed}
	}
	rootFS := newSanitizingFS(composed, lo.interp)

	source := skill.NewFileSystemSource(rootFS)
	frontmatters, err := source.ListFrontmatters(ctx)
	if err != nil {
		return Skills{}, fmt.Errorf("skills: list: %w", err)
	}
	if len(frontmatters) == 0 {
		return Skills{}, nil
	}

	skillTS, err := skilltoolset.New(ctx, skilltoolset.Config{Source: source})
	if err != nil {
		return Skills{}, fmt.Errorf("skills: build toolset: %w", err)
	}
	var ts adktool.Toolset = skillTS
	if gate != nil {
		ts = coretools.GateToolset(ts, gate, "skill")
	}

	infos := make([]Info, 0, len(frontmatters))
	for _, fm := range frontmatters {
		infos = append(infos, Info{Name: fm.Name, Description: fm.Description})
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Name < infos[j].Name })

	return Skills{Toolset: ts, Infos: infos}, nil
}

// openSkillsDir returns an fs.FS rooted at dir/skills/, plus a bool
// reporting whether the directory exists and is a directory. Empty
// dir or missing dir/skills returns (nil, false); any other error
// (e.g., permission denied) also returns (nil, false) — the caller
// silently skips that source.
func openSkillsDir(dir string) (fs.FS, bool) {
	if dir == "" {
		return nil, false
	}
	skillsDir := filepath.Join(dir, SkillDirName)
	info, err := os.Stat(skillsDir)
	if err != nil || !info.IsDir() {
		return nil, false
	}
	return os.DirFS(skillsDir), true
}

// newSanitizingFS wraps an fs.FS with two behaviors:
//
//   - Files named "SKILL.md" get their frontmatter sanitized (drop
//     extended fields the ADK parser doesn't recognize).
//   - Any .md file (SKILL.md and everything under references/) gets
//     ${env:VAR} interpolation applied when interp != nil.
//
// Non-.md files pass through unchanged — no wrapping overhead for
// binary attachments, scripts, or other assets skills might include.
func newSanitizingFS(filesystem fs.FS, interp func(string) string) fs.FS {
	return &sanitizingFS{fs: filesystem, interp: interp}
}

type sanitizingFS struct {
	fs     fs.FS
	interp func(string) string
}

// ReadDir delegates to the wrapped filesystem so fs.ReadDir(sanitizingFS,
// ".") sees merged entries when wrapping an overlayFS (project + user-
// global skill discovery). Without this method, fs.ReadDir falls back
// to Open(".") + iterate, which only sees the primary FS's entries
// in the overlay case.
func (s *sanitizingFS) ReadDir(name string) ([]fs.DirEntry, error) {
	return fs.ReadDir(s.fs, name)
}

func (s *sanitizingFS) Open(name string) (fs.File, error) {
	file, err := s.fs.Open(name)
	if err != nil {
		return nil, err
	}
	base := filepath.Base(name)
	// Non-.md files (assets bundled with skills) pass through raw.
	// Extension check catches SKILL.md, references/*.md, and anything
	// else in the skill tree that flows through the ADK parser.
	if !strings.HasSuffix(base, ".md") {
		return file, nil
	}
	defer func() { _ = file.Close() }()

	stat, err := file.Stat()
	if err != nil {
		return nil, err
	}

	data, err := io.ReadAll(file)
	if err != nil {
		return nil, err
	}

	// SKILL.md gets frontmatter sanitized before anything else — the
	// interpolation pass would preserve it, but a broken frontmatter
	// would cause parse failures downstream.
	if base == "SKILL.md" {
		data = sanitizeFrontmatter(data)
	}

	// ${env:VAR} interpolation. Applies to SKILL.md and reference
	// files uniformly so recipe authors don't have to remember which
	// files support substitution.
	if s.interp != nil {
		data = []byte(s.interp(string(data)))
	}

	return &memFile{
		name:    name,
		data:    data,
		modTime: stat.ModTime(),
		mode:    stat.Mode(),
	}, nil
}

func sanitizeFrontmatter(data []byte) []byte {
	// Check for YAML frontmatter block starting with "---"
	if !bytes.HasPrefix(data, []byte("---\n")) && !bytes.HasPrefix(data, []byte("---\r\n")) {
		return data
	}
	parts := bytes.SplitN(data, []byte("---"), 3)
	if len(parts) < 3 {
		return data
	}
	fmBytes := parts[1]
	bodyBytes := parts[2]

	var raw map[string]any
	if err := yaml.Unmarshal(fmBytes, &raw); err != nil {
		// If it's invalid YAML, fall back and let the ADK parser report/handle it
		return data
	}

	// Filter down to fields strictly supported by google.golang.org/adk/tool/skilltoolset/skill.Frontmatter.
	// This ensures maximum compatibility and prevents yaml unmarshal errors for extended schemas (e.g. Claude Skills 2.0).
	sanitized := make(map[string]any)
	if name, ok := raw["name"]; ok {
		sanitized["name"] = name
	}
	if desc, ok := raw["description"]; ok {
		sanitized["description"] = desc
	}
	if lic, ok := raw["license"]; ok {
		sanitized["license"] = lic
	}
	if comp, ok := raw["compatibility"]; ok {
		switch v := comp.(type) {
		case string:
			sanitized["compatibility"] = v
		default:
			// If compatibility is a map or array, stringify it nicely to prevent ADK parsing errors
			b, err := yaml.Marshal(v)
			if err == nil {
				sanitized["compatibility"] = string(bytes.TrimSpace(b))
			}
		}
	}
	if meta, ok := raw["metadata"]; ok {
		if m, isMap := meta.(map[string]any); isMap {
			// Convert all values to strings to fit map[string]string schema safely
			strMeta := make(map[string]string)
			for k, val := range m {
				if vStr, isStr := val.(string); isStr {
					strMeta[k] = vStr
				} else {
					strMeta[k] = fmt.Sprintf("%v", val)
				}
			}
			sanitized["metadata"] = strMeta
		}
	}
	if tools, ok := raw["allowed-tools"]; ok {
		sanitized["allowed-tools"] = tools
	}

	newFMBytes, err := yaml.Marshal(sanitized)
	if err != nil {
		return data
	}

	var buf bytes.Buffer
	buf.WriteString("---\n")
	buf.Write(newFMBytes)
	buf.WriteString("---\n")
	buf.Write(bodyBytes)
	return buf.Bytes()
}

type memFile struct {
	name    string
	data    []byte
	off     int
	modTime time.Time
	mode    fs.FileMode
}

func (m *memFile) Stat() (fs.FileInfo, error) {
	return &memFileInfo{
		name:    filepath.Base(m.name),
		size:    int64(len(m.data)),
		modTime: m.modTime,
		mode:    m.mode,
	}, nil
}

func (m *memFile) Read(p []byte) (int, error) {
	if m.off >= len(m.data) {
		return 0, io.EOF
	}
	n := copy(p, m.data[m.off:])
	m.off += n
	return n, nil
}

func (m *memFile) Close() error {
	return nil
}

type memFileInfo struct {
	name    string
	size    int64
	modTime time.Time
	mode    fs.FileMode
}

func (m *memFileInfo) Name() string       { return m.name }
func (m *memFileInfo) Size() int64        { return m.size }
func (m *memFileInfo) Mode() fs.FileMode  { return m.mode }
func (m *memFileInfo) ModTime() time.Time { return m.modTime }
func (m *memFileInfo) IsDir() bool        { return false }
func (m *memFileInfo) Sys() any           { return nil }
