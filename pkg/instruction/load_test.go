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
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoad_NothingFound(t *testing.T) {
	t.Parallel()
	loaded, err := Load(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.Empty() {
		t.Errorf("expected empty Loaded, got %+v", loaded)
	}
}

func TestLoad_ProjectFallbackChain(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		fileName     string
		wantInPrompt string
	}{
		{"agents.md", "AGENTS.md", "AGENTS body"},
		{"claude.md", "CLAUDE.md", "CLAUDE body"},
		{"gemini.md", "GEMINI.md", "GEMINI body"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			writeFile(t, root, tc.fileName, tc.wantInPrompt)
			loaded, err := Load(root, "")
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(loaded.Instruction, tc.wantInPrompt) {
				t.Errorf("instruction missing %q:\n%s", tc.wantInPrompt, loaded.Instruction)
			}
			if len(loaded.Sources) != 1 || loaded.Sources[0].Scope != "project" {
				t.Errorf("expected one project source, got %+v", loaded.Sources)
			}
		})
	}
}

func TestLoad_FirstMatchWins(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, root, "AGENTS.md", "primary AGENTS")
	writeFile(t, root, "CLAUDE.md", "secondary CLAUDE")
	writeFile(t, root, "GEMINI.md", "tertiary GEMINI")

	loaded, err := Load(root, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(loaded.Instruction, "primary AGENTS") {
		t.Errorf("AGENTS.md not chosen as first match")
	}
	if strings.Contains(loaded.Instruction, "secondary CLAUDE") {
		t.Errorf("CLAUDE.md should be ignored when AGENTS.md is present")
	}
}

func TestLoad_UserAndProjectConcatenated(t *testing.T) {
	t.Parallel()
	user := t.TempDir()
	project := t.TempDir()
	writeFile(t, user, "AGENTS.md", "USER stuff")
	writeFile(t, project, "AGENTS.md", "PROJECT stuff")

	loaded, err := Load(project, user)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(loaded.Instruction, "USER stuff") || !strings.Contains(loaded.Instruction, "PROJECT stuff") {
		t.Fatalf("expected both user + project content:\n%s", loaded.Instruction)
	}
	if strings.Index(loaded.Instruction, "USER stuff") >= strings.Index(loaded.Instruction, "PROJECT stuff") {
		t.Errorf("user memory should precede project memory")
	}
	if len(loaded.Sources) != 2 {
		t.Errorf("expected 2 sources, got %+v", loaded.Sources)
	}
}

func TestLoad_TruncatesLargeFile(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	body := strings.Repeat("x", maxFileBytes+1024)
	writeFile(t, root, "AGENTS.md", body)

	loaded, err := Load(root, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(loaded.Instruction, "truncated by core-agent") {
		t.Errorf("expected truncation marker:\n%s", loaded.Instruction[:200])
	}
	if !loaded.Sources[0].Truncated {
		t.Errorf("Source.Truncated should be true")
	}
}

func TestLoad_OnlyUser(t *testing.T) {
	t.Parallel()
	user := t.TempDir()
	writeFile(t, user, "AGENTS.md", "user only")
	loaded, err := Load("", user)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(loaded.Instruction, "user only") {
		t.Errorf("expected user content:\n%s", loaded.Instruction)
	}
	if len(loaded.Sources) != 1 || loaded.Sources[0].Scope != "user" {
		t.Errorf("expected single user source, got %+v", loaded.Sources)
	}
}

// --- v2: @include directive --------------------------------------------------

func TestLoad_Include_HappyPath(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, root, "AGENTS.md", "parent line\n@include child.md\ntail line\n")
	writeFile(t, root, "child.md", "child body\n")

	loaded, err := Load(root, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(loaded.Instruction, "parent line") {
		t.Errorf("missing parent: %s", loaded.Instruction)
	}
	if !strings.Contains(loaded.Instruction, "child body") {
		t.Errorf("missing included body: %s", loaded.Instruction)
	}
	if !strings.Contains(loaded.Instruction, "tail line") {
		t.Errorf("missing tail: %s", loaded.Instruction)
	}
	// The literal @include line should not appear in the assembled prompt.
	if strings.Contains(loaded.Instruction, "@include child.md") {
		t.Errorf("@include directive should be expanded, not left literal:\n%s", loaded.Instruction)
	}
	if len(loaded.Sources) != 2 {
		t.Errorf("expected 2 sources (parent + child), got %d: %+v", len(loaded.Sources), loaded.Sources)
	}
}

func TestLoad_Include_Nested(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, root, "AGENTS.md", "A\n@include b.md\n")
	writeFile(t, root, "b.md", "B\n@include c.md\n")
	writeFile(t, root, "c.md", "C\n")

	loaded, err := Load(root, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"A", "B", "C"} {
		if !strings.Contains(loaded.Instruction, want) {
			t.Errorf("missing %q: %s", want, loaded.Instruction)
		}
	}
}

