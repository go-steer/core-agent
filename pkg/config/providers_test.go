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

// TestProfileContextWindows: windows come only from profiles the process
// routes to — model.provider first, then subagents[].model.provider —
// and never from a profile that is merely declared. Ordering by route,
// not by declaration, is the point: big-vllm declares gemma first at
// 131072, but small-vllm is the server actually serving it at 65536, and
// sizing compaction to the larger window would overrun that server.
func TestProfileContextWindows(t *testing.T) {
	t.Parallel()
	big := `{"name":"big-vllm","extends":"vllm","base_url":"http://10.0.0.2:8000/v1",
		"models":[{"id":"google/gemma-4-26B-A4B-it","context_window":131072},{"id":"openai/gpt-oss-20b","context_window":131072}]}`
	small := `{"name":"small-vllm","extends":"vllm","base_url":"http://10.0.0.3:8000/v1",
		"models":[{"id":"google/gemma-4-26B-A4B-it","context_window":65536},{"id":"qwen/qwen3-8b"}]}`
	proxy := `{"name":"litellm-proxy","extends":"openai-compatible","base_url":"http://10.0.0.4:4000/v1",
		"models":[{"id":"gemini-2.5-flash","context_window":32768}]}`
	providers := `"providers":[` + big + `,` + small + `,` + proxy + `]`

	for _, tc := range []struct {
		name string
		body string
		want map[string]int
	}{
		{
			name: "the selected profile outranks an earlier declaration of the same model",
			body: `{"version":1,"model":{"provider":"small-vllm","name":"google/gemma-4-26B-A4B-it"},` + providers + `}`,
			want: map[string]int{"google/gemma-4-26B-A4B-it": 65536},
		},
		{
			name: "a subagent's profile counts, behind the selected one",
			body: `{"version":1,"model":{"provider":"small-vllm","name":"google/gemma-4-26B-A4B-it"},` +
				`"subagents":[{"name":"wide","instructions":"x","model":{"provider":"big-vllm","name":"openai/gpt-oss-20b"}}],` + providers + `}`,
			want: map[string]int{"google/gemma-4-26B-A4B-it": 65536, "openai/gpt-oss-20b": 131072},
		},
		{
			name: "declared-but-unused profiles contribute nothing",
			body: `{"version":1,"model":{"provider":"vertex","name":"gemini-2.5-flash"},` + providers + `}`,
			want: nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg, _, err := loadBody(t, tc.body)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			got := cfg.ProfileContextWindows()
			if len(got) != len(tc.want) {
				t.Fatalf("ProfileContextWindows = %v, want %v", got, tc.want)
			}
			for id, n := range tc.want {
				if got[id] != n {
					t.Errorf("ProfileContextWindows[%q] = %d, want %d (all: %v)", id, got[id], n, got)
				}
			}
		})
	}
	if w := DefaultConfig().ProfileContextWindows(); w != nil {
		t.Errorf("no profiles: ProfileContextWindows = %v, want nil", w)
	}
}

// Every core-models built-in is selectable with no declaration, and
// core-agent's own names never resolve to a profile.
func TestKnownProvider(t *testing.T) {
	t.Parallel()
	cfg := DefaultConfig()
	for _, name := range []string{"", ProviderGemini, ProviderAnthropicVertex, ProviderScripted, "vertex-maas", "vllm", "ollama", "sglang", "openai-compatible", "sap-ai-core"} {
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

func TestProfileRates_SkipsIsolatedProfiles(t *testing.T) {
	t.Parallel()
	sap := `{"name":"sap-custom","extends":"sap-ai-core","models":[{"id":"gpt-5-mini","rates":{"input_per_mtok":99,"output_per_mtok":99}}]}`
	cfg, _, err := loadBody(t, `{"version":1,"model":{"name":"gpt-5-mini","provider":"sap-custom"},"providers":[`+sap+`, `+houseVLLM+`]}`)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rates := cfg.ProfileRates()
	if _, ok := rates["gpt-5-mini"]; ok {
		t.Errorf("ProfileRates leaked isolated profile's bare model rate: %+v", rates["gpt-5-mini"])
	}
	if r := rates["google/gemma-4-26B-A4B-it"]; r.InputPerMTok != 0.1 {
		t.Errorf("ProfileRates missing shared profile's rate: %+v", rates)
	}
}

func TestLoad_RejectsBadProfiles(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		providers string
		want      string
	}{
		"core name":        {`{"name":"anthropic","extends":"vllm","base_url":"http://x/v1"}`, "core-agent's own providers"},
		"no name":          {`{"extends":"vllm","base_url":"http://x/v1"}`, "has no name"},
		"duplicate":        {houseVLLM + `,` + houseVLLM, "declared twice"},
		"unknown extend":   {`{"name":"x","extends":"nope","base_url":"http://x/v1"}`, "not a built-in"},
		"misspelt key":     {`{"name":"x","extends":"vllm","base_url":"http://x/v1","tierz":{"mid":"m"}}`, "tierz"},
		"pricing conflict": {`{"name":"a","extends":"vllm","backend":"shared-b","base_url":"http://x/v1","pricing":"isolated"},{"name":"b","extends":"vllm","backend":"shared-b","base_url":"http://y/v1","pricing":"shared"}`, "disagree on pricing"},
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
