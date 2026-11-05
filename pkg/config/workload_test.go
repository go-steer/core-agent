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
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
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
		"mcp:gke_triage-classifier*",
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
		"spawn_agent:triage-classifier",
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
	if fb.Name != "_fallback" || fb.Mode != SubagentModeTask || fb.OutputSchema != "../schemas/finding.json" || fb.Root != "roots/_fallback" {
		t.Errorf("Subagents[0] = %+v", fb)
	}
	if fb.Tools == nil || len(fb.Tools) != 0 {
		t.Errorf("Subagents[0].Tools = %v, want non-nil empty slice", fb.Tools)
	}
	ce := cfg.Subagents[1]
	if ce.Name != "change-executor" || ce.Mode != SubagentModeTask || ce.Capability != CapabilityChangeExecutor || ce.OutputSchema != "../schemas/change-report.json" || ce.Root != "roots/change-executor" {
		t.Errorf("Subagents[1] = %+v", ce)
	}
	if _, err := os.Stat(filepath.Join(outDir, "AGENTS.md")); err != nil {
		t.Errorf("AGENTS.md not written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outDir, ".agents", "mcp.json")); !os.IsNotExist(err) {
		t.Errorf(".agents/mcp.json exists (err=%v); want main agent to have no mcp.json (#1498)", err)
	}
	for _, specName := range []string{"_fallback", "change-executor"} {
		rootMCP := filepath.Join(outDir, ".agents", "roots", specName, "mcp.json")
		if _, err := os.Stat(rootMCP); err != nil {
			t.Errorf("%s not written: %v", rootMCP, err)
		}
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
		filepath.Join("..", "..", "..", "shipmind", "agents", "sre-triage", "gke-triage-f"),
		filepath.Join("..", "..", "..", "shipmind", "agents", "sre-triage", "gke-triage"),
		filepath.Join("..", "..", "..", "mast", "examples", "workloads", "gke-triage"),
		filepath.Join("..", "..", "..", "mast", "examples", "workloads", "gke-triage-routed"),
		filepath.Join("..", "..", "..", "mast", "examples", "workloads", "bounded-triage"),
		filepath.Join("..", "..", "..", "mast", "examples", "workloads", "ns-audit"),
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

func writeScheduledWorkloadFixture(t *testing.T, yamlBody string) string {
	t.Helper()
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
	if err := os.WriteFile(filepath.Join(dir, "specialists", "_fallback.specialist.md"), []byte(testFallbackSpecialist), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "workload.yaml"), []byte(yamlBody), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestLoadWorkload_ScheduledTrigger(t *testing.T) {
	t.Parallel()
	srcDir := writeScheduledWorkloadFixture(t, `name: x
specialists: [_fallback]
edge_trigger:
  http:
    path: /inject
    auth: bearer
  scheduled:
    interval: 15m
    jitter: 45s
    prompt: Sweep every namespace for pods in CrashLoopBackOff.
`)
	b, err := LoadWorkload(srcDir)
	if err != nil {
		t.Fatalf("LoadWorkload: %v", err)
	}
	s := b.EdgeTrigger.Scheduled
	if s == nil {
		t.Fatal("edge_trigger.scheduled did not survive the load")
	}
	if b.EdgeTrigger.HTTP == nil || b.EdgeTrigger.HTTP.Path != "/inject" {
		t.Errorf("edge_trigger.http = %+v, want the declared HTTP trigger alongside the schedule", b.EdgeTrigger.HTTP)
	}
	if got, err := s.EffectiveInterval(); err != nil || got != 15*time.Minute {
		t.Errorf("EffectiveInterval() = %v, %v; want 15m", got, err)
	}
	if got, err := s.EffectiveJitter(); err != nil || got != 45*time.Second {
		t.Errorf("EffectiveJitter() = %v, %v; want 45s", got, err)
	}
	if !strings.HasPrefix(s.Prompt, "Sweep every namespace") {
		t.Errorf("prompt = %q, want the declared text", s.Prompt)
	}

	outDir := t.TempDir()
	cfg, err := ImportWorkload(srcDir, outDir)
	if err != nil {
		t.Fatalf("ImportWorkload: %v", err)
	}
	if cfg.Schedule == nil {
		t.Fatal("cfg.Schedule = nil after ImportWorkload")
	}
	if got, err := cfg.Schedule.EffectiveInterval(); err != nil || got != 15*time.Minute {
		t.Errorf("cfg.Schedule.EffectiveInterval() = %v, %v; want 15m", got, err)
	}
	if got, err := cfg.Schedule.EffectiveJitter(); err != nil || got != 45*time.Second {
		t.Errorf("cfg.Schedule.EffectiveJitter() = %v, %v; want 45s", got, err)
	}
}

func TestLoadWorkload_NoScheduledTriggerIsNil(t *testing.T) {
	t.Parallel()
	srcDir := writeScheduledWorkloadFixture(t, "name: x\nspecialists: [_fallback]\nedge_trigger:\n  http:\n    path: /inject\n")
	b, err := LoadWorkload(srcDir)
	if err != nil {
		t.Fatalf("LoadWorkload: %v", err)
	}
	if b.EdgeTrigger.Scheduled != nil {
		t.Fatalf("edge_trigger.scheduled = %+v, want nil", b.EdgeTrigger.Scheduled)
	}
	if sc, err := b.ParsedSchedule(); sc != nil || err != nil {
		t.Errorf("ParsedSchedule() = %+v, %v; want nil, nil", sc, err)
	}
	if d, err := b.EdgeTrigger.Scheduled.EffectiveInterval(); d != 0 || err != nil {
		t.Errorf("EffectiveInterval() on nil trigger = %v, %v; want 0, nil", d, err)
	}
	if d, err := b.EdgeTrigger.Scheduled.EffectiveJitter(); d != 0 || err != nil {
		t.Errorf("EffectiveJitter() on nil trigger = %v, %v; want 0, nil", d, err)
	}
}

func TestLoadWorkload_ScheduledJitterDefaultAndZero(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, interval, jitter string
		want                   time.Duration
	}{
		{"a tenth of the interval", "5m", "", 30 * time.Second},
		{"capped for a long interval", "24h", "", 30 * time.Second},
		{"scales down for a short interval", "10s", "", time.Second},
		{"explicit zero is honored", "5m", "0s", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			body := "name: x\nspecialists: [_fallback]\nedge_trigger:\n  scheduled:\n    interval: " + tc.interval + "\n"
			if tc.jitter != "" {
				body += "    jitter: " + tc.jitter + "\n"
			}
			srcDir := writeScheduledWorkloadFixture(t, body)
			b, err := LoadWorkload(srcDir)
			if err != nil {
				t.Fatalf("LoadWorkload: %v", err)
			}
			got, err := b.EdgeTrigger.Scheduled.EffectiveJitter()
			if err != nil {
				t.Fatalf("EffectiveJitter: %v", err)
			}
			if got != tc.want {
				t.Errorf("EffectiveJitter() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLoadWorkload_ScheduledErrors(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, body, wantErr string }{
		{
			"no interval",
			"name: x\nspecialists: [_fallback]\nedge_trigger:\n  scheduled:\n    prompt: sweep\n",
			"names no interval",
		},
		{
			"unparseable interval",
			"name: x\nspecialists: [_fallback]\nedge_trigger:\n  scheduled:\n    interval: every 15 minutes\n",
			"is not a duration",
		},
		{
			"zero interval",
			"name: x\nspecialists: [_fallback]\nedge_trigger:\n  scheduled:\n    interval: 0s\n",
			"is a bill, not a cadence",
		},
		{
			"sub-second interval",
			"name: x\nspecialists: [_fallback]\nedge_trigger:\n  scheduled:\n    interval: 250ms\n",
			"is a bill, not a cadence",
		},
		{
			"negative interval",
			"name: x\nspecialists: [_fallback]\nedge_trigger:\n  scheduled:\n    interval: -15m\n",
			"is a bill, not a cadence",
		},
		{
			"unparseable jitter",
			"name: x\nspecialists: [_fallback]\nedge_trigger:\n  scheduled:\n    interval: 15m\n    jitter: a bit\n",
			"jitter \"a bit\" is not a duration",
		},
		{
			"negative jitter",
			"name: x\nspecialists: [_fallback]\nedge_trigger:\n  scheduled:\n    interval: 15m\n    jitter: -1s\n",
			"cannot pull one earlier",
		},
		{
			"jitter as wide as the interval",
			"name: x\nspecialists: [_fallback]\nedge_trigger:\n  scheduled:\n    interval: 15m\n    jitter: 15m\n",
			"no cadence left to keep",
		},
		{
			"both every and interval disagree",
			"name: x\nspecialists: [_fallback]\nschedule:\n  every: 5m\n  interval: 10m\n",
			"schedule sets both every",
		},
		{
			"both schedule and edge_trigger.scheduled",
			"name: x\nspecialists: [_fallback]\nschedule:\n  every: 5m\nedge_trigger:\n  scheduled:\n    interval: 5m\n",
			"both schedule: and edge_trigger.scheduled: are set",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srcDir := writeScheduledWorkloadFixture(t, tc.body)
			_, err := LoadWorkload(srcDir)
			if err == nil {
				t.Fatalf("LoadWorkload accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to mention %q", err, tc.wantErr)
			}
			if !strings.Contains(err.Error(), WorkloadFileName) {
				t.Errorf("error = %v, want it to name %q", err, WorkloadFileName)
			}
		})
	}
}

func TestImportWorkload_HardeningAndPolicies(t *testing.T) {
	t.Parallel()

	t.Run("coordinator.md copied and refreshed, stale subagents pruned, park_hint_fields preserved, MCP prefix sanitized", func(t *testing.T) {
		t.Parallel()
		srcDir := writeSyntheticGKETriageF(t)
		outDir := t.TempDir()

		// Add coordinator.md and park_hint_fields + dotted MCP server name.
		if err := os.WriteFile(filepath.Join(srcDir, "coordinator.md"), []byte("# Coordinator V1\nCoordinate triage.\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		staleDir := filepath.Join(outDir, ".agents", "subagents")
		if err := os.MkdirAll(staleDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(staleDir, "stale-old.md"), []byte("---\nname: stale-old\ndescription: stale\n---\nbody\n"), 0o644); err != nil {
			t.Fatal(err)
		}

		wlPath := filepath.Join(srcDir, "workload.yaml")
		wlBytes, err := os.ReadFile(wlPath)
		if err != nil {
			t.Fatal(err)
		}
		wlModified := strings.Replace(string(wlBytes), "    - server: gke\n", "    - server: gke.prod-eu\n", 1)
		wlModified = strings.Replace(wlModified, "    - name: patch_k8s_resource\n      mutating: true\n", "    - name: patch_k8s_resource\n      mutating: true\n      park_hint_fields: [namespace, name]\n", 1)
		if err := os.WriteFile(wlPath, []byte(wlModified), 0o644); err != nil {
			t.Fatal(err)
		}
		fbPath := filepath.Join(srcDir, "specialists", "_fallback.specialist.md")
		fbBytes, err := os.ReadFile(fbPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fbPath, []byte(strings.ReplaceAll(string(fbBytes), "server: gke", "server: gke.prod-eu")), 0o644); err != nil {
			t.Fatal(err)
		}
		cePath := filepath.Join(srcDir, "specialists", "change-executor.specialist.md")
		ceBytes, err := os.ReadFile(cePath)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(cePath, []byte(strings.ReplaceAll(string(ceBytes), "server: gke", "server: gke.prod-eu")), 0o644); err != nil {
			t.Fatal(err)
		}

		cfg, err := ImportWorkload(srcDir, outDir)
		if err != nil {
			t.Fatalf("ImportWorkload: %v", err)
		}
		if _, err := os.Stat(filepath.Join(staleDir, "stale-old.md")); !os.IsNotExist(err) {
			t.Errorf("stale subagent file still exists: err = %v", err)
		}
		agentsBytes, err := os.ReadFile(filepath.Join(outDir, "AGENTS.md"))
		if err != nil || !strings.Contains(string(agentsBytes), "# Coordinator V1") {
			t.Errorf("AGENTS.md = %q, err = %v; want Coordinator V1", string(agentsBytes), err)
		}
		if !slices.Contains(cfg.Permissions.Allow, "mcp:gke_prod_eu_get_k8s_resource*") {
			t.Errorf("Permissions.Allow = %v, want sanitized prefix mcp:gke_prod_eu_get_k8s_resource*", cfg.Permissions.Allow)
		}
		var foundPatch bool
		for _, p := range cfg.Tools.Policies {
			if p.Name == "patch_k8s_resource" {
				foundPatch = true
				if !slices.Equal(p.ParkHintFields, []string{"namespace", "name"}) {
					t.Errorf("ParkHintFields = %v, want [namespace name]", p.ParkHintFields)
				}
			}
		}
		if !foundPatch {
			t.Errorf("patch_k8s_resource not found in Tools.Policies: %+v", cfg.Tools.Policies)
		}

		// Update coordinator.md and re-import: AGENTS.md should refresh.
		if err := os.WriteFile(filepath.Join(srcDir, "coordinator.md"), []byte("# Coordinator V2\nUpdated.\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := ImportWorkload(srcDir, outDir); err != nil {
			t.Fatalf("second ImportWorkload: %v", err)
		}
		agentsBytes2, err := os.ReadFile(filepath.Join(outDir, "AGENTS.md"))
		if err != nil || !strings.Contains(string(agentsBytes2), "# Coordinator V2") {
			t.Errorf("refreshed AGENTS.md = %q, err = %v; want Coordinator V2", string(agentsBytes2), err)
		}

		// Updating a separate outDir with a hand-authored AGENTS.md preserves it.
		customOut := t.TempDir()
		if err := os.WriteFile(filepath.Join(srcDir, "coordinator.md"), []byte(""), 0o644); err != nil {
			t.Fatal(err)
		}
		_ = os.Remove(filepath.Join(srcDir, "coordinator.md"))
		if err := os.WriteFile(filepath.Join(customOut, "AGENTS.md"), []byte("# Hand-Authored Custom Instructions\nKeep me.\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := ImportWorkload(srcDir, customOut); err != nil {
			t.Fatalf("ImportWorkload into customOut: %v", err)
		}
		customAgents, err := os.ReadFile(filepath.Join(customOut, "AGENTS.md"))
		if err != nil || !strings.Contains(string(customAgents), "# Hand-Authored Custom Instructions") {
			t.Errorf("customOut AGENTS.md = %q, err = %v; want preserved hand-authored file", string(customAgents), err)
		}

		// LoadWorkloadConfig loads directly from srcDir without writing .agents/config.json.
		directCfg, err := LoadWorkloadConfig(srcDir)
		if err != nil {
			t.Fatalf("LoadWorkloadConfig: %v", err)
		}
		if directCfg.Agent.DisplayName != "gke-triage" || len(directCfg.Subagents) != 2 {
			t.Errorf("LoadWorkloadConfig = %+v, subagents=%d", directCfg.Agent, len(directCfg.Subagents))
		}
	})

	t.Run("copyWorkloadSchemas rejects symlinks", func(t *testing.T) {
		t.Parallel()
		srcDir := writeSyntheticGKETriageF(t)
		outDir := t.TempDir()
		secretFile := filepath.Join(t.TempDir(), "secret.json")
		if err := os.WriteFile(secretFile, []byte(`{"type":"object"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(secretFile, filepath.Join(srcDir, "schemas", "evil.json")); err != nil {
			t.Skipf("symlink not supported: %v", err)
		}
		if _, err := ImportWorkload(srcDir, outDir); err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("ImportWorkload err = %v, want symlink rejection", err)
		}
	})

	t.Run("Config.Validate rejects negative MaxWallclockSeconds and invalid tool policies, and Load rejects typo tools.polices", func(t *testing.T) {
		t.Parallel()
		cfg := DefaultConfig()
		cfg.Agent.MaxWallclockSeconds = -5
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "max_wallclock_seconds") {
			t.Fatalf("Validate negative MaxWallclockSeconds = %v, want error", err)
		}

		cfg2 := DefaultConfig()
		mutTrue := true
		cfg2.Tools.Policies = []ToolPolicyConfig{
			{
				Name:         "patch_k8s",
				Mutating:     &mutTrue,
				Precondition: &ToolPreconditionConfig{Read: "patch_k8s"},
			},
		}
		if err := cfg2.Validate(); err == nil || !strings.Contains(err.Error(), "cannot be its own precondition") {
			t.Fatalf("Validate invalid precondition = %v, want self-precondition error", err)
		}

		agentsDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(agentsDir, "config.json"), []byte(`{"tools":{"polices":[]}}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(agentsDir); err == nil || !strings.Contains(err.Error(), "polices") {
			t.Fatalf("Load(tools.polices) = %v, want fatal typo error", err)
		}
	})
}

func TestImportWorkload_SpecialistRootsIsolateMCPFromMainAgent(t *testing.T) {
	t.Parallel()
	srcDir := t.TempDir()
	outDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(srcDir, "specialists"), 0o755); err != nil {
		t.Fatal(err)
	}
	mcpJSON := `{
  "version": 1,
  "servers": {
    "gke": {"transport": "http", "url": "https://container.googleapis.com/mcp"},
    "lookout": {"transport": "http", "url": "http://lookout.default.svc:8080/mcp"}
  }
}`
	if err := os.WriteFile(filepath.Join(srcDir, "mcp.json"), []byte(mcpJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	diagMD := `---
name: _fallback
description: Diagnoses cluster incidents.
mode: Task
tools:
  mcp:
    - server: gke
      tools: [get_k8s_resource]
---
Diagnose it.
`
	if err := os.WriteFile(filepath.Join(srcDir, "specialists", "_fallback.specialist.md"), []byte(diagMD), 0o644); err != nil {
		t.Fatal(err)
	}
	synthMD := `---
name: synthesizer
description: Synthesizes findings without MCP tools.
mode: SingleTurn
---
Synthesize findings.
`
	if err := os.WriteFile(filepath.Join(srcDir, "specialists", "synthesizer.specialist.md"), []byte(synthMD), 0o644); err != nil {
		t.Fatal(err)
	}
	inheritMD := `---
name: inherit-reader
description: Inherits all workload MCP servers when no mutating tools exist.
mode: Task
---
Read from all servers.
`
	if err := os.WriteFile(filepath.Join(srcDir, "specialists", "inherit-reader.specialist.md"), []byte(inheritMD), 0o644); err != nil {
		t.Fatal(err)
	}
	wlYAML := `schema_version: 1
name: multi-server-triage
specialists: [_fallback, synthesizer, inherit-reader]
tool_catalog:
  mcp:
    - server: gke
    - server: lookout
  tools:
    - name: get_k8s_resource
      mutating: false
`
	if err := os.WriteFile(filepath.Join(srcDir, "workload.yaml"), []byte(wlYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	// Seed a stale main-agent .agents/mcp.json and a stale specialist root to
	// verify re-importing removes both (#1498).
	if err := os.MkdirAll(filepath.Join(outDir, ".agents", "roots", "retired-spec"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outDir, ".agents", "mcp.json"), []byte(mcpJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outDir, ".agents", "roots", "retired-spec", "mcp.json"), []byte(mcpJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := ImportWorkload(srcDir, outDir)
	if err != nil {
		t.Fatalf("ImportWorkload: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outDir, ".agents", "mcp.json")); !os.IsNotExist(err) {
		t.Errorf("main-agent .agents/mcp.json exists (err=%v); want removed so coordinator has no MCP servers (#1498)", err)
	}
	if _, err := os.Stat(filepath.Join(outDir, ".agents", "roots", "retired-spec")); !os.IsNotExist(err) {
		t.Errorf("stale root .agents/roots/retired-spec still exists (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(outDir, ".agents", "roots", "synthesizer")); !os.IsNotExist(err) {
		t.Errorf("synthesizer (SingleTurn, no MCP) unexpectedly got a root directory (err=%v)", err)
	}

	byName := make(map[string]SubagentSpec, len(cfg.Subagents))
	for _, s := range cfg.Subagents {
		byName[s.Name] = s
	}
	if got := byName["_fallback"].Root; got != "roots/_fallback" {
		t.Errorf("_fallback.Root = %q, want roots/_fallback", got)
	}
	if got := byName["synthesizer"].Root; got != "" {
		t.Errorf("synthesizer.Root = %q, want empty", got)
	}

	rootMCPBytes, err := os.ReadFile(filepath.Join(outDir, ".agents", "roots", "_fallback", "mcp.json"))
	if err != nil {
		t.Fatalf("read _fallback root mcp.json: %v", err)
	}
	var parsed struct {
		Servers map[string]any `json:"servers"`
	}
	if err := json.Unmarshal(rootMCPBytes, &parsed); err != nil {
		t.Fatalf("unmarshal _fallback root mcp.json: %v", err)
	}
	if _, ok := parsed.Servers["gke"]; !ok {
		t.Errorf("_fallback root mcp.json servers = %v, want gke", parsed.Servers)
	}
	if _, ok := parsed.Servers["lookout"]; ok {
		t.Errorf("_fallback root mcp.json servers = %v, unexpectedly includes unreferenced server lookout", parsed.Servers)
	}

	if got := byName["inherit-reader"].Root; got != "roots/inherit-reader" {
		t.Errorf("inherit-reader.Root = %q, want roots/inherit-reader", got)
	}
	inheritMCPBytes, err := os.ReadFile(filepath.Join(outDir, ".agents", "roots", "inherit-reader", "mcp.json"))
	if err != nil {
		t.Fatalf("read inherit-reader root mcp.json: %v", err)
	}
	var inheritParsed struct {
		Servers map[string]any `json:"servers"`
	}
	if err := json.Unmarshal(inheritMCPBytes, &inheritParsed); err != nil {
		t.Fatalf("unmarshal inherit-reader root mcp.json: %v", err)
	}
	if _, okGKE := inheritParsed.Servers["gke"]; !okGKE {
		t.Errorf("inherit-reader root mcp.json servers = %v, want gke", inheritParsed.Servers)
	}
	if _, okLookout := inheritParsed.Servers["lookout"]; !okLookout {
		t.Errorf("inherit-reader root mcp.json servers = %v, want lookout", inheritParsed.Servers)
	}
}