func TestLoad_Include_RelativeToContainingFile(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, root, "AGENTS.md", "@include sub/leaf.md\n")
	writeFile(t, filepath.Join(root, "sub"), "leaf.md", "from sub\n@include sibling.md\n")
	writeFile(t, filepath.Join(root, "sub"), "sibling.md", "from sub/sibling\n")

	loaded, err := Load(root, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(loaded.Instruction, "from sub") || !strings.Contains(loaded.Instruction, "from sub/sibling") {
		t.Errorf("relative-to-container resolution broken:\n%s", loaded.Instruction)
	}
}

func TestLoad_Include_Cycle(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	// A → B → A. With dedup, the second visit to A is a silent skip,
	// so the load completes — but B's body still appears once.
	writeFile(t, root, "AGENTS.md", "A start\n@include b.md\nA end\n")
	writeFile(t, root, "b.md", "B start\n@include AGENTS.md\nB end\n")

	loaded, err := Load(root, "")
	if err != nil {
		t.Fatalf("cycles should be handled by dedup, not error: %v", err)
	}
	// Each marker should appear exactly once — dedup breaks the loop.
	for _, want := range []string{"A start", "A end", "B start", "B end"} {
		got := strings.Count(loaded.Instruction, want)
		if got != 1 {
			t.Errorf("%q should appear once, got %d:\n%s", want, got, loaded.Instruction)
		}
	}
}

func TestLoad_Include_MissingTarget(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, root, "AGENTS.md", "@include nope.md\n")

	_, err := Load(root, "")
	if err == nil {
		t.Fatal("expected error for missing @include target")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should mention 'not found': %v", err)
	}
}

func TestLoad_Include_RejectAbsolute(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, root, "AGENTS.md", "@include /etc/passwd\n")

	_, err := Load(root, "")
	if err == nil {
		t.Fatal("expected error for absolute @include path")
	}
	if !strings.Contains(err.Error(), "absolute") {
		t.Errorf("error should mention 'absolute': %v", err)
	}
}

func TestLoad_Include_RejectURL(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, root, "AGENTS.md", "@include https://example.com/foo.md\n")

	_, err := Load(root, "")
	if err == nil {
		t.Fatal("expected error for URL @include")
	}
	if !strings.Contains(err.Error(), "URL") {
		t.Errorf("error should mention 'URL': %v", err)
	}
}

func TestLoad_Include_RejectEscape(t *testing.T) {
	t.Parallel()
	// Build a project root inside a parent so ../escape.md actually
	// resolves to a real file outside the scope.
	parent := t.TempDir()
	root := filepath.Join(parent, "project")
	writeFile(t, root, "AGENTS.md", "@include ../escape.md\n")
	writeFile(t, parent, "escape.md", "secret\n")

	_, err := Load(root, "")
	if err == nil {
		t.Fatal("expected error for path escaping scope root")
	}
	if !strings.Contains(err.Error(), "escape") && !strings.Contains(err.Error(), "scope") {
		t.Errorf("error should mention scope escape: %v", err)
	}
}

func TestLoad_Include_DepthExceeded(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	// Build a chain longer than maxIncludeDepth: AGENTS.md → 0.md → 1.md → ...
	writeFile(t, root, "AGENTS.md", "@include 0.md\n")
	for i := 0; i < maxIncludeDepth+2; i++ {
		writeFile(t, root, fmt.Sprintf("%d.md", i), fmt.Sprintf("body %d\n@include %d.md\n", i, i+1))
	}
	writeFile(t, root, fmt.Sprintf("%d.md", maxIncludeDepth+2), "leaf\n")

	_, err := Load(root, "")
	if err == nil {
		t.Fatal("expected depth-exceeded error")
	}
	if !strings.Contains(err.Error(), "depth") {
		t.Errorf("error should mention depth: %v", err)
	}
}

func TestLoad_Include_InsideCodeFenceLeftLiteral(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	body := "before\n" +
		"```\n" +
		"@include should-not-resolve.md\n" +
		"```\n" +
		"after\n"
	writeFile(t, root, "AGENTS.md", body)

	loaded, err := Load(root, "")
	if err != nil {
		t.Fatalf("@include inside code fence should not be processed, got error: %v", err)
	}
	if !strings.Contains(loaded.Instruction, "@include should-not-resolve.md") {
		t.Errorf("@include in code fence should remain literal:\n%s", loaded.Instruction)
	}
}

func TestLoad_Include_InsideTildeFenceLeftLiteral(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	body := "~~~\n@include nope.md\n~~~\n"
	writeFile(t, root, "AGENTS.md", body)

	if _, err := Load(root, ""); err != nil {
		t.Fatalf("tilde fence should suppress @include: %v", err)
	}
}

