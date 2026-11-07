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

package background

import (
	"context"
	"encoding/json"
	"iter"
	"slices"
	"strings"
	"sync"
	"testing"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v3/pkg/agent"
	"github.com/go-steer/core-agent/v3/pkg/config"
	"github.com/go-steer/core-agent/v3/pkg/memory"
)

type memoryInspectLLM struct {
	mu        sync.Mutex
	prompts   []string
	toolNames []string
	callTool  string
	callArgs  map[string]any
	called    bool
}

func (m *memoryInspectLLM) Name() string { return "memory-inspect-llm" }

func (m *memoryInspectLLM) GenerateContent(_ context.Context, req *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		m.mu.Lock()
		var sb strings.Builder
		for _, c := range req.Contents {
			if c == nil {
				continue
			}
			for _, p := range c.Parts {
				if p != nil && p.Text != "" {
					sb.WriteString(p.Text)
					sb.WriteByte('\n')
				}
			}
		}
		m.prompts = append(m.prompts, sb.String())
		if req.Config != nil {
			var names []string
			for _, t := range req.Config.Tools {
				if t == nil {
					continue
				}
				for _, fd := range t.FunctionDeclarations {
					if fd != nil {
						names = append(names, fd.Name)
					}
				}
			}
			m.toolNames = names
		}
		shouldCall := m.callTool != "" && !m.called
		if shouldCall {
			m.called = true
		}
		toolName := m.callTool
		toolArgs := m.callArgs
		m.mu.Unlock()

		if shouldCall {
			yield(&adkmodel.LLMResponse{
				Content: &genai.Content{
					Role: "model",
					Parts: []*genai.Part{{
						FunctionCall: &genai.FunctionCall{
							ID:   "call-mem-1",
							Name: toolName,
							Args: toolArgs,
						},
					}},
				},
				TurnComplete: true,
			}, nil)
			return
		}
		yield(&adkmodel.LLMResponse{
			Content:      genai.NewContentFromText("TASK_COMPLETE: done observed fact", "model"),
			TurnComplete: true,
		}, nil)
	}
}

type consolidateRecordingBackend struct {
	*memory.InMemory
	mu     sync.Mutex
	scopes []memory.ScopeMap
}

func (b *consolidateRecordingBackend) ConsolidateConversation(ctx context.Context, scope memory.ScopeMap, req memory.ConversationRequest) ([]memory.ConsolidationDelta, error) {
	b.mu.Lock()
	b.scopes = append(b.scopes, scope.Clone())
	b.mu.Unlock()
	return b.InMemory.ConsolidateConversation(ctx, scope, req)
}

func buildParentRouterForSubagentTest(t *testing.T) (*memory.Router, *consolidateRecordingBackend) {
	t.Helper()
	ctx := context.Background()
	raw := memory.NewInMemory()
	t.Cleanup(func() { _ = raw.Close() })
	rec := &consolidateRecordingBackend{InMemory: raw}

	router, err := memory.NewRouter(rec, []memory.MountTemplate{
		memory.AgentMount("prod", "root-orchestrator", memory.AccessReadWrite),
		memory.SharedMount("prod", "fleet", memory.AccessReadWrite),
	})
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	if _, err := router.Remember(ctx, memory.MountAgent, []memory.CandidateFact{{Fact: "private parent fact"}}); err != nil {
		t.Fatalf("seed parent agent fact: %v", err)
	}
	if _, err := router.Remember(ctx, "shared:fleet", []memory.CandidateFact{{Fact: "shared fleet fact"}}); err != nil {
		t.Fatalf("seed shared fleet fact: %v", err)
	}
	return router, rec
}

