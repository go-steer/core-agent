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

package agent

import (
	"context"
	"errors"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-steer/core-models/profile"
	adkagent "google.golang.org/adk/v2/agent"
	adkmodel "google.golang.org/adk/v2/model"
	adktool "google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v3/pkg/config"
	"github.com/go-steer/core-agent/v3/pkg/permissions"
	"github.com/go-steer/core-agent/v3/pkg/pricing"
	"github.com/go-steer/core-agent/v3/pkg/tools"
	"github.com/go-steer/core-agent/v3/pkg/usage"
)

type scriptedFacadeLLM struct {
	name  string
	mu    sync.Mutex
	steps []func(*adkmodel.LLMRequest) (*adkmodel.LLMResponse, error)
	calls []*adkmodel.LLMRequest
}

func (m *scriptedFacadeLLM) Name() string {
	if m.name != "" {
		return m.name
	}
	return "gemini-2.5-flash"
}

func (m *scriptedFacadeLLM) GenerateContent(_ context.Context, req *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		m.mu.Lock()
		m.calls = append(m.calls, req)
		idx := len(m.calls) - 1
		if idx >= len(m.steps) {
			m.mu.Unlock()
			yield(&adkmodel.LLMResponse{
				Content:      genai.NewContentFromText("fallback", genai.RoleModel),
				TurnComplete: true,
			}, nil)
			return
		}
		step := m.steps[idx]
		m.mu.Unlock()
		resp, err := step(req)
		yield(resp, err)
	}
}

