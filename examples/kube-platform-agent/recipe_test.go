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
// skills, the translated MCP surface, and the read-only `cluster` subagent
// it delegates to — WITHOUT any cloud credentials
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
	"reflect"
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

// configuredContentRoots resolves the config's content_roots exactly as
// cmd/core-agent does (relative entries against the agents dir, absolute
// passthrough), so these loader tests exercise the *shipped* value rather
// than a hard-coded copy of it. The recipe ships `["../upstream"]`, which
// resolves to the vendored snapshot; pointing it (or --agents-content-dir)
// at a live kube-agents checkout is the documented "live" mode.
func configuredContentRoots(t *testing.T) []string {
	t.Helper()
	cfg, err := config.Load(agentsDir)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if len(cfg.ContentRoots) == 0 {
		t.Fatal("config.content_roots is empty; the recipe must load its skills + workspace from a content root, not copies")
	}
	roots := make([]string, 0, len(cfg.ContentRoots))
	for _, r := range cfg.ContentRoots {
		if filepath.IsAbs(r) {
			roots = append(roots, r)
			continue
		}
		roots = append(roots, filepath.Join(agentsDir, r))
	}
	return roots
}

// TestInstructionsLoad asserts the persona assembles from the content root
// plus the recipe's own scope: the upstream workspace `AGENTS.md` is loaded
// verbatim from the content root (the vendored snapshot, or a live checkout),
// the `SOUL.md` persona is pulled in by the project-root `@include`, the
// runtime overlay is present, and the AGENTS.d governance index loads. A
// missing content root or a broken @include target would make the loader
// return an error here — that is the primary "the loader can run this
// content" signal.
func TestInstructionsLoad(t *testing.T) {
	loaded, err := instruction.Load(projectRoot, "",
		instruction.WithContentRoots(configuredContentRoots(t)))
	if err != nil {
		t.Fatalf("instruction.Load: %v", err)
	}
	if loaded.Empty() {
		t.Fatal("instruction.Load returned empty instruction")
	}
	wantSubstrings := []string{
		// upstream/SOUL.md, pulled in by the project-root @include
		"Platform Agent (Harness Custodian & Architect)",
		// upstream/AGENTS.md workspace file, loaded from the content root
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
// the v2 skills loader from the content root — the recipe no longer copies
// them under .agents/skills/, so a failure here means the content_roots
// wiring (config → resolve → skills.WithContentRoots) regressed.
func TestSkillsLoad(t *testing.T) {
	got, err := skills.Load(context.Background(), agentsDir, nil,
		skills.WithContentRoots(configuredContentRoots(t)))
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

// TestSkillsAreNotCopied guards the whole point of the content-root mode:
// the recipe must NOT ship a copied .agents/skills/ tree. A stray copy would
// silently win (project skills out-rank content roots), shadowing the
// content root and defeating the "run kube-agents unmodified" story — so a
// re-introduced copy is a regression, not a convenience.
func TestSkillsAreNotCopied(t *testing.T) {
	if info, err := os.Stat(filepath.Join(agentsDir, "skills")); err == nil {
		t.Errorf(".agents/skills exists (%v); skills must load from the content root, not a copy", info.IsDir())
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat .agents/skills: %v", err)
	}
}

// TestContentRootIsPlatformShaped asserts the default content root resolves
// to a real platform-shaped tree — a workspace AGENTS.md and a skills/ dir —
// so the loader tests above are exercising an actual snapshot, not an empty
// directory that would make every "discovered" assertion vacuously pass.
func TestContentRootIsPlatformShaped(t *testing.T) {
	for _, root := range configuredContentRoots(t) {
		if _, err := os.Stat(filepath.Join(root, "AGENTS.md")); err != nil {
			t.Errorf("content root %q has no workspace AGENTS.md: %v", root, err)
		}
		info, err := os.Stat(filepath.Join(root, "skills"))
		if err != nil || !info.IsDir() {
			t.Errorf("content root %q has no skills/ dir: %v", root, err)
		}
	}
}

// TestExternalCheckoutMode is the live-mode proof: the recipe run against a
// content root that is NOT the vendored snapshot (a stand-in for a real,
// unmodified kube-agents checkout passed via --agents-content-dir) still
// assembles correctly — the external workspace AGENTS.md and its skills load
// verbatim, while the SOUL persona continues to come from the recipe's own
// vendored @include (upstream splits persona across files a content root does
// not auto-assemble). This is what makes "point content_roots at your
// checkout" a supported mode rather than an accident of the snapshot's path.
func TestExternalCheckoutMode(t *testing.T) {
	// A minimal platform-shaped tree standing in for agents/platform in a
	// real checkout: a workspace AGENTS.md and one skill under skills/.
	ext := t.TempDir()
	const workspaceMarker = "EXTERNAL_PLATFORM_WORKSPACE_MARKER"
	if err := os.WriteFile(filepath.Join(ext, "AGENTS.md"), []byte("# "+workspaceMarker+"\n"), 0o644); err != nil {
		t.Fatalf("write external AGENTS.md: %v", err)
	}
	skillDir := filepath.Join(ext, "skills", "gke-live-probe")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("mkdir external skill: %v", err)
	}
	skillBody := "---\nname: gke-live-probe\ndescription: a skill from the external checkout\n---\n\nbody\n"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(skillBody), 0o644); err != nil {
		t.Fatalf("write external SKILL.md: %v", err)
	}

	// Instructions: the external workspace loads from the content root, and
	// the vendored SOUL persona + this recipe's overlay still assemble.
	loaded, err := instruction.Load(projectRoot, "",
		instruction.WithContentRoots([]string{ext}))
	if err != nil {
		t.Fatalf("instruction.Load(external): %v", err)
	}
	if !strings.Contains(loaded.Instruction, workspaceMarker) {
		t.Error("external workspace AGENTS.md not loaded from the content root")
	}
	if !strings.Contains(loaded.Instruction, "Platform Agent (Harness Custodian & Architect)") {
		t.Error("vendored SOUL persona did not assemble in external mode")
	}
	if !strings.Contains(loaded.Instruction, "Runtime overlay (core-agent)") {
		t.Error("recipe overlay missing in external mode")
	}

	// Skills: the external skill is discovered from the content root.
	got, err := skills.Load(context.Background(), agentsDir, nil,
		skills.WithContentRoots([]string{ext}))
	if err != nil {
		t.Fatalf("skills.Load(external): %v", err)
	}
	var sawExternal bool
	for _, in := range got.Infos {
		if in.Name == "gke-live-probe" {
			sawExternal = true
		}
	}
	if !sawExternal {
		names := make([]string, 0, len(got.Infos))
		for _, in := range got.Infos {
			names = append(names, in.Name)
		}
		t.Errorf("external skill not discovered from the content root: %v", names)
	}
}

// TestAppendedRootIsShadowedBySnapshot pins the exact loader behavior the
// README's "Running against a live checkout" section warns about, so the docs
// and the recipe can't drift apart: content roots layer in listed order and
// skills resolve first-declarer-wins, so a second root APPENDED after the
// vendored `../upstream` (the shape `--agents-content-dir` produces) is
// SHADOWED on every colliding skill name — a real kube-agents checkout carries
// the same 18 skill names as the snapshot, so appending it yields the snapshot's
// skills, not the checkout's. That is why live mode must *replace* the config
// root rather than append. If a future change made appended roots win (or the
// recipe "helpfully" appended the checkout), this test fails and the README's
// guidance would be wrong.
func TestAppendedRootIsShadowedBySnapshot(t *testing.T) {
	snapshot := configuredContentRoots(t) // ["…/upstream"], the vendored default

	// A stand-in checkout that redefines an existing snapshot skill name with a
	// distinguishing description, plus its own workspace file.
	ext := t.TempDir()
	if err := os.WriteFile(filepath.Join(ext, "AGENTS.md"), []byte("# APPENDED_ROOT_WORKSPACE\n"), 0o644); err != nil {
		t.Fatalf("write appended AGENTS.md: %v", err)
	}
	skillDir := filepath.Join(ext, "skills", "fleet-audit") // collides with upstream
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("mkdir appended skill: %v", err)
	}
	const appendedDesc = "APPENDED_ROOT_FLEET_AUDIT_SHOULD_BE_SHADOWED"
	body := "---\nname: fleet-audit\ndescription: " + appendedDesc + "\n---\n\nbody\n"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatalf("write appended SKILL.md: %v", err)
	}

	// Append the checkout after the snapshot, exactly as --agents-content-dir does.
	roots := append(append([]string{}, snapshot...), ext)

	got, err := skills.Load(context.Background(), agentsDir, nil,
		skills.WithContentRoots(roots))
	if err != nil {
		t.Fatalf("skills.Load(snapshot+appended): %v", err)
	}
	var fleetAuditDesc string
	var found bool
	for _, in := range got.Infos {
		if in.Name == "fleet-audit" {
			fleetAuditDesc = in.Description
			found = true
		}
	}
	if !found {
		t.Fatal("fleet-audit skill not discovered from either root")
	}
	if fleetAuditDesc == appendedDesc {
		t.Errorf("appended root's fleet-audit won (%q); the snapshot (first-declared) must shadow it — "+
			"live mode must REPLACE ../upstream, not append", appendedDesc)
	}

	// And instructions concatenate rather than override: the appended workspace
	// AND the snapshot workspace both land, which is the other half of why
	// appending is the wrong way to switch runtimes.
	loaded, err := instruction.Load(projectRoot, "",
		instruction.WithContentRoots(roots))
	if err != nil {
		t.Fatalf("instruction.Load(snapshot+appended): %v", err)
	}
	if !strings.Contains(loaded.Instruction, "APPENDED_ROOT_WORKSPACE") {
		t.Error("appended workspace did not load (expected it to concatenate)")
	}
	if !strings.Contains(loaded.Instruction, "AGENTS.md - Your Workspace") {
		t.Error("snapshot workspace dropped when a root was appended (expected both to concatenate)")
	}
}

