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
	"strings"
	"testing"
)

func TestValidate_A2AConfig(t *testing.T) {
	t.Parallel()

	t.Run("valid full a2a config", func(t *testing.T) {
		t.Parallel()
		cfg := DefaultConfig()
		cfg.A2A = A2AConfig{
			Expose:           true,
			SkillName:        "gke-triage",
			SkillDescription: "Diagnose GKE incidents",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"cluster": map[string]any{"type": "string"},
				},
				"required": []any{"cluster"},
			},
			OutputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"severity": map[string]any{"type": "string"},
				},
			},
			Auth: A2AAuthConfig{
				Required: true,
				Scopes:   []string{"triage:invoke", "gke:read"},
			},
		}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("Validate() unexpected error: %v", err)
		}
	})

	t.Run("whitespace skill_name rejected", func(t *testing.T) {
		t.Parallel()
		cfg := DefaultConfig()
		cfg.A2A.SkillName = "   "
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "a2a.skill_name") {
			t.Fatalf("Validate() = %v, want a2a.skill_name error", err)
		}
	})

	t.Run("empty auth scope rejected", func(t *testing.T) {
		t.Parallel()
		cfg := DefaultConfig()
		cfg.A2A.Auth.Scopes = []string{"triage:invoke", "  "}
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "a2a.auth.scopes[1]") {
			t.Fatalf("Validate() = %v, want a2a.auth.scopes[1] error", err)
		}
	})

	t.Run("invalid input_schema rejected", func(t *testing.T) {
		t.Parallel()
		cfg := DefaultConfig()
		cfg.A2A.InputSchema = map[string]any{
			"type": "not-a-json-schema-type",
		}
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "a2a.input_schema") {
			t.Fatalf("Validate() = %v, want a2a.input_schema error", err)
		}
	})

	t.Run("invalid output_schema rejected", func(t *testing.T) {
		t.Parallel()
		cfg := DefaultConfig()
		cfg.A2A.OutputSchema = map[string]any{
			"type": 12345,
		}
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "a2a.output_schema") {
			t.Fatalf("Validate() = %v, want a2a.output_schema error", err)
		}
	})
}

func TestLoadAndSave_A2ARoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	agentsDir := filepath.Join(dir, AgentsDirName)
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(agentsDir, ConfigFileName)
	raw := `{
  "version": 1,
  "model": { "name": "gemini-2.5-flash" },
  "a2a": {
    "expose": true,
    "skill_name": "incident-triage",
    "skill_description": "Triages cluster alerts.",
    "input_schema": {
      "type": "object",
      "properties": { "cluster": { "type": "string" } },
      "required": ["cluster"]
    },
    "auth": {
      "required": true,
      "scopes": ["a2a:invoke"]
    }
  }
}
`
	if err := os.WriteFile(cfgPath, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(agentsDir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.A2A.Expose || cfg.A2A.SkillName != "incident-triage" || !cfg.A2A.Auth.Required {
		t.Fatalf("loaded A2A = %+v", cfg.A2A)
	}
	if len(cfg.A2A.Auth.Scopes) != 1 || cfg.A2A.Auth.Scopes[0] != "a2a:invoke" {
		t.Fatalf("loaded A2A.Auth.Scopes = %v", cfg.A2A.Auth.Scopes)
	}

	if err := Save(cfgPath, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}
	reloaded, err := Load(agentsDir)
	if err != nil {
		t.Fatalf("Load after Save: %v", err)
	}
	if !reloaded.A2A.Expose || reloaded.A2A.SkillName != "incident-triage" {
		t.Fatalf("reloaded A2A = %+v", reloaded.A2A)
	}
}

func TestImportWorkload_A2AConversionAndDefaults(t *testing.T) {
	t.Parallel()
	srcDir := writeSyntheticGKETriageF(t)
	wlPath := filepath.Join(srcDir, "workload.yaml")
	data, err := os.ReadFile(wlPath)
	if err != nil {
		t.Fatal(err)
	}
	a2aBlock := `
a2a:
  expose: true
  input_schema:
    type: object
    required: [cluster]
    properties:
      cluster:
        type: string
  auth:
    required: true
    scopes:
      - triage:invoke
`
	if err := os.WriteFile(wlPath, append(data, []byte(a2aBlock)...), 0o644); err != nil {
		t.Fatal(err)
	}

	b, err := LoadWorkload(srcDir)
	if err != nil {
		t.Fatalf("LoadWorkload: %v", err)
	}
	parsed, err := b.ParsedA2A()
	if err != nil {
		t.Fatalf("ParsedA2A: %v", err)
	}
	if !parsed.Expose || !parsed.Auth.Required {
		t.Fatalf("ParsedA2A = %+v", parsed)
	}

	outDir := t.TempDir()
	cfg, err := ImportWorkload(srcDir, outDir)
	if err != nil {
		t.Fatalf("ImportWorkload: %v", err)
	}
	if !cfg.A2A.Expose {
		t.Errorf("cfg.A2A.Expose = false, want true")
	}
	if cfg.A2A.SkillName != "gke-triage" {
		t.Errorf("cfg.A2A.SkillName = %q, want defaulted workload name gke-triage", cfg.A2A.SkillName)
	}
	if cfg.A2A.SkillDescription == "" {
		t.Errorf("cfg.A2A.SkillDescription is empty, want defaulted workload description")
	}
	if len(cfg.A2A.Auth.Scopes) != 1 || cfg.A2A.Auth.Scopes[0] != "triage:invoke" {
		t.Errorf("cfg.A2A.Auth.Scopes = %v, want [triage:invoke]", cfg.A2A.Auth.Scopes)
	}
}

func TestLoadWorkload_RejectsUnknownA2AKey(t *testing.T) {
	t.Parallel()
	srcDir := writeSyntheticGKETriageF(t)
	wlPath := filepath.Join(srcDir, "workload.yaml")
	data, err := os.ReadFile(wlPath)
	if err != nil {
		t.Fatal(err)
	}
	badBlock := `
a2a:
  expose: true
  unknown_field: true
`
	if err := os.WriteFile(wlPath, append(data, []byte(badBlock)...), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = LoadWorkload(srcDir)
	if err == nil || !strings.Contains(err.Error(), "unknown_field") {
		t.Fatalf("LoadWorkload() = %v, want unknown_field error", err)
	}
}

func TestLoad_RejectsUnknownA2AKeyInConfigJSON(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	agentsDir := filepath.Join(dir, AgentsDirName)
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(agentsDir, ConfigFileName)
	raw := `{
  "version": 1,
  "a2a": {
    "expsoe": true
  }
}`
	if err := os.WriteFile(cfgPath, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(agentsDir); err == nil || !strings.Contains(err.Error(), "expsoe") {
		t.Fatalf("Load() = %v, want unknown key error for a2a.expsoe", err)
	}
}
