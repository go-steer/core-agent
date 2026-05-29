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
