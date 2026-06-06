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

package attach

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadAgentCardFile_Missing(t *testing.T) {
	t.Parallel()
	cfg, present, err := LoadAgentCardFile(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("missing file = %v, want nil error", err)
	}
	if present {
		t.Fatalf("missing file present=true, want false")
	}
	if cfg.Enabled() {
		t.Fatalf("missing file produced enabled config %+v", cfg)
	}
}

func TestLoadAgentCardFile_Valid(t *testing.T) {
	t.Parallel()
	path := writeTemp(t, `{
		"version": 1,
		"name": "agent-x",
		"description": "Does stuff.",
		"external_url": "https://example.invalid:7777",
		"agent_version": "v1.2.3",
		"documentation_url": "https://example.invalid/docs",
		"provider": {"organization": "Org", "url": "https://example.invalid"},
		"extra_skills": [
			{
				"id": "skill-a",
				"name": "Skill A",
				"description": "Does A.",
				"tags": ["t1", "t2"],
				"examples": ["example 1"]
			}
		]
	}`)
	cfg, present, err := LoadAgentCardFile(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !present {
		t.Fatalf("present=false on valid file")
	}
	if !cfg.Enabled() {
		t.Fatalf("config not Enabled: %+v", cfg)
	}
	if cfg.Name != "agent-x" || cfg.Description != "Does stuff." || cfg.Version != "v1.2.3" {
		t.Errorf("scalar fields: %+v", cfg)
	}
	if cfg.Provider.Organization != "Org" || cfg.Provider.URL != "https://example.invalid" {
		t.Errorf("provider: %+v", cfg.Provider)
	}
	if len(cfg.ExtraSkills) != 1 {
		t.Fatalf("got %d skills, want 1", len(cfg.ExtraSkills))
	}
	s := cfg.ExtraSkills[0]
	if s.ID != "skill-a" || s.Name != "Skill A" || len(s.Tags) != 2 || s.Tags[0] != "t1" {
		t.Errorf("skill: %+v", s)
	}
}

func TestLoadAgentCardFile_Errors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		body    string
		wantErr string
	}{
		{"bad json", `{not json`, "parse agent-card"},
		{"missing version", `{"description":"x","external_url":"https://x"}`, "unsupported version 0"},
		{"wrong version", `{"version":2,"description":"x","external_url":"https://x"}`, "unsupported version 2"},
		{"unknown field", `{"version":1,"description":"x","external_url":"https://x","unknown_field":"v"}`, "unknown_field"},
		{"provider org only", `{"version":1,"description":"x","provider":{"organization":"o"}}`, "Provider.Organization and Provider.URL"},
		{"provider url only", `{"version":1,"description":"x","provider":{"url":"https://o"}}`, "Provider.Organization and Provider.URL"},
		{"extra_skills without description", `{"version":1,"extra_skills":[{"id":"x","name":"X","description":"d"}]}`, "side-channel"},
		{"skill missing id", `{"version":1,"description":"x","extra_skills":[{"name":"X","description":"d"}]}`, "ExtraSkills[0].ID"},
		{"skill missing name", `{"version":1,"description":"x","extra_skills":[{"id":"i","description":"d"}]}`, "ExtraSkills[0].Name"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := writeTemp(t, tc.body)
			_, _, err := LoadAgentCardFile(path)
			if err == nil {
				t.Fatalf("got nil error, want substring %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("got error %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}

func writeTemp(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, AgentCardFileName)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	return path
}
