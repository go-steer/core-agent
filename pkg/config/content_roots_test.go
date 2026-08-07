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

package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestContentRoots_RoundTrip proves the content_roots list survives a
// Load (JSON → struct) with order preserved — the field the cmd wiring and
// the B″ recipe read.
func TestContentRoots_RoundTrip(t *testing.T) {
	t.Parallel()

	body := `{
		"version": 1,
		"model": { "name": "claude-opus-4-7" },
		"content_roots": ["../kube-agents/agents/platform", "/opt/shared/agents"]
	}`

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ConfigFileName), []byte(body), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []string{"../kube-agents/agents/platform", "/opt/shared/agents"}
	if !reflect.DeepEqual(cfg.ContentRoots, want) {
		t.Errorf("content_roots round-trip mismatch:\n got:  %+v\n want: %+v", cfg.ContentRoots, want)
	}
}

// TestContentRoots_OmittedIsNil proves an absent content_roots section leaves
// the field nil — the "today's behavior exactly" default the loader treats as
// a no-op.
func TestContentRoots_OmittedIsNil(t *testing.T) {
	t.Parallel()

	body := `{ "version": 1, "model": { "name": "claude-opus-4-7" } }`
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ConfigFileName), []byte(body), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ContentRoots != nil {
		t.Errorf("content_roots should be nil when absent, got: %+v", cfg.ContentRoots)
	}
}

// TestValidate_ContentRoots exercises the Validate case: non-empty entries
// pass (Validate is environment-free — no existence check), an empty or
// whitespace-only entry is rejected with its index.
func TestValidate_ContentRoots(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		roots   []string
		wantErr bool
	}{
		{"nil", nil, false},
		{"empty list", []string{}, false},
		{"one relative", []string{"../kube-agents/agents/platform"}, false},
		{"one absolute", []string{"/opt/agents"}, false},
		{"missing-on-disk is fine at validate time", []string{"/does/not/exist"}, false},
		{"multiple", []string{"a", "b", "c"}, false},
		{"empty string rejected", []string{""}, true},
		{"whitespace-only rejected", []string{"   "}, true},
		{"empty amid valid rejected", []string{"ok", "", "also-ok"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			cfg := DefaultConfig()
			cfg.ContentRoots = c.roots
			err := cfg.Validate()
			if c.wantErr && err == nil {
				t.Errorf("Validate(content_roots=%v): want error, got nil", c.roots)
			}
			if !c.wantErr && err != nil {
				t.Errorf("Validate(content_roots=%v): unexpected error: %v", c.roots, err)
			}
		})
	}
}
