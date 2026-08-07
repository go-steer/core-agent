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

// Package kubeplatformagent_test is the loader-only validation for the
// kube-platform-agent recipe (v2.9 Phase 0 / W5, epic #589). It proves
// that core-agent's v2 loader can consume a vendored, unmodified snapshot
// of the kube-agents Platform Agent — persona, governance SOPs, all 18
// skills, and the translated MCP surface — WITHOUT any cloud credentials
// or a live cluster. The live GKE run is a manual UAT documented in
// README.md; this test guards the plumbing.
//
// It runs as an ordinary unit test (`go test ./...`, hence CI's test-unit
// presubmit) and standalone via dev/tools/e2e-recipe-kube-platform-agent.
package kubeplatformagent_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/instruction"
	"github.com/go-steer/core-agent/v2/pkg/mcp"
	"github.com/go-steer/core-agent/v2/pkg/skills"
)

// The recipe's project root is this package's directory; the loader reads
// its bundle from ".agents/" beneath it. `go test` runs with the package
// dir as the working directory, so relative paths resolve correctly.
const (
	projectRoot = "."
	agentsDir   = ".agents"
	upstreamDir = "upstream"
)

// TestInstructionsLoad asserts the persona assembles: the @include chain
// pulls in the vendored SOUL.md and AGENTS.md, the runtime overlay is
// present, and the AGENTS.d governance index loads. A missing @include
// target would make instruction.Load return an error here — that is the
// primary "the loader can run this content" signal.
func TestInstructionsLoad(t *testing.T) {
	loaded, err := instruction.Load(projectRoot, "")
	if err != nil {
		t.Fatalf("instruction.Load: %v", err)
	}
	if loaded.Empty() {
		t.Fatal("instruction.Load returned empty instruction")
	}
	wantSubstrings := []string{
		// vendored upstream/SOUL.md (@include)
		"Platform Agent (Harness Custodian & Architect)",
		// vendored upstream/AGENTS.md (@include)
		"AGENTS.md - Your Workspace",
		// this recipe's overlay (project-root AGENTS.md)
		"Runtime overlay (core-agent)",
		// AGENTS.d/50-governance.md
		"Governance SOPs — read on demand",
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(loaded.Instruction, want) {
			t.Errorf("assembled instruction missing %q", want)
		}
	}
}

// TestGovernanceSOPsDiscoverable asserts every governance playbook the
// recipe promises is present, readable, non-empty, and named by the
// on-demand index. The SOPs are read at runtime (not @include'd into every
// turn), so "discoverable" — not "injected" — is the correct guarantee.
func TestGovernanceSOPsDiscoverable(t *testing.T) {
	// The 10 SOPs + inventory.md vendored from agents/platform/governance/.
	wantSOPs := []string{
		"blueprint_sync_sop.md",
		"compliance_audit_sop.md",
		"fleet_consistency_drift_sop.md",
		"fleet_wide_cost_analysis_sop.md",
		"global_capacity_orchestrator_sop.md",
		"inventory.md",
		"lifecycle_deprecation_manager_sop.md",
		"obtainability_audit_sop.md",
		"policy_propagation_sop.md",
		"security_patch_orchestrator_sop.md",
		"standardization_validator_sop.md",
	}

	index, err := os.ReadFile(filepath.Join("AGENTS.d", "50-governance.md"))
	if err != nil {
		t.Fatalf("read governance index: %v", err)
	}

	for _, name := range wantSOPs {
		info, err := os.Stat(filepath.Join(upstreamDir, "governance", name))
		if err != nil {
			t.Errorf("governance SOP %q missing: %v", name, err)
			continue
		}
		if info.Size() == 0 {
			t.Errorf("governance SOP %q is empty", name)
		}
		if !strings.Contains(string(index), name) {
			t.Errorf("governance index does not reference %q", name)
		}
	}
}

