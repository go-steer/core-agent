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
	"slices"
	"strings"
	"testing"
)

const testFindingSchema = `{
  "title": "Finding",
  "type": "object",
  "properties": {
    "severity": {"type": "string", "enum": ["critical", "warning", "info"]},
    "title": {"type": "string"},
    "detail": {"type": "string"},
    "reason": {"type": "string"}
  },
  "required": ["severity", "title", "detail", "reason"]
}`

const testChangeReportSchema = `{
  "title": "ChangeReport",
  "type": "object",
  "properties": {
    "applied": {"type": "boolean"},
    "action": {"type": "string"},
    "outcome": {"type": "string"},
    "rollback": {"type": "string"}
  },
  "required": ["applied", "action", "outcome"]
}`

const testFallbackSpecialist = `---
name: _fallback
description: Diagnoses any GKE incident.
mode: Task
output_schema: ../schemas/finding.json
budget:
  max_turns: 12
  max_wallclock_seconds: 120
  max_cost_usd: 1.00
tools:
  mcp:
    - server: gke
      tools:
        - get_k8s_resource
        - describe_k8s_resource
        - get_k8s_logs
        - list_k8s_events
---

You diagnose Kubernetes incidents on GKE.
`

const testChangeExecutorSpecialist = `---
name: change-executor
description: Carries out one approved remediation against the cluster.
mode: Task
capability: change_executor
output_schema: ../schemas/change-report.json
budget:
  max_turns: 10
  max_wallclock_seconds: 120
  max_cost_usd: 1.00
tools:
  mcp:
    - server: gke
      tools:
        - get_k8s_resource
        - describe_k8s_resource
        - get_k8s_rollout_status
        - patch_k8s_resource
        - apply_k8s_manifest
        - delete_k8s_resource
---

You are the change executor for this workload.
`

