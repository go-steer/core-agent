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
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	adktool "google.golang.org/adk/tool"
	"google.golang.org/adk/tool/skilltoolset"
	"google.golang.org/adk/tool/skilltoolset/skill"

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

// Load discovers skills under agentsDir/skills/. A missing directory
// (or empty agentsDir) yields a zero Skills with no error — most
// projects don't use skills.
//
// gate (optional) wraps the resulting toolset so skill invocations go
// through the permission system. Pass nil to skip gating.
func Load(ctx context.Context, agentsDir string, gate *permissions.Gate) (Skills, error) {
	if agentsDir == "" {
		return Skills{}, nil
	}
	skillsDir := filepath.Join(agentsDir, SkillDirName)
	info, err := os.Stat(skillsDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Skills{}, nil
		}
		return Skills{}, fmt.Errorf("skills: stat %s: %w", skillsDir, err)
	}
	if !info.IsDir() {
		return Skills{}, fmt.Errorf("skills: %s is not a directory", skillsDir)
	}

	source := skill.NewFileSystemSource(os.DirFS(skillsDir))
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
