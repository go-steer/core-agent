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
	"errors"
	"testing"

	"google.golang.org/adk/tool/skilltoolset/skill"
)

// loadThree returns a Skills over a project with skills alpha/beta/gamma.
func loadThree(t *testing.T) Skills {
	t.Helper()
	project := t.TempDir()
	writeSkill(t, project, "alpha", "the alpha skill")
	writeSkill(t, project, "beta", "the beta skill")
	writeSkill(t, project, "gamma", "the gamma skill")
	s, err := LoadAll(context.Background(), project, "", nil)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(s.Infos) != 3 {
		t.Fatalf("expected 3 skills loaded, got %d", len(s.Infos))
	}
	return s
}

// TestScoped_SelectsNamedSubset: Scoped to a subset returns exactly those
// skills' Infos, and the filtered source hides the rest — the "a subagent
// that names a skill subset sees only those" acceptance criterion.
func TestScoped_SelectsNamedSubset(t *testing.T) {
	t.Parallel()
	s := loadThree(t)

	scoped, err := s.Scoped(context.Background(), []string{"alpha", "gamma"})
	if err != nil {
		t.Fatalf("Scoped: %v", err)
	}
	if scoped.Empty() {
		t.Fatal("scoped skills should not be empty")
	}
	got := map[string]bool{}
	for _, in := range scoped.Infos {
		got[in.Name] = true
	}
	if len(got) != 2 || !got["alpha"] || !got["gamma"] {
		t.Errorf("scoped Infos = %v, want {alpha, gamma}", got)
	}

	// The underlying filtered source must actually hide beta — not just
	// omit it from Infos. list shows only the two; loading beta fails
	// with the not-found sentinel exactly as if it were never on disk.
	fs, ok := scoped.source.(*filteredSource)
	if !ok {
		t.Fatalf("scoped.source is %T, want *filteredSource", scoped.source)
	}
	fms, err := fs.ListFrontmatters(context.Background())
	if err != nil {
		t.Fatalf("ListFrontmatters: %v", err)
	}
	if len(fms) != 2 {
		t.Errorf("filtered source lists %d skills, want 2", len(fms))
	}
	if _, err := fs.LoadFrontmatter(context.Background(), "beta"); !errors.Is(err, skill.ErrSkillNotFound) {
		t.Errorf("LoadFrontmatter(beta) err = %v, want ErrSkillNotFound", err)
	}
	if _, err := fs.LoadFrontmatter(context.Background(), "alpha"); err != nil {
		t.Errorf("LoadFrontmatter(alpha) should succeed, got %v", err)
	}
}

// TestScoped_FilteredSourceHidesContentPaths guards the actual exfiltration
// surface: it is not enough that a disallowed skill is absent from the
// listing — every by-name Source method that streams a skill's *content*
// must refuse it with the ErrSkillNotFound sentinel, exactly as if it were
// never on disk, while the allowed skill's content still loads. A regression
// that dropped the allow check from any of these (LoadInstructions and
// LoadResource are the content paths; ListResources enumerates them) would
// leak a hidden skill even though it never appears in list_skills.
func TestScoped_FilteredSourceHidesContentPaths(t *testing.T) {
	t.Parallel()
	s := loadThree(t)
	scoped, err := s.Scoped(context.Background(), []string{"alpha"})
	if err != nil {
		t.Fatalf("Scoped: %v", err)
	}
	fs, ok := scoped.source.(*filteredSource)
	if !ok {
		t.Fatalf("scoped.source is %T, want *filteredSource", scoped.source)
	}
	ctx := context.Background()

	// Disallowed skill: every by-name method refuses with the sentinel.
	if _, err := fs.LoadInstructions(ctx, "beta"); !errors.Is(err, skill.ErrSkillNotFound) {
		t.Errorf("LoadInstructions(beta) err = %v, want ErrSkillNotFound", err)
	}
	if _, err := fs.LoadResource(ctx, "beta", "SKILL.md"); !errors.Is(err, skill.ErrSkillNotFound) {
		t.Errorf("LoadResource(beta) err = %v, want ErrSkillNotFound", err)
	}
	if _, err := fs.ListResources(ctx, "beta", ""); !errors.Is(err, skill.ErrSkillNotFound) {
		t.Errorf("ListResources(beta) err = %v, want ErrSkillNotFound", err)
	}

	// Allowed skill: content still loads (the filter is a scope, not a wall).
	instr, err := fs.LoadInstructions(ctx, "alpha")
	if err != nil {
		t.Errorf("LoadInstructions(alpha) should succeed, got %v", err)
	}
	if instr == "" {
		t.Error("LoadInstructions(alpha) returned empty instructions")
	}
}

// TestScoped_EmptyGrantsNone: an explicit empty allow list yields a zero
// Skills so the caller adds no skill toolset — the "grant none of the
// dimension" half of the nil-vs-empty contract.
func TestScoped_EmptyGrantsNone(t *testing.T) {
	t.Parallel()
	s := loadThree(t)
	scoped, err := s.Scoped(context.Background(), []string{})
	if err != nil {
		t.Fatalf("Scoped: %v", err)
	}
	if !scoped.Empty() {
		t.Errorf("empty allow should yield Empty() skills, got %d infos", len(scoped.Infos))
	}
}

// TestScoped_UnknownSkillErrors: referencing a skill that wasn't loaded is
// a fail-loud config error, not a silently empty scope.
func TestScoped_UnknownSkillErrors(t *testing.T) {
	t.Parallel()
	s := loadThree(t)
	_, err := s.Scoped(context.Background(), []string{"alpha", "nope"})
	if err == nil {
		t.Fatal("expected error for unknown skill")
	}
	if !contains(err.Error(), "nope") {
		t.Errorf("error should name the unknown skill: %v", err)
	}
}

// TestScoped_NoSkillsLoaded: scoping a bundle that loaded no skills to a
// non-empty list errors rather than pretending success.
func TestScoped_NoSkillsLoaded(t *testing.T) {
	t.Parallel()
	var empty Skills // zero value: no source, no skills
	_, err := empty.Scoped(context.Background(), []string{"alpha"})
	if err == nil {
		t.Fatal("expected error scoping an empty bundle to a non-empty list")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
