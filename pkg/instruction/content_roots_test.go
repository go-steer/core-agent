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

package instruction

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoad_ContentRoot_Resolves proves an operator-declared external
// content root is loaded, and that an @include inside it resolves within
// that root — the core "run an unmodified external tree" capability.
func TestLoad_ContentRoot_Resolves(t *testing.T) {
	t.Parallel()
	// An external tree that does NOT sit under the project root.
	external := t.TempDir()
	writeFile(t, external, "AGENTS.md", "external persona\n@include soul.md\n")
	writeFile(t, external, "soul.md", "external soul\n")

	project := t.TempDir()
	writeFile(t, project, "AGENTS.md", "project overlay\n")

	loaded, err := Load(project, "", WithContentRoots([]string{external}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !strings.Contains(loaded.Instruction, "external persona") {
		t.Errorf("external root primary not loaded:\n%s", loaded.Instruction)
	}
	if !strings.Contains(loaded.Instruction, "external soul") {
		t.Errorf("@include inside external root not resolved:\n%s", loaded.Instruction)
	}
}

// TestLoad_ContentRoot_ConcatenatedBeforeProject pins the ordering the
// design specifies: content roots are inserted just before the project
// block, so external content appears ahead of the project's own AGENTS.md
// (concatenation order, not an override).
func TestLoad_ContentRoot_ConcatenatedBeforeProject(t *testing.T) {
	t.Parallel()
	external := t.TempDir()
	writeFile(t, external, "AGENTS.md", "EXTERNAL_MARKER\n")

	project := t.TempDir()
	writeFile(t, project, "AGENTS.md", "PROJECT_MARKER\n")

	loaded, err := Load(project, "", WithContentRoots([]string{external}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	ext := strings.Index(loaded.Instruction, "EXTERNAL_MARKER")
	proj := strings.Index(loaded.Instruction, "PROJECT_MARKER")
	if ext < 0 || proj < 0 {
		t.Fatalf("both markers should be present:\n%s", loaded.Instruction)
	}
	if ext > proj {
		t.Errorf("external content should precede project content: ext=%d proj=%d\n%s", ext, proj, loaded.Instruction)
	}
}

// TestLoad_ContentRoot_MultipleInListedOrder loads two roots and asserts
// they concatenate in the order declared (before the project block).
func TestLoad_ContentRoot_MultipleInListedOrder(t *testing.T) {
	t.Parallel()
	first := t.TempDir()
	writeFile(t, first, "AGENTS.md", "FIRST_ROOT\n")
	second := t.TempDir()
	writeFile(t, second, "AGENTS.md", "SECOND_ROOT\n")

	project := t.TempDir()
	writeFile(t, project, "AGENTS.md", "PROJECT_LAST\n")

	loaded, err := Load(project, "", WithContentRoots([]string{first, second}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	a := strings.Index(loaded.Instruction, "FIRST_ROOT")
	b := strings.Index(loaded.Instruction, "SECOND_ROOT")
	p := strings.Index(loaded.Instruction, "PROJECT_LAST")
	if a < 0 || b < 0 || p < 0 {
		t.Fatalf("all markers should be present:\n%s", loaded.Instruction)
	}
	// Listed order among roots, and both roots ahead of the project block.
	if a >= b || b >= p {
		t.Errorf("expected first<second<project, got first=%d second=%d project=%d\n%s", a, b, p, loaded.Instruction)
	}
}

// TestLoad_ContentRoot_DuplicateDeclarationDedups proves the shared
// visited set dedups across content-root iterations: declaring the same
// root twice loads its primary file exactly once, not twice.
func TestLoad_ContentRoot_DuplicateDeclarationDedups(t *testing.T) {
	t.Parallel()
	external := t.TempDir()
	writeFile(t, external, "AGENTS.md", "DEDUP_MARKER\n")

	loaded, err := Load(t.TempDir(), "", WithContentRoots([]string{external, external}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if n := strings.Count(loaded.Instruction, "DEDUP_MARKER"); n != 1 {
		t.Errorf("expected the duplicated root's content exactly once, got %d:\n%s", n, loaded.Instruction)
	}
}

// TestLoad_ContentRoot_IncludeConfinedToRoot proves the confinement
// invariant still holds WITHIN a trusted island: an @include inside the
// content root that points outside it (../escape.md) is rejected. The
// operator opt-in relaxes only the cross-root ban, not confinement itself.
func TestLoad_ContentRoot_IncludeConfinedToRoot(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	external := filepath.Join(parent, "external")
	writeFile(t, external, "AGENTS.md", "@include ../escape.md\n")
	writeFile(t, parent, "escape.md", "secret outside the root\n")

	_, err := Load(t.TempDir(), "", WithContentRoots([]string{external}))
	if err == nil {
		t.Fatal("expected error: @include escaping the content root should be rejected")
	}
	if !strings.Contains(err.Error(), "escape") && !strings.Contains(err.Error(), "scope") {
		t.Errorf("error should mention scope escape: %v", err)
	}
}

// TestLoad_ContentRoot_SymlinkEscapeRejected is the symlink variant of the
// confinement check: a symlink inside a declared root that points outside
// it is still rejected by ensureWithinScope (the root is that scope's
// scopeRoot).
func TestLoad_ContentRoot_SymlinkEscapeRejected(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	external := filepath.Join(parent, "external")
	writeFile(t, external, "AGENTS.md", "@include leak.md\n")
	writeFile(t, parent, "secret.md", "top secret\n")
	if err := os.Symlink(filepath.Join(parent, "secret.md"), filepath.Join(external, "leak.md")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	_, err := Load(t.TempDir(), "", WithContentRoots([]string{external}))
	if err == nil {
		t.Fatal("expected error: symlink escaping the content root should be rejected")
	}
	if !strings.Contains(err.Error(), "escape") && !strings.Contains(err.Error(), "scope") {
		t.Errorf("error should mention scope escape: %v", err)
	}
}

// TestLoad_ContentRoot_ProjectStillConfined proves declaring a content
// root does NOT globally relax confinement: a project-scope @include to an
// undeclared sibling still errors. Trust is scoped to the declared roots
// only.
func TestLoad_ContentRoot_ProjectStillConfined(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	project := filepath.Join(parent, "project")
	writeFile(t, project, "AGENTS.md", "@include ../escape.md\n")
	writeFile(t, parent, "escape.md", "sibling secret\n")

	// A perfectly valid, unrelated content root is declared — it must not
	// make the project's own out-of-scope include suddenly legal.
	external := t.TempDir()
	writeFile(t, external, "AGENTS.md", "external\n")

	_, err := Load(project, "", WithContentRoots([]string{external}))
	if err == nil {
		t.Fatal("expected error: project @include escaping projectRoot must still be rejected")
	}
	if !strings.Contains(err.Error(), "escape") && !strings.Contains(err.Error(), "scope") {
		t.Errorf("error should mention scope escape: %v", err)
	}
}

// TestLoad_ContentRoot_MissingIsLoudError proves a declared-but-missing
// root fails loudly (operator typo), rather than silently loading nothing
// like an auto-discovered scope.
func TestLoad_ContentRoot_MissingIsLoudError(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "does-not-exist")

	_, err := Load(t.TempDir(), "", WithContentRoots([]string{missing}))
	if err == nil {
		t.Fatal("expected error for a missing content root")
	}
	if !strings.Contains(err.Error(), "content root") {
		t.Errorf("error should name the content root: %v", err)
	}
}

// TestLoad_ContentRoot_NotADirIsError proves a content root pointing at a
// regular file is rejected.
func TestLoad_ContentRoot_NotADirIsError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "afile.md", "x\n")
	notDir := filepath.Join(dir, "afile.md")

	_, err := Load(t.TempDir(), "", WithContentRoots([]string{notDir}))
	if err == nil {
		t.Fatal("expected error for a non-directory content root")
	}
	if !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("error should say not a directory: %v", err)
	}
}

// TestLoad_ContentRoot_EmptyIsNoop proves the zero/empty case matches
// today's behavior exactly — an empty (or nil) content-roots list adds no
// scope and skips empty entries.
func TestLoad_ContentRoot_EmptyIsNoop(t *testing.T) {
	t.Parallel()
	project := t.TempDir()
	writeFile(t, project, "AGENTS.md", "just project\n")

	baseline, err := Load(project, "")
	if err != nil {
		t.Fatalf("baseline Load: %v", err)
	}

	// nil, empty slice, and a slice with an empty string all behave the
	// same as no option at all — byte-identical to the baseline load.
	for _, roots := range [][]string{nil, {}, {""}} {
		loaded, err := Load(project, "", WithContentRoots(roots))
		if err != nil {
			t.Fatalf("Load(roots=%v): %v", roots, err)
		}
		if loaded.Instruction != baseline.Instruction {
			t.Errorf("roots=%v: instruction differs from baseline:\n got:%s\nwant:%s", roots, loaded.Instruction, baseline.Instruction)
		}
	}
}

// TestLoad_ContentRoot_AgentsDScanned proves an external root's AGENTS.d/
// overlay is scanned like any other scope's.
func TestLoad_ContentRoot_AgentsDScanned(t *testing.T) {
	t.Parallel()
	external := t.TempDir()
	writeFile(t, external, "AGENTS.md", "primary\n")
	writeFile(t, filepath.Join(external, agentsDirName), "10-extra.md", "OVERLAY_ENTRY\n")

	loaded, err := Load(t.TempDir(), "", WithContentRoots([]string{external}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !strings.Contains(loaded.Instruction, "OVERLAY_ENTRY") {
		t.Errorf("external AGENTS.d/ overlay not scanned:\n%s", loaded.Instruction)
	}
}

// TestLoad_ContentRoot_SourceScope records external files under the
// "content" scope so provenance (e.g. a /memory command) can attribute them.
func TestLoad_ContentRoot_SourceScope(t *testing.T) {
	t.Parallel()
	external := t.TempDir()
	writeFile(t, external, "AGENTS.md", "external\n")

	loaded, err := Load(t.TempDir(), "", WithContentRoots([]string{external}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var found bool
	for _, s := range loaded.Sources {
		if s.Scope == "content" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a Source with scope \"content\", got %+v", loaded.Sources)
	}
}
