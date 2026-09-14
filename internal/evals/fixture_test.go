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

package evals

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixtureRoot builds a fixtures root on disk: a _shared overlay holding
// a fake tool, and one fixture whose fixture.json is the caller's.
func fixtureRoot(t *testing.T, name, fixtureJSON string) string {
	t.Helper()
	root := t.TempDir()

	mustWrite(t, filepath.Join(root, SharedDir, "bin", "kubectl"), "#!/bin/sh\necho shared\n", 0o755)

	dir := filepath.Join(root, name)
	mustWrite(t, filepath.Join(dir, "fixture.json"), fixtureJSON, 0o644)
	mustWrite(t, filepath.Join(dir, "cluster.json"),
		`{"planted":{"namespace":"payments-prod","workload":"ledger-writer"}}`, 0o644)
	mustWrite(t, filepath.Join(dir, "workspace", "NOTES.md"), "on-call notes\n", 0o644)
	return root
}

func mustWrite(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

const goodFixture = `{
  "name": "f1",
  "description": "a small world",
  "roles": {
    "world":     {"path": "cluster.json"},
    "tools":     {"path": "bin", "probes": ["kubectl"]},
    "workspace": {"path": "workspace", "probes": ["NOTES.md"]}
  },
  "path_role": "tools",
  "workdir_role": "workspace",
  "facts_from": "world#planted",
  "witnesses": {"cluster-reads": "witness/cluster-reads.log"},
  "env": {
    "EVAL_WORLD": "${role.world}",
    "EVAL_WITNESS_CLUSTER_READS": "${witness.cluster-reads}"
  }
}`

func TestLoadFixtureReadsFactsFromTheWorldFile(t *testing.T) {
	f, err := LoadFixture(fixtureRoot(t, "f1", goodFixture), "f1")
	if err != nil {
		t.Fatalf("LoadFixture: %v", err)
	}
	// The facts come out of the same file the shim renders, so the value
	// a check asserts and the value the agent can observe are one string.
	if f.Facts["workload"] != "ledger-writer" || f.Facts["namespace"] != "payments-prod" {
		t.Fatalf("facts = %+v", f.Facts)
	}
}

func TestLoadFixtureRejects(t *testing.T) {
	tests := []struct {
		name string
		json string
		want string
	}{
		{
			name: "no witnesses",
			json: `{"name":"f1","description":"d","roles":{"workspace":{"path":"workspace"}},"workdir_role":"workspace","facts_from":"workspace#planted","witnesses":{}}`,
			want: "graded on narration",
		},
		{
			name: "workdir_role is not a role",
			json: `{"name":"f1","description":"d","roles":{"workspace":{"path":"workspace"}},"workdir_role":"nope","facts_from":"workspace#planted","witnesses":{"w":"w.log"}}`,
			want: "is not a declared role",
		},
		{
			name: "escaping role path",
			json: `{"name":"f1","description":"d","roles":{"workspace":{"path":"../elsewhere"}},"workdir_role":"workspace","facts_from":"workspace#planted","witnesses":{"w":"w.log"}}`,
			want: "must not escape",
		},
		{
			name: "escaping witness path",
			json: `{"name":"f1","description":"d","roles":{"workspace":{"path":"workspace"}},"workdir_role":"workspace","facts_from":"workspace#planted","witnesses":{"w":"/tmp/w.log"}}`,
			want: "must not escape",
		},
		{
			name: "facts_from without a key",
			json: `{"name":"f1","description":"d","roles":{"world":{"path":"cluster.json"}},"workdir_role":"world","facts_from":"world","witnesses":{"w":"w.log"}}`,
			want: `must be "<role>#<key>"`,
		},
		{
			name: "facts_from names a missing key",
			json: `{"name":"f1","description":"d","roles":{"world":{"path":"cluster.json"}},"workdir_role":"world","facts_from":"world#absent","witnesses":{"w":"w.log"}}`,
			want: "has no top-level",
		},
		{
			name: "no description",
			json: `{"name":"f1","roles":{"world":{"path":"cluster.json"}},"workdir_role":"world","facts_from":"world#planted","witnesses":{"w":"w.log"}}`,
			want: "description is required",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadFixture(fixtureRoot(t, "f1", tt.json), "f1")
			if err == nil {
				t.Fatal("want an error, got none")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error %q does not mention %q", err, tt.want)
			}
		})
	}
}

// The directory is the identity, so a fixture that kept its old name in
// the file after being renamed cannot quietly keep answering to it.
func TestLoadFixtureRejectsANameThatDisagreesWithTheDirectory(t *testing.T) {
	// The directory is "renamed"; the file still says "f1".
	root := fixtureRoot(t, "renamed", goodFixture)
	_, err := LoadFixture(root, "renamed")
	if err == nil || !strings.Contains(err.Error(), "the directory is the identity") {
		t.Fatalf("want an identity error, got %v", err)
	}
}

