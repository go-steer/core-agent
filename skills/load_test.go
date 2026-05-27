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
