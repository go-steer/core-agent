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
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/core-agent/v3/pkg/memory"
)

func TestMemoryAndAgentEngineConfigRoundTripAndEnvRefs(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	agentsDir := filepath.Join(dir, ".agents")
	subDir := filepath.Join(agentsDir, "subagents")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}

	cfgJSON := `{
  "version": 1,
  "model": {
    "provider": "vertex",
    "name": "gemini-2.5-pro",
    "vertex": {
      "project": "prod-sre-proj",
      "location": "us-central1"
    }
  },
  "agent_engine": {
    "resource_env": "CORE_AGENT_AGENT_ENGINE"
  },
  "memory": {
    "backend": "memorybank",
    "namespace_env": "CORE_AGENT_MEMORY_NAMESPACE",
    "agent": {
      "id": "incident-coordinator",
      "mode": "rw",
      "write_mode": "consolidate",
      "ttl": "720h"
    },
    "shared": [
      {
        "pool": "fleet-learnings",
        "mode": "rw",
        "ttl": "2160h"
      },
      {
        "pool": "approved-runbooks",
        "mode": "ro"
      },
      {
        "pool": "legacy-adk",
        "mode": "ro",
        "partition_by_user": true,
        "scope_attrs": {
          "app_name": "sre-portal"
        }
      }
    ],
    "preload": "session_start",
    "consolidate": "session_end",
    "consolidate_mount": "agent",
    "top_k": 15
  },
  "subagents": [
    {
      "name": "remote-specialist",
      "description": "Remote A2A specialist.",
      "remote": {
        "a2a": "https://a2a.example.com",
        "propagate_memory_scope": true
      }
    }
  ]
}`
	cfgPath := filepath.Join(agentsDir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(cfgJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	mdSubagent := `---
name: db-investigator
description: Investigates Postgres and Cloud SQL lock contention.
mode: task
capability: read_only
tier: mid
memory:
  agent: rw
  inherit_shared: ro
  shared:
    - pool: db-heuristics
      mode: rw
  preload: turn_start
---
Check pg_stat_activity and lock trees first.
`
	if err := os.WriteFile(filepath.Join(subDir, "db-investigator.md"), []byte(mdSubagent), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(agentsDir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	// Verify EnvRefs captures env names and never leaks literal values.
	wantEnvRefs := []string{"CORE_AGENT_AGENT_ENGINE", "CORE_AGENT_MEMORY_NAMESPACE"}
	if got := cfg.EnvRefs(); !reflect.DeepEqual(got, wantEnvRefs) {
		t.Fatalf("EnvRefs() = %v, want %v", got, wantEnvRefs)
	}

	// Verify shared agent_engine inheritance into ResolveMemoryRef.
	env := map[string]string{
		"CORE_AGENT_AGENT_ENGINE":     "projects/prod-sre-proj/locations/us-central1/reasoningEngines/555666",
		"CORE_AGENT_MEMORY_NAMESPACE": "prod-sre",
	}
	getenv := func(k string) string { return env[k] }

	gotRef, err := cfg.ResolveMemoryRef(getenv)
	if err != nil {
		t.Fatalf("ResolveMemoryRef: %v", err)
	}
	wantRef := "memorybank://projects/prod-sre-proj/locations/us-central1/reasoningEngines/555666"
	if gotRef != wantRef {
		t.Fatalf("ResolveMemoryRef = %q, want %q", gotRef, wantRef)
	}
	if ns := cfg.Memory.ResolveNamespace(getenv); ns != "prod-sre" {
		t.Fatalf("ResolveNamespace = %q, want %q", ns, "prod-sre")
	}
	if ttl, err := cfg.Memory.Agent.ResolvedTTL(); err != nil || ttl != 720*time.Hour {
		t.Fatalf("Agent.ResolvedTTL = %v, err=%v; want 720h", ttl, err)
	}

	// Verify Markdown subagent memory parsed and validated cleanly.
	if len(cfg.Subagents) != 2 {
		t.Fatalf("len(Subagents) = %d, want 2", len(cfg.Subagents))
	}
	if !cfg.Subagents[0].Remote.PropagateMemoryScope {
		t.Errorf("expected remote.propagate_memory_scope=true")
	}
	mdSpec := cfg.Subagents[1]
	if mdSpec.Name != "db-investigator" || mdSpec.Memory == nil {
		t.Fatalf("unexpected markdown subagent: %+v", mdSpec)
	}
	if mdSpec.Memory.Agent != "rw" || mdSpec.Memory.InheritShared != "ro" || mdSpec.Memory.Preload != "turn_start" {
		t.Fatalf("unexpected markdown subagent memory block: %+v", mdSpec.Memory)
	}
	if len(mdSpec.Memory.Shared) != 1 || mdSpec.Memory.Shared[0].Pool != "db-heuristics" {
		t.Fatalf("unexpected markdown subagent shared pools: %+v", mdSpec.Memory.Shared)
	}

	// Save round-trip preserves agent_engine and memory without copying Markdown subagent into config.json.
	if err := Save(cfgPath, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}
	reloaded, err := Load(agentsDir)
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if reloaded.AgentEngine == nil || reloaded.AgentEngine.ResourceEnv != "CORE_AGENT_AGENT_ENGINE" {
		t.Fatalf("AgentEngine lost on Save round-trip: %+v", reloaded.AgentEngine)
	}
	if reloaded.Memory == nil || reloaded.Memory.TopK != 15 {
		t.Fatalf("Memory lost on Save round-trip: %+v", reloaded.Memory)
	}
}

func TestResolveAgentEngineComponentFallback(t *testing.T) {
	t.Parallel()
	cfg := DefaultConfig()
	cfg.Model.Provider = ProviderVertex
	cfg.Model.Vertex = &VertexConfig{
		Project:  "vertex-proj",
		Location: "europe-west1",
	}
	cfg.AgentEngine = &AgentEngineConfig{
		IDEnv: "ENGINE_ID",
	}
	cfg.Memory = &MemoryConfig{
		Backend: MemoryBackendMemoryBank,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	ref, err := cfg.ResolveMemoryRef(func(k string) string {
		if k == "ENGINE_ID" {
			return "998877"
		}
		return ""
	})
	if err != nil {
		t.Fatalf("ResolveMemoryRef: %v", err)
	}
	want := "memorybank://projects/vertex-proj/locations/europe-west1/reasoningEngines/998877"
	if ref != want {
		t.Fatalf("ResolveMemoryRef = %q, want %q", ref, want)
	}
}

func TestMemoryConfigValidationErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		mutate  func(*Config)
		wantSub string
	}{
		{
			name: "agent_engine_resource_and_id_mutually_exclusive",
			mutate: func(c *Config) {
				c.AgentEngine = &AgentEngineConfig{
					Resource: "projects/p/locations/us-central1/reasoningEngines/1",
					ID:       "1",
				}
			},
			wantSub: "mutually exclusive",
		},
		{
			name: "malformed_agent_engine_resource",
			mutate: func(c *Config) {
				c.AgentEngine = &AgentEngineConfig{
					Resource: "projects/p/only-two",
				}
			},
			wantSub: "agent_engine.resource",
		},
		{
			name: "duplicate_shared_pool",
			mutate: func(c *Config) {
				c.Memory = &MemoryConfig{
					Backend: MemoryBackendInMemory,
					Shared: []SharedMemoryConfig{
						{Pool: "fleet", Mode: "rw"},
						{Pool: "fleet", Mode: "ro"},
					},
				}
			},
			wantSub: "duplicate pool",
		},
		{
			name: "scope_attrs_wildcard_rejected",
			mutate: func(c *Config) {
				c.Memory = &MemoryConfig{
					Backend: MemoryBackendInMemory,
					Shared: []SharedMemoryConfig{
						{Pool: "legacy", ScopeAttrs: map[string]string{"app": "*"}},
					},
				}
			},
			wantSub: "wildcards",
		},
		{
			name: "scope_attrs_partition_by_user_exceeds_4_static_keys",
			mutate: func(c *Config) {
				c.Memory = &MemoryConfig{
					Backend: MemoryBackendInMemory,
					Shared: []SharedMemoryConfig{{
						Pool:            "legacy",
						PartitionByUser: true,
						ScopeAttrs: map[string]string{
							"k1": "v1", "k2": "v2", "k3": "v3", "k4": "v4", "k5": "v5",
						},
					}},
				}
			},
			wantSub: "max 4",
		},
		{
			name: "memorybank_missing_coords",
			mutate: func(c *Config) {
				c.Memory = &MemoryConfig{
					Backend: MemoryBackendMemoryBank,
				}
			},
			wantSub: "memory.backend=\"memorybank\" requires",
		},
		{
			name: "sqlite_missing_coords",
			mutate: func(c *Config) {
				c.Memory = &MemoryConfig{
					Backend: MemoryBackendSQLite,
				}
			},
			wantSub: "memory.backend=\"sqlite\" requires",
		},
		{
			name: "ref_sqlite_root_only",
			mutate: func(c *Config) {
				c.Memory = &MemoryConfig{
					Ref: "sqlite:///",
				}
			},
			wantSub: "requires a file path",
		},
		{
			name: "ref_userinfo_rejected",
			mutate: func(c *Config) {
				c.Memory = &MemoryConfig{
					Ref: "memorybank://user:pass@proj/loc/123",
				}
			},
			wantSub: "userinfo",
		},
		{
			name: "ref_inmemory_slash_rejected",
			mutate: func(c *Config) {
				c.Memory = &MemoryConfig{
					Ref: "inmemory://nested/bus",
				}
			},
			wantSub: "must not contain '/'",
		},
		{
			name: "ref_custom_empty_target",
			mutate: func(c *Config) {
				c.Memory = &MemoryConfig{
					Ref: "redis-ams://",
				}
			},
			wantSub: "empty target",
		},
		{
			name: "consolidate_targets_readonly_mount",
			mutate: func(c *Config) {
				c.Memory = &MemoryConfig{
					Backend:          MemoryBackendInMemory,
					Agent:            &AgentMemoryConfig{Mode: "ro"},
					Consolidate:      MemoryConsolidateSessionEnd,
					ConsolidateMount: "agent",
				}
			},
			wantSub: "not configured as writable",
		},
		{
			name: "single_turn_subagent_with_rw_private_memory",
			mutate: func(c *Config) {
				c.Subagents = []SubagentSpec{{
					Name: "classifier",
					Mode: SubagentModeSingleTurn,
					Memory: &SubagentMemoryConfig{
						Agent: "rw",
					},
				}}
			},
			wantSub: "single_turn",
		},
		{
			name: "remote_a2a_subagent_with_inprocess_memory_block",
			mutate: func(c *Config) {
				c.Subagents = []SubagentSpec{subagentRemoteSpecHelper()}
			},
			wantSub: "propagate_memory_scope",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultConfig()
			tc.mutate(cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("expected validation error containing %q, got nil", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("Validate() error = %q, want substring %q", err.Error(), tc.wantSub)
			}
		})
	}
}

func subagentRemoteSpecHelper() SubagentSpec {
	return SubagentSpec{
		Name: "remote-helper",
		Remote: &SubagentRemoteConfig{
			A2A: "https://a2a.example.com",
		},
		Memory: &SubagentMemoryConfig{
			Agent: "rw",
		},
	}
}

func TestMemoryUnknownKeysFailClosed(t *testing.T) {
	t.Parallel()
	payloads := []struct {
		name string
		json string
	}{
		{
			name: "root_memory_nested_typo",
			json: `{"version":1,"memory":{"backend":"inmemory","agent":{"modes":"ro","partition_by_users":true}}}`,
		},
		{
			name: "subagent_memory_nested_typo",
			json: `{"version":1,"subagents":[{"name":"helper","mode":"task","memory":{"agents":"off","inherit_share":"off"}}]}`,
		},
		{
			name: "agent_engine_nested_typo",
			json: `{"version":1,"agent_engine":{"resources":"projects/p/locations/l/reasoningEngines/1"}}`,
		},
		{
			name: "top_level_memory_container_typo",
			json: `{"version":1,"memories":{"backend":"inmemory"}}`,
		},
		{
			name: "subagent_memory_container_typo",
			json: `{"version":1,"subagents":[{"name":"helper","mode":"task","memories":{"agent":"off"}}]}`,
		},
	}
	for _, tc := range payloads {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), ".agents")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(tc.json), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(dir); err == nil {
				t.Fatalf("Load(%s) succeeded on unknown security/scope key; want fatal error", tc.name)
			}
		})
	}
}

