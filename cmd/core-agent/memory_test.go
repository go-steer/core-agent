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

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v3/pkg/config"
	"github.com/go-steer/core-agent/v3/pkg/memory"
	"github.com/go-steer/core-agent/v3/pkg/models/mock"
)

func TestApplyMemoryFlagsAndEnv_Precedence(t *testing.T) {
	t.Parallel()

	cfg := config.DefaultConfig()
	cfg.Memory = &config.MemoryConfig{
		Ref:       "inmemory://from-config",
		Namespace: "cfg-ns",
		Agent: &config.AgentMemoryConfig{
			ID:   "cfg-bot",
			Mode: config.MemoryModeRO,
		},
		Shared: []config.SharedMemoryConfig{
			{Pool: "fleet", Mode: config.MemoryModeRO},
		},
		Preload:     config.MemoryPreloadOff,
		Consolidate: config.MemoryConsolidateOff,
	}

	env := map[string]string{
		"CORE_AGENT_MEMORY_REF":         "inmemory://from-env",
		"CORE_AGENT_AGENT_ENGINE":       "projects/env-p/locations/us-central1/reasoningEngines/111",
		"CORE_AGENT_MEMORY_NAMESPACE":   "env-ns",
		"CORE_AGENT_MEMORY_AGENT":       "env-bot:rw",
		"CORE_AGENT_MEMORY_USER":        "env-user",
		"CORE_AGENT_MEMORY_SHARED":      "fleet:rw,runbooks:ro",
		"CORE_AGENT_MEMORY_PRELOAD":     config.MemoryPreloadSessionStart,
		"CORE_AGENT_MEMORY_CONSOLIDATE": config.MemoryConsolidateTurnEnd,
	}
	getenv := func(k string) string { return env[k] }

	// Step 1: Env beats config when CLI flags are empty.
	user, err := applyMemoryFlagsAndEnv(cfg, memoryFlags{}, getenv)
	if err != nil {
		t.Fatalf("applyMemoryFlagsAndEnv (env): %v", err)
	}
	if user != "env-user" {
		t.Errorf("user = %q, want env-user", user)
	}
	if cfg.Memory.Ref != "inmemory://from-env" {
		t.Errorf("Ref = %q, want inmemory://from-env", cfg.Memory.Ref)
	}
	if cfg.AgentEngine == nil || cfg.AgentEngine.Resource != "projects/env-p/locations/us-central1/reasoningEngines/111" {
		t.Errorf("AgentEngine = %+v", cfg.AgentEngine)
	}
	if cfg.Memory.Namespace != "env-ns" {
		t.Errorf("Namespace = %q, want env-ns", cfg.Memory.Namespace)
	}
	if cfg.Memory.Agent == nil || cfg.Memory.Agent.ID != "env-bot" || cfg.Memory.Agent.Mode != config.MemoryModeRW || !cfg.Memory.Agent.PartitionByUser {
		t.Errorf("Agent = %+v", cfg.Memory.Agent)
	}
	if len(cfg.Memory.Shared) != 2 || cfg.Memory.Shared[0].Mode != config.MemoryModeRW || cfg.Memory.Shared[1].Pool != "runbooks" {
		t.Errorf("Shared = %+v", cfg.Memory.Shared)
	}

	// Step 2: CLI flags beat env.
	user, err = applyMemoryFlagsAndEnv(cfg, memoryFlags{
		ref:         "inmemory://from-cli",
		agentEngine: "projects/cli-p/locations/us-central1/reasoningEngines/222",
		namespace:   "cli-ns",
		agent:       "cli-bot:ro",
		user:        "cli-user",
		shared:      []string{"runbooks:rw", "incidents"},
		preload:     config.MemoryPreloadTurnStart,
		consolidate: config.MemoryConsolidateSessionEnd,
	}, getenv)
	if err != nil {
		t.Fatalf("applyMemoryFlagsAndEnv (cli): %v", err)
	}
	if user != "cli-user" {
		t.Errorf("user = %q, want cli-user", user)
	}
	if cfg.Memory.Ref != "inmemory://from-cli" {
		t.Errorf("Ref = %q, want inmemory://from-cli", cfg.Memory.Ref)
	}
	if cfg.AgentEngine.Resource != "projects/cli-p/locations/us-central1/reasoningEngines/222" {
		t.Errorf("AgentEngine.Resource = %q", cfg.AgentEngine.Resource)
	}
	if cfg.Memory.Namespace != "cli-ns" {
		t.Errorf("Namespace = %q, want cli-ns", cfg.Memory.Namespace)
	}
	if cfg.Memory.Agent.ID != "cli-bot" || cfg.Memory.Agent.Mode != config.MemoryModeRO {
		t.Errorf("Agent = %+v", cfg.Memory.Agent)
	}
	if cfg.Memory.Preload != config.MemoryPreloadTurnStart {
		t.Errorf("Preload = %q, want turn_start", cfg.Memory.Preload)
	}
	if cfg.Memory.Consolidate != config.MemoryConsolidateSessionEnd {
		t.Errorf("Consolidate = %q, want session_end", cfg.Memory.Consolidate)
	}
}