func TestLoad_Include_OnlyOnOwnLine(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	// @include embedded in prose should NOT be processed.
	writeFile(t, root, "AGENTS.md", "see @include foo.md for details\n")

	loaded, err := Load(root, "")
	if err != nil {
		t.Fatalf("prose @include should not be processed: %v", err)
	}
	if !strings.Contains(loaded.Instruction, "see @include foo.md for details") {
		t.Errorf("prose @include should remain literal:\n%s", loaded.Instruction)
	}
}

// --- v2: AGENTS.d/ directory -------------------------------------------------

func TestLoad_AgentsDir_LexicalOrder(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, root, "AGENTS.md", "primary\n")
	dir := filepath.Join(root, agentsDirName)
	writeFile(t, dir, "20-second.md", "TWO\n")
	writeFile(t, dir, "10-first.md", "ONE\n")
	writeFile(t, dir, "30-third.md", "THREE\n")

	loaded, err := Load(root, "")
	if err != nil {
		t.Fatal(err)
	}
	got := loaded.Instruction
	iOne := strings.Index(got, "ONE")
	iTwo := strings.Index(got, "TWO")
	iThree := strings.Index(got, "THREE")
	if iOne < 0 || iTwo < 0 || iThree < 0 {
		t.Fatalf("missing one of ONE/TWO/THREE:\n%s", got)
	}
	if iOne >= iTwo || iTwo >= iThree {
		t.Errorf("AGENTS.d files not lexically ordered:\n%s", got)
	}
	// Primary file comes before AGENTS.d/ entries.
	if strings.Index(got, "primary") >= iOne {
		t.Errorf("primary file should precede AGENTS.d entries:\n%s", got)
	}
}

