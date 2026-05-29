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
	"time"

	adktool "google.golang.org/adk/tool"
	"google.golang.org/adk/tool/skilltoolset"
	"google.golang.org/adk/tool/skilltoolset/skill"
	"gopkg.in/yaml.v3"

	"github.com/go-steer/core-agent/permissions"
	coretools "github.com/go-steer/core-agent/tools"
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
func Load(ctx context.Context, agentsDir string, gate *permissions.Gate) (Skills, error) {
	return LoadAll(ctx, agentsDir, "", gate)
}

// LoadAll discovers skills from up to two sources and merges them
// into a single toolset:
//
//  1. projectAgentsDir/skills/ — project-scoped skills, checked in
//     to the repo (or wherever .agents/ lives). Takes precedence on
//     name collision.
//  2. userCoreHome/skills/ — user-global skills (typically
//     ~/.core-agent/skills/). Falls back when no project-scoped
//     skill by the same name exists.
//
// Either path may be "" to skip that source. Missing directories
// (vs missing parent) are silently treated as empty — most operators
// won't have either populated.
//
// The two sources are merged via an overlayFS so the underlying
// skilltoolset sees a single virtual root; primary entries win on
// name collision. Both sources share the same sanitizingFS wrapper
// so extended-frontmatter properties get filtered the same way.
//
// gate (optional) wraps the resulting toolset so skill invocations
// go through the permission system. Pass nil to skip gating.
func LoadAll(ctx context.Context, projectAgentsDir, userCoreHome string, gate *permissions.Gate) (Skills, error) {
	primary, primaryOK := openSkillsDir(projectAgentsDir)
	fallback, fallbackOK := openSkillsDir(userCoreHome)

	var rootFS fs.FS
	switch {
	case primaryOK && fallbackOK:
		rootFS = newSanitizingFS(&overlayFS{primary: primary, fallback: fallback})
	case primaryOK:
		rootFS = newSanitizingFS(primary)
	case fallbackOK:
		rootFS = newSanitizingFS(fallback)
	default:
		return Skills{}, nil
	}

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

// newSanitizingFS wraps an fs.FS and intercepts files named "SKILL.md",
// stripping out unsupported/extended frontmatter properties before
// they are passed to the underlying ADK parser.
func newSanitizingFS(filesystem fs.FS) fs.FS {
	return &sanitizingFS{fs: filesystem}
}

type sanitizingFS struct {
	fs fs.FS
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
	if filepath.Base(name) != "SKILL.md" {
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

	sanitized := sanitizeFrontmatter(data)

	return &memFile{
		name:    name,
		data:    sanitized,
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
