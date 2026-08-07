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
	"context"
	"fmt"
	"io"
	"sort"

	adktool "google.golang.org/adk/tool"
	"google.golang.org/adk/tool/skilltoolset"
	"google.golang.org/adk/tool/skilltoolset/skill"

	coretools "github.com/go-steer/core-agent/v2/pkg/tools"
)

// Scoped returns a Skills exposing only the named skills — the mechanism
// declarative subagents use to narrow the parent's skill surface
// (docs/declarative-subagents-design.md). It builds a fresh skill toolset
// over a name-filtered view of the same composed source the full toolset
// was built from, so no filesystem re-walk and no second LoadAll happen;
// the toolset carries the same permission gate the parent's did.
//
// The skill toolset is a three-tool facade (list_skills / load_skill /
// load_skill_resource) over a skill.Source — individual skills are *data*,
// not tools — so scoping is a Source filter, not a tool-name filter: the
// scoped facade's list_skills enumerates only the allowed skills and its
// load_skill can only reach them.
//
// allow is the exact set of skill names to expose. An empty (but non-nil)
// allow grants none of the skill dimension and returns a zero Skills
// (Empty() == true), so the caller adds no skill toolset at all. Callers
// that want the full surface must NOT call Scoped — they reuse the parent
// Skills directly (nil-vs-empty "inherit vs grant-none" lives in the
// caller). Every name in allow must be a skill that was actually loaded;
// an unknown name is a config error (fail loud rather than silently
// exposing an empty scope).
func (s Skills) Scoped(ctx context.Context, allow []string) (Skills, error) {
	if len(allow) == 0 {
		return Skills{}, nil
	}
	if s.source == nil {
		return Skills{}, fmt.Errorf("skills: cannot scope to %v: no skills are loaded", allow)
	}

	// Validate every requested name against what was loaded — an unknown
	// skill reference is a bundle/config bug, not an empty scope.
	known := make(map[string]bool, len(s.Infos))
	for _, in := range s.Infos {
		known[in.Name] = true
	}
	want := make(map[string]bool, len(allow))
	for _, name := range allow {
		if !known[name] {
			return Skills{}, fmt.Errorf("skills: unknown skill %q (not among the %d loaded)", name, len(s.Infos))
		}
		want[name] = true
	}

	filtered := &filteredSource{inner: s.source, allow: want}
	skillTS, err := skilltoolset.New(ctx, skilltoolset.Config{Source: filtered})
	if err != nil {
		return Skills{}, fmt.Errorf("skills: build scoped toolset: %w", err)
	}
	var ts adktool.Toolset = skillTS
	if s.gate != nil {
		ts = coretools.GateToolset(ts, s.gate, "skill")
	}

	infos := make([]Info, 0, len(want))
	for _, in := range s.Infos {
		if want[in.Name] {
			infos = append(infos, in)
		}
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Name < infos[j].Name })

	return Skills{Toolset: ts, Infos: infos, source: filtered, gate: s.gate}, nil
}

// filteredSource wraps a skill.Source and exposes only the skills whose
// names are in allow. Lookups for a disallowed skill return
// skill.ErrSkillNotFound — the sentinel the Source contract mandates — so
// the scoped facade behaves exactly as if the hidden skills were never on
// disk. It is safe for concurrent use because it only reads from the inner
// source and an immutable allow set.
type filteredSource struct {
	inner skill.Source
	allow map[string]bool
}

func (f *filteredSource) ListFrontmatters(ctx context.Context) ([]*skill.Frontmatter, error) {
	all, err := f.inner.ListFrontmatters(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*skill.Frontmatter, 0, len(f.allow))
	for _, fm := range all {
		if f.allow[fm.Name] {
			out = append(out, fm)
		}
	}
	return out, nil
}

func (f *filteredSource) ListResources(ctx context.Context, name, subpath string) ([]string, error) {
	if !f.allow[name] {
		return nil, fmt.Errorf("%w: %q", skill.ErrSkillNotFound, name)
	}
	return f.inner.ListResources(ctx, name, subpath)
}

func (f *filteredSource) LoadFrontmatter(ctx context.Context, name string) (*skill.Frontmatter, error) {
	if !f.allow[name] {
		return nil, fmt.Errorf("%w: %q", skill.ErrSkillNotFound, name)
	}
	return f.inner.LoadFrontmatter(ctx, name)
}

func (f *filteredSource) LoadInstructions(ctx context.Context, name string) (string, error) {
	if !f.allow[name] {
		return "", fmt.Errorf("%w: %q", skill.ErrSkillNotFound, name)
	}
	return f.inner.LoadInstructions(ctx, name)
}

func (f *filteredSource) LoadResource(ctx context.Context, name, resourcePath string) (io.ReadCloser, error) {
	if !f.allow[name] {
		return nil, fmt.Errorf("%w: %q", skill.ErrSkillNotFound, name)
	}
	return f.inner.LoadResource(ctx, name, resourcePath)
}