func systemInstructionFromReq(req *adkmodel.LLMRequest) string {
	if req == nil || req.Config == nil || req.Config.SystemInstruction == nil {
		return ""
	}
	var parts []string
	for _, p := range req.Config.SystemInstruction.Parts {
		if p != nil && p.Text != "" {
			parts = append(parts, p.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func TestRunOneShot_EmptyPrompt(t *testing.T) {
	t.Parallel()
	_, err := RunOneShot(context.Background(), OneShotConfig{}, "   ")
	if err == nil {
		t.Fatal("RunOneShot with blank prompt succeeded; want error")
	}
}

func TestRunOneShot_ProgrammaticWithCustomToolsAndGate(t *testing.T) {
	t.Parallel()

	type lookupArgs struct {
		Service string `json:"service"`
	}
	type lookupOut struct {
		Owner string `json:"owner"`
	}
	lookupTool, err := functiontool.New(functiontool.Config{
		Name:        "lookup_service",
		Description: "Look up service ownership.",
	}, func(_ adkagent.Context, args lookupArgs) (lookupOut, error) {
		return lookupOut{Owner: "team-" + args.Service}, nil
	})
	if err != nil {
		t.Fatalf("functiontool.New lookup: %v", err)
	}

	dropCalled := false
	dropTool, err := functiontool.New(functiontool.Config{
		Name:        "drop_service",
		Description: "Drop a service.",
	}, func(_ adkagent.Context, _ lookupArgs) (lookupOut, error) {
		dropCalled = true
		return lookupOut{Owner: "dropped"}, nil
	})
	if err != nil {
		t.Fatalf("functiontool.New drop: %v", err)
	}

	llm := &scriptedFacadeLLM{
		name: "gemini-2.5-flash",
		steps: []func(*adkmodel.LLMRequest) (*adkmodel.LLMResponse, error){
			func(_ *adkmodel.LLMRequest) (*adkmodel.LLMResponse, error) {
				// First try allowed tool + denied tool.
				return &adkmodel.LLMResponse{
					Content: &genai.Content{
						Role: genai.RoleModel,
						Parts: []*genai.Part{
							{FunctionCall: &genai.FunctionCall{ID: "c1", Name: "lookup_service", Args: map[string]any{"service": "payments"}}},
							{FunctionCall: &genai.FunctionCall{ID: "c2", Name: "drop_service", Args: map[string]any{"service": "payments"}}},
						},
					},
					UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
						PromptTokenCount:     100,
						CandidatesTokenCount: 25,
					},
					TurnComplete: true,
				}, nil
			},
			func(_ *adkmodel.LLMRequest) (*adkmodel.LLMResponse, error) {
				return &adkmodel.LLMResponse{
					Content: genai.NewContentFromText(`{"service":"payments","owner":"team-payments","drop_blocked":true}`, genai.RoleModel),
					UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
						PromptTokenCount:     140,
						CandidatesTokenCount: 30,
					},
					TurnComplete: true,
				}, nil
			},
		},
	}

	cfg := config.DefaultConfig()
	cfg.Permissions.Mode = config.PermissionModeAllow
	cfg.Permissions.Allow = []string{"lookup_service:*"}
	cfg.Permissions.Deny = []string{"drop_service:*"}

	res, err := RunOneShot(context.Background(), OneShotConfig{
		Config:    cfg,
		Model:     llm,
		Tools:     []adktool.Tool{lookupTool, dropTool},
		SessionID: "sess-prog-1",
	}, "Check payments ownership and test drop_service.")
	if err != nil {
		t.Fatalf("RunOneShot: %v", err)
	}
	if dropCalled {
		t.Fatal("denied tool drop_service was executed; gate wrapper did not block it")
	}
	if res.SessionID != "sess-prog-1" {
		t.Errorf("SessionID = %q, want sess-prog-1", res.SessionID)
	}
	if got := res.Output["owner"]; got != "team-payments" {
		t.Errorf("Output[owner] = %v, want team-payments (full Output: %v)", got, res.Output)
	}
	if res.Usage.Turns != 2 {
		t.Errorf("Usage.Turns = %d, want 2", res.Usage.Turns)
	}
	if res.Usage.InputTokens != 240 || res.Usage.OutputTokens != 55 {
		t.Errorf("Usage tokens = (%d in, %d out), want (240 in, 55 out)", res.Usage.InputTokens, res.Usage.OutputTokens)
	}
}

func TestRunRecipe_LoadsDirConfigInstructionAndMarkdownSubagents(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	agentsDir := filepath.Join(dir, ".agents")
	subagentsDir := filepath.Join(agentsDir, "subagents")
	schemasDir := filepath.Join(agentsDir, "schemas")
	if err := os.MkdirAll(subagentsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll subagents: %v", err)
	}
	if err := os.MkdirAll(schemasDir, 0o755); err != nil {
		t.Fatalf("MkdirAll schemas: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("Recipe root instruction: always consult the reviewer subagent."), 0o644); err != nil {
		t.Fatalf("WriteFile AGENTS.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentsDir, "config.json"), []byte(`{
  "version": 1,
  "model": { "name": "gemini-2.5-flash" },
  "permissions": {
    "mode": "allow",
    "allow": ["reviewer*"]
  },
  "agent": {
    "append_system_prompt": "Always format the final answer concisely."
  }
}`), 0o644); err != nil {
		t.Fatalf("WriteFile config.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(schemasDir, "verdict.json"), []byte(`{
  "type": "object",
  "properties": {
    "verdict": { "type": "string" },
    "score": { "type": "integer" }
  },
  "required": ["verdict", "score"]
}`), 0o644); err != nil {
		t.Fatalf("WriteFile verdict.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(subagentsDir, "reviewer.md"), []byte(`---
description: Single-turn structured code reviewer.
mode: single_turn
output_schema: ../schemas/verdict.json
---
Return a JSON object with verdict and score.
`), 0o644); err != nil {
		t.Fatalf("WriteFile reviewer.md: %v", err)
	}

	var rootSysInst string
	llm := &scriptedFacadeLLM{
		name: "gemini-2.5-flash",
		steps: []func(*adkmodel.LLMRequest) (*adkmodel.LLMResponse, error){
			// Turn 1 (root): delegates to reviewer subagent.
			func(req *adkmodel.LLMRequest) (*adkmodel.LLMResponse, error) {
				rootSysInst = systemInstructionFromReq(req)
				return &adkmodel.LLMResponse{
					Content: &genai.Content{
						Role: genai.RoleModel,
						Parts: []*genai.Part{
							{FunctionCall: &genai.FunctionCall{
								ID:   "call-rev-1",
								Name: "reviewer",
								Args: map[string]any{"request": "Review change #42"},
							}},
						},
					},
					UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
						PromptTokenCount:     50,
						CandidatesTokenCount: 15,
					},
					TurnComplete: true,
				}, nil
			},
			// Turn 2 (reviewer subagent, single_turn mode with output_schema).
			func(_ *adkmodel.LLMRequest) (*adkmodel.LLMResponse, error) {
				return &adkmodel.LLMResponse{
					Content: genai.NewContentFromText(`{"verdict":"ship","score":95}`, genai.RoleModel),
					UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
						PromptTokenCount:     40,
						CandidatesTokenCount: 10,
					},
					TurnComplete: true,
				}, nil
			},
			// Turn 3 (root): summarizes reviewer result.
			func(_ *adkmodel.LLMRequest) (*adkmodel.LLMResponse, error) {
				return &adkmodel.LLMResponse{
					Content: genai.NewContentFromText(`{"summary":"Approved by reviewer","verdict":"ship"}`, genai.RoleModel),
					UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
						PromptTokenCount:     80,
						CandidatesTokenCount: 20,
					},
					TurnComplete: true,
				}, nil
			},
		},
	}

	res, err := RunRecipe(context.Background(), dir, OneShotConfig{
		Model: llm,
	}, "Run the review recipe on change #42.")
	if err != nil {
		t.Fatalf("RunRecipe: %v", err)
	}
	if !strings.Contains(rootSysInst, "Recipe root instruction: always consult the reviewer subagent.") {
		t.Errorf("root system instruction missing AGENTS.md text; got:\n%s", rootSysInst)
	}
	if !strings.Contains(rootSysInst, "Always format the final answer concisely.") {
		t.Errorf("root system instruction missing append_system_prompt; got:\n%s", rootSysInst)
	}
	if got := res.Output["verdict"]; got != "ship" {
		t.Errorf("Output[verdict] = %v, want ship (Output: %v)", got, res.Output)
	}
	// Root ran 2 turns + reviewer subagent ran 1 turn = 3 turns total billed to tracker.
	if res.Usage.Turns != 3 {
		t.Errorf("Usage.Turns = %d, want 3 (2 root + 1 subagent)", res.Usage.Turns)
	}
	if res.Usage.InputTokens != 170 || res.Usage.OutputTokens != 45 {
		t.Errorf("Usage tokens = (%d in, %d out), want (170 in, 45 out)", res.Usage.InputTokens, res.Usage.OutputTokens)
	}
}

func TestRunWorkload_FlatDirectoryWithTaskSubagent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	subDir := filepath.Join(dir, "subagents")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("Workload instructions."), 0o644); err != nil {
		t.Fatalf("WriteFile AGENTS.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(subDir, "worker.md"), []byte(`---
description: Autonomous task worker.
mode: task
---
Complete the delegated task and call finish_task.
`), 0o644); err != nil {
		t.Fatalf("WriteFile worker.md: %v", err)
	}

	llm := &scriptedFacadeLLM{
		name: "gemini-2.5-flash",
		steps: []func(*adkmodel.LLMRequest) (*adkmodel.LLMResponse, error){
			// Root delegates to worker.
			func(_ *adkmodel.LLMRequest) (*adkmodel.LLMResponse, error) {
				return &adkmodel.LLMResponse{
					Content: &genai.Content{
						Role: genai.RoleModel,
						Parts: []*genai.Part{
							{FunctionCall: &genai.FunctionCall{
								ID:   "w-1",
								Name: "worker",
								Args: map[string]any{"request": "Investigate latency"},
							}},
						},
					},
					TurnComplete: true,
				}, nil
			},
			// Worker calls finish_task (which escalates and ends the worker loop immediately).
			func(_ *adkmodel.LLMRequest) (*adkmodel.LLMResponse, error) {
				return &adkmodel.LLMResponse{
					Content: &genai.Content{
						Role: genai.RoleModel,
						Parts: []*genai.Part{
							{FunctionCall: &genai.FunctionCall{
								ID:   "ft-1",
								Name: FinishTaskToolName,
								Args: map[string]any{"result": "p99 latency is normal at 12ms"},
							}},
						},
					},
					TurnComplete: true,
				}, nil
			},
			// Root final answer.
			func(_ *adkmodel.LLMRequest) (*adkmodel.LLMResponse, error) {
				return &adkmodel.LLMResponse{
					Content:      genai.NewContentFromText("Investigation complete: p99 latency is normal at 12ms.", genai.RoleModel),
					TurnComplete: true,
				}, nil
			},
		},
	}

	cfg := config.DefaultConfig()
	cfg.Permissions.Mode = config.PermissionModeYolo

	res, err := RunWorkload(context.Background(), dir, OneShotConfig{
		Config: cfg,
		Model:  llm,
	}, "Run workload check.")
	if err != nil {
		t.Fatalf("RunWorkload: %v", err)
	}
	if !strings.Contains(res.Text, "p99 latency is normal at 12ms") {
		t.Errorf("Text = %q, want latency summary", res.Text)
	}
}

func TestMergeFacadeConfig_PreservesBaseNestedFields(t *testing.T) {
	t.Parallel()

	f := false
	tr := true
	base := config.DefaultConfig()
	base.Permissions.PlanMode = config.PlanModeRequired
	base.Permissions.UseBuiltinAllow = &f
	base.Permissions.BuiltinAllowExtras = []string{"git_read"}
	base.Permissions.ApprovalTimeout = "5m"
	base.Permissions.ApprovalNotify = "ops-slack"
	base.PathScope.Allow = []string{"/var/log/..."}
	base.Safety.Watchdog = "enforce"
	base.URLScope.Allow = []string{"*.internal.example.com"}
	base.Agent.DisplayName = "Cluster Guardian"
	base.ContentRoots = []string{"../shared-agents"}
	base.Model.Vertex = &config.VertexConfig{Project: "prod-proj", Location: "us-central1"}
	base.Model.Anthropic = &config.AnthropicConfig{
		PromptCache: &config.PromptCacheConfig{TTL: config.PromptCacheTTL1h},
	}
	base.Model.BuiltinTools = &config.BuiltinToolsConfig{
		WebSearch:  &f,
		URLContext: &tr,
	}

	override := &config.Config{
		Permissions: config.PermissionsConfig{
			Mode: config.PermissionModeYolo,
		},
		Model: config.ModelConfig{
			Name: "gemini-2.5-pro",
			BuiltinTools: &config.BuiltinToolsConfig{
				CodeExecution: &tr,
			},
		},
	}

	merged := mergeFacadeConfig(base, override)
	if merged.Permissions.Mode != config.PermissionModeYolo {
		t.Errorf("Permissions.Mode = %q, want yolo", merged.Permissions.Mode)
	}
	if merged.Permissions.PlanMode != config.PlanModeRequired {
		t.Errorf("Permissions.PlanMode = %q, want required", merged.Permissions.PlanMode)
	}
	if merged.Permissions.UseBuiltinAllow == nil || *merged.Permissions.UseBuiltinAllow != false {
		t.Errorf("Permissions.UseBuiltinAllow = %v, want false", merged.Permissions.UseBuiltinAllow)
	}
	if len(merged.Permissions.BuiltinAllowExtras) != 1 || merged.Permissions.BuiltinAllowExtras[0] != "git_read" {
		t.Errorf("Permissions.BuiltinAllowExtras = %v, want [git_read]", merged.Permissions.BuiltinAllowExtras)
	}
	if merged.Permissions.ApprovalTimeout != "5m" || merged.Permissions.ApprovalNotify != "ops-slack" {
		t.Errorf("ApprovalTimeout/Notify = (%q, %q), want (5m, ops-slack)", merged.Permissions.ApprovalTimeout, merged.Permissions.ApprovalNotify)
	}
	if len(merged.PathScope.Allow) != 1 || merged.PathScope.Allow[0] != "/var/log/..." {
		t.Errorf("PathScope.Allow = %v, want [/var/log/...]", merged.PathScope.Allow)
	}
	if merged.Safety.Watchdog != "enforce" {
		t.Errorf("Safety.Watchdog = %q, want enforce", merged.Safety.Watchdog)
	}
	if len(merged.URLScope.Allow) != 1 || merged.URLScope.Allow[0] != "*.internal.example.com" {
		t.Errorf("URLScope.Allow = %v, want [*.internal.example.com]", merged.URLScope.Allow)
	}
	if merged.Agent.DisplayName != "Cluster Guardian" {
		t.Errorf("Agent.DisplayName = %q, want Cluster Guardian", merged.Agent.DisplayName)
	}
	if len(merged.ContentRoots) != 1 || merged.ContentRoots[0] != "../shared-agents" {
		t.Errorf("ContentRoots = %v, want [../shared-agents]", merged.ContentRoots)
	}
	if merged.Model.Vertex == nil || merged.Model.Vertex.Project != "prod-proj" {
		t.Errorf("Model.Vertex = %+v, want prod-proj", merged.Model.Vertex)
	}
	if merged.Model.Anthropic == nil || merged.Model.Anthropic.PromptCache == nil || merged.Model.Anthropic.PromptCache.TTL != config.PromptCacheTTL1h {
		t.Errorf("Model.Anthropic = %+v, want 1h prompt cache", merged.Model.Anthropic)
	}
	if merged.Model.BuiltinTools == nil ||
		merged.Model.BuiltinTools.WebSearch == nil || *merged.Model.BuiltinTools.WebSearch != false ||
		merged.Model.BuiltinTools.URLContext == nil || *merged.Model.BuiltinTools.URLContext != true ||
		merged.Model.BuiltinTools.CodeExecution == nil || *merged.Model.BuiltinTools.CodeExecution != true {
		t.Errorf("Model.BuiltinTools = %+v, want WebSearch=false, URLContext=true, CodeExecution=true", merged.Model.BuiltinTools)
	}
}

type staticToolset struct {
	name  string
	tools []adktool.Tool
}

func (s *staticToolset) Name() string { return s.name }
func (s *staticToolset) Tools(_ adkagent.ReadonlyContext) ([]adktool.Tool, error) {
	return s.tools, nil
}

func TestFacadeSubagents_GovernanceAndContracts(t *testing.T) {
	t.Parallel()

	t.Run("unknown tool in allowlist returns error", func(t *testing.T) {
		t.Parallel()
		spec := config.SubagentSpec{
			Name:  "helper",
			Tools: []string{"nonexistent_tool"},
		}
		if _, err := selectFacadeSubagentTools(spec, nil, nil); err == nil || !strings.Contains(err.Error(), "unknown tool") {
			t.Fatalf("selectFacadeSubagentTools err = %v, want unknown tool error", err)
		}
	})

	t.Run("inherit tools carves out spawn_agent and spawn_remote_agent", func(t *testing.T) {
		t.Parallel()
		type dummyIn struct{}
		type dummyOut struct{}
		spawnTool, _ := functiontool.New(functiontool.Config{Name: "spawn_agent", Description: "spawn"}, func(_ adkagent.Context, _ dummyIn) (dummyOut, error) {
			return dummyOut{}, nil
		})
		spawnRemoteTool, _ := functiontool.New(functiontool.Config{Name: "spawn_remote_agent", Description: "spawn remote"}, func(_ adkagent.Context, _ dummyIn) (dummyOut, error) {
			return dummyOut{}, nil
		})
		readTool, _ := functiontool.New(functiontool.Config{Name: "read_file", Description: "read"}, func(_ adkagent.Context, _ dummyIn) (dummyOut, error) {
			return dummyOut{}, nil
		})
		got, err := selectFacadeSubagentTools(config.SubagentSpec{Name: "reader"}, []adktool.Tool{spawnTool, spawnRemoteTool, readTool}, nil)
		if err != nil {
			t.Fatalf("selectFacadeSubagentTools: %v", err)
		}
		if len(got) != 1 || got[0].Name() != "read_file" {
			t.Fatalf("inherited tools = %v, want only [read_file]", got)
		}
	})

	t.Run("capability read_only rejects mutating tool and mutating toolset", func(t *testing.T) {
		t.Parallel()
		type dummyIn struct{}
		type dummyOut struct{}
		mutTool, _ := functiontool.New(functiontool.Config{Name: "write_file", Description: "write"}, func(_ adkagent.Context, _ dummyIn) (dummyOut, error) {
			return dummyOut{}, nil
		})
		spec := config.SubagentSpec{
			Name:       "auditor",
			Capability: config.CapabilityReadOnly,
		}
		if err := checkFacadeSubagentCapability(context.Background(), spec, nil, []adktool.Tool{mutTool}, nil); err == nil {
			t.Fatal("expected error for mutating built-in tool on read_only subagent")
		}
		ts := &staticToolset{name: "k8s", tools: []adktool.Tool{mutTool}}
		if err := checkFacadeSubagentCapability(context.Background(), spec, nil, nil, []adktool.Toolset{ts}); err == nil {
			t.Fatal("expected error for mutating toolset tool on read_only subagent")
		}
	})

	t.Run("tools empty slice still inherits MCP toolsets when MCP is nil", func(t *testing.T) {
		t.Parallel()
		ts := &staticToolset{name: "k8s"}
		got, err := selectFacadeSubagentToolsets(context.Background(), config.SubagentSpec{
			Name:  "mcp_only",
			Tools: []string{},
			MCP:   nil,
		}, []adktool.Toolset{ts})
		if err != nil {
			t.Fatalf("selectFacadeSubagentToolsets: %v", err)
		}
		if len(got) != 1 || got[0].Name() != "k8s" {
			t.Fatalf("selected toolsets = %v, want [k8s]", got)
		}

		none, err := selectFacadeSubagentToolsets(context.Background(), config.SubagentSpec{
			Name: "no_mcp",
			MCP:  []string{},
		}, []adktool.Toolset{ts})
		if err != nil {
			t.Fatalf("selectFacadeSubagentToolsets empty: %v", err)
		}
		if len(none) != 0 {
			t.Fatalf("selected toolsets for mcp: [] = %v, want empty", none)
		}
	})

	t.Run("yaml output_schema loads via BaseDir", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		schemaPath := filepath.Join(dir, "verdict.yaml")
		if err := os.WriteFile(schemaPath, []byte("type: object\nproperties:\n  ok:\n    type: boolean\nrequired: [ok]\n"), 0o644); err != nil {
			t.Fatalf("WriteFile schema: %v", err)
		}
		s, err := loadFacadeOutputSchema("verdict.yaml", dir)
		if err != nil {
			t.Fatalf("loadFacadeOutputSchema: %v", err)
		}
		if s == nil || s.Properties["ok"] == nil {
			t.Fatalf("loaded schema = %+v, want properties.ok", s)
		}
	})
}

type noopApprover struct{}

func (noopApprover) Judge(_ context.Context, _ permissions.ApproverRequest) (permissions.Verdict, error) {
	return permissions.Verdict{Outcome: permissions.VerdictAllow}, nil
}

func TestRunOneShot_StampsOperatorTaskOnContext(t *testing.T) {
	t.Parallel()

	var observedTask string
	type pingIn struct{}
	type pingOut struct {
		OK bool `json:"ok"`
	}
	pingTool, err := functiontool.New(functiontool.Config{
		Name:        "check_task",
		Description: "Reads ApproverContext.Task from context.",
	}, func(ctx adkagent.Context, _ pingIn) (pingOut, error) {
		if ac, ok := permissions.ApproverContextFromContext(ctx); ok {
			observedTask = ac.Task()
		}
		return pingOut{OK: true}, nil
	})
	if err != nil {
		t.Fatalf("functiontool.New: %v", err)
	}

	llm := &scriptedFacadeLLM{
		name: "gemini-2.5-flash",
		steps: []func(*adkmodel.LLMRequest) (*adkmodel.LLMResponse, error){
			func(_ *adkmodel.LLMRequest) (*adkmodel.LLMResponse, error) {
				return &adkmodel.LLMResponse{
					Content: &genai.Content{
						Role: genai.RoleModel,
						Parts: []*genai.Part{
							{FunctionCall: &genai.FunctionCall{ID: "c1", Name: "check_task", Args: map[string]any{}}},
						},
					},
					TurnComplete: true,
				}, nil
			},
			func(_ *adkmodel.LLMRequest) (*adkmodel.LLMResponse, error) {
				return &adkmodel.LLMResponse{
					Content:      genai.NewContentFromText("done", genai.RoleModel),
					TurnComplete: true,
				}, nil
			},
		},
	}

	gate := permissions.New(permissions.Options{
		Mode:     permissions.ModeYolo,
		Approver: noopApprover{},
	})
	const wantPrompt = "Verify operator task context propagation"
	if _, err := RunOneShot(context.Background(), OneShotConfig{
		Gate:  gate,
		Model: llm,
		Tools: []adktool.Tool{pingTool},
	}, wantPrompt); err != nil {
		t.Fatalf("RunOneShot: %v", err)
	}
	if observedTask != wantPrompt {
		t.Errorf("ApproverContext.Task() = %q, want %q", observedTask, wantPrompt)
	}
}

func TestRunOneShot_PerAgentPricingCatalogAndProfileTierSubagent(t *testing.T) {
	// Not parallel: installs a process-global usage.SetCatalog to prove
	// RunOneShot's per-agent pricing catalog still honors Config.Providers
	// profile rates and .agents/pricing.json even after SetCatalog.
	prevCat, err := pricing.NewCatalog(pricing.Options{})
	if err != nil {
		t.Fatalf("pricing.NewCatalog: %v", err)
	}
	usage.SetCatalog(prevCat)
	t.Cleanup(func() { usage.SetCatalog(nil) })

	dir := t.TempDir()
	agentsDir := filepath.Join(dir, ".agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentsDir, "pricing.json"), []byte(`{
  "models": {
    "file-priced-model": { "input_per_mtok": 4.0, "output_per_mtok": 8.0 }
  }
}`), 0o644); err != nil {
		t.Fatalf("WriteFile pricing.json: %v", err)
	}

	llm := &scriptedFacadeLLM{
		name: "custom-model",
		steps: []func(*adkmodel.LLMRequest) (*adkmodel.LLMResponse, error){
			// Root turn 1: delegates to tier-routed subagent.
			func(_ *adkmodel.LLMRequest) (*adkmodel.LLMResponse, error) {
				return &adkmodel.LLMResponse{
					Content: &genai.Content{
						Role: genai.RoleModel,
						Parts: []*genai.Part{
							{FunctionCall: &genai.FunctionCall{
								ID:   "sub-1",
								Name: "tier_helper",
								Args: map[string]any{"request": "check status"},
							}},
						},
					},
					UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
						PromptTokenCount:     1000,
						CandidatesTokenCount: 500,
					},
					TurnComplete: true,
				}, nil
			},
			// Subagent turn: 1000 input + 500 output tokens.
			func(_ *adkmodel.LLMRequest) (*adkmodel.LLMResponse, error) {
				return &adkmodel.LLMResponse{
					Content: genai.NewContentFromText(`{"ok":true}`, genai.RoleModel),
					UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
						PromptTokenCount:     1000,
						CandidatesTokenCount: 500,
					},
					TurnComplete: true,
				}, nil
			},
			// Root turn 2: 1000 input + 500 output tokens.
			func(_ *adkmodel.LLMRequest) (*adkmodel.LLMResponse, error) {
				return &adkmodel.LLMResponse{
					Content: genai.NewContentFromText(`{"done":true}`, genai.RoleModel),
					UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
						PromptTokenCount:     1000,
						CandidatesTokenCount: 500,
					},
					TurnComplete: true,
				}, nil
			},
		},
	}

	cfg := config.DefaultConfig()
	cfg.Permissions.Mode = config.PermissionModeYolo
	cfg.Model.Provider = "house-vllm"
	cfg.Model.Name = "custom-model"
	cfg.Providers = []profile.Profile{
		{
			Name:    "house-vllm",
			Extends: "vllm",
			BaseURL: "http://127.0.0.1:8000/v1",
			Models: []profile.Model{
				{
					ID:    "custom-model",
					Rates: &profile.Rates{InputPerMTok: 10.0, OutputPerMTok: 20.0},
				},
				{
					ID:    "custom-small-model",
					Rates: &profile.Rates{InputPerMTok: 2.0, OutputPerMTok: 4.0},
				},
			},
			Tiers: map[profile.Tier]string{
				profile.Tier("small"): "custom-small-model",
				profile.Tier("mid"):   "custom-model",
			},
		},
	}
	cfg.Subagents = []config.SubagentSpec{
		{
			Name:         "tier_helper",
			Description:  "Helper using small tier on profile provider.",
			Mode:         config.SubagentModeSingleTurn,
			Tier:         "small",
			Instructions: "Return ok.",
		},
	}

	res, err := RunOneShot(context.Background(), OneShotConfig{
		Dir:    dir,
		Config: cfg,
		Model:  llm,
	}, "Run profile pricing test.")
	if err != nil {
		t.Fatalf("RunOneShot: %v", err)
	}
	// Root turns (2): each 1000 in ($0.01) + 500 out ($0.01) = $0.02/turn -> $0.04.
	// Subagent turn (1 on custom-small-model): 1000 in ($0.002) + 500 out ($0.002) = $0.004.
	// Total = $0.044.
	if res.Usage.UnpricedTurns > 0 {
		t.Errorf("Usage.UnpricedTurns = %d, want 0", res.Usage.UnpricedTurns)
	}
	if res.Usage.CostUSD < 0.0439 || res.Usage.CostUSD > 0.0441 {
		t.Errorf("Usage.CostUSD = %f, want ~0.044", res.Usage.CostUSD)
	}

	// Verify .agents/pricing.json is also honored via NewCatalogFromConfig.
	fileLLM := &scriptedFacadeLLM{
		name: "file-priced-model",
		steps: []func(*adkmodel.LLMRequest) (*adkmodel.LLMResponse, error){
			func(_ *adkmodel.LLMRequest) (*adkmodel.LLMResponse, error) {
				return &adkmodel.LLMResponse{
					Content: genai.NewContentFromText("done", genai.RoleModel),
					UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
						PromptTokenCount:     1000,
						CandidatesTokenCount: 1000,
					},
					TurnComplete: true,
				}, nil
			},
		},
	}
	fileCfg := config.DefaultConfig()
	fileCfg.Model.Name = "file-priced-model"
	fileRes, err := RunOneShot(context.Background(), OneShotConfig{
		Dir:    dir,
		Config: fileCfg,
		Model:  fileLLM,
	}, "Check file pricing.")
	if err != nil {
		t.Fatalf("RunOneShot file pricing: %v", err)
	}
	// 1000 in @ $4/MTok ($0.004) + 1000 out @ $8/MTok ($0.008) = $0.012.
	if fileRes.Usage.CostUSD < 0.0119 || fileRes.Usage.CostUSD > 0.0121 {
		t.Errorf("file-priced CostUSD = %f, want ~0.012", fileRes.Usage.CostUSD)
	}
}

type readOnlyHintedTool struct {
	adktool.Tool
	readOnly bool
}

func (r readOnlyHintedTool) ReadOnlyHint() bool { return r.readOnly }

func TestFacadeSubagents_MCPToolsServerMatchingAndSchemaScope(t *testing.T) {
	t.Parallel()

	type dummyIn struct{}
	type dummyOut struct{}
	rawGetTool, _ := functiontool.New(functiontool.Config{Name: "kube_api_get_pods", Description: "get pods"}, func(_ adkagent.Context, _ dummyIn) (dummyOut, error) {
		return dummyOut{}, nil
	})
	getTool := readOnlyHintedTool{Tool: rawGetTool, readOnly: true}
	delTool, _ := functiontool.New(functiontool.Config{Name: "kube_api_delete_pod", Description: "delete pod"}, func(_ adkagent.Context, _ dummyIn) (dummyOut, error) {
		return dummyOut{}, nil
	})

	kubeTS := &staticToolset{name: "kube_api_mcp_tool_set", tools: []adktool.Tool{getTool, delTool}}
	k8sAdminTS := &staticToolset{name: "k8s_admin_mcp_tool_set", tools: []adktool.Tool{delTool}}

	t.Run("prefix collision k8s vs k8s_admin is rejected", func(t *testing.T) {
		t.Parallel()
		_, err := selectFacadeSubagentToolsets(context.Background(), config.SubagentSpec{
			Name: "auditor",
			MCP:  []string{"k8s"},
		}, []adktool.Toolset{k8sAdminTS})
		if err == nil || !strings.Contains(err.Error(), `unknown mcp server "k8s"`) {
			t.Fatalf("selectFacadeSubagentToolsets err = %v, want unknown mcp server k8s", err)
		}
	})

	t.Run("hyphenated server and mcp_tools allowlist filter tools before capability check", func(t *testing.T) {
		t.Parallel()
		spec := config.SubagentSpec{
			Name:       "auditor",
			Capability: config.CapabilityReadOnly,
			MCP:        []string{"kube-api"},
			MCPTools: []config.MCPToolAllowlist{
				{Server: "kube-api", Tools: []string{"get_pods"}},
			},
		}
		sets, err := selectFacadeSubagentToolsets(context.Background(), spec, []adktool.Toolset{kubeTS, k8sAdminTS})
		if err != nil {
			t.Fatalf("selectFacadeSubagentToolsets: %v", err)
		}
		if len(sets) != 1 {
			t.Fatalf("len(sets) = %d, want 1", len(sets))
		}
		// Without mcp_tools filtering, kube_api_delete_pod would fail read_only capability check.
		if err := checkFacadeSubagentCapability(context.Background(), spec, nil, nil, sets); err != nil {
			t.Fatalf("checkFacadeSubagentCapability after mcp_tools filter: %v", err)
		}

		// Verify a filtered MCP toolset that was already gated under "mcp" is not
		// re-wrapped under ts.Name() when GateToolset runs again.
		mcpGate := permissions.New(permissions.Options{
			Mode: permissions.ModeAllow,
		})
		gatedMCP := tools.GateToolset(kubeTS, mcpGate, "mcp")
		filteredSets, err := selectFacadeSubagentToolsets(context.Background(), spec, []adktool.Toolset{gatedMCP})
		if err != nil || len(filteredSets) != 1 {
			t.Fatalf("selectFacadeSubagentToolsets(gatedMCP): len=%d, err=%v", len(filteredSets), err)
		}
		reGated := tools.GateToolset(filteredSets[0], mcpGate, filteredSets[0].Name())
		if reGated != filteredSets[0] {
			t.Errorf("tools.GateToolset re-wrapped already-gated filtered toolset under %q", filteredSets[0].Name())
		}
	})

	t.Run("output_schema escaping .agents into projectRoot is rejected", func(t *testing.T) {
		t.Parallel()
		projectRoot := t.TempDir()
		agentsDir := filepath.Join(projectRoot, ".agents")
		subDir := filepath.Join(agentsDir, "subagents")
		if err := os.MkdirAll(subDir, 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(filepath.Join(projectRoot, "secret-schema.json"), []byte(`{"type":"object"}`), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		spec := config.SubagentSpec{
			Name:         "escaped",
			Mode:         config.SubagentModeSingleTurn,
			OutputSchema: "../../secret-schema.json",
		}
		spec.SetBaseDir(subDir)
		_, err := buildSingleFacadeSubagent(
			context.Background(), OneShotConfig{}, spec, config.DefaultConfig(),
			&scriptedFacadeLLM{}, "", permissions.New(permissions.Options{Mode: permissions.ModeYolo}),
			usage.NewTracker(), nil, nil, nil, nil, "", projectRoot, agentsDir,
		)
		if err == nil || !strings.Contains(err.Error(), "path escapes scope root") {
			t.Fatalf("buildSingleFacadeSubagent err = %v, want path escapes scope root", err)
		}
	})
}

func TestRunOneShot_SystemPromptFileAndAppendAtFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "replace.md"), []byte("Custom replaced root prompt."), 0o644); err != nil {
		t.Fatalf("WriteFile replace.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "append.md"), []byte("Appended prompt from @file."), 0o644); err != nil {
		t.Fatalf("WriteFile append.md: %v", err)
	}

	var gotSysInst string
	llm := &scriptedFacadeLLM{
		name: "gemini-2.5-flash",
		steps: []func(*adkmodel.LLMRequest) (*adkmodel.LLMResponse, error){
			func(req *adkmodel.LLMRequest) (*adkmodel.LLMResponse, error) {
				gotSysInst = systemInstructionFromReq(req)
				return &adkmodel.LLMResponse{
					Content:      genai.NewContentFromText("ok", genai.RoleModel),
					TurnComplete: true,
				}, nil
			},
		},
	}

	cfg := config.DefaultConfig()
	cfg.Agent.SystemPromptFile = "replace.md"
	cfg.Agent.AppendSystemPrompt = "@append.md"

	if _, err := RunOneShot(context.Background(), OneShotConfig{
		Dir:    dir,
		Config: cfg,
		Model:  llm,
	}, "Ping."); err != nil {
		t.Fatalf("RunOneShot: %v", err)
	}
	if !strings.Contains(gotSysInst, "Custom replaced root prompt.") {
		t.Errorf("system instruction missing system_prompt_file content; got:\n%s", gotSysInst)
	}
	if !strings.Contains(gotSysInst, "Appended prompt from @file.") {
		t.Errorf("system instruction missing @append.md content; got:\n%s", gotSysInst)
	}
}

func TestWithMaxWallclock_EnforcesTurnTimeout(t *testing.T) {
	t.Parallel()

	llm := &scriptedFacadeLLM{
		name: "gemini-2.5-flash",
		steps: []func(*adkmodel.LLMRequest) (*adkmodel.LLMResponse, error){
			func(_ *adkmodel.LLMRequest) (*adkmodel.LLMResponse, error) {
				time.Sleep(120 * time.Millisecond)
				return &adkmodel.LLMResponse{
					Content:      genai.NewContentFromText("too slow", genai.RoleModel),
					TurnComplete: true,
				}, nil
			},
		},
	}

	a, err := New(llm, WithName("wallclock_test"), WithMaxWallclock(25*time.Millisecond))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var gotErr error
	for _, err := range a.Run(context.Background(), "hello") {
		if err != nil {
			gotErr = err
			break
		}
	}
	if !errors.Is(gotErr, context.DeadlineExceeded) {
		t.Fatalf("Run err = %v, want context.DeadlineExceeded", gotErr)
	}
}

func TestSubagentFinalReport_GrantsLastChanceFinishTaskOnMaxTurns(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	subDir := filepath.Join(dir, "subagents")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subDir, "diagnoser.md"), []byte(`---
description: Autonomous task diagnoser.
mode: task
budgets:
  max_turns: 1
---
Diagnose the pod and call finish_task.
`), 0o644); err != nil {
		t.Fatal(err)
	}

	type readIn struct{}
	type readOut struct {
		Status string `json:"status"`
	}
	readTool, err := functiontool.New(functiontool.Config{
		Name:        "read_file",
		Description: "Reads cluster status.",
	}, func(_ adkagent.Context, _ readIn) (readOut, error) {
		return readOut{Status: "CrashLoopBackOff"}, nil
	})
	if err != nil {
		t.Fatalf("functiontool.New: %v", err)
	}

	var secondCallTools []string
	llm := &scriptedFacadeLLM{
		name: "gemini-2.5-flash",
		steps: []func(*adkmodel.LLMRequest) (*adkmodel.LLMResponse, error){
			// Root spawns diagnoser.
			func(_ *adkmodel.LLMRequest) (*adkmodel.LLMResponse, error) {
				return &adkmodel.LLMResponse{
					Content: &genai.Content{
						Role: genai.RoleModel,
						Parts: []*genai.Part{
							{FunctionCall: &genai.FunctionCall{ID: "d1", Name: "diagnoser", Args: map[string]any{"request": "diagnose pod"}}},
						},
					},
					TurnComplete: true,
				}, nil
			},
			// Subagent turn 1 (normal): calls read_file -> exhausts MaxTurns=1.
			func(_ *adkmodel.LLMRequest) (*adkmodel.LLMResponse, error) {
				return &adkmodel.LLMResponse{
					Content: &genai.Content{
						Role: genai.RoleModel,
						Parts: []*genai.Part{
							{FunctionCall: &genai.FunctionCall{ID: "r1", Name: "read_file", Args: map[string]any{}}},
						},
					},
					UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
						PromptTokenCount:     10,
						CandidatesTokenCount: 5,
					},
					TurnComplete: true,
				}, nil
			},
			// Subagent turn 2 (final report turn): only finish_task should be offered!
			func(req *adkmodel.LLMRequest) (*adkmodel.LLMResponse, error) {
				if req.Config != nil {
					for _, gt := range req.Config.Tools {
						for _, fd := range gt.FunctionDeclarations {
							secondCallTools = append(secondCallTools, fd.Name)
						}
					}
				}
				return &adkmodel.LLMResponse{
					Content: &genai.Content{
						Role: genai.RoleModel,
						Parts: []*genai.Part{
							{FunctionCall: &genai.FunctionCall{
								ID:   "f1",
								Name: FinishTaskToolName,
								Args: map[string]any{"result": "best-effort report after budget hit"},
							}},
						},
					},
					UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
						PromptTokenCount:     15,
						CandidatesTokenCount: 8,
					},
					TurnComplete: true,
				}, nil
			},
			// Root receives final report and finishes.
			func(_ *adkmodel.LLMRequest) (*adkmodel.LLMResponse, error) {
				return &adkmodel.LLMResponse{
					Content:      genai.NewContentFromText("Root got: best-effort report after budget hit", genai.RoleModel),
					TurnComplete: true,
				}, nil
			},
		},
	}

	cfg := config.DefaultConfig()
	cfg.Permissions.Mode = config.PermissionModeYolo
	cfg.Agent.FinalReport = true

	res, err := RunWorkload(context.Background(), dir, OneShotConfig{
		Config: cfg,
		Model:  llm,
		Tools:  []adktool.Tool{readTool},
	}, "diagnose pod")
	if err != nil {
		t.Fatalf("RunWorkload: %v", err)
	}
	if len(secondCallTools) != 1 || secondCallTools[0] != FinishTaskToolName {
		t.Errorf("second subagent call tools = %v, want only [%s]", secondCallTools, FinishTaskToolName)
	}
	if !strings.Contains(res.Text, "best-effort report after budget hit") {
		t.Errorf("RunWorkload Text = %q, want final report summary", res.Text)
	}
}

func TestRunWorkload_DirectBundleAndMCPToolsFiltering(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "specialists"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "coordinator.md"), []byte("Coordinator system instruction sentinel."), 0o644); err != nil {
		t.Fatal(err)
	}
	specMD := `---
name: _fallback
description: Read-only diagnoser.
mode: Task
tools:
  mcp:
    - server: gke
      tools: [get_pods]
---
Diagnose pods.
`
	if err := os.WriteFile(filepath.Join(dir, "specialists", "_fallback.specialist.md"), []byte(specMD), 0o644); err != nil {
		t.Fatal(err)
	}
	wlYAML := `schema_version: 1
name: raw-wl
specialists: [_fallback]
budget:
  max_wallclock_seconds: 60
  final_report: true
tool_catalog:
  mcp:
    - server: gke
  tools:
    - name: get_pods
      mutating: false
    - name: delete_pod
      mutating: true
`
	if err := os.WriteFile(filepath.Join(dir, "workload.yaml"), []byte(wlYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	type toolIn struct{}
	type toolOut struct {
		Result string `json:"result"`
	}
	getPods, _ := functiontool.New(functiontool.Config{Name: "gke_get_pods", Description: "get pods"}, func(_ adkagent.Context, _ toolIn) (toolOut, error) {
		return toolOut{Result: "ok"}, nil
	})
	deletePod, _ := functiontool.New(functiontool.Config{Name: "gke_delete_pod", Description: "delete pod"}, func(_ adkagent.Context, _ toolIn) (toolOut, error) {
		return toolOut{Result: "deleted"}, nil
	})
	gkeToolset := &staticToolset{name: "gke", tools: []adktool.Tool{getPods, deletePod}}

	var sawCoordinatorPrompt bool
	llm := &scriptedFacadeLLM{
		name: "gemini-2.5-flash",
		steps: []func(*adkmodel.LLMRequest) (*adkmodel.LLMResponse, error){
			func(req *adkmodel.LLMRequest) (*adkmodel.LLMResponse, error) {
				if req.Config != nil && req.Config.SystemInstruction != nil {
					for _, p := range req.Config.SystemInstruction.Parts {
						if strings.Contains(p.Text, "Coordinator system instruction sentinel.") {
							sawCoordinatorPrompt = true
						}
					}
				}
				return &adkmodel.LLMResponse{
					Content:      genai.NewContentFromText("completed raw workload", genai.RoleModel),
					TurnComplete: true,
				}, nil
			},
		},
	}

	res, err := RunWorkload(context.Background(), dir, OneShotConfig{
		Model:    llm,
		Toolsets: []adktool.Toolset{gkeToolset},
	}, "check cluster")
	if err != nil {
		t.Fatalf("RunWorkload on raw workload.yaml dir: %v", err)
	}
	if !sawCoordinatorPrompt {
		t.Error("RunWorkload did not load coordinator.md instruction")
	}
	if res.Text != "completed raw workload" {
		t.Errorf("Text = %q, want 'completed raw workload'", res.Text)
	}
}

func TestWithMaxSteps_EnforcesPerTurnCapAndFinalReport(t *testing.T) {
	t.Parallel()

	type pingIn struct{}
	type pingOut struct {
		OK bool `json:"ok"`
	}
	pingTool, err := functiontool.New(functiontool.Config{
		Name:        "ping_tool",
		Description: "Pings a target.",
	}, func(_ adkagent.Context, _ pingIn) (pingOut, error) {
		return pingOut{OK: true}, nil
	})
	if err != nil {
		t.Fatalf("functiontool.New: %v", err)
	}

	t.Run("stops turn after max_steps without final_report", func(t *testing.T) {
		t.Parallel()
		var providerCalls int
		llm := &scriptedFacadeLLM{
			name: "gemini-2.5-flash",
			steps: []func(*adkmodel.LLMRequest) (*adkmodel.LLMResponse, error){
				func(_ *adkmodel.LLMRequest) (*adkmodel.LLMResponse, error) {
					providerCalls++
					return &adkmodel.LLMResponse{
						Content: &genai.Content{
							Role:  genai.RoleModel,
							Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{ID: "c1", Name: "ping_tool", Args: map[string]any{}}}},
						},
						TurnComplete: true,
					}, nil
				},
				func(_ *adkmodel.LLMRequest) (*adkmodel.LLMResponse, error) {
					providerCalls++
					return &adkmodel.LLMResponse{
						Content: &genai.Content{
							Role:  genai.RoleModel,
							Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{ID: "c2", Name: "ping_tool", Args: map[string]any{}}}},
						},
						TurnComplete: true,
					}, nil
				},
				func(_ *adkmodel.LLMRequest) (*adkmodel.LLMResponse, error) {
					providerCalls++
					return &adkmodel.LLMResponse{
						Content:      genai.NewContentFromText("unreachable third call", genai.RoleModel),
						TurnComplete: true,
					}, nil
				},
			},
		}

		a, err := New(llm,
			WithName("step_capped"),
			WithTools([]adktool.Tool{pingTool}),
			WithMaxSteps(2),
		)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		var sb strings.Builder
		for ev, err := range a.Run(context.Background(), "loop forever") {
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			collectFinalText(&sb, ev)
		}
		if providerCalls != 2 {
			t.Fatalf("providerCalls = %d, want 2 (stopped before 3rd call)", providerCalls)
		}
		if !Refused(sb.String()) || !strings.Contains(sb.String(), "max_steps limit of 2") {
			t.Errorf("final text = %q, want refusal naming max_steps limit of 2", sb.String())
		}

		// A subsequent turn on the same Agent gets a fresh step counter.
		providerCalls = 0
		llm.calls = nil
		llm.steps = []func(*adkmodel.LLMRequest) (*adkmodel.LLMResponse, error){
			func(_ *adkmodel.LLMRequest) (*adkmodel.LLMResponse, error) {
				providerCalls++
				return &adkmodel.LLMResponse{
					Content:      genai.NewContentFromText("second turn ok", genai.RoleModel),
					TurnComplete: true,
				}, nil
			},
		}
		sb.Reset()
		for ev, err := range a.Run(context.Background(), "next turn") {
			if err != nil {
				t.Fatalf("second Run: %v", err)
			}
			collectFinalText(&sb, ev)
		}
		if providerCalls != 1 || sb.String() != "second turn ok" {
			t.Errorf("second turn: calls=%d text=%q, want 1 and 'second turn ok'", providerCalls, sb.String())
		}
	})

	t.Run("final_report grants one tool-less synthesis call on workload max_turns", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "specialists"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "specialists", "_fallback.specialist.md"), []byte(`---
name: _fallback
description: Fallback specialist.
mode: SingleTurn
---
Return ok.
`), 0o644); err != nil {
			t.Fatal(err)
		}
		wlYAML := `schema_version: 1
name: capped-wl
specialists: [_fallback]
budget:
  max_turns: 2
  final_report: true
`
		if err := os.WriteFile(filepath.Join(dir, "workload.yaml"), []byte(wlYAML), 0o644); err != nil {
			t.Fatal(err)
		}

		var (
			providerCalls       int
			thirdCallToolCount  int
			sawFinalReportNudge bool
		)
		llm := &scriptedFacadeLLM{
			name: "gemini-2.5-flash",
			steps: []func(*adkmodel.LLMRequest) (*adkmodel.LLMResponse, error){
				func(_ *adkmodel.LLMRequest) (*adkmodel.LLMResponse, error) {
					providerCalls++
					return &adkmodel.LLMResponse{
						Content: &genai.Content{
							Role:  genai.RoleModel,
							Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{ID: "c1", Name: "ping_tool", Args: map[string]any{}}}},
						},
						TurnComplete: true,
					}, nil
				},
				func(_ *adkmodel.LLMRequest) (*adkmodel.LLMResponse, error) {
					providerCalls++
					return &adkmodel.LLMResponse{
						Content: &genai.Content{
							Role:  genai.RoleModel,
							Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{ID: "c2", Name: "ping_tool", Args: map[string]any{}}}},
						},
						TurnComplete: true,
					}, nil
				},
				func(req *adkmodel.LLMRequest) (*adkmodel.LLMResponse, error) {
					providerCalls++
					if req.Config != nil {
						for _, gt := range req.Config.Tools {
							if gt != nil {
								thirdCallToolCount += len(gt.FunctionDeclarations)
							}
						}
					}
					for _, c := range req.Contents {
						if c == nil || c.Role != genai.RoleUser {
							continue
						}
						for _, p := range c.Parts {
							if p != nil && strings.Contains(p.Text, "reached your step limit for this turn (max_steps limit of 2)") {
								sawFinalReportNudge = true
							}
						}
					}
					return &adkmodel.LLMResponse{
						Content:      genai.NewContentFromText("Synthesized final report from 2 tool cycles.", genai.RoleModel),
						TurnComplete: true,
					}, nil
				},
				func(_ *adkmodel.LLMRequest) (*adkmodel.LLMResponse, error) {
					providerCalls++
					return &adkmodel.LLMResponse{
						Content:      genai.NewContentFromText("unreachable fourth call", genai.RoleModel),
						TurnComplete: true,
					}, nil
				},
			},
		}

		cfg := config.DefaultConfig()
		cfg.Permissions.Mode = config.PermissionModeYolo
		res, err := RunWorkload(context.Background(), dir, OneShotConfig{
			Config: cfg,
			Model:  llm,
			Tools:  []adktool.Tool{pingTool},
		}, "triage incident")
		if err != nil {
			t.Fatalf("RunWorkload: %v", err)
		}
		if providerCalls != 3 {
			t.Fatalf("providerCalls = %d, want 3 (2 tool cycles + 1 tool-less final report)", providerCalls)
		}
		if thirdCallToolCount != 0 {
			t.Errorf("thirdCallToolCount = %d, want 0 (all tools stripped on final report call)", thirdCallToolCount)
		}
		if !sawFinalReportNudge {
			t.Error("third call did not receive ChatFinalReportInstruction user nudge")
		}
		if res.Text != "Synthesized final report from 2 tool cycles." {
			t.Errorf("res.Text = %q, want 'Synthesized final report from 2 tool cycles.'", res.Text)
		}
	})
}
