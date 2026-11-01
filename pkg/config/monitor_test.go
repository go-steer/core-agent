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

// Originally derived from go-steer/mast.

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const validMonitorWorkloadYAML = `name: cluster-watch
description: Continuous GKE watch monitor
mode: single_session

tool_catalog:
  mcp:
    - server: lookout
  tools:
    - name: cluster_watch_diff
      mutating: false
    - name: cluster_watch_ack
      mutating: true

specialists:
  - diagnoser

schedule:
  every: 60s
  jitter: 5s
  prompt: "Diagnose the cluster transitions reported above."

monitor:
  collect:
    tool: cluster_watch_diff
    args:
      window: 60s
  transitions_from: cluster_watch_diff
  wake_on: [opened, re_opened]
  notify:
    channel: C0123456789
    endpoint: http://switchboard.default.svc:8080
    token_env: SWITCHBOARD_TOKEN
    update: replace
    remind_after: 30m
    digest_after: 6h
  ack:
    tool: cluster_watch_ack
`

func writeMonitorWorkloadFixture(t *testing.T, workloadYAML string) string {
	t.Helper()
	dir := t.TempDir()
	specDir := filepath.Join(dir, "specialists")
	if err := os.MkdirAll(specDir, 0o755); err != nil {
		t.Fatal(err)
	}
	specMD := `---
name: diagnoser
description: Diagnoses cluster watch transitions
mode: SingleTurn
tier: small
---
Summarize the transitions.
`
	if err := os.WriteFile(filepath.Join(specDir, "diagnoser.specialist.md"), []byte(specMD), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "workload.yaml"), []byte(workloadYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestLoadMonitorWorkload_ValidFullConfig(t *testing.T) {
	t.Parallel()
	dir := writeMonitorWorkloadFixture(t, validMonitorWorkloadYAML)
	outDir := t.TempDir()
	cfg, err := ImportWorkload(dir, outDir)
	if err != nil {
		t.Fatalf("ImportWorkload() unexpected error: %v", err)
	}
	if cfg.Monitor == nil || cfg.Monitor.Collect == nil || cfg.Monitor.Collect.Tool != "cluster_watch_diff" {
		t.Fatalf("Monitor.Collect = %+v", cfg.Monitor)
	}
	if cfg.Monitor.TransitionsFrom != "cluster_watch_diff" {
		t.Errorf("TransitionsFrom = %q, want cluster_watch_diff", cfg.Monitor.TransitionsFrom)
	}
	if len(cfg.Monitor.WakeOn) != 2 || cfg.Monitor.WakeOn[0] != "opened" || cfg.Monitor.WakeOn[1] != "re_opened" {
		t.Errorf("WakeOn = %v", cfg.Monitor.WakeOn)
	}
	if cfg.Monitor.Notify == nil {
		t.Fatalf("Monitor.Notify = nil")
	}
	if cfg.Monitor.Notify.ResolvedRemindAfter() != 30*time.Minute {
		t.Errorf("ResolvedRemindAfter() = %v, want 30m", cfg.Monitor.Notify.ResolvedRemindAfter())
	}
	if cfg.Monitor.Notify.ResolvedDigestAfter() != 6*time.Hour {
		t.Errorf("ResolvedDigestAfter() = %v, want 6h", cfg.Monitor.Notify.ResolvedDigestAfter())
	}
	selfRun := cfg.Monitor.SelfRunTools()
	if len(selfRun) != 2 || selfRun[0] != "cluster_watch_diff" || selfRun[1] != "cluster_watch_ack" {
		t.Errorf("SelfRunTools() = %v, want [cluster_watch_diff cluster_watch_ack]", selfRun)
	}
}

func TestValidateMonitor_RejectionCases(t *testing.T) {
	t.Parallel()
	replace := func(old, rep string) string {
		if !strings.Contains(validMonitorWorkloadYAML, old) {
			t.Fatalf("fixture does not contain %q", old)
		}
		return strings.Replace(validMonitorWorkloadYAML, old, rep, 1)
	}

	cases := []struct {
		name    string
		yaml    string
		wantSub string
	}{
		{
			name:    "monitor without schedule.every",
			yaml:    replace("  every: 60s\n", ""),
			wantSub: "monitor requires schedule.every",
		},
		{
			name:    "collect tool empty",
			yaml:    replace("    tool: cluster_watch_diff\n", "    tool: \"\"\n"),
			wantSub: "monitor.collect[0].tool is required",
		},
		{
			name:    "collect tool not in tool_catalog",
			yaml:    replace("    tool: cluster_watch_diff\n", "    tool: undeclared_tool\n"),
			wantSub: "not declared in tools.policies / tool_catalog.tools",
		},
		{
			name: "collect tool mutating true",
			yaml: replace(
				"    - name: cluster_watch_diff\n      mutating: false",
				"    - name: cluster_watch_diff\n      mutating: true",
			),
			wantSub: "declared mutating",
		},
		{
			name:    "transitions_from without collect",
			yaml:    replace("  collect:\n    tool: cluster_watch_diff\n    args:\n      window: 60s\n", ""),
			wantSub: "transitions_from",
		},
		{
			name:    "transitions_from mismatches collect tool",
			yaml:    replace("  transitions_from: cluster_watch_diff", "  transitions_from: other_tool"),
			wantSub: "must match monitor.collect tool",
		},
		{
			name:    "wake_on without transitions_from",
			yaml:    replace("  transitions_from: cluster_watch_diff\n", "") + "\n",
			wantSub: "requires monitor.transitions_from",
		},
		{
			name:    "wake_on unknown edge",
			yaml:    replace("  wake_on: [opened, re_opened]", "  wake_on: [opened, bogus]"),
			wantSub: `must be one of "opened", "remained", "closed", "re_opened", "degraded"`,
		},
		{
			name:    "wake_on duplicate edge",
			yaml:    replace("  wake_on: [opened, re_opened]", "  wake_on: [opened, opened]"),
			wantSub: "duplicate edge",
		},
		{
			name:    "notify missing channel",
			yaml:    replace("    channel: C0123456789\n", ""),
			wantSub: "monitor.notify.channel is required",
		},
		{
			name:    "notify missing endpoint",
			yaml:    replace("    endpoint: http://switchboard.default.svc:8080\n", ""),
			wantSub: "monitor.notify.endpoint is required",
		},
		{
			name:    "notify bad endpoint scheme",
			yaml:    replace("    endpoint: http://switchboard.default.svc:8080", "    endpoint: ftp://switchboard"),
			wantSub: "http or https URL",
		},
		{
			name:    "notify missing token_env",
			yaml:    replace("    token_env: SWITCHBOARD_TOKEN\n", ""),
			wantSub: "monitor.notify.token_env is required",
		},
		{
			name:    "notify unknown update mode",
			yaml:    replace("    update: replace", "    update: overwrite"),
			wantSub: "monitor.notify.update",
		},
		{
			name: "notify remind_after without transitions_from",
			yaml: replace("  transitions_from: cluster_watch_diff\n  wake_on: [opened, re_opened]\n", "") +
				"",
			wantSub: "remind_after requires monitor.transitions_from",
		},
		{
			name:    "notify remind_after zero duration",
			yaml:    replace("    remind_after: 30m", "    remind_after: 0s"),
			wantSub: "must be > 0",
		},
		{
			name:    "notify digest_after negative duration",
			yaml:    replace("    digest_after: 6h", "    digest_after: -1h"),
			wantSub: "must be > 0",
		},
		{
			name: "ack without transitions_from",
			yaml: strings.Replace(
				replace("  transitions_from: cluster_watch_diff\n  wake_on: [opened, re_opened]\n", ""),
				"    remind_after: 30m\n", "", 1,
			),
			wantSub: "monitor.ack requires monitor.transitions_from",
		},
		{
			name:    "ack tool not in tool_catalog",
			yaml:    replace("    tool: cluster_watch_ack", "    tool: unknown_ack"),
			wantSub: "not declared in tools.policies / tool_catalog.tools",
		},
		{
			name: "ack tool mutating false",
			yaml: replace(
				"    - name: cluster_watch_ack\n      mutating: true",
				"    - name: cluster_watch_ack\n      mutating: false",
			),
			wantSub: "must be declared with mutating: true",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := writeMonitorWorkloadFixture(t, tc.yaml)
			_, err := LoadWorkload(dir)
			if err == nil {
				t.Fatalf("LoadWorkload() succeeded, want error containing %q", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("LoadWorkload() error = %q, want substring %q", err.Error(), tc.wantSub)
			}
		})
	}
}

func TestCheckMonitorCollectSurface(t *testing.T) {
	t.Parallel()
	baseCfg := func(specs ...SubagentSpec) *Config {
		return &Config{
			Monitor: &MonitorConfig{
				Collect: &MonitorCollectConfig{Tool: "watch_diff"},
				Ack:     &MonitorAckConfig{Tool: "watch_ack"},
			},
			Subagents: specs,
		}
	}

	t.Run("explicit MCP allowlist excludes collect tool", func(t *testing.T) {
		cfg := baseCfg(SubagentSpec{
			Name:     "diag",
			Mode:     SubagentModeTask,
			MCPTools: []MCPToolAllowlist{{Server: "lookout", Tools: []string{"get_pods", "get_events"}}},
		})
		if err := CheckMonitorCollectSurface(cfg, true); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("explicit MCP allowlist includes collect tool", func(t *testing.T) {
		cfg := baseCfg(SubagentSpec{
			Name:     "diag",
			Mode:     SubagentModeTask,
			MCPTools: []MCPToolAllowlist{{Server: "lookout", Tools: []string{"get_pods", "watch_diff"}}},
		})
		err := CheckMonitorCollectSurface(cfg, true)
		if err == nil || !strings.Contains(err.Error(), "watch_diff") || !strings.Contains(err.Error(), "diag") {
			t.Fatalf("CheckMonitorCollectSurface() = %v, want error mentioning watch_diff and diag", err)
		}
	})

	t.Run("explicit MCP allowlist includes ack.tool", func(t *testing.T) {
		cfg := baseCfg(SubagentSpec{
			Name:     "diag",
			Mode:     SubagentModeTask,
			MCPTools: []MCPToolAllowlist{{Server: "lookout", Tools: []string{"get_pods", "watch_ack"}}},
		})
		err := CheckMonitorCollectSurface(cfg, true)
		if err == nil || !strings.Contains(err.Error(), "monitor.ack.tool") || !strings.Contains(err.Error(), "watch_ack") {
			t.Fatalf("CheckMonitorCollectSurface() = %v, want error mentioning monitor.ack.tool and watch_ack", err)
		}
	})

	t.Run("unrestricted MCP inheritance when MCP servers exist", func(t *testing.T) {
		cfg := baseCfg(SubagentSpec{
			Name: "diag",
			Mode: SubagentModeTask,
		})
		err := CheckMonitorCollectSurface(cfg, true)
		if err == nil || !strings.Contains(err.Error(), "diag") {
			t.Fatalf("CheckMonitorCollectSurface() = %v, want error for unrestricted MCP specialist", err)
		}
	})

	t.Run("SingleTurn specialist ignores unrestricted MCP", func(t *testing.T) {
		cfg := baseCfg(SubagentSpec{
			Name: "summarizer",
			Mode: SubagentModeSingleTurn,
		})
		if err := CheckMonitorCollectSurface(cfg, true); err != nil {
			t.Fatalf("SingleTurn specialist should be exempt, got: %v", err)
		}
	})

	t.Run("explicit empty MCP slice grants zero MCP tools", func(t *testing.T) {
		cfg := baseCfg(SubagentSpec{
			Name:     "diag",
			Mode:     SubagentModeTask,
			MCPTools: []MCPToolAllowlist{},
		})
		if err := CheckMonitorCollectSurface(cfg, true); err != nil {
			t.Fatalf("explicit empty MCP allowlist should pass, got: %v", err)
		}
	})

	t.Run("builtin tool match rejected", func(t *testing.T) {
		cfg := baseCfg(SubagentSpec{
			Name:     "diag",
			Mode:     SubagentModeTask,
			Tools:    []string{"watch_diff"},
			MCPTools: []MCPToolAllowlist{},
		})
		if err := CheckMonitorCollectSurface(cfg, false); err == nil {
			t.Fatalf("expected error when builtin tool matches collect tool")
		}
	})
}