// TestSkillsLoad asserts all 18 Platform Agent skills are discovered by
// the v2 skills loader from the recipe's .agents/skills/.
func TestSkillsLoad(t *testing.T) {
	got, err := skills.Load(context.Background(), agentsDir, nil)
	if err != nil {
		t.Fatalf("skills.Load: %v", err)
	}
	const wantCount = 18
	if len(got.Infos) != wantCount {
		names := make([]string, 0, len(got.Infos))
		for _, in := range got.Infos {
			names = append(names, in.Name)
		}
		t.Fatalf("discovered %d skills, want %d: %v", len(got.Infos), wantCount, names)
	}
	// Spot-check a few load-bearing skills by name.
	discovered := make(map[string]bool, len(got.Infos))
	for _, in := range got.Infos {
		discovered[in.Name] = true
	}
	for _, want := range []string{"gke-cluster-creator", "fleet-audit", "manage-cluster", "submit-suggestion"} {
		if !discovered[want] {
			t.Errorf("skill %q not discovered", want)
		}
	}
}

// TestMCPServersParse asserts the translated MCP surface parses and holds
// exactly the two remote HTTP servers the recipe keeps — gke and
// developer_knowledge — reachable over core-agent's native HTTP transport
// (no node mcp-remote proxy, no dropped platform_control/agent_common).
func TestMCPServersParse(t *testing.T) {
	servers, err := mcp.Load(agentsDir)
	if err != nil {
		t.Fatalf("mcp.Load: %v", err)
	}
	for _, want := range []string{"gke", "developer_knowledge"} {
		spec, ok := servers.Servers[want]
		if !ok {
			t.Errorf("mcp server %q not present", want)
			continue
		}
		if spec.Transport != "http" {
			t.Errorf("mcp server %q transport = %q, want http", want, spec.Transport)
		}
	}
	for _, dropped := range []string{"platform_control", "agent_common"} {
		if _, ok := servers.Servers[dropped]; ok {
			t.Errorf("mcp server %q should have been dropped (Hermes-runtime-specific)", dropped)
		}
	}
}

// TestConfigPolicy asserts the recipe ships the plan-first-for-a-hub
// policy: bash disabled, and every mutation (including all MCP calls)
// gated behind record_plan via require_plan_artifact composed over yolo
// mode (which lets a no-TTY daemon proceed once a plan is recorded).
func TestConfigPolicy(t *testing.T) {
	cfg, err := config.Load(agentsDir)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if cfg.Permissions.Mode != config.PermissionModeYolo {
		t.Errorf("permissions.mode = %q, want %q", cfg.Permissions.Mode, config.PermissionModeYolo)
	}
	if !cfg.Permissions.RequirePlanArtifact {
		t.Error("permissions.require_plan_artifact = false, want true (plan-first gate)")
	}
	var bashDisabled bool
	for _, name := range cfg.Tools.Disable {
		if name == "bash" {
			bashDisabled = true
		}
	}
	if !bashDisabled {
		t.Error("tools.disable does not include \"bash\"")
	}
}

// TestHubConfigParses guards the second shipped artifact: config.hub.json,
// the attach-hub variant the README documents. It must parse and validate
// the same way `core-agent -c <file>` loads it (DefaultConfig ← Unmarshal ←
// Validate), keep the same plan-first policy as the local config, and turn
// on multi-session with a bearer table. A broken hub config would otherwise
// only surface when an operator tried to boot the daemon.
func TestHubConfigParses(t *testing.T) {
	cfg := config.DefaultConfig()
	body, err := os.ReadFile(filepath.Join(agentsDir, "config.hub.json"))
	if err != nil {
		t.Fatalf("read config.hub.json: %v", err)
	}
	if err := json.Unmarshal(body, cfg); err != nil {
		t.Fatalf("parse config.hub.json: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config.hub.json failed validation: %v", err)
	}
	if cfg.Permissions.Mode != config.PermissionModeYolo || !cfg.Permissions.RequirePlanArtifact {
		t.Errorf("hub config policy drifted from local: mode=%q require_plan_artifact=%v",
			cfg.Permissions.Mode, cfg.Permissions.RequirePlanArtifact)
	}
	if cfg.Attach.Listen == "" {
		t.Error("hub config has no attach.listen")
	}
	if !cfg.Attach.MultiSession.Enabled {
		t.Error("hub config does not enable attach.multi_session")
	}
}
