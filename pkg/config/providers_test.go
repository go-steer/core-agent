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

// loadBody writes body as .agents/config.json under a fresh root and
// loads it.
func loadBody(t *testing.T, body string) (*Config, string, error) {
	t.Helper()
	agents := filepath.Join(t.TempDir(), AgentsDirName)
	if err := os.MkdirAll(agents, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(agents, ConfigFileName)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(agents)
	return cfg, path, err
}

const houseVLLM = `{"name":"house-vllm","extends":"vllm","base_url":"http://10.0.0.2:8000/v1",
	"auth":{"kind":"bearer","env":"HOUSE_VLLM_API_KEY"},
	"models":[{"id":"google/gemma-4-26B-A4B-it","rates":{"input_per_mtok":0.1,"output_per_mtok":0.4}}],
	"tiers":{"mid":"google/gemma-4-26B-A4B-it"}}`

func TestLoad_DeclaredProfileIsSelectable(t *testing.T) {
	t.Parallel()
	cfg, _, err := loadBody(t, `{"version":1,"model":{"name":"google/gemma-4-26B-A4B-it","provider":"house-vllm"},"providers":[`+houseVLLM+`]}`)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.IsProfileProvider("house-vllm") {
		t.Error("a declared profile is not a profile provider")
	}
	p, ok := cfg.Profile("house-vllm")
	if !ok || p.Extends != "vllm" {
		t.Fatalf("Profile(house-vllm) = %+v, %v", p, ok)
	}
	rates := cfg.ProfileRates()
	if r := rates["google/gemma-4-26B-A4B-it"]; r.InputPerMTok != 0.1 || r.OutputPerMTok != 0.4 {
		t.Errorf("ProfileRates = %+v, want the declared 0.1/0.4", rates)
	}
}

// Every core-models built-in is selectable with no declaration, and
// core-agent's own names never resolve to a profile.
func TestKnownProvider(t *testing.T) {
	t.Parallel()
	cfg := DefaultConfig()
	for _, name := range []string{"", ProviderGemini, ProviderAnthropicVertex, ProviderScripted, "vertex-maas", "vllm", "ollama", "sglang", "openai-compatible"} {
		if !cfg.KnownProvider(name) {
			t.Errorf("KnownProvider(%q) = false", name)
		}
	}
	for _, name := range []string{ProviderGemini, ProviderVertex, ""} {
		if cfg.IsProfileProvider(name) {
			t.Errorf("IsProfileProvider(%q) = true; core-agent's own names are not profiles", name)
		}
	}
	if cfg.KnownProvider("not-a-provider") {
		t.Error("an undeclared name is known")
	}
}

func TestLoad_RejectsBadProfiles(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		providers string
		want      string
	}{
		"core name":      {`{"name":"anthropic","extends":"vllm","base_url":"http://x/v1"}`, "core-agent's own providers"},
		"no name":        {`{"extends":"vllm","base_url":"http://x/v1"}`, "has no name"},
		"duplicate":      {houseVLLM + `,` + houseVLLM, "declared twice"},
		"unknown extend": {`{"name":"x","extends":"nope","base_url":"http://x/v1"}`, "not a built-in"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, path, err := loadBody(t, `{"version":1,"model":{"name":"m"},"providers":[`+tc.providers+`]}`)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Load err = %v, want one containing %q", err, tc.want)
			}
			if !strings.Contains(err.Error(), path) {
				t.Errorf("Load err = %v, want it to name %s", err, path)
			}
		})
	}
}

func TestLoad_UnknownProviderListsProfiles(t *testing.T) {
	t.Parallel()
	_, _, err := loadBody(t, `{"version":1,"model":{"name":"m","provider":"house"},"providers":[`+houseVLLM+`]}`)
	if err == nil || !strings.Contains(err.Error(), "house-vllm") || !strings.Contains(err.Error(), "vertex-maas") {
		t.Fatalf("err = %v, want the declared and built-in profiles listed", err)
	}
}
