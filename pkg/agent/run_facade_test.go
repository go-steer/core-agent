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
	"iter"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	adkmodel "google.golang.org/adk/v2/model"
	adktool "google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v3/pkg/config"
	"github.com/go-steer/core-agent/v3/pkg/permissions"
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
		if err := checkFacadeSubagentCapability(context.Background(), spec, []adktool.Tool{mutTool}, nil); err == nil {
			t.Fatal("expected error for mutating built-in tool on read_only subagent")
		}
		ts := &staticToolset{name: "k8s", tools: []adktool.Tool{mutTool}}
		if err := checkFacadeSubagentCapability(context.Background(), spec, nil, []adktool.Toolset{ts}); err == nil {
			t.Fatal("expected error for mutating toolset tool on read_only subagent")
		}
	})

	t.Run("tools empty slice still inherits MCP toolsets when MCP is nil", func(t *testing.T) {
		t.Parallel()
		ts := &staticToolset{name: "k8s"}
		got, err := selectFacadeSubagentToolsets(config.SubagentSpec{
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

		none, err := selectFacadeSubagentToolsets(config.SubagentSpec{
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
