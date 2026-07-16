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
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

// operatorRichRegistrant satisfies every operator-state capability
// interface (UsageProvider, ContextProvider, MemoryProvider,
// SkillsProvider, MCPProvider, PricingProvider) on top of
// stubRegistrant. Used for the per-endpoint happy-path tests.
type operatorRichRegistrant struct {
	stubRegistrant
	usage   UsageInfo
	ctx     ContextInfo
	memory  []MemorySource
	skills  []SkillInfo
	mcp     MCPInfo
	pricing PricingInfo
}

func (r *operatorRichRegistrant) AttachUsage() UsageInfo       { return r.usage }
func (r *operatorRichRegistrant) AttachContext() ContextInfo   { return r.ctx }
func (r *operatorRichRegistrant) AttachMemory() []MemorySource { return r.memory }
func (r *operatorRichRegistrant) AttachSkills() []SkillInfo    { return r.skills }
func (r *operatorRichRegistrant) AttachMCP() MCPInfo           { return r.mcp }
func (r *operatorRichRegistrant) AttachPricing() PricingInfo   { return r.pricing }

func TestIntegration_UsageEndpoint(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	ag := &operatorRichRegistrant{
		stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
		usage: UsageInfo{
			Overall: UsageTotals{InputTokens: 1200, OutputTokens: 800, Turns: 3, CostUSD: 0.014},
			PerModel: map[string]UsageTotals{
				"claude-opus-4-7":  {InputTokens: 1000, OutputTokens: 700, Turns: 2, CostUSD: 0.012},
				"gemini-2.5-flash": {InputTokens: 200, OutputTokens: 100, Turns: 1, CostUSD: 0.002},
			},
		},
	}
	if _, err := reg.Register(ag); err != nil {
		t.Fatal(err)
	}
	base, cleanup := startTestServer(t, reg)
	defer cleanup()

	resp, err := http.Get(base + "/sessions/core-agent/s1/usage")
	if err != nil {
		t.Fatalf("GET /usage: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	var got UsageInfo
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Overall.InputTokens != 1200 || got.Overall.Turns != 3 || got.Overall.CostUSD != 0.014 {
		t.Errorf("overall = %+v", got.Overall)
	}
	if len(got.PerModel) != 2 || got.PerModel["claude-opus-4-7"].CostUSD != 0.012 {
		t.Errorf("per_model = %+v", got.PerModel)
	}
}

func TestIntegration_UsageEndpoint_NoProvider_ZeroValue(t *testing.T) {
	t.Parallel()
	// stubRegistrant doesn't implement UsageProvider — endpoint
	// returns 200 with zero UsageInfo (same convention as /tools).
	reg := NewSessionRegistry()
	ag := &stubRegistrant{app: "core-agent", user: "u", sid: "s1"}
	if _, err := reg.Register(ag); err != nil {
		t.Fatal(err)
	}
	base, cleanup := startTestServer(t, reg)
	defer cleanup()

	resp, err := http.Get(base + "/sessions/core-agent/s1/usage")
	if err != nil {
		t.Fatalf("GET /usage: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	var got UsageInfo
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Overall.Turns != 0 || got.Overall.CostUSD != 0 {
		t.Errorf("expected zero value, got %+v", got)
	}
}

func TestIntegration_ContextEndpoint(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	ag := &operatorRichRegistrant{
		stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
		ctx: ContextInfo{
			Compactions:          2,
			Checkpoints:          3,
			LastTaskNote:         "shipped the docs reorg",
			TotalCharsSummarized: 12345,
			SubtaskTurns:         5,
			SubtaskInputTokens:   4000,
			SubtaskOutputTokens:  500,
			SubtaskCostUSD:       0.003,
		},
	}
	if _, err := reg.Register(ag); err != nil {
		t.Fatal(err)
	}
	base, cleanup := startTestServer(t, reg)
	defer cleanup()

	resp, err := http.Get(base + "/sessions/core-agent/s1/context")
	if err != nil {
		t.Fatalf("GET /context: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var got ContextInfo
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Compactions != 2 || got.Checkpoints != 3 || got.LastTaskNote != "shipped the docs reorg" {
		t.Errorf("got = %+v", got)
	}
}

func TestIntegration_MemoryEndpoint(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	ag := &operatorRichRegistrant{
		stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
		memory: []MemorySource{
			{Scope: "user-global", Path: "/home/u/.core-agent/AGENTS.md", Size: 512},
			{Scope: "project", Path: "AGENTS.md", Size: 2048},
		},
	}
	if _, err := reg.Register(ag); err != nil {
		t.Fatal(err)
	}
	base, cleanup := startTestServer(t, reg)
	defer cleanup()

	resp, err := http.Get(base + "/sessions/core-agent/s1/memory")
	if err != nil {
		t.Fatalf("GET /memory: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var got struct {
		Sources []MemorySource `json:"sources"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Sources) != 2 || got.Sources[0].Scope != "user-global" || got.Sources[1].Path != "AGENTS.md" {
		t.Errorf("sources = %+v", got.Sources)
	}
}

func TestIntegration_SkillsEndpoint(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	ag := &operatorRichRegistrant{
		stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
		skills: []SkillInfo{
			{Name: "cli-setup", Description: "walk a user through configuring core-agent"},
			{Name: "library-embedding", Description: "embed core-agent in a Go binary"},
		},
	}
	if _, err := reg.Register(ag); err != nil {
		t.Fatal(err)
	}
	base, cleanup := startTestServer(t, reg)
	defer cleanup()

	resp, err := http.Get(base + "/sessions/core-agent/s1/skills")
	if err != nil {
		t.Fatalf("GET /skills: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var got struct {
		Skills []SkillInfo `json:"skills"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Skills) != 2 || got.Skills[0].Name != "cli-setup" {
		t.Errorf("skills = %+v", got.Skills)
	}
}

func TestIntegration_MCPEndpoint(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	ag := &operatorRichRegistrant{
		stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
		mcp: MCPInfo{
			Servers: []MCPServerInfo{
				{
					Name:      "kube-mcp",
					Status:    "running",
					Transport: "stdio",
					Tools: []MCPToolInfo{
						{Name: "kube_get", Description: "kubectl get"},
						{Name: "kube_logs", Description: "kubectl logs"},
					},
				},
			},
		},
	}
	if _, err := reg.Register(ag); err != nil {
		t.Fatal(err)
	}
	base, cleanup := startTestServer(t, reg)
	defer cleanup()

	resp, err := http.Get(base + "/sessions/core-agent/s1/mcp")
	if err != nil {
		t.Fatalf("GET /mcp: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var got MCPInfo
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Servers) != 1 || got.Servers[0].Name != "kube-mcp" || len(got.Servers[0].Tools) != 2 {
		t.Errorf("mcp = %+v", got)
	}
}

func TestIntegration_PricingEndpoint(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	ag := &operatorRichRegistrant{
		stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
		pricing: PricingInfo{
			Source:       "litellm-cache",
			KnownModels:  847,
			CurrentModel: "claude-opus-4-7",
			Current: &ModelPricing{
				InputUSDPerMTok:  15.00,
				OutputUSDPerMTok: 75.00,
			},
		},
	}
	if _, err := reg.Register(ag); err != nil {
		t.Fatal(err)
	}
	base, cleanup := startTestServer(t, reg)
	defer cleanup()

	resp, err := http.Get(base + "/sessions/core-agent/s1/pricing")
	if err != nil {
		t.Fatalf("GET /pricing: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var got PricingInfo
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.KnownModels != 847 || got.Current == nil || got.Current.InputUSDPerMTok != 15.00 {
		t.Errorf("pricing = %+v", got)
	}
}

func TestIntegration_OperatorEndpoints_ShortcutForms(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	ag := &operatorRichRegistrant{
		stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
		usage:          UsageInfo{Overall: UsageTotals{Turns: 1}},
	}
	if _, err := reg.Register(ag); err != nil {
		t.Fatal(err)
	}
	base, cleanup := startTestServer(t, reg)
	defer cleanup()

	// Spot-check one endpoint via the single-segment shortcut — the
	// remaining five share identical routing.
	resp, err := http.Get(base + "/sessions/s1/usage")
	if err != nil {
		t.Fatalf("GET shortcut: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
}

func TestOperatorView_NilFuncsRender_EmptyData(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	// OperatorView wraps a bare Registrant with no func fields set.
	// Each Attach* method returns nil/zero; handlers emit empty data.
	view := &OperatorView{
		Registrant: &stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
	}
	if _, err := reg.Register(view); err != nil {
		t.Fatal(err)
	}
	base, cleanup := startTestServer(t, reg)
	defer cleanup()

	for _, path := range []string{"/memory", "/skills", "/mcp", "/pricing"} {
		resp, err := http.Get(base + "/sessions/core-agent/s1" + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s status %d, want 200", path, resp.StatusCode)
		}
	}
}

func TestOperatorView_PopulatedFuncs_RenderData(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	view := &OperatorView{
		Registrant: &stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
		Memory: func() []MemorySource {
			return []MemorySource{{Scope: "project", Path: "AGENTS.md", Size: 100}}
		},
		Skills: func() []SkillInfo {
			return []SkillInfo{{Name: "cli-setup", Description: "configure"}}
		},
	}
	if _, err := reg.Register(view); err != nil {
		t.Fatal(err)
	}
	base, cleanup := startTestServer(t, reg)
	defer cleanup()

	resp, err := http.Get(base + "/sessions/core-agent/s1/memory")
	if err != nil {
		t.Fatalf("GET /memory: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var got struct {
		Sources []MemorySource `json:"sources"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Sources) != 1 || got.Sources[0].Path != "AGENTS.md" {
		t.Errorf("sources = %+v", got.Sources)
	}
}