func TestResolveMemoryRefSQLiteRoundTripsThroughParseRef(t *testing.T) {
	t.Parallel()
	paths := []struct {
		rawPath      string
		wantResource string
	}{
		{"/var/lib/core-agent/memory.db", "/var/lib/core-agent/memory.db"},
		{"/tmp/run#1/mem?x=1%20y.db", "/tmp/run#1/mem?x=1%20y.db"},
		{"localhost/mem.db", "localhost/mem.db"},
		{".agents/memory.db", ".agents/memory.db"},
		{"./sub/mem#2.db", "sub/mem#2.db"},
	}
	for _, tc := range paths {
		cfg := DefaultConfig()
		cfg.Memory = &MemoryConfig{
			Backend: MemoryBackendSQLite,
			SQLite:  &SQLiteMemoryBackendConfig{Path: tc.rawPath},
		}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("Validate(%q): %v", tc.rawPath, err)
		}
		ref, err := cfg.ResolveMemoryRef(nil)
		if err != nil {
			t.Fatalf("ResolveMemoryRef(%q): %v", tc.rawPath, err)
		}
		parsed, err := memory.ParseRef(ref)
		if err != nil {
			t.Fatalf("memory.ParseRef(%q) from path %q failed: %v", ref, tc.rawPath, err)
		}
		if parsed.Resource != tc.wantResource {
			t.Fatalf("path %q -> ref %q -> Resource %q, want %q", tc.rawPath, ref, parsed.Resource, tc.wantResource)
		}
	}
}