func TestSpawn_AdHocMemoryClampedToSharedRO(t *testing.T) {
	t.Parallel()
	router, _ := buildParentRouterForSubagentTest(t)
	inspect := &memoryInspectLLM{}
	prov := &recordingProvider{llm: inspect}

	mgr, err := NewManager(
		WithProvider(prov, "parent-model"),
		WithDefaultBudgets(Budgets{MaxTurns: 2}),
	)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer mgr.Close()

	parentLLM := echoLLM(t)
	_, err = agent.New(
		parentLLM,
		agent.WithBackgroundManager(mgr),
		agent.WithMemory(router, agent.MemorySettings{
			Preload:     agent.MemoryPreloadTurnStart,
			PreloadTopK: 5,
		}),
	)
	if err != nil {
		t.Fatalf("agent.New(parent): %v", err)
	}

	// Ad-hoc spawn (Ref == ""): agent scope must be off and shared:fleet clamped to ro.
	h, err := mgr.Spawn(context.Background(), "", Spec{
		Name:         "adhoc-worker",
		SystemPrompt: "inspect fleet",
		Goal:         "fleet fact status",
	})
	if err != nil {
		t.Fatalf("Spawn(adhoc): %v", err)
	}
	waitDone(t, h)

	inspect.mu.Lock()
	prompts := append([]string(nil), inspect.prompts...)
	tools := append([]string(nil), inspect.toolNames...)
	inspect.mu.Unlock()

	if len(prompts) == 0 {
		t.Fatal("expected at least 1 LLM call from ad-hoc subagent")
	}
	if !strings.Contains(prompts[0], "shared fleet fact") {
		t.Errorf("ad-hoc subagent preload missing shared fleet fact; got:\n%s", prompts[0])
	}
	if strings.Contains(prompts[0], "private parent fact") {
		t.Errorf("ad-hoc subagent preload leaked parent private fact:\n%s", prompts[0])
	}
	if !slices.Contains(tools, "recall_memory") {
		t.Errorf("expected recall_memory tool on ad-hoc subagent; got tools=%v", tools)
	}
	if slices.Contains(tools, "persist_memory") {
		t.Errorf("persist_memory must not be offered when all mounts are ro; got tools=%v", tools)
	}
}

func TestSpawnTemplate_PrivateAgentMemoryAndConsolidateOnEnd(t *testing.T) {
	t.Parallel()
	router, rec := buildParentRouterForSubagentTest(t)
	inspect := &memoryInspectLLM{
		callTool: "persist_memory",
		callArgs: map[string]any{
			"mount": "agent",
			"fact":  "subagent private observation",
		},
	}
	prov := &recordingProvider{llm: inspect}

	mgr := newTemplateManager(t, prov, []SubagentTemplate{{
		Name:         "sre-bot",
		Instruction:  "triage fleet",
		ModelFactory: tmplFactory(prov, "sre-model"),
		ModelID:      "sre-model",
	}}, WithDefaultBudgets(Budgets{MaxTurns: 3}))
	defer mgr.Close()

	parentLLM := echoLLM(t)
	_, err := agent.New(
		parentLLM,
		agent.WithBackgroundManager(mgr),
		agent.WithMemory(router, agent.MemorySettings{
			Preload:     agent.MemoryPreloadTurnStart,
			PreloadTopK: 5,
			Consolidate: agent.MemoryConsolidateSessionEnd,
		}),
	)
	if err != nil {
		t.Fatalf("agent.New(parent): %v", err)
	}

	h, err := mgr.SpawnTemplate(context.Background(), "", "sre-bot", RefOverrides{Goal: "fleet triage"}, "")
	if err != nil {
		t.Fatalf("SpawnTemplate: %v", err)
	}
	waitDone(t, h)

	// Verify private fact was written under agent_id=sre-bot and not visible to root-orchestrator.
	subItems, err := rec.Retrieve(context.Background(), memory.ScopeMap{
		memory.ScopeKeyNamespace: "prod",
		memory.ScopeKeyKind:      memory.ScopeKindAgent,
		memory.ScopeKeyAgentID:   "sre-bot",
	}, memory.RetrieveRequest{
		Query: "observation",
		TopK:  5,
	})
	if err != nil {
		t.Fatalf("Retrieve(sre-bot): %v", err)
	}
	if len(subItems) == 0 || !strings.Contains(subItems[0].Fact, "subagent private observation") {
		t.Fatalf("sre-bot private items = %+v, want [subagent private observation]", subItems)
	}

	// Verify autonomous.Run exit triggered session_end consolidation on sre-bot's agent mount.
	rec.mu.Lock()
	scopes := append([]memory.ScopeMap(nil), rec.scopes...)
	rec.mu.Unlock()
	if len(scopes) == 0 {
		t.Fatal("expected session_end consolidation on subagent exit, got 0 ConsolidateConversation calls")
	}
	if got := scopes[0][memory.ScopeKeyAgentID]; got != "sre-bot" {
		t.Errorf("consolidated scope agent_id = %q, want sre-bot (scopes=%v)", got, scopes)
	}
}

