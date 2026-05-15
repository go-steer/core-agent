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

func TestDefaultConfig_Validates(t *testing.T) {
	t.Parallel()
	if err := DefaultConfig().Validate(); err != nil {
		t.Fatalf("DefaultConfig() should validate: %v", err)
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		mutate  func(*Config)
		wantErr string // substring; "" means must succeed
	}{
		{
			name:    "default ok",
			mutate:  func(c *Config) {},
			wantErr: "",
		},
		{
			name:    "empty model name",
			mutate:  func(c *Config) { c.Model.Name = "" },
			wantErr: "model.name is required",
		},
		{
			name:    "unknown provider",
			mutate:  func(c *Config) { c.Model.Provider = "openai" },
			wantErr: "unknown model.provider",
		},
		{
			name: "vertex without project",
			mutate: func(c *Config) {
				c.Model.Provider = ProviderVertex
				c.Model.Vertex = &VertexConfig{Location: "us-central1"}
			},
			wantErr: "vertex.project",
		},
		{
			name: "vertex with both",
			mutate: func(c *Config) {
				c.Model.Provider = ProviderVertex
				c.Model.Vertex = &VertexConfig{Project: "p", Location: "us-central1"}
			},
			wantErr: "",
		},
		{
			name: "anthropic ok",
			mutate: func(c *Config) {
				c.Model.Provider = ProviderAnthropic
				c.Model.Name = "claude-opus-4-7"
			},
			wantErr: "",
		},
		{
			name: "anthropic-vertex with creds ok",
			mutate: func(c *Config) {
				c.Model.Provider = ProviderAnthropicVertex
				c.Model.Name = "claude-opus-4-7"
				c.Model.Anthropic = &AnthropicConfig{
					Vertex: &VertexConfig{Project: "p", Location: "us-east5"},
				}
			},
			wantErr: "",
		},
		{
			name: "anthropic-vertex without project errors",
			mutate: func(c *Config) {
				c.Model.Provider = ProviderAnthropicVertex
				c.Model.Name = "claude-opus-4-7"
				c.Model.Anthropic = &AnthropicConfig{
					Vertex: &VertexConfig{Location: "us-east5"},
				}
			},
			wantErr: "anthropic.vertex.project",
		},
		{
			name:    "unknown permissions mode",
			mutate:  func(c *Config) { c.Permissions.Mode = "wild" },
			wantErr: "unknown permissions.mode",
		},
		{
			name:    "wrong schema version",
			mutate:  func(c *Config) { c.Version = 99 },
			wantErr: "unsupported schema version",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := DefaultConfig()
			tc.mutate(cfg)
			err := cfg.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("want nil error, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("want error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestFind_WalksUp(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	deep := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, AgentsDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	got, ok, err := Find(deep)
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if !ok {
		t.Fatal("expected to find .agents/")
	}
	want := filepath.Join(root, AgentsDirName)
	gotResolved, _ := filepath.EvalSymlinks(got)
	wantResolved, _ := filepath.EvalSymlinks(want)
	if gotResolved != wantResolved {
		t.Fatalf("Find returned %q, want %q", gotResolved, wantResolved)
	}
}

func TestFind_NoMatch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	_, ok, err := Find(dir)
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if ok {
		t.Fatal("expected no match in fresh tempdir")
	}
}

func TestLoad_MissingFileReturnsDefaults(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	agents := filepath.Join(root, AgentsDirName)
	if err := os.MkdirAll(agents, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(agents)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Model.Name != "gemini-3.1-pro-preview" {
		t.Fatalf("expected default model name, got %q", cfg.Model.Name)
	}
}

func TestLoad_MergesPartialOverrides(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	agents := filepath.Join(root, AgentsDirName)
	if err := os.MkdirAll(agents, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"version":1,"model":{"name":"gemini-3-flash-preview","provider":"gemini"},"agent":{"max_steps":10}}`
	if err := os.WriteFile(filepath.Join(agents, ConfigFileName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(agents)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Model.Name != "gemini-3-flash-preview" {
		t.Errorf("model.name not merged: %q", cfg.Model.Name)
	}
	if cfg.Model.Provider != ProviderGemini {
		t.Errorf("model.provider not merged: %q", cfg.Model.Provider)
	}
	if cfg.Agent.MaxSteps != 10 {
		t.Errorf("agent.max_steps not merged: %d", cfg.Agent.MaxSteps)
	}
	// Unspecified field keeps its default.
	if cfg.Permissions.Mode != PermissionModeAsk {
		t.Errorf("permissions.mode lost default: %q", cfg.Permissions.Mode)
	}
}

func TestLoad_RejectsBadProvider(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	agents := filepath.Join(root, AgentsDirName)
	if err := os.MkdirAll(agents, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"version":1,"model":{"name":"x","provider":"openai"}}`
	if err := os.WriteFile(filepath.Join(agents, ConfigFileName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(agents)
	if err == nil || !strings.Contains(err.Error(), "unknown model.provider") {
		t.Fatalf("expected provider error, got %v", err)
	}
}

func TestLoad_ToolsDisableRoundtrips(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	agents := filepath.Join(root, AgentsDirName)
	if err := os.MkdirAll(agents, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"version":1,"model":{"name":"gemini-3.1-pro-preview"},"tools":{"disable":["bash","write_file"]}}`
	if err := os.WriteFile(filepath.Join(agents, ConfigFileName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(agents)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := cfg.Tools.Disable, []string{"bash", "write_file"}; !equalStringSlice(got, want) {
		t.Errorf("tools.disable: got %v, want %v", got, want)
	}
}

func TestLoad_ToolsDefaultEmpty(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	agents := filepath.Join(root, AgentsDirName)
	if err := os.MkdirAll(agents, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(agents)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Tools.Disable) != 0 {
		t.Errorf("expected empty Tools.Disable by default, got %v", cfg.Tools.Disable)
	}
}

func equalStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestLoadOrDefault_NoAgentsDir(t *testing.T) {
	t.Parallel()
	cfg, agents, err := LoadOrDefault(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrDefault: %v", err)
	}
	if agents != "" {
		t.Errorf("expected empty agentsDir, got %q", agents)
	}
	if cfg.Model.Name == "" {
		t.Error("expected populated default model name")
	}
}