func TestLoad_AgentsDir_IgnoresNonMarkdown(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dir := filepath.Join(root, agentsDirName)
	writeFile(t, dir, "10-keep.md", "KEEP\n")
	writeFile(t, dir, "20-drop.txt", "DROP\n")
	writeFile(t, dir, "README.MD", "case-mismatch\n") // lowercase .md only
	writeFile(t, dir, ".hidden.md", "HIDDEN\n")

	loaded, err := Load(root, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(loaded.Instruction, "KEEP") {
		t.Errorf("expected KEEP: %s", loaded.Instruction)
	}
	if strings.Contains(loaded.Instruction, "DROP") {
		t.Errorf("non-.md file should be skipped: %s", loaded.Instruction)
	}
	if strings.Contains(loaded.Instruction, "case-mismatch") {
		t.Errorf("uppercase .MD should be skipped (lowercase only): %s", loaded.Instruction)
	}
	if strings.Contains(loaded.Instruction, "HIDDEN") {
		t.Errorf("hidden file should be skipped: %s", loaded.Instruction)
	}
}

func TestLoad_AgentsDir_IgnoresSubdirs(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dir := filepath.Join(root, agentsDirName)
	writeFile(t, dir, "10-top.md", "TOP\n")
	writeFile(t, filepath.Join(dir, "subdir"), "20-nested.md", "NESTED\n")

	loaded, err := Load(root, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(loaded.Instruction, "TOP") {
		t.Errorf("expected TOP: %s", loaded.Instruction)
	}
	if strings.Contains(loaded.Instruction, "NESTED") {
		t.Errorf("nested file should not be loaded (top-level only): %s", loaded.Instruction)
	}
}

func TestLoad_AgentsDir_BothScopes(t *testing.T) {
	t.Parallel()
	user := t.TempDir()
	project := t.TempDir()
	writeFile(t, filepath.Join(user, agentsDirName), "10-user.md", "USER-DROPIN\n")
	writeFile(t, filepath.Join(project, agentsDirName), "10-proj.md", "PROJECT-DROPIN\n")

	loaded, err := Load(project, user)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"USER-DROPIN", "PROJECT-DROPIN"} {
		if !strings.Contains(loaded.Instruction, want) {
			t.Errorf("missing %q:\n%s", want, loaded.Instruction)
		}
	}
	// User precedes project.
	if strings.Index(loaded.Instruction, "USER-DROPIN") >= strings.Index(loaded.Instruction, "PROJECT-DROPIN") {
		t.Errorf("user AGENTS.d should precede project:\n%s", loaded.Instruction)
	}
}

func TestLoad_AgentsDir_AbsentIsNotError(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, root, "AGENTS.md", "just primary\n")
	loaded, err := Load(root, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(loaded.Instruction, "just primary") {
		t.Errorf("primary should still load with no AGENTS.d: %s", loaded.Instruction)
	}
}

// --- v2: dedup ---------------------------------------------------------------

func TestLoad_Dedup_IncludeAndDir(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dir := filepath.Join(root, agentsDirName)
	// shared.md is BOTH @included and present in AGENTS.d/. Should
	// load once. AGENTS.md is read first (primary), so its @include
	// pulls shared.md in; the AGENTS.d/ scan finds it already
	// visited and skips silently.
	writeFile(t, root, "AGENTS.md", "@include "+filepath.Join(agentsDirName, "10-shared.md")+"\n")
	writeFile(t, dir, "10-shared.md", "SHARED-BODY\n")

	loaded, err := Load(root, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(loaded.Instruction, "SHARED-BODY"); got != 1 {
		t.Errorf("SHARED-BODY should appear exactly once, got %d:\n%s", got, loaded.Instruction)
	}
	// Source list also dedupes: one entry for shared, one for AGENTS.md = 2 total.
	if len(loaded.Sources) != 2 {
		t.Errorf("expected 2 sources, got %d: %+v", len(loaded.Sources), loaded.Sources)
	}
}

func TestLoad_Dedup_IncludedTwice(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, root, "AGENTS.md", "@include x.md\n@include x.md\n")
	writeFile(t, root, "x.md", "X-BODY\n")

	loaded, err := Load(root, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(loaded.Instruction, "X-BODY"); got != 1 {
		t.Errorf("X-BODY should appear once, got %d:\n%s", got, loaded.Instruction)
	}
}

// --- v2: frontmatter & UTF-8 -------------------------------------------------

func TestLoad_StripsFrontmatter(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, root, "AGENTS.md", "---\ntitle: foo\ntags: [a, b]\n---\nactual body\n")

	loaded, err := Load(root, "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(loaded.Instruction, "title: foo") {
		t.Errorf("frontmatter should be stripped:\n%s", loaded.Instruction)
	}
	if !strings.Contains(loaded.Instruction, "actual body") {
		t.Errorf("body should remain after frontmatter strip:\n%s", loaded.Instruction)
	}
}

func TestLoad_FrontmatterOnlyAtFileStart(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	// `---` later in the file (markdown hr) should NOT be treated as frontmatter.
	writeFile(t, root, "AGENTS.md", "intro\n\n---\nthen a section\n---\nmore\n")

	loaded, err := Load(root, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(loaded.Instruction, "intro") || !strings.Contains(loaded.Instruction, "then a section") {
		t.Errorf("mid-file --- should not strip:\n%s", loaded.Instruction)
	}
}

func TestLoad_RejectsNonUTF8(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	// invalid UTF-8: lone continuation byte.
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte{0xc3, 0x28}, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(root, "")
	if err == nil {
		t.Fatal("expected UTF-8 validation error")
	}
	if !strings.Contains(err.Error(), "UTF-8") {
		t.Errorf("error should mention UTF-8: %v", err)
	}
}

// --- v2: cross-scope dedup ---------------------------------------------------

func TestLoad_Dedup_CrossScope(t *testing.T) {
	t.Parallel()
	// User AGENTS.d/ and project AGENTS.d/ both @include the same
	// absolute file via... actually with relative-only includes,
	// the only way two scopes "share" a file is via a symlink. Test
	// that with a symlink from project's AGENTS.d into a real file
	// that user also pulls in.
	user := t.TempDir()
	project := t.TempDir()
	shared := t.TempDir()
	writeFile(t, shared, "common.md", "COMMON-BODY\n")

	// Symlink common.md into both scopes' AGENTS.d directories.
	for _, scopeRoot := range []string{user, project} {
		dir := filepath.Join(scopeRoot, agentsDirName)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(shared, "common.md"), filepath.Join(dir, "10-common.md")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
	}

	loaded, err := Load(project, user)
	if err != nil {
		t.Fatalf("symlink within scope is allowed: %v", err)
	}
	if got := strings.Count(loaded.Instruction, "COMMON-BODY"); got != 1 {
		t.Errorf("cross-scope dedup failed; expected 1 occurrence, got %d:\n%s", got, loaded.Instruction)
	}
}

// --- v2: include directive parsing edge cases --------------------------------

func TestLoad_Include_RequiresSpaceAfterDirective(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	// "@includefoo.md" must NOT match — needs a separator.
	writeFile(t, root, "AGENTS.md", "@includefoo.md\n")
	writeFile(t, root, "foo.md", "should-not-load\n")

	loaded, err := Load(root, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(loaded.Instruction, "should-not-load") {
		t.Errorf("@includefoo.md should not match @include directive:\n%s", loaded.Instruction)
	}
}

func TestLoad_Include_LeadingWhitespaceAllowed(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, root, "AGENTS.md", "  @include child.md\n")
	writeFile(t, root, "child.md", "CHILD\n")

	loaded, err := Load(root, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(loaded.Instruction, "CHILD") {
		t.Errorf("@include with leading whitespace should work:\n%s", loaded.Instruction)
	}
}