func TestA2AClient_SpawnMemoryScopePropagation(t *testing.T) {
	t.Parallel()
	memSpec := &memory.BindingSpec{
		Ref:       "memorybank://projects/secret-p/locations/us-central1/reasoningEngines/999",
		Namespace: "prod-ns",
		Mounts: []memory.MountTemplate{
			memory.AgentMount("prod-ns", "remote-sre", memory.AccessReadWrite),
			memory.SharedMount("prod-ns", "fleet", memory.AccessReadWrite),
		},
	}

	runCase := func(t *testing.T, propagate bool) map[string]any {
		t.Helper()
		var (
			mu      sync.Mutex
			gotMeta map[string]any
		)
		f := newFakeA2A(t, func(s *fakeA2AServer) {
			s.onSend = func(_ rpcRequest, msg A2AMessage) (any, *A2ARPCError) {
				mu.Lock()
				gotMeta = msg.Metadata
				mu.Unlock()
				return A2ATask{
					ID:     "task-1",
					Status: A2ATaskStatus{State: A2ATaskStateCompleted},
				}, nil
			}
		})
		client, err := NewA2AClient(A2AConfig{
			Name:                 "remote-sre",
			AgentCardURL:         f.srv.URL,
			PropagateMemoryScope: propagate,
		})
		if err != nil {
			t.Fatalf("NewA2AClient: %v", err)
		}
		ctx := memory.WithUserID(context.Background(), "alice@example.com")
		h, err := client.Spawn(ctx, RemoteAgentSpec{
			Name:   "remote-sre",
			Goal:   "check health",
			Memory: memSpec,
		})
		if err != nil {
			t.Fatalf("client.Spawn: %v", err)
		}
		for range h.Events() {
		}
		mu.Lock()
		defer mu.Unlock()
		return gotMeta
	}

	t.Run("disabled by default omits core_agent.memory_scope", func(t *testing.T) {
		meta := runCase(t, false)
		if _, exists := meta["core_agent.memory_scope"]; exists {
			t.Fatalf("expected core_agent.memory_scope to be omitted when PropagateMemoryScope=false; got %+v", meta)
		}
	})

	t.Run("enabled propagates logical scope and strips Ref URIs", func(t *testing.T) {
		meta := runCase(t, true)
		rawScope, exists := meta["core_agent.memory_scope"]
		if !exists {
			t.Fatalf("expected core_agent.memory_scope in metadata when PropagateMemoryScope=true; got %+v", meta)
		}
		encoded, err := json.Marshal(rawScope)
		if err != nil {
			t.Fatalf("json.Marshal(rawScope): %v", err)
		}
		jsonStr := string(encoded)
		if strings.Contains(jsonStr, "memorybank://") || strings.Contains(jsonStr, "secret-p") {
			t.Fatalf("core_agent.memory_scope leaked backend Ref URI: %s", jsonStr)
		}
		if !strings.Contains(jsonStr, `"namespace":"prod-ns"`) || !strings.Contains(jsonStr, `"user_id":"alice@example.com"`) || !strings.Contains(jsonStr, `"pool":"fleet"`) || !strings.Contains(jsonStr, `"mode":"ro"`) {
			t.Fatalf("core_agent.memory_scope missing expected logical coordinates or ro mode clamp: %s", jsonStr)
		}
	})

	// Ensure config.SubagentMemoryConfig reference compiles and works with Spec.
	_ = config.SubagentMemoryConfig{}
}
