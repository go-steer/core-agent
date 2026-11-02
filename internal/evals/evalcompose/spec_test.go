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

package evalcompose

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"
)

type stubTool struct {
	name string
}

func (s stubTool) Name() string        { return s.name }
func (s stubTool) Description() string { return "stub tool " + s.name }
func (s stubTool) IsLongRunning() bool { return false }

type stubToolset struct {
	name  string
	tools []tool.Tool
}

func (s stubToolset) Name() string { return s.name }
func (s stubToolset) Tools(agent.ReadonlyContext) ([]tool.Tool, error) {
	return s.tools, nil
}

func TestFilterTools_HonorsSpecBuiltinAllowlist(t *testing.T) {
	t.Parallel()
	tRead := stubTool{name: "read_file"}
	tWrite := stubTool{name: "write_file"}
	all := []tool.Tool{tRead, tWrite}

	// nil Builtin leaves all caller-supplied tools intact.
	gotAll := filterTools(Spec{}, all)
	if len(gotAll) != 2 {
		t.Fatalf("nil Builtin: got %d tools, want 2", len(gotAll))
	}

	// Explicit empty slice drops all built-in tools.
	gotNone := filterTools(Spec{
		Tools: ToolAllowlist{Builtin: []string{}},
	}, all)
	if len(gotNone) != 0 {
		t.Fatalf("empty Builtin: got %d tools, want 0", len(gotNone))
	}

	// Named subset keeps only declared built-in tools.
	gotSub := filterTools(Spec{
		Tools: ToolAllowlist{Builtin: []string{"read_file"}},
	}, all)
	if len(gotSub) != 1 || gotSub[0].Name() != "read_file" {
		t.Fatalf("subset Builtin: got %v, want [read_file]", gotSub)
	}
}

func TestFilterToolsets_MatchesNamespacedMCPTools(t *testing.T) {
	t.Parallel()
	// pkg/mcp.Build (withNamespace) names the toolset <server>_mcp_tool_set
	// and prefixes each tool as <server>_<tool>; callers may also pass a
	// toolset named <server> directly.
	for _, tsName := range []string{"lookout", "lookout_mcp_tool_set"} {
		t.Run(tsName, func(t *testing.T) {
			ts := stubToolset{
				name: tsName,
				tools: []tool.Tool{
					stubTool{name: "lookout_triage_workload"},
					stubTool{name: "lookout_scale_deployment"},
				},
			}
			spec := Spec{
				Tools: ToolAllowlist{
					MCP: []MCPAllowlist{{
						Server: "lookout",
						Tools:  []string{"triage_workload"},
					}},
				},
			}
			filtered := filterToolsets(spec, []tool.Toolset{nil, ts})
			if len(filtered) != 1 {
				t.Fatalf("filterToolsets len = %d, want 1", len(filtered))
			}
			tools, err := filtered[0].Tools(nil)
			if err != nil {
				t.Fatalf("filtered[0].Tools: %v", err)
			}
			if len(tools) != 1 || tools[0].Name() != "lookout_triage_workload" {
				t.Fatalf("filtered tools = %v, want [lookout_triage_workload]", tools)
			}
		})
	}
}

func TestEvalEchoModel_DetectsFinishTaskInConfigTools(t *testing.T) {
	t.Parallel()
	m := evalEchoModel{name: "eval-echo-model"}
	req := &model.LLMRequest{
		// ADK populates req.Config.Tools, leaving req.Tools nil.
		Config: &genai.GenerateContentConfig{
			Tools: []*genai.Tool{{
				FunctionDeclarations: []*genai.FunctionDeclaration{
					{Name: "finish_task"},
				},
			}},
		},
	}
	var gotCall string
	for resp, err := range m.GenerateContent(context.Background(), req, false) {
		if err != nil {
			t.Fatalf("GenerateContent: %v", err)
		}
		if resp != nil && resp.Content != nil {
			for _, p := range resp.Content.Parts {
				if p.FunctionCall != nil {
					gotCall = p.FunctionCall.Name
				}
			}
		}
	}
	if gotCall != "finish_task" {
		t.Fatalf("evalEchoModel FunctionCall = %q, want %q", gotCall, "finish_task")
	}
}

func TestDiscoverProvidersFrom_ExplicitConfigPath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	const body = `{
  "version": 1,
  "providers": [
    {
      "name": "house-vllm",
      "extends": "vllm",
      "base_url": "http://10.0.0.2:8000/v1",
      "auth": {"kind": "bearer", "env": "HOUSE_VLLM_API_KEY"},
      "models": [{"id": "google/gemma-4-26B-A4B-it", "rates": {"input_per_mtok": 0.1, "output_per_mtok": 0.4}}],
      "tiers": {"mid": "google/gemma-4-26B-A4B-it"}
    }
  ]
}`
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write config.json: %v", err)
	}
	profs, err := DiscoverProvidersFrom(cfgPath)
	if err != nil {
		t.Fatalf("DiscoverProvidersFrom: %v", err)
	}
	if len(profs) != 1 || profs[0].Name != "house-vllm" {
		t.Fatalf("DiscoverProvidersFrom = %+v, want [house-vllm]", profs)
	}
}
