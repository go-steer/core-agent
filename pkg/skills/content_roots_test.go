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
	"testing"
)

// TestLoadAll_ContentRootDiscovered proves a skill living under an
// operator-declared external content root is discovered alongside project
// skills — the "run an unmodified external tree's skills" capability.
func TestLoadAll_ContentRootDiscovered(t *testing.T) {
	t.Parallel()
	project := t.TempDir()
	external := t.TempDir()
	writeSkill(t, project, "project-skill", "from project")
	writeSkill(t, external, "external-skill", "from external root")

	got, err := LoadAll(context.Background(), project, "", nil, WithContentRoots([]string{external}))
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]string{}
	for _, info := range got.Infos {
		names[info.Name] = info.Description
	}
	if len(names) != 2 {
		t.Fatalf("expected 2 skills, got %d: %+v", len(names), got.Infos)
	}
	if names["external-skill"] != "from external root" {
		t.Errorf("external-skill missing/wrong: %v", names)
	}
	if names["project-skill"] != "from project" {
		t.Errorf("project-skill missing/wrong: %v", names)
	}
}

// TestLoadAll_ProjectShadowsContentRootOnCollision pins precedence:
// project skills win over content-root skills of the same name (project
// appears first in the overlay fold).
func TestLoadAll_ProjectShadowsContentRootOnCollision(t *testing.T) {
	t.Parallel()
	project := t.TempDir()
	external := t.TempDir()
	writeSkill(t, project, "shared", "PROJECT version")
	writeSkill(t, external, "shared", "EXTERNAL version")

	got, err := LoadAll(context.Background(), project, "", nil, WithContentRoots([]string{external}))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Infos) != 1 {
		t.Fatalf("expected 1 merged skill, got %d: %+v", len(got.Infos), got.Infos)
	}
	if got.Infos[0].Description != "PROJECT version" {
		t.Errorf("project should shadow content root: %+v", got.Infos[0])
	}
}

// TestLoadAll_ContentRootShadowsHomeAndUser pins the middle of the
// precedence chain: a content root wins over home-agents and user-global
// on a name collision (project > content_roots > home-agents > user).
func TestLoadAll_ContentRootShadowsHomeAndUser(t *testing.T) {
	t.Parallel()
	external := t.TempDir()
	homeAgents := t.TempDir()
	coreHome := t.TempDir()
	writeSkill(t, external, "shared", "CONTENT version")
	writeSkill(t, homeAgents, "shared", "HOME version")
	writeSkill(t, coreHome, "shared", "USER version")

	got, err := LoadAll(context.Background(), "", coreHome, nil,
		WithContentRoots([]string{external}),
		WithHomeAgentsSkillsDir(homeAgents))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Infos) != 1 {
		t.Fatalf("expected 1 merged skill, got %d: %+v", len(got.Infos), got.Infos)
	}
	if got.Infos[0].Description != "CONTENT version" {
		t.Errorf("content root should shadow home + user: %+v", got.Infos[0])
	}
}

// TestLoadAll_MultipleContentRootsListedOrderWins proves that when two
// content roots carry the same skill name, the earlier-listed root wins.
func TestLoadAll_MultipleContentRootsListedOrderWins(t *testing.T) {
	t.Parallel()
	first := t.TempDir()
	second := t.TempDir()
	writeSkill(t, first, "shared", "FIRST version")
	writeSkill(t, second, "shared", "SECOND version")
	writeSkill(t, second, "second-only", "only in second")

	got, err := LoadAll(context.Background(), "", "", nil,
		WithContentRoots([]string{first, second}))
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]string{}
	for _, info := range got.Infos {
		names[info.Name] = info.Description
	}
	if names["shared"] != "FIRST version" {
		t.Errorf("earlier-listed content root should win: %v", names)
	}
	if names["second-only"] != "only in second" {
		t.Errorf("second-only should still be discovered: %v", names)
	}
}

// TestLoadAll_ContentRootWithoutSkillsDirSkipped proves a declared root
// that has no skills/ subdir is silently skipped (not an error) — the
// instruction loader owns loud validation of a genuinely-missing root.
func TestLoadAll_ContentRootWithoutSkillsDirSkipped(t *testing.T) {
	t.Parallel()
	project := t.TempDir()
	writeSkill(t, project, "project-skill", "from project")
	emptyRoot := t.TempDir() // exists, but has no skills/ subdir

	got, err := LoadAll(context.Background(), project, "", nil, WithContentRoots([]string{emptyRoot}))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Infos) != 1 || got.Infos[0].Name != "project-skill" {
		t.Fatalf("expected only the project skill, got %+v", got.Infos)
	}
}

// TestLoadAll_ContentRootsEmptyIsNoop proves nil/empty content roots leave
// behavior unchanged.
func TestLoadAll_ContentRootsEmptyIsNoop(t *testing.T) {
	t.Parallel()
	project := t.TempDir()
	writeSkill(t, project, "only", "x")

	for _, roots := range [][]string{nil, {}} {
		got, err := LoadAll(context.Background(), project, "", nil, WithContentRoots(roots))
		if err != nil {
			t.Fatalf("roots=%v: %v", roots, err)
		}
		if len(got.Infos) != 1 || got.Infos[0].Name != "only" {
			t.Errorf("roots=%v: unexpected skills %+v", roots, got.Infos)
		}
	}
}