func TestParseMemoryAgentAndSharedSpecs(t *testing.T) {
	t.Parallel()

	m, err := parseMemoryAgentSpec("off")
	if err != nil || m.Mode != config.MemoryModeOff || m.ID != "" {
		t.Fatalf("parseMemoryAgentSpec(off) = %+v, %v", m, err)
	}
	m, err = parseMemoryAgentSpec("ro")
	if err != nil || m.Mode != config.MemoryModeRO || m.ID != "" {
		t.Fatalf("parseMemoryAgentSpec(ro) = %+v, %v", m, err)
	}
	m, err = parseMemoryAgentSpec("triage-bot")
	if err != nil || m.ID != "triage-bot" || m.Mode != config.MemoryModeRW {
		t.Fatalf("parseMemoryAgentSpec(triage-bot) = %+v, %v", m, err)
	}
	if _, err := parseMemoryAgentSpec("triage-bot:bad"); err == nil {
		t.Fatal("expected error for invalid --memory-agent mode")
	}
	if _, err := parseMemorySharedSpec("pool:bad"); err == nil {
		t.Fatal("expected error for invalid --memory-shared mode")
	}
}

func TestOpenDaemonMemory_AndSubagentDerivation(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Subagents = []config.SubagentSpec{
		{
			Name:         "reader",
			Description:  "read-only helper",
			Instructions: "Help.",
			Mode:         config.SubagentModeTask,
			Capability:   config.CapabilityReadOnly,
			Tools:        []string{"recall_memory"},
		},
	}

	ctx := context.Background()
	var bootLines []string
	router, settings, closeMem, err := openDaemonMemory(
		ctx,
		cfg,
		memoryFlags{
			ref:       "inmemory://cmd-subagent-test",
			namespace: "prod",
			agent:     "coord:rw",
			user:      "alice",
			shared:    []string{"team:rw"},
			preload:   config.MemoryPreloadTurnStart,
		},
		t.TempDir(),
		false,
		nil,
		func(s string) { bootLines = append(bootLines, s) },
	)
	if err != nil {
		t.Fatalf("openDaemonMemory: %v", err)
	}
	defer closeMem()
	if router == nil {
		t.Fatal("expected non-nil memory router")
	}
	if len(bootLines) == 0 || !strings.Contains(bootLines[0], "memory: ref=inmemory://cmd-subagent-test") {
		t.Errorf("bootLines = %v", bootLines)
	}

	// Write a memory as user alice and verify user partitioning works.
	if _, err := router.Remember(ctx, memory.MountAgent, []memory.CandidateFact{{Fact: "alice preference"}}); err != nil {
		t.Fatalf("Remember: %v", err)
	}
	items, _, err := router.Recall(ctx, "alice", nil, nil, 5)
	if err != nil || len(items) != 1 || items[0].Scope["user_id"] != "alice" {
		t.Fatalf("Recall items = %+v, err = %v", items, err)
	}

	subs, templates, servers, err := buildDeclaredSubagents(
		ctx,
		cfg,
		mock.NewEcho(),
		t.TempDir(),
		parentSurface{},
		subagentDeps{
			send:           func(string) {},
			memoryRouter:   router,
			memorySettings: settings,
		},
	)
	if err != nil {
		t.Fatalf("buildDeclaredSubagents: %v", err)
	}
	if len(servers) != 0 {
		t.Errorf("servers = %v, want empty", servers)
	}
	if len(subs) != 1 || subs[0].MemoryRouter() == nil {
		t.Fatalf("subs = %v, want 1 subagent with MemoryRouter", subs)
	}
	if len(templates) != 1 || templates[0].MemoryRouter == nil {
		t.Fatalf("templates = %+v, want 1 template with MemoryRouter", templates)
	}
	// capability: read_only must clamp the subagent's mounts to ro.
	for _, m := range subs[0].MemoryRouter().Mounts() {
		if m.Mode != memory.AccessReadOnly {
			t.Errorf("subagent mount %q mode = %q, want ro", m.Name, m.Mode)
		}
	}
}

func TestBuildDeclaredSubagents_RootedStandaloneMemory(t *testing.T) {
	rootDir := t.TempDir()
	agentsDir := filepath.Join(rootDir, ".agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentsDir, "config.json"), []byte(`{
		"version": 1,
		"memory": {
			"ref": "inmemory://rooted-standalone",
			"namespace": "isolated",
			"agent": {"id": "specialist", "mode": "rw"}
		}
	}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootDir, "AGENTS.md"), []byte("Specialist persona."), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := config.DefaultConfig()
	cfg.Subagents = []config.SubagentSpec{
		{
			Name:        "specialist",
			Description: "rooted specialist",
			Root:        rootDir,
		},
	}

	var closed int
	subs, templates, _, err := buildDeclaredSubagents(
		context.Background(),
		cfg,
		mock.NewEcho(),
		t.TempDir(),
		parentSurface{},
		subagentDeps{
			send:    func(string) {},
			onClose: func(fn func()) { closed++; fn() },
		},
	)
	if err != nil {
		t.Fatalf("buildDeclaredSubagents: %v", err)
	}
	if len(subs) != 1 || subs[0].MemoryRouter() == nil {
		t.Fatalf("expected rooted subagent to open standalone memory router")
	}
	if subs[0].MemoryRouter().Namespace() != "isolated" {
		t.Errorf("Namespace = %q, want isolated", subs[0].MemoryRouter().Namespace())
	}
	if len(templates) != 1 || templates[0].MemoryRouter == nil {
		t.Fatalf("expected template to carry standalone memory router")
	}
	if closed != 1 {
		t.Errorf("closed = %d, want 1", closed)
	}
}
