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

	"google.golang.org/genai"
)

func TestSubagentSpec_EffectiveMode(t *testing.T) {
	t.Parallel()
	cases := []struct {
		mode string
		want string
	}{
		{"", SubagentModeChat},
		{SubagentModeChat, SubagentModeChat},
		{SubagentModeTask, SubagentModeTask},
		{SubagentModeSingleTurn, SubagentModeSingleTurn},
	}
	for _, tc := range cases {
		s := SubagentSpec{Mode: tc.mode}
		if got := s.EffectiveMode(); got != tc.want {
			t.Errorf("SubagentSpec{Mode: %q}.EffectiveMode() = %q, want %q", tc.mode, got, tc.want)
		}
	}
}

func TestValidateSubagents_ModesTiersCapabilityMCPTools(t *testing.T) {
	t.Parallel()
	baseCfg := func(sa SubagentSpec) *Config {
		cfg := DefaultConfig()
		cfg.Subagents = []SubagentSpec{sa}
		return cfg
	}

	validCases := []struct {
		name string
		sa   SubagentSpec
	}{
		{
			name: "unset mode defaults to chat",
			sa:   SubagentSpec{Name: "helper"},
		},
		{
			name: "task mode with tier and read_only capability",
			sa: SubagentSpec{
				Name:       "diagnoser",
				Mode:       SubagentModeTask,
				Tier:       "small",
				Capability: CapabilityReadOnly,
				MCPTools: []MCPToolAllowlist{
					{Server: "gke", Tools: []string{"get_pod", "list_pods"}},
				},
			},
		},
		{
			name: "single_turn mode with change_executor capability",
			sa: SubagentSpec{
				Name:       "router",
				Mode:       SubagentModeSingleTurn,
				Tier:       "mid",
				Capability: CapabilityChangeExecutor,
			},
		},
	}
	for _, tc := range validCases {
		t.Run(tc.name, func(t *testing.T) {
			if err := baseCfg(tc.sa).Validate(); err != nil {
				t.Fatalf("Validate() unexpected error: %v", err)
			}
		})
	}

	errCases := []struct {
		name    string
		sa      SubagentSpec
		wantSub string
	}{
		{
			name:    "unknown mode",
			sa:      SubagentSpec{Name: "x", Mode: "batch"},
			wantSub: "subagents[0].mode",
		},
		{
			name:    "output_schema with default chat mode",
			sa:      SubagentSpec{Name: "x", OutputSchema: "schemas/finding.json"},
			wantSub: "output_schema requires mode",
		},
		{
			name:    "output_schema with explicit chat mode",
			sa:      SubagentSpec{Name: "x", Mode: SubagentModeChat, OutputSchema: "schemas/finding.json"},
			wantSub: "output_schema requires mode",
		},
		{
			name:    "output_schema absolute path",
			sa:      SubagentSpec{Name: "x", Mode: SubagentModeTask, OutputSchema: "/tmp/finding.json"},
			wantSub: "relative path",
		},
		{
			name:    "scheduler sleep with task mode",
			sa:      SubagentSpec{Name: "x", Mode: SubagentModeTask, Scheduler: "sleep"},
			wantSub: "scheduler",
		},
		{
			name:    "scheduler sleep with single_turn mode",
			sa:      SubagentSpec{Name: "x", Mode: SubagentModeSingleTurn, Scheduler: "sleep"},
			wantSub: "scheduler",
		},
		{
			name:    "unknown tier",
			sa:      SubagentSpec{Name: "x", Tier: "cheap"},
			wantSub: "unknown model tier",
		},
		{
			name:    "tier and model both set",
			sa:      SubagentSpec{Name: "x", Tier: "small", Model: &ModelConfig{Name: "gemini-3.5-flash"}},
			wantSub: "tier \"small\" and model cannot both be set",
		},
		{
			name:    "unknown capability",
			sa:      SubagentSpec{Name: "x", Capability: "admin"},
			wantSub: "subagents[0].capability",
		},
		{
			name: "mcp_tools empty server",
			sa: SubagentSpec{
				Name:     "x",
				MCPTools: []MCPToolAllowlist{{Server: "", Tools: []string{"t1"}}},
			},
			wantSub: "mcp_tools[0].server is required",
		},
		{
			name: "mcp_tools duplicate server",
			sa: SubagentSpec{
				Name: "x",
				MCPTools: []MCPToolAllowlist{
					{Server: "gke", Tools: []string{"t1"}},
					{Server: "gke", Tools: []string{"t2"}},
				},
			},
			wantSub: "duplicate server \"gke\"",
		},
		{
			name: "mcp_tools empty tool name",
			sa: SubagentSpec{
				Name:     "x",
				MCPTools: []MCPToolAllowlist{{Server: "gke", Tools: []string{""}}},
			},
			wantSub: "mcp_tools[0].tools[0] is empty",
		},
		{
			name: "remote with mode",
			sa: SubagentSpec{
				Name:   "x",
				Mode:   SubagentModeTask,
				Remote: &SubagentRemoteConfig{A2A: "https://agent.example.com"},
			},
			wantSub: "remote cannot be combined with in-process fields",
		},
		{
			name: "remote with tier",
			sa: SubagentSpec{
				Name:   "x",
				Tier:   "small",
				Remote: &SubagentRemoteConfig{A2A: "https://agent.example.com"},
			},
			wantSub: "remote cannot be combined with in-process fields",
		},
		{
			name: "remote with capability",
			sa: SubagentSpec{
				Name:       "x",
				Capability: CapabilityReadOnly,
				Remote:     &SubagentRemoteConfig{A2A: "https://agent.example.com"},
			},
			wantSub: "remote cannot be combined with in-process fields",
		},
		{
			name: "remote with mcp_tools",
			sa: SubagentSpec{
				Name:     "x",
				MCPTools: []MCPToolAllowlist{{Server: "gke", Tools: []string{"t1"}}},
				Remote:   &SubagentRemoteConfig{A2A: "https://agent.example.com"},
			},
			wantSub: "remote cannot be combined with in-process fields",
		},
	}
	for _, tc := range errCases {
		t.Run(tc.name, func(t *testing.T) {
			err := baseCfg(tc.sa).Validate()
			if err == nil {
				t.Fatalf("Validate() succeeded, want error containing %q", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("Validate() error = %q, want substring %q", err.Error(), tc.wantSub)
			}
		})
	}
}

func TestLoadMarkdownSubagents_AndSaveExcludesMarkdown(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	agentsDir := filepath.Join(tmp, AgentsDirName)
	subagentsDir := filepath.Join(agentsDir, SubagentsDirName)
	schemasDir := filepath.Join(agentsDir, "schemas")
	if err := os.MkdirAll(subagentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(schemasDir, 0o755); err != nil {
		t.Fatal(err)
	}

	schemaJSON := `{
  "type": "object",
  "required": ["finding", "severity"],
  "properties": {
    "finding": { "type": "string" },
    "severity": { "type": "string", "enum": ["low", "high"] }
  }
}`
	if err := os.WriteFile(filepath.Join(schemasDir, "finding.json"), []byte(schemaJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	configJSON := `{
  "version": 1,
  "model": { "name": "gemini-3.7-flash" },
  "subagents": [
    {
      "name": "inline-helper",
      "description": "Inline helper in config.json"
    }
  ]
}`
	cfgPath := filepath.Join(agentsDir, ConfigFileName)
	if err := os.WriteFile(cfgPath, []byte(configJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	mdDiag := `---
description: Diagnoses pod failures.
mode: task
tier: small
capability: read_only
output_schema: ../schemas/finding.json
budgets:
  max_turns: 6
  max_wallclock_seconds: 90
mcp_tools:
  - server: gke
    tools:
      - get_pod
      - describe_pod
---

You are the pod diagnoser. Literal braces like {pod_name} and ${HOME} are allowed.
`
	if err := os.WriteFile(filepath.Join(subagentsDir, "diagnoser.md"), []byte(mdDiag), 0o600); err != nil {
		t.Fatal(err)
	}

	mdClassifier := `---
name: router
description: Routes incoming alerts.
mode: single_turn
model: gemini-3.5-flash-lite
---
Return the target route.
`
	if err := os.WriteFile(filepath.Join(subagentsDir, "a-router.md"), []byte(mdClassifier), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(agentsDir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := len(cfg.Subagents), 3; got != want {
		t.Fatalf("len(cfg.Subagents) = %d, want %d", got, want)
	}

	// [0] is inline-helper from config.json; [1] is a-router.md ("router"); [2] is diagnoser.md ("diagnoser").
	if cfg.Subagents[0].Name != "inline-helper" || cfg.Subagents[0].FromMarkdown() {
		t.Errorf("Subagents[0] = %+v, want inline-helper (fromMarkdown=false)", cfg.Subagents[0])
	}
	if cfg.Subagents[1].Name != "router" || !cfg.Subagents[1].FromMarkdown() || cfg.Subagents[1].Mode != SubagentModeSingleTurn {
		t.Errorf("Subagents[1] = %+v, want router (fromMarkdown=true, single_turn)", cfg.Subagents[1])
	}
	if cfg.Subagents[1].Model == nil || cfg.Subagents[1].Model.Name != "gemini-3.5-flash-lite" {
		t.Errorf("Subagents[1].Model = %+v, want gemini-3.5-flash-lite", cfg.Subagents[1].Model)
	}
	diag := cfg.Subagents[2]
	if diag.Name != "diagnoser" || !diag.FromMarkdown() || diag.Mode != SubagentModeTask {
		t.Errorf("Subagents[2] = %+v, want diagnoser (fromMarkdown=true, task)", diag)
	}
	if diag.Tier != "small" || diag.Capability != CapabilityReadOnly {
		t.Errorf("diag tier/capability = %q/%q, want small/read_only", diag.Tier, diag.Capability)
	}
	if len(diag.MCPTools) != 1 || diag.MCPTools[0].Server != "gke" || len(diag.MCPTools[0].Tools) != 2 {
		t.Errorf("diag.MCPTools = %+v", diag.MCPTools)
	}
	schema, err := LoadOutputSchema(diag.BaseDir(), diag.OutputSchema)
	if err != nil {
		t.Fatalf("LoadOutputSchema: %v", err)
	}
	if schema.Type != genai.TypeObject || schema.Properties["finding"] == nil {
		t.Errorf("loaded schema = %+v", schema)
	}

	// Saving cfg back to config.json must NOT copy markdown subagents into config.json.
	if err := Save(cfgPath, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}
	savedBytes, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	savedText := string(savedBytes)
	if strings.Contains(savedText, "diagnoser") || strings.Contains(savedText, "router") {
		t.Errorf("Save copied Markdown subagent into config.json:\n%s", savedText)
	}
	if !strings.Contains(savedText, "inline-helper") {
		t.Errorf("Save lost inline subagent from config.json:\n%s", savedText)
	}
}

func TestLoadSubagentMarkdownFile_Errors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		content string
		wantSub string
	}{
		{
			name: "missing mode in markdown file",
			content: `---
description: Missing mode field
---
Do work.
`,
			wantSub: "mode is required in Markdown subagent files",
		},
		{
			name: "unknown frontmatter field rejected by strict decoder",
			content: `---
description: Unknown field
mode: task
model_typo: gemini-3.5-flash
---
Do work.
`,
			wantSub: "model_typo",
		},
		{
			name:    "missing frontmatter",
			content: "Just a markdown body without frontmatter.\n",
			wantSub: "missing frontmatter",
		},
		{
			name: "stale optional session state placeholder rejected",
			content: `---
description: Stale placeholder
mode: task
---
Check cluster {cluster_name?} and {app:env?}.
`,
			wantSub: "asked for session-state key",
		},
		{
			name: "stale optional artifact placeholder rejected",
			content: `---
description: Artifact placeholder
mode: task
---
Read {artifact.report?}.
`,
			wantSub: "asked for artifact \"report\"",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "sub.md")
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadSubagentMarkdownFile(path)
			if err == nil {
				t.Fatalf("LoadSubagentMarkdownFile succeeded, want error containing %q", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error = %q, want substring %q", err.Error(), tc.wantSub)
			}
		})
	}
}

func TestLoadOutputSchema_ContainmentAndValidation(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	agentsDir := filepath.Join(tmp, ".agents")
	subagentsDir := filepath.Join(agentsDir, "subagents")
	if err := os.MkdirAll(subagentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	outsidePath := filepath.Join(tmp, "outside.json")
	validSchema := `{"type": "object", "properties": {"ok": {"type": "boolean"}}}`
	if err := os.WriteFile(outsidePath, []byte(validSchema), 0o600); err != nil {
		t.Fatal(err)
	}

	// Traversal escaping .agents from subagentsDir must fail.
	if _, err := LoadOutputSchema(subagentsDir, "../../outside.json"); err == nil || !strings.Contains(err.Error(), "escapes scope root") {
		t.Fatalf("LoadOutputSchema(../../outside.json) = %v, want scope escape error", err)
	}
	// Traversal escaping .agents from agentsDir must fail.
	if _, err := LoadOutputSchema(agentsDir, "../outside.json"); err == nil || !strings.Contains(err.Error(), "escapes scope root") {
		t.Fatalf("LoadOutputSchema(../outside.json) = %v, want scope escape error", err)
	}
	// Symlink directory with nested non-existent subpath escaping .agents must fail.
	symlinkDir := filepath.Join(subagentsDir, "escape_link")
	if err := os.Symlink(tmp, symlinkDir); err == nil {
		if _, err := LoadOutputSchema(subagentsDir, "escape_link/nested/nonexistent.json"); err == nil || !strings.Contains(err.Error(), "escapes scope root") {
			t.Fatalf("LoadOutputSchema(symlink escape) = %v, want scope escape error", err)
		}
	}
	// Inline JSON schema in LoadOutputSchema and config.Load must succeed.
	if s, err := LoadOutputSchema(agentsDir, validSchema); err != nil || s == nil {
		t.Fatalf("LoadOutputSchema(inline JSON) = (%v, %v), want valid schema", s, err)
	}
	// Bad extension rejected.
	if _, err := LoadOutputSchema(agentsDir, "schema.txt"); err == nil || !strings.Contains(err.Error(), "extension") {
		t.Fatalf("LoadOutputSchema(schema.txt) = %v, want extension error", err)
	}

	badSchemas := []struct {
		name    string
		content string
		wantSub string
	}{
		{
			name:    "unknown field in schema",
			content: `{"type": "object", "propertys": {"a": {"type": "string"}}}`,
			wantSub: "unknown field",
		},
		{
			name:    "non-object root",
			content: `{"type": "string"}`,
			wantSub: "top-level type must be object",
		},
		{
			name:    "empty object properties",
			content: `{"type": "object"}`,
			wantSub: "object has no properties",
		},
		{
			name:    "required field not in properties",
			content: `{"type": "object", "required": ["missing"], "properties": {"present": {"type": "string"}}}`,
			wantSub: "required name \"missing\" is not a property",
		},
		{
			name:    "array without items",
			content: `{"type": "object", "properties": {"list": {"type": "array"}}}`,
			wantSub: "array has no items schema",
		},
	}
	for _, tc := range badSchemas {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(agentsDir, "bad.json")
			if err := os.WriteFile(p, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadOutputSchema(agentsDir, "bad.json")
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("LoadOutputSchema(%s) = %v, want error containing %q", tc.name, err, tc.wantSub)
			}
		})
	}
}

func TestResolveTierModel(t *testing.T) {
	t.Parallel()
	cfg := DefaultConfig()
	cfg.Model = ModelConfig{Provider: ProviderGemini, Name: "gemini-3.7-flash"}

	small, err := cfg.ResolveTierModel("small")
	if err != nil {
		t.Fatalf("ResolveTierModel(small): %v", err)
	}
	if small.Name != "gemini-3.5-flash-lite" || small.Provider != ProviderGemini {
		t.Errorf("ResolveTierModel(small) = %+v, want gemini-3.5-flash-lite on gemini", small)
	}

	cfgClaude := DefaultConfig()
	cfgClaude.Model = ModelConfig{Name: "claude-opus-5-5"}
	mid, err := cfgClaude.ResolveTierModel("mid")
	if err != nil {
		t.Fatalf("ResolveTierModel(mid): %v", err)
	}
	if mid.Name != "claude-sonnet-5-5" {
		t.Errorf("ResolveTierModel(mid) = %+v, want claude-sonnet-5-5", mid)
	}
}
