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

package agentenv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadManifestMissing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	m, err := LoadManifest(dir)
	if err != nil {
		t.Fatalf("LoadManifest(empty dir) unexpected error: %v", err)
	}
	if m != nil {
		t.Fatalf("LoadManifest(empty dir) = %+v; want nil (no manifest present)", m)
	}
}

func TestLoadManifestEmptyAgentsDir(t *testing.T) {
	t.Parallel()
	m, err := LoadManifest("")
	if err != nil {
		t.Fatalf("LoadManifest(\"\") unexpected error: %v", err)
	}
	if m != nil {
		t.Fatalf("LoadManifest(\"\") = %+v; want nil", m)
	}
}

func TestLoadManifestYAML(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	body := `version: 1
env:
  - name: GCP_PROJECT
    required: true
    description: GCP project the daemon operates in
    used_by: [AGENTS.md]
  - name: ONCALL_EMAIL
    required: false
    default: unassigned@example.com
    description: CC address for escalations
  - name: SLACK_TOKEN
    required: true
    sensitive: true
    description: Bearer token for Slack MCP
`
	if err := os.WriteFile(filepath.Join(dir, ManifestFileYAML), []byte(body), 0o600); err != nil {
		t.Fatalf("write env.yaml: %v", err)
	}
	m, err := LoadManifest(dir)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if m == nil {
		t.Fatal("LoadManifest returned nil; want parsed manifest")
	}
	if m.Version != 1 {
		t.Errorf("Version = %d; want 1", m.Version)
	}
	if got, want := len(m.Env), 3; got != want {
		t.Fatalf("len(Env) = %d; want %d", got, want)
	}
	// Spot-check that YAML parsing preserved the fields.
	if m.Env[2].Name != "SLACK_TOKEN" || !m.Env[2].Sensitive {
		t.Errorf("SLACK_TOKEN entry not marked sensitive: %+v", m.Env[2])
	}
	if m.Env[1].Default != "unassigned@example.com" {
		t.Errorf("ONCALL_EMAIL default = %q; want unassigned@example.com", m.Env[1].Default)
	}
}

func TestLoadManifestJSON(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	body := `{
  "version": 1,
  "env": [
    { "name": "FOO", "required": true, "description": "the foo" },
    { "name": "BAR", "default": "baz", "sensitive": false }
  ]
}
`
	if err := os.WriteFile(filepath.Join(dir, ManifestFileJSON), []byte(body), 0o600); err != nil {
		t.Fatalf("write env.json: %v", err)
	}
	m, err := LoadManifest(dir)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if m == nil {
		t.Fatal("LoadManifest returned nil; want parsed manifest")
	}
	if len(m.Env) != 2 {
		t.Fatalf("len(Env) = %d; want 2", len(m.Env))
	}
	if m.Env[0].Name != "FOO" || !m.Env[0].Required {
		t.Errorf("FOO entry: %+v; expected required=true", m.Env[0])
	}
	if m.Env[1].Default != "baz" {
		t.Errorf("BAR default = %q; want baz", m.Env[1].Default)
	}
}

func TestLoadManifestBothPresentIsError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ManifestFileYAML), []byte("env: []"), 0o600); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ManifestFileJSON), []byte(`{"env":[]}`), 0o600); err != nil {
		t.Fatalf("write json: %v", err)
	}
	_, err := LoadManifest(dir)
	if err == nil {
		t.Fatal("LoadManifest with both files present should error")
	}
	if !strings.Contains(err.Error(), "pick one") {
		t.Errorf("error should point out ambiguity; got %v", err)
	}
}

func TestManifestValidate(t *testing.T) {
	tests := []struct {
		name    string
		m       *Manifest
		wantErr string // empty = expect no error
	}{
		{"nil is fine", nil, ""},
		{"empty is fine", &Manifest{}, ""},
		{"valid v1", &Manifest{Version: 1, Env: []Entry{{Name: "OK"}}}, ""},
		{"missing version treated as v1", &Manifest{Env: []Entry{{Name: "OK"}}}, ""},
		{"future version rejected", &Manifest{Version: 99, Env: []Entry{{Name: "OK"}}}, "exceeds supported"},
		{"empty name rejected", &Manifest{Env: []Entry{{Name: ""}}}, "name is required"},
		{"leading-digit name rejected", &Manifest{Env: []Entry{{Name: "1FOO"}}}, "valid identifier"},
		{"space in name rejected", &Manifest{Env: []Entry{{Name: "FOO BAR"}}}, "valid identifier"},
		{"duplicate names rejected", &Manifest{Env: []Entry{{Name: "FOO"}, {Name: "FOO"}}}, "duplicate name"},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.m.Validate()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("Validate: unexpected error %v", err)
			case tc.wantErr != "" && err == nil:
				t.Errorf("Validate: want error containing %q; got nil", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Errorf("Validate: want error containing %q; got %v", tc.wantErr, err)
			}
		})
	}
}
