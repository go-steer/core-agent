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
	"os"
	"path/filepath"
	"testing"
)

func TestLoad_NoSkillsDir(t *testing.T) {
	t.Parallel()
	got, err := Load(context.Background(), t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Empty() {
		t.Errorf("expected empty Skills, got %+v", got)
	}
}

func TestLoad_EmptyAgentsDir(t *testing.T) {
	t.Parallel()
	got, err := Load(context.Background(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Empty() {
		t.Errorf("expected empty Skills, got %+v", got)
	}
}

func TestLoad_DiscoversSkills(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	skill := filepath.Join(dir, SkillDirName, "weather")
	if err := os.MkdirAll(skill, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `---
name: weather
description: Returns the weather for a given city
---

When asked about the weather, reply with a witty observation about the sky.`
	if err := os.WriteFile(filepath.Join(skill, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := Load(context.Background(), dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Empty() {
		t.Fatal("expected skills loaded")
	}
	if len(got.Infos) != 1 || got.Infos[0].Name != "weather" {
		t.Errorf("infos = %+v", got.Infos)
	}
	if got.Infos[0].Description == "" {
		t.Errorf("description not parsed: %+v", got.Infos[0])
	}
}

func TestLoad_SanitizesExtendedFrontmatter(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	skill := filepath.Join(dir, SkillDirName, "extended-skill")
	if err := os.MkdirAll(skill, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `---
name: extended-skill
description: A skill that uses Claude Skills 2.0 extension features
user-invocable: true
disable-model-invocation: false
compatibility:
  go: ">=1.20"
  charm.land/bubbletea: ">=v2.0"
metadata:
  origin: reference-implementation
  scope: testing-scopes
references:
  - references/example1.md
  - references/example2.md
---

Some instruction body content.`
	if err := os.WriteFile(filepath.Join(skill, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := Load(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("Load should have succeeded after sanitizing, but got: %v", err)
	}
	if got.Empty() {
		t.Fatal("expected skills loaded")
	}
	if len(got.Infos) != 1 || got.Infos[0].Name != "extended-skill" {
		t.Errorf("infos = %+v", got.Infos)
	}
}

func TestLoad_EmptySkillsDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, SkillDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := Load(context.Background(), dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Empty() {
		t.Errorf("empty skills dir should yield empty Skills, got %+v", got)
	}
}

// writeSkill is a tiny helper for the LoadAll tests: drop a
// {name: <name>, description: <desc>} skill at dir/skills/<name>/.
func writeSkill(t *testing.T, dir, name, desc string) {
	t.Helper()
	skillPath := filepath.Join(dir, SkillDirName, name)
	if err := os.MkdirAll(skillPath, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: " + name + "\ndescription: " + desc + "\n---\n\nbody for " + name
	if err := os.WriteFile(filepath.Join(skillPath, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadAll_BothSourcesMerge(t *testing.T) {
	t.Parallel()
	project := t.TempDir()
	userHome := t.TempDir()

	writeSkill(t, project, "project-only", "from project")
	writeSkill(t, userHome, "user-only", "from user-home")

	got, err := LoadAll(context.Background(), project, userHome, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Infos) != 2 {
		t.Fatalf("expected 2 skills merged, got %d: %+v", len(got.Infos), got.Infos)
	}
	names := map[string]string{}
	for _, info := range got.Infos {
		names[info.Name] = info.Description
	}
	if names["project-only"] != "from project" {
		t.Errorf("project-only missing/wrong: %v", names)
	}
	if names["user-only"] != "from user-home" {
		t.Errorf("user-only missing/wrong: %v", names)
	}
}

func TestLoadAll_ProjectShadowsUserGlobalOnCollision(t *testing.T) {
	t.Parallel()
	project := t.TempDir()
	userHome := t.TempDir()

	// Both have a "cli-setup" skill with different descriptions —
	// project's description should win because it appears first in
	// the overlay's ReadDir merge.
	writeSkill(t, project, "cli-setup", "PROJECT version")
	writeSkill(t, userHome, "cli-setup", "USER-GLOBAL version")

	got, err := LoadAll(context.Background(), project, userHome, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Infos) != 1 {
		t.Fatalf("expected 1 skill (project shadows), got %d: %+v", len(got.Infos), got.Infos)
	}
	if got.Infos[0].Description != "PROJECT version" {
		t.Errorf("description = %q, want PROJECT version (project should shadow user-global)", got.Infos[0].Description)
	}
}

func TestLoadAll_OnlyUserGlobal(t *testing.T) {
	t.Parallel()
	userHome := t.TempDir()
	writeSkill(t, userHome, "user-only", "lives in user-home")

	// Project agentsDir omitted entirely.
	got, err := LoadAll(context.Background(), "", userHome, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Infos) != 1 || got.Infos[0].Name != "user-only" {
		t.Errorf("expected user-only skill, got %+v", got.Infos)
	}
}

func TestLoadAll_OnlyProject(t *testing.T) {
	t.Parallel()
	project := t.TempDir()
	writeSkill(t, project, "project-only", "lives in project")

	// User home omitted entirely. Behavior equivalent to old Load.
	got, err := LoadAll(context.Background(), project, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Infos) != 1 || got.Infos[0].Name != "project-only" {
		t.Errorf("expected project-only skill, got %+v", got.Infos)
	}
}

func TestLoadAll_NeitherSource(t *testing.T) {
	t.Parallel()
	got, err := LoadAll(context.Background(), "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Empty() {
		t.Errorf("no sources should yield empty Skills, got %+v", got)
	}
}

func TestLoadAll_ProjectExistsUserHomeMissing(t *testing.T) {
	t.Parallel()
	project := t.TempDir()
	userHome := t.TempDir() // exists but no skills/ subdirectory

	writeSkill(t, project, "p1", "p1")

	got, err := LoadAll(context.Background(), project, userHome, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Infos) != 1 || got.Infos[0].Name != "p1" {
		t.Errorf("got %+v", got.Infos)
	}
}