// TestMCPServersParse asserts the translated MCP surface parses and holds
// the remote HTTP servers the recipe keeps — gke (read-write, for the
// platform agent), gke-readonly (the read-only endpoint the cluster
// subagent is scoped to), and developer_knowledge — all reachable over
// core-agent's native HTTP transport (no node mcp-remote proxy, no dropped
// platform_control/agent_common).
func TestMCPServersParse(t *testing.T) {
	servers, err := mcp.Load(agentsDir)
	if err != nil {
		t.Fatalf("mcp.Load: %v", err)
	}
	for _, want := range []string{"gke", "gke-readonly", "developer_knowledge"} {
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

// TestClusterSubagentDeclared asserts the recipe wires the read-only
// `cluster` subagent (v2.9 PR B′): the platform agent delegates a single
// cluster's diagnostics to a declarative subagent whose tool surface is
// strictly narrower than its own. It pins the nil/list/empty scoping
// contract as the recipe uses it — MCP scoped to the read-only servers
// (never the read-write `gke`), skills explicitly granted none, tools
// inherited — and cross-checks that every MCP the subagent names actually
// exists in mcp.json, and that the vendored persona it @includes is present.
func TestClusterSubagentDeclared(t *testing.T) {
	cfg, err := config.Load(agentsDir)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if len(cfg.Subagents) != 1 {
		t.Fatalf("expected exactly 1 subagent, got %d", len(cfg.Subagents))
	}
	sa := cfg.Subagents[0]
	if sa.Name != "cluster" {
		t.Errorf("subagent name = %q, want %q", sa.Name, "cluster")
	}

	// MCP: scoped (list) to the read-only surface — and crucially NOT the
	// read-write `gke` the platform agent itself uses. This is the
	// least-privilege payoff of declarative subagents.
	wantMCP := map[string]bool{"gke-readonly": true, "developer_knowledge": true}
	if len(sa.MCP) != len(wantMCP) {
		t.Errorf("subagent mcp = %v, want %v", sa.MCP, wantMCP)
	}
	for _, name := range sa.MCP {
		if !wantMCP[name] {
			t.Errorf("subagent mcp includes unexpected server %q", name)
		}
		if name == "gke" {
			t.Error("cluster subagent must NOT see the read-write gke server")
		}
	}

	// Skills: explicit empty list → grant none (a single-cluster read-only
	// SRE inherits none of the fleet/provisioning skills). Non-nil-but-empty
	// is the "grant none" half of the contract; nil would mean "inherit".
	if sa.Skills == nil {
		t.Error("subagent skills is nil (would inherit); recipe grants none via []")
	}
	if len(sa.Skills) != 0 {
		t.Errorf("subagent skills = %v, want none", sa.Skills)
	}

	// Tools: omitted → inherit the parent's built-ins (bash already disabled
	// at the parent, so the subagent is shell-less too).
	if sa.Tools != nil {
		t.Errorf("subagent tools = %v, want nil (inherit)", sa.Tools)
	}

	// Own model, declared explicitly.
	if sa.Model == nil {
		t.Fatal("subagent has no model")
	}
	if sa.Model.Provider != "vertex" || sa.Model.Name != "gemini-3.5-flash" {
		t.Errorf("subagent model = %s/%s, want vertex/gemini-3.5-flash", sa.Model.Provider, sa.Model.Name)
	}

	// Persona: @includes the vendored cluster SOUL, then overlays the
	// core-agent runtime reconciliation.
	if !strings.Contains(sa.Instructions, "@include upstream/cluster/SOUL.md") {
		t.Error("subagent instructions do not @include the vendored cluster SOUL")
	}
	if !strings.Contains(sa.Instructions, "Runtime overlay (core-agent)") {
		t.Error("subagent instructions missing the core-agent runtime overlay")
	}

	// Every MCP the subagent names must resolve against mcp.json — a typo
	// here would only surface at daemon boot otherwise.
	servers, err := mcp.Load(agentsDir)
	if err != nil {
		t.Fatalf("mcp.Load: %v", err)
	}
	for _, name := range sa.MCP {
		if _, ok := servers.Servers[name]; !ok {
			t.Errorf("subagent references mcp server %q not in mcp.json", name)
		}
	}

	// The vendored persona it @includes must exist and carry the read-only
	// boundary the whole scope relies on.
	soul, err := os.ReadFile(filepath.Join(upstreamDir, "cluster", "SOUL.md"))
	if err != nil {
		t.Fatalf("read vendored cluster SOUL.md: %v", err)
	}
	if !strings.Contains(string(soul), "Read-Only Boundary") {
		t.Error("vendored cluster SOUL.md missing its read-only boundary section")
	}

	// The subagent's @include must actually resolve at the recipe's project
	// root the same way buildDeclaredSubagents expands it — a path typo here
	// would otherwise only surface at daemon boot. instruction.Expand uses
	// the project root as both base and scope root, exactly as the cmd path.
	expanded, _, err := instruction.Expand(sa.Instructions, projectRoot, projectRoot)
	if err != nil {
		t.Fatalf("expand subagent instructions: %v", err)
	}
	if !strings.Contains(expanded, "Read-Only Boundary") {
		t.Error("expanded subagent instructions did not pull in the vendored cluster SOUL")
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
	// The cluster subagent must not drift out of the hub variant: an
	// operator running the hub should get the *same* delegation surface,
	// down to the scoped mcp/skills/tools least-privilege. Compare the
	// whole Subagents block against the local config, not just the name —
	// a hub subagent that silently regained the read-write `gke` server,
	// or whose `skills` went nil (inherit-all), would otherwise slip by.
	local := config.DefaultConfig()
	localBody, err := os.ReadFile(filepath.Join(agentsDir, "config.json"))
	if err != nil {
		t.Fatalf("read config.json: %v", err)
	}
	if err := json.Unmarshal(localBody, local); err != nil {
		t.Fatalf("parse config.json: %v", err)
	}
	if len(cfg.Subagents) != 1 || cfg.Subagents[0].Name != "cluster" {
		t.Errorf("hub config subagents drifted from local: %+v", cfg.Subagents)
	}
	if !reflect.DeepEqual(cfg.Subagents, local.Subagents) {
		t.Errorf("hub subagents block drifted from local:\n hub=%+v\nlocal=%+v",
			cfg.Subagents, local.Subagents)
	}
	// The content root must not drift between the two configs either: the hub
	// (multi_session) path threads content_roots through pkg/compose, so a hub
	// that lost the root would silently serve sessions without the platform
	// skills/workspace the local REPL loads. Require exact parity.
	if !reflect.DeepEqual(cfg.ContentRoots, local.ContentRoots) {
		t.Errorf("hub content_roots drifted from local: hub=%v local=%v",
			cfg.ContentRoots, local.ContentRoots)
	}
	if len(cfg.ContentRoots) == 0 {
		t.Error("hub config has no content_roots; sessions would load no skills/workspace")
	}
}

// TestDeployWatcherAuthWiring guards the cross-file auth invariant the K8s
// deploy depends on. The lookout watcher POSTs incident injects under its
// OWN bearer identity while asserting an admin owner, so two things must
// agree across config.hub.json and the watcher manifest:
//
//  1. the watcher's identity ("sa:k8s-event-watcher") must be a
//     proxy_identity — otherwise it may not assert X-Asserted-Caller and
//     every inject is rejected;
//  2. the manifest's --owner must be an admin_identity — otherwise incident
//     sessions are owned by a non-admin (or an unknown identity).
//
// Both failures only surface at runtime as a 403 on the first event; this
// is the only test that catches a drift between the two files.
func TestDeployWatcherAuthWiring(t *testing.T) {
	cfg := config.DefaultConfig()
	body, err := os.ReadFile(filepath.Join(agentsDir, "config.hub.json"))
	if err != nil {
		t.Fatalf("read config.hub.json: %v", err)
	}
	if err := json.Unmarshal(body, cfg); err != nil {
		t.Fatalf("parse config.hub.json: %v", err)
	}
	ms := cfg.Attach.MultiSession

	const watcherIdentity = "sa:k8s-event-watcher"
	if !sliceContains(ms.ProxyIdentities, watcherIdentity) {
		t.Errorf("config.hub.json proxy_identities %v missing %q; the watcher could not assert an owner and every inject would 403",
			ms.ProxyIdentities, watcherIdentity)
	}

	watcher, err := os.ReadFile(filepath.Join("deploy", "base", "51-deployment-watcher.yaml"))
	if err != nil {
		t.Fatalf("read watcher manifest: %v", err)
	}
	owner := manifestArgValue(string(watcher), "--owner=")
	if owner == "" {
		t.Fatal("watcher manifest has no --owner= arg")
	}
	if !sliceContains(ms.AdminIdentities, owner) {
		t.Errorf("watcher --owner=%q is not an admin_identity %v; incident sessions would be owned by a non-admin",
			owner, ms.AdminIdentities)
	}
	if !strings.Contains(string(watcher), "--token-env=WATCHER_TOKEN") {
		t.Error("watcher manifest missing --token-env=WATCHER_TOKEN; the watcher would POST unauthenticated")
	}
}

// TestDeployContentMountIsSelfConsistent guards that the daemon manifest's
// -c path, the content-volume mountPath, the nested plans mount, the
// content image build, and config.hub.json's content_roots all agree — so
// the mounted recipe tree actually resolves at runtime the way the loader
// test proves it does on disk. Any one of these drifting (e.g. the mount
// path changing but not -c, or content.Dockerfile dropping upstream/)
// yields a daemon that boots against an incomplete tree.
func TestDeployContentMountIsSelfConsistent(t *testing.T) {
	const mountPath = "/opt/kube-platform-agent"
	const cfgPath = mountPath + "/.agents/config.hub.json"

	daemonBytes, err := os.ReadFile(filepath.Join("deploy", "base", "50-deployment-daemon.yaml"))
	if err != nil {
		t.Fatalf("read daemon manifest: %v", err)
	}
	daemon := string(daemonBytes)
	if !strings.Contains(daemon, cfgPath) {
		t.Errorf("daemon -c does not point at %s", cfgPath)
	}
	if !strings.Contains(daemon, "mountPath: "+mountPath+"\n") {
		t.Errorf("content volume not mounted at %s", mountPath)
	}
	// record_plan writes agentsDir+"/plans"; under a read-only content
	// mount that only works if a writable volume nests there.
	if !strings.Contains(daemon, "mountPath: "+mountPath+"/.agents/plans") {
		t.Errorf("plans emptyDir not nested at %s/.agents/plans; record_plan would fail read-only", mountPath)
	}
	// The plans emptyDir nests INSIDE the read-only OCI image volume. A
	// read-only image layer can't have a mount point created in it at mount
	// time, so .agents/plans/ must be pre-baked into the content image (via
	// COPY .agents/). If the recipe has no plans/ dir, the image lacks the
	// mount point and the pod fails to start — unlike gke-troubleshoot's
	// host-backed ConfigMap, where the runtime can create the nested dir.
	if info, err := os.Stat(filepath.Join(agentsDir, "plans")); err != nil || !info.IsDir() {
		t.Errorf(".agents/plans/ must exist as a directory so it is baked into the content image as the nested-mount point (err=%v); a read-only image volume cannot create it at mount time", err)
	}

	// content_roots ../upstream (relative to agentsDir) resolves to
	// <mount>/upstream, so the content image MUST carry upstream/.
	cfg := config.DefaultConfig()
	body, err := os.ReadFile(filepath.Join(agentsDir, "config.hub.json"))
	if err != nil {
		t.Fatalf("read config.hub.json: %v", err)
	}
	if err := json.Unmarshal(body, cfg); err != nil {
		t.Fatalf("parse config.hub.json: %v", err)
	}
	if len(cfg.ContentRoots) != 1 || cfg.ContentRoots[0] != "../upstream" {
		t.Fatalf("content_roots %v; the deploy mount layout assumes exactly [../upstream]", cfg.ContentRoots)
	}
	dockerfile, err := os.ReadFile(filepath.Join("deploy", "content.Dockerfile"))
	if err != nil {
		t.Fatalf("read content.Dockerfile: %v", err)
	}
	for _, needed := range []string{"COPY .agents/", "COPY AGENTS.md", "COPY AGENTS.d/", "COPY upstream/"} {
		if !strings.Contains(string(dockerfile), needed) {
			t.Errorf("content.Dockerfile missing %q; the mounted tree would be incomplete", needed)
		}
	}
}

func sliceContains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// manifestArgValue extracts the value of a container arg like "--owner=foo"
// from a manifest's YAML text, tolerating both quoted (- "--owner=foo") and
// bare (- --owner=foo) list-item forms.
func manifestArgValue(manifest, prefix string) string {
	for _, line := range strings.Split(manifest, "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimPrefix(line, "- ")
		line = strings.Trim(line, `"`)
		if strings.HasPrefix(line, prefix) {
			return strings.TrimPrefix(line, prefix)
		}
	}
	return ""
}