func TestLoadFixtureRefusesTheSharedOverlay(t *testing.T) {
	_, err := LoadFixture(fixtureRoot(t, "f1", goodFixture), SharedDir)
	if err == nil || !strings.Contains(err.Error(), "shared overlay") {
		t.Fatalf("want a refusal, got %v", err)
	}
}

func TestMaterializeLaysSharedDownFirst(t *testing.T) {
	root := fixtureRoot(t, "f1", goodFixture)
	f, err := LoadFixture(root, "f1")
	if err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	w, err := f.Materialize(dst)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}

	// The shim arrives from _shared without the fixture naming its path.
	shim := filepath.Join(dst, "bin", "kubectl")
	info, err := os.Stat(shim)
	if err != nil {
		t.Fatalf("shared shim not materialized: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("shim lost its executable bit: %v", info.Mode())
	}

	if w.Workdir != filepath.Join(dst, "workspace") {
		t.Fatalf("Workdir = %s", w.Workdir)
	}
	if w.PathDir != filepath.Join(dst, "bin") {
		t.Fatalf("PathDir = %s", w.PathDir)
	}
	if got, want := w.Env["EVAL_WORLD"], filepath.Join(dst, "cluster.json"); got != want {
		t.Fatalf("EVAL_WORLD = %q, want %q", got, want)
	}
	if got, want := w.Env["EVAL_WITNESS_CLUSTER_READS"], filepath.Join(dst, "witness", "cluster-reads.log"); got != want {
		t.Fatalf("witness env = %q, want %q", got, want)
	}
}

// A fixture inherits the shared shim by default and overrides it by
// shipping a file at the same relative path. One rule, no new concept.
func TestMaterializeLetsAFixtureOverrideTheSharedOverlay(t *testing.T) {
	root := fixtureRoot(t, "f1", goodFixture)
	mustWrite(t, filepath.Join(root, "f1", "bin", "kubectl"), "#!/bin/sh\necho mine\n", 0o755)

	f, err := LoadFixture(root, "f1")
	if err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	if _, err := f.Materialize(dst); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(dst, "bin", "kubectl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "echo mine") {
		t.Fatalf("the fixture's own file should win, got %q", body)
	}
}

// Probes are what entitle an absence check to be called a violation: if
// the tool was never there, a check that fails on its absence is an
// environment failure the agent is being blamed for.
func TestMaterializeFailsWhenAProbeIsMissing(t *testing.T) {
	root := fixtureRoot(t, "f1", goodFixture)
	if err := os.Remove(filepath.Join(root, SharedDir, "bin", "kubectl")); err != nil {
		t.Fatal(err)
	}
	f, err := LoadFixture(root, "f1")
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.Materialize(t.TempDir())
	if err == nil || !strings.Contains(err.Error(), `probe "kubectl" not present`) {
		t.Fatalf("want a probe failure, got %v", err)
	}
}

// Absent and touched-but-empty are different findings, and verify.go
// spends the difference. Pre-creating the file would erase it.
func TestWitnessesAreNotPreCreated(t *testing.T) {
	root := fixtureRoot(t, "f1", goodFixture)
	f, err := LoadFixture(root, "f1")
	if err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	w, err := f.Materialize(dst)
	if err != nil {
		t.Fatal(err)
	}

	if _, present := w.ReadWitness("cluster-reads"); present {
		t.Fatal("an untouched witness must report absent")
	}

	mustWrite(t, filepath.Join(dst, "witness", "cluster-reads.log"), "", 0o644)
	text, present := w.ReadWitness("cluster-reads")
	if !present || text != "" {
		t.Fatalf("a touched-but-empty witness must report present: %q, %v", text, present)
	}

	if _, present := w.ReadWitness("no-such-witness"); present {
		t.Fatal("an undeclared witness must report absent")
	}
}

func TestListFixturesSkipsTheSharedOverlay(t *testing.T) {
	root := fixtureRoot(t, "f1", goodFixture)
	if err := os.MkdirAll(filepath.Join(root, ".hidden"), 0o755); err != nil {
		t.Fatal(err)
	}
	names, err := ListFixtures(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != "f1" {
		t.Fatalf("ListFixtures = %v, want [f1]", names)
	}
}

func TestCopyTreeRefusesASymlink(t *testing.T) {
	root := fixtureRoot(t, "f1", goodFixture)
	link := filepath.Join(root, "f1", "workspace", "escape")
	if err := os.Symlink("/etc/passwd", link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	f, err := LoadFixture(root, "f1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Materialize(t.TempDir()); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("want a symlink refusal, got %v", err)
	}
}
