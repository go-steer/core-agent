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
	"runtime"
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/childenv"
)

const includeSecret = "tok-1201-include-secret"

// tableProject lays out a project whose AGENTS.md includes users.json,
// the shape an agent holding only write_file can produce.
func tableProject(t *testing.T) (project, table string) {
	t.Helper()
	project = t.TempDir()
	table = filepath.Join(project, "users.json")
	if err := os.WriteFile(table, []byte(`{"version":1,"users":[{"identity":"a","token":"`+includeSecret+`"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	writeFile(t, project, "AGENTS.md", "# Notes\n\n@include users.json\n")
	return project, table
}

// #1201: the loader splices @include targets into the system prompt and
// takes no permission gate, so it consults the process-wide credential
// set itself. The control — an identical project whose table is not
// withheld — proves the include would otherwise have landed.
func TestIncludeOfACredentialFileIsRefused(t *testing.T) {
	t.Parallel()
	control, _ := tableProject(t)
	loaded, err := Load(control, t.TempDir())
	if err != nil || !strings.Contains(loaded.Instruction, includeSecret) {
		t.Fatalf("control: the include did not land (err=%v), so the refusal below proves nothing", err)
	}

	project, table := tableProject(t)
	childenv.WithholdFiles(table)
	loaded, err = Load(project, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "credential files") {
		t.Fatalf("an @include of a withheld credential file loaded (err=%v)", err)
	}
	if strings.Contains(loaded.Instruction, includeSecret) || strings.Contains(err.Error(), includeSecret) {
		t.Fatalf("the token reached the prompt or the error")
	}
}

// The AGENTS.d overlay reads every *.md in the directory; a symlink
// named like an overlay must not launder the table in either.
func TestOverlaySymlinkToACredentialFileIsRefused(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics")
	}
	project, table := tableProject(t)
	writeFile(t, project, "AGENTS.md", "# Notes\n")
	if err := os.MkdirAll(filepath.Join(project, "AGENTS.d"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(table, filepath.Join(project, "AGENTS.d", "10-notes.md")); err != nil {
		t.Fatal(err)
	}
	childenv.WithholdFiles(table)
	loaded, err := Load(project, t.TempDir())
	if strings.Contains(loaded.Instruction, includeSecret) {
		t.Fatalf("an AGENTS.d symlink spliced the withheld table into the prompt (err=%v)", err)
	}
}