func writeSyntheticGKETriageF(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "specialists"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "schemas"), 0o755); err != nil {
		t.Fatal(err)
	}
	workloadYAML := `name: gke-triage
description: Autonomous triage of GKE cluster incidents surfaced by k8s-event-watcher.
mode: single_session

tool_catalog:
  mcp:
    - server: gke
  tools:
    - name: triage-classifier
      mutating: false
    - name: get_k8s_resource
      mutating: false
    - name: describe_k8s_resource
      mutating: false
    - name: list_k8s_events
      mutating: false
    - name: get_k8s_logs
      mutating: false
    - name: get_k8s_rollout_status
      mutating: false
    - name: patch_k8s_resource
      mutating: true
      precondition:
        read: get_k8s_resource
        args_from: {name: name, namespace: namespace}
      capture:
        read: get_k8s_resource
        args_from: {name: name, namespace: namespace}
        revert:
          call: patch_k8s_resource
          args_from_change: {name: name, namespace: namespace}
    - name: apply_k8s_manifest
      mutating: true
    - name: delete_k8s_resource
      mutating: true

specialists:
  - _fallback
  - change-executor

budget:
  max_wallclock_seconds: 300
  final_report: true
  max_cost_usd: 2.50

safety:
  watchdog: enforce

hitl:
  require_approval: true

builtin_tools:
  web_search: false
  url_context: false
  code_execution: false

prompt_cache:
  ttl: 1h

edge_trigger:
  http:
    path: /inject
    auth: bearer
`
	if err := os.WriteFile(filepath.Join(dir, "workload.yaml"), []byte(workloadYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mcp.json"), []byte(`{"mcpServers":{"gke":{"url":"https://container.googleapis.com/mcp"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "schemas", "finding.json"), []byte(testFindingSchema), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "schemas", "change-report.json"), []byte(testChangeReportSchema), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "specialists", "_fallback.specialist.md"), []byte(testFallbackSpecialist), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "specialists", "change-executor.specialist.md"), []byte(testChangeExecutorSpecialist), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestImportWorkload_GKETriageF(t *testing.T) {
	t.Parallel()
	srcDir := writeSyntheticGKETriageF(t)
	outDir := t.TempDir()

	cfg, err := ImportWorkload(srcDir, outDir)
	if err != nil {
		t.Fatalf("ImportWorkload: %v", err)
	}
	if cfg.Agent.DisplayName != "gke-triage" {
		t.Errorf("DisplayName = %q, want gke-triage", cfg.Agent.DisplayName)
	}
	if cfg.Attach.MultiSession.Enabled {
		t.Errorf("MultiSession.Enabled = true, want false for mode: single_session")
	}
	if cfg.Permissions.Mode != PermissionModeAsk {
		t.Errorf("Permissions.Mode = %q, want %q", cfg.Permissions.Mode, PermissionModeAsk)
	}
	if cfg.Safety.Watchdog != WatchdogEnforce {
		t.Errorf("Safety.Watchdog = %q, want %q", cfg.Safety.Watchdog, WatchdogEnforce)
	}
	if !cfg.Agent.FinalReport {
		t.Errorf("Agent.FinalReport = false, want true")
	}
	if cfg.Agent.MaxWallclockSeconds != 300 {
		t.Errorf("Agent.MaxWallclockSeconds = %d, want 300", cfg.Agent.MaxWallclockSeconds)
	}
	if cfg.Agent.MaxSessionCostUSD == nil || *cfg.Agent.MaxSessionCostUSD != 2.50 {
		t.Errorf("Agent.MaxSessionCostUSD = %v, want 2.50", cfg.Agent.MaxSessionCostUSD)
	}
	if cfg.Model.BuiltinTools == nil || cfg.Model.BuiltinTools.WebSearch == nil || *cfg.Model.BuiltinTools.WebSearch {
		t.Errorf("Model.BuiltinTools.WebSearch = %v, want false", cfg.Model.BuiltinTools)
	}
	if cfg.Model.Anthropic == nil || cfg.Model.Anthropic.PromptCache == nil || cfg.Model.Anthropic.PromptCache.TTL != PromptCacheTTL1h {
		t.Errorf("PromptCache = %+v, want TTL=1h", cfg.Model.Anthropic)
	}

	for _, wantAllow := range []string{
		"spawn_agent:_fallback",
		"spawn_agent:change-executor",
		"spawn_agent:triage-classifier",
		"mcp:gke_get_k8s_resource*",
		"mcp:gke_describe_k8s_resource*",
		"mcp:gke_list_k8s_events*",
		"mcp:gke_get_k8s_logs*",
		"mcp:gke_get_k8s_rollout_status*",
	} {
		if !slices.Contains(cfg.Permissions.Allow, wantAllow) {
			t.Errorf("Permissions.Allow %v missing %q", cfg.Permissions.Allow, wantAllow)
		}
	}
	for _, mustNotAllow := range []string{
		"mcp:gke_patch_k8s_resource*",
		"mcp:gke_apply_k8s_manifest*",
		"mcp:gke_delete_k8s_resource*",
	} {
		if slices.Contains(cfg.Permissions.Allow, mustNotAllow) {
			t.Errorf("Permissions.Allow %v unexpectedly contains mutating tool %q", cfg.Permissions.Allow, mustNotAllow)
		}
	}

	if len(cfg.Tools.Policies) != 9 {
		t.Fatalf("len(Tools.Policies) = %d, want 9", len(cfg.Tools.Policies))
	}

	if len(cfg.Subagents) != 2 {
		t.Fatalf("len(Subagents) = %d, want 2", len(cfg.Subagents))
	}
	fb := cfg.Subagents[0]
	if fb.Name != "_fallback" || fb.Mode != SubagentModeTask || fb.OutputSchema != "../schemas/finding.json" {
		t.Errorf("Subagents[0] = %+v", fb)
	}
	if fb.Tools == nil || len(fb.Tools) != 0 {
		t.Errorf("Subagents[0].Tools = %v, want non-nil empty slice", fb.Tools)
	}
	ce := cfg.Subagents[1]
	if ce.Name != "change-executor" || ce.Mode != SubagentModeTask || ce.Capability != CapabilityChangeExecutor || ce.OutputSchema != "../schemas/change-report.json" {
		t.Errorf("Subagents[1] = %+v", ce)
	}
	if _, err := os.Stat(filepath.Join(outDir, "AGENTS.md")); err != nil {
		t.Errorf("AGENTS.md not written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outDir, ".agents", "mcp.json")); err != nil {
		t.Errorf(".agents/mcp.json not copied: %v", err)
	}
}

func TestImportWorkload_MultiSessionAndEmptyMCPRoundTrip(t *testing.T) {
	t.Parallel()
	srcDir := t.TempDir()
	outDir := t.TempDir()

	if err := os.MkdirAll(filepath.Join(srcDir, "specialists"), 0o755); err != nil {
		t.Fatal(err)
	}
	specMD := `---
name: _fallback
description: Multi-session diagnoser with empty MCP list and scalar model.
mode: Task
model: gemini-2.5-flash
tools:
  mcp: []
---
Diagnoser body.
`
	if err := os.WriteFile(filepath.Join(srcDir, "specialists", "_fallback.specialist.md"), []byte(specMD), 0o644); err != nil {
		t.Fatal(err)
	}
	wlYAML := `schema_version: 1
name: ms-test
mode: multi_session
specialists: [_fallback]
`
	if err := os.WriteFile(filepath.Join(srcDir, "workload.yaml"), []byte(wlYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := ImportWorkload(srcDir, outDir)
	if err != nil {
		t.Fatalf("ImportWorkload: %v", err)
	}
	if !cfg.Attach.MultiSession.Enabled {
		t.Fatalf("Attach.MultiSession = %+v, want enabled", cfg.Attach.MultiSession)
	}
	if len(cfg.Subagents) != 1 {
		t.Fatalf("len(Subagents) = %d, want 1", len(cfg.Subagents))
	}
	if cfg.Subagents[0].MCP == nil || len(cfg.Subagents[0].MCP) != 0 {
		t.Errorf("Subagents[0].MCP = %v, want non-nil empty slice", cfg.Subagents[0].MCP)
	}
	if cfg.Subagents[0].Model == nil || cfg.Subagents[0].Model.Name != "gemini-2.5-flash" {
		t.Errorf("Subagents[0].Model = %+v, want gemini-2.5-flash", cfg.Subagents[0].Model)
	}
}

func TestLoadWorkload_Rejections(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		yaml    string
		specMD  string
		wantErr string
	}{
		{
			name:    "schema_version_too_new",
			yaml:    "schema_version: 2\nname: x\nspecialists: [_fallback]\n",
			wantErr: "bundle declares schema_version 2 and this binary speaks 1",
		},
		{
			name:    "schema_version_zero",
			yaml:    "schema_version: 0\nname: x\nspecialists: [_fallback]\n",
			wantErr: "schema_version is 0; must be a positive integer",
		},
		{
			name:    "unknown_key",
			yaml:    "name: x\nspecialists: [_fallback]\nunknown_key: true\n",
			wantErr: "unrecognised key in workload.yaml",
		},
		{
			name:    "dispatch_graph_refused",
			yaml:    "name: x\ndispatch: graph\nspecialists: [_fallback]\n",
			wantErr: "dispatch \"graph\" is retired in core-agent",
		},
		{
			name:    "planner_enabled_refused",
			yaml:    "name: x\nplanner:\n  enabled: true\nspecialists: [_fallback]\n",
			wantErr: "planner.enabled is retired in core-agent",
		},
		{
			name:    "both_hitl_and_hitl_policy",
			yaml:    "name: x\nspecialists: [_fallback]\nhitl:\n  require_approval: true\nhitl_policy:\n  on_mutation: apply\n",
			wantErr: "both hitl: and hitl_policy: are set",
		},
		{
			name: "capability_split_violation",
			yaml: `name: x
tool_catalog:
  mcp: [{server: gke}]
  tools:
    - name: patch_k8s_resource
      mutating: true
specialists: [_fallback, change-executor]
`,
			specMD: `---
name: _fallback
description: Diagnoser that illegally holds a write tool.
mode: Task
tools:
  mcp:
    - server: gke
      tools: [patch_k8s_resource]
---
Body.
`,
			wantErr: "diagnoser specialist \"_fallback\" declares mutating tool \"patch_k8s_resource\"",
		},
		{
			name:    "invalid_specialist_name_path_traversal",
			yaml:    "name: x\nspecialists: ['../evil']\n",
			wantErr: "specialists roster entry \"../evil\" is not a valid subagent name",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(dir, "specialists"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(dir, "schemas"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "schemas", "finding.json"), []byte(testFindingSchema), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "schemas", "change-report.json"), []byte(testChangeReportSchema), 0o644); err != nil {
				t.Fatal(err)
			}
			fb := testFallbackSpecialist
			if tc.specMD != "" {
				fb = tc.specMD
			}
			if err := os.WriteFile(filepath.Join(dir, "specialists", "_fallback.specialist.md"), []byte(fb), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "specialists", "change-executor.specialist.md"), []byte(testChangeExecutorSpecialist), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "workload.yaml"), []byte(tc.yaml), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := LoadWorkload(dir)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("LoadWorkload err = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}

func TestImportWorkload_RealShipmindAndMastBundles(t *testing.T) {
	t.Parallel()
	candidates := []string{
		"/usr/local/google/home/garisingh/projects/shipmind/agents/sre-triage/gke-triage-f",
		"/usr/local/google/home/garisingh/projects/shipmind/agents/sre-triage/gke-triage",
		"/usr/local/google/home/garisingh/projects/mast/examples/workloads/gke-triage",
		"/usr/local/google/home/garisingh/projects/mast/examples/workloads/gke-triage-routed",
		"/usr/local/google/home/garisingh/projects/mast/examples/workloads/bounded-triage",
		"/usr/local/google/home/garisingh/projects/mast/examples/workloads/ns-audit",
	}
	for _, src := range candidates {
		t.Run(filepath.Base(src), func(t *testing.T) {
			t.Parallel()
			if _, err := os.Stat(filepath.Join(src, "workload.yaml")); err != nil {
				t.Skipf("external workload bundle %q not present on this machine: %v", src, err)
			}
			outDir := t.TempDir()
			cfg, err := ImportWorkload(src, outDir)
			if err != nil {
				t.Fatalf("ImportWorkload(%q): %v", src, err)
			}
			if len(cfg.Subagents) == 0 {
				t.Fatalf("ImportWorkload(%q) produced 0 subagents", src)
			}
		})
	}
}
