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
// of the kube-agents Platform Agent — persona, governance SOPs, the 18
// platform skills (from the content root), the six cluster domain skills the
// read-only `cluster` subagent loads from its OWN `../cluster` content root
// (#621), the translated read-only MCP surface, and the `cluster` subagent
// it delegates to — WITHOUT any cloud credentials
// or a live cluster. The live GKE run is a manual UAT documented in
// README.md; this test guards the plumbing.
//
// It runs as an ordinary unit test (`go test ./...`, hence CI's test-unit
// presubmit) and standalone via dev/tools/e2e-recipe-kube-platform-agent.
package kubeplatformagent_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/go-steer/core-agent/v2/examples/internal/recipecheck"
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
	// clusterRoot is the `cluster` subagent's own content root (#621): its
	// AGENTS.md persona, cluster/skills/ (the six domain skills), and
	// cluster/mcp.json. The subagent config declares `"root": "../cluster"`,
	// which the daemon resolves against the agents dir; this test resolves the
	// same tree relative to the package dir.
	clusterRoot = "cluster"
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

// clusterSkills are the six GKE domain-diagnostic skills the read-only
// `cluster` subagent carries. Since #621 they live under the subagent's OWN
// content root (cluster/skills/), loaded independently of the parent via the
// per-subagent `root` feature (#619) — NOT vendored into the parent's
// `.agents/skills/` (which they were before, only because the old declarative
// subagent could scope skills but not load its own). See PROVENANCE.md.
var clusterSkills = []string{
	"gke-workload-troubleshooting",
	"gke-observability",
	"gke-reliability",
	"gke-storage",
	"gke-workload-scaling",
	"gke-workload-security",
}

// TestSkillsLoad asserts the parent (platform) agent discovers exactly the 18
// Platform Agent skills from the `../upstream` content root — and NOT the six
// cluster domain skills, which since #621 load only into the `cluster`
// subagent from its own root. A failure here means either the content_roots
// wiring (config → resolve → skills.WithContentRoots) regressed, a platform
// skill went missing/renamed, or a cluster skill leaked back into the parent
// surface (the pollution #621 removed).
func TestSkillsLoad(t *testing.T) {
	got, err := skills.Load(context.Background(), agentsDir, nil,
		skills.WithContentRoots(configuredContentRoots(t)))
	if err != nil {
		t.Fatalf("skills.Load: %v", err)
	}
	const platformSkills = 18
	if len(got.Infos) != platformSkills {
		names := make([]string, 0, len(got.Infos))
		for _, in := range got.Infos {
			names = append(names, in.Name)
		}
		t.Fatalf("parent discovered %d skills, want %d (the 18 platform skills; the six cluster skills load from the subagent's own root, not the parent): %v",
			len(got.Infos), platformSkills, names)
	}
	discovered := make(map[string]bool, len(got.Infos))
	for _, in := range got.Infos {
		discovered[in.Name] = true
	}
	// Spot-check a few load-bearing platform skills from the content root.
	for _, want := range []string{"gke-cluster-creator", "fleet-audit", "manage-cluster", "submit-suggestion"} {
		if !discovered[want] {
			t.Errorf("platform skill %q not discovered from the content root", want)
		}
	}
	// The six cluster skills must NOT be in the parent's set — #621 moved them
	// out of `.agents/skills/` into the cluster subagent's own root. A copy
	// lingering in the parent would out-rank the content root and reintroduce
	// the namespace pollution the migration removed.
	for _, notWant := range clusterSkills {
		if discovered[notWant] {
			t.Errorf("cluster skill %q loaded into the PARENT surface; after #621 it must live only under cluster/skills/", notWant)
		}
	}
}

// TestClusterSkillsLiveUnderSubagentRoot guards the #621 migration: the six
// cluster domain skills load from the `cluster` subagent's OWN content root
// (cluster/skills/), NOT a copy under the parent's `.agents/skills/`. Two
// invariants:
//
//  1. The parent project scope carries no vendored skills — `.agents/skills/`
//     is absent or empty. A skill left there would out-rank the content root
//     (project skills win) and reintroduce the parent-surface pollution #621
//     removed. (An empty dir is fine — git does not track it, so a fresh
//     checkout has no `.agents/skills/` at all.)
//  2. The cluster root carries exactly the six cluster skills, loaded the same
//     way loadSubagentRoot does (skills.Load against the root dir).
func TestClusterSkillsLiveUnderSubagentRoot(t *testing.T) {
	if entries, err := os.ReadDir(filepath.Join(agentsDir, "skills")); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				t.Errorf(".agents/skills/%s exists; after #621 the parent must carry no vendored skills (they live under cluster/skills/)", e.Name())
			}
		}
	}

	got, err := skills.Load(context.Background(), clusterRoot, nil)
	if err != nil {
		t.Fatalf("skills.Load(cluster root): %v", err)
	}
	want := make(map[string]bool, len(clusterSkills))
	for _, n := range clusterSkills {
		want[n] = true
	}
	if len(got.Infos) != len(clusterSkills) {
		names := make([]string, 0, len(got.Infos))
		for _, in := range got.Infos {
			names = append(names, in.Name)
		}
		t.Fatalf("cluster root discovered %d skills, want the six cluster skills: %v", len(got.Infos), names)
	}
	for _, in := range got.Infos {
		if !want[in.Name] {
			t.Errorf("cluster/skills/ has unexpected skill %q", in.Name)
		}
		delete(want, in.Name)
	}
	for n := range want {
		t.Errorf("cluster skill %q missing from cluster/skills/", n)
	}
}

// TestClusterRuntimeContentHasNoDeadKanbanHandoff guards #703. The `cluster`
// subagent's ONLY channel back to the platform parent is its report, and no
// kanban tool is registered in this runtime (cluster/mcp.json declares the
// read-only `gke` + `developer_knowledge` HTTP servers and nothing else). So a
// runtime instruction to hand off via `kanban_complete` — or to keep the RCA
// *out* of the reply — directs the agent to discard the investigation it just
// finished. Observed live in GKE UAT as content-free handoffs.
//
// Scope is deliberately the RUNTIME content root only (cluster/AGENTS.md and
// cluster/skills/). cluster/SOUL.md is an unmodified vendored persona whose §6
// describes the Hermes kanban lifecycle by design, and upstream/ is a faithful
// snapshot — both are evidence for the portability case study (#704) and are
// reconciled by the overlay in cluster/AGENTS.md rather than edited. What must
// not survive is a *skill* re-asserting the dead channel, because skills load
// at the point of use and so speak last.
func TestClusterRuntimeContentHasNoDeadKanbanHandoff(t *testing.T) {
	// Instructions that route the deliverable somewhere it cannot go.
	banned := []string{"kanban_complete", "kanban_block", "kanban_show"}

	skillsDir := filepath.Join(clusterRoot, "skills")
	err := filepath.WalkDir(skillsDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || filepath.Ext(path) != ".md" {
			return nil
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		rel, _ := filepath.Rel(clusterRoot, path)
		for _, tok := range banned {
			if bytes.Contains(body, []byte(tok)) {
				t.Errorf("%s instructs the cluster subagent to use %s, but no kanban tool "+
					"is registered in this runtime — its report is the only channel to the "+
					"parent (#703). Rewrite the step to put the RCA in the report.", rel, tok)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk cluster/skills: %v", err)
	}

	// The overlay must claim precedence, or a vendored skill's instructions win
	// on proximity — that is exactly how #703 slipped through with a correct
	// overlay already in place.
	overlay, err := os.ReadFile(filepath.Join(clusterRoot, "AGENTS.md"))
	if err != nil {
		t.Fatalf("read cluster/AGENTS.md: %v", err)
	}
	if !bytes.Contains(overlay, []byte("this overlay wins")) {
		t.Error("cluster/AGENTS.md must state that the core-agent overlay overrides " +
			"conflicting skill-level instructions; without it a vendored skill loaded " +
			"at the point of use is the most proximate instruction (#703)")
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
// exactly the remote HTTP servers the recipe keeps — a single read-only
// `gke` (there is no read-write GKE endpoint at all: propose-only is
// enforced by the transport, not persona) and `developer_knowledge` — both
// reachable over core-agent's native HTTP transport (no node mcp-remote
// proxy, no dropped platform_control/agent_common). The old separate
// `gke-readonly` server is gone: the parent and the `cluster` subagent share
// the one read-only endpoint.
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
	// The `gke` server must point at the read-only endpoint — this is the
	// mechanism (not just the persona) that makes the platform agent
	// propose-only. A regression back to the read-write endpoint would hand
	// the agent live-mutation verbs again.
	if gke, ok := servers.Servers["gke"]; ok {
		const readOnlyURL = "https://container.googleapis.com/mcp/read-only"
		if gke.URL != readOnlyURL {
			t.Errorf("gke url = %q, want the read-only endpoint %q", gke.URL, readOnlyURL)
		}
	}
	// The pre-split read-write endpoint must not linger under any name.
	if _, ok := servers.Servers["gke-readonly"]; ok {
		t.Error("mcp server \"gke-readonly\" should have been collapsed into the single read-only \"gke\"")
	}
	for _, dropped := range []string{"platform_control", "agent_common"} {
		if _, ok := servers.Servers[dropped]; ok {
			t.Errorf("mcp server %q should have been dropped (Hermes-runtime-specific)", dropped)
		}
	}
}

// TestClusterSubagentDeclared asserts the recipe wires the read-only
// `cluster` subagent (v2.9 PR B′, migrated to a per-subagent content root in
// #621): the platform agent delegates a single cluster's diagnostics to a
// declarative subagent that loads its OWN persona + skills + MCP from a
// dedicated `../cluster` root, independent of the parent's shared surface. It
// pins that the config uses `root` (not the old inline skills/mcp scoping),
// that the root resolves to a real tree, and that the tree carries the
// read-only propose-only surface (persona read-only boundary, six domain
// skills, and a read-only `gke` endpoint).
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

	// #621: the subagent loads its own persona/skills/MCP from a dedicated
	// content root instead of scoping the parent's shared surface. `root` is
	// resolved against the agents dir, so "../cluster" is a sibling of
	// `.agents/`.
	if sa.Root != "../cluster" {
		t.Errorf("subagent root = %q, want ../cluster (#621 per-subagent content root)", sa.Root)
	}

	// The old inline shared-surface fields must be gone — the root supplies
	// them now. A lingering skills/mcp list or inline instructions would mean
	// the migration is half-done (the parent would still have to vendor those
	// skills), which is exactly what #621 removes.
	if sa.Skills != nil {
		t.Errorf("subagent skills = %v, want nil (loaded from the root, not scoped from the parent)", sa.Skills)
	}
	if sa.MCP != nil {
		t.Errorf("subagent mcp = %v, want nil (loaded from cluster/mcp.json)", sa.MCP)
	}
	if sa.Instructions != "" {
		t.Error("subagent has inline instructions; the persona must auto-assemble from cluster/AGENTS.md")
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

	// The root must resolve to a real directory relative to the agents dir,
	// exactly as loadSubagentRoot does (base = agents dir). A typo here would
	// otherwise only surface at daemon boot.
	rootAbs := filepath.Join(agentsDir, sa.Root)
	if info, err := os.Stat(rootAbs); err != nil || !info.IsDir() {
		t.Fatalf("subagent root %q does not resolve to a directory (%v)", rootAbs, err)
	}

	// Persona: the root's AGENTS.md auto-assembles (rootedSubagentInstruction
	// with no inline instructions loads <root>/AGENTS.md), pulls in the
	// vendored SOUL's read-only boundary via a root-scoped @include, and
	// overlays the core-agent runtime reconciliation.
	loaded, err := instruction.Load("", "",
		instruction.WithContentRoots([]string{rootAbs}))
	if err != nil {
		t.Fatalf("instruction.Load(cluster root): %v", err)
	}
	if !strings.Contains(loaded.Instruction, "Read-Only Boundary") {
		t.Error("cluster root persona missing the read-only boundary (cluster/SOUL.md @include did not resolve)")
	}
	if !strings.Contains(loaded.Instruction, "Runtime overlay (core-agent)") {
		t.Error("cluster root persona missing the core-agent runtime overlay")
	}

	// Skills: the root carries the six cluster domain skills (loaded the same
	// way loadSubagentRoot does). Depth-checked by
	// TestClusterSkillsLiveUnderSubagentRoot; count spot-check here.
	clSkills, err := skills.Load(context.Background(), clusterRoot, nil)
	if err != nil {
		t.Fatalf("skills.Load(cluster root): %v", err)
	}
	if len(clSkills.Infos) != len(clusterSkills) {
		t.Errorf("cluster root has %d skills, want %d", len(clSkills.Infos), len(clusterSkills))
	}

	// MCP: the root's own mcp.json carries the read-only `gke` + knowledge
	// servers. The propose-only posture (#617 finding 2) is enforced by the
	// transport — the endpoint must stay the read-only variant across the move.
	clMCP, err := mcp.Load(clusterRoot)
	if err != nil {
		t.Fatalf("mcp.Load(cluster root): %v", err)
	}
	for _, want := range []string{"gke", "developer_knowledge"} {
		if _, ok := clMCP.Servers[want]; !ok {
			t.Errorf("cluster/mcp.json missing server %q", want)
		}
	}
	if gke, ok := clMCP.Servers["gke"]; ok {
		const readOnlyURL = "https://container.googleapis.com/mcp/read-only"
		if gke.URL != readOnlyURL {
			t.Errorf("cluster gke url = %q, want the read-only endpoint %q", gke.URL, readOnlyURL)
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
	// The property, not the spelling — either plan_mode: "required" or
	// the deprecated bool arms the gate; advisory does not.
	if !cfg.Permissions.PlanGateArmed() {
		t.Errorf("plan gate not armed (plan_mode resolved to %q), want required", cfg.Permissions.ResolvedPlanMode())
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
	if cfg.Permissions.Mode != config.PermissionModeYolo || !cfg.Permissions.PlanGateArmed() {
		t.Errorf("hub config policy drifted from local: mode=%q plan_mode=%q",
			cfg.Permissions.Mode, cfg.Permissions.ResolvedPlanMode())
	}
	if cfg.Attach.Listen == "" {
		t.Error("hub config has no attach.listen")
	}
	if !cfg.Attach.MultiSession.Enabled {
		t.Error("hub config does not enable attach.multi_session")
	}
	// The cluster subagent must not drift out of the hub variant: an
	// operator running the hub should get the *same* delegation surface,
	// down to the `root` that supplies its persona/skills/MCP. Compare the
	// whole Subagents block against the local config, not just the name —
	// a hub subagent that lost its `root` (falling back to the parent
	// surface), or regained an inline read-write scoping, would otherwise
	// slip by.
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
//  1. the watcher's identity ("sa:lookout-watch") must be a
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

	const watcherIdentity = "sa:lookout-watch"
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
	// cluster/ is the `cluster` subagent's own content root (#621); the
	// subagent's `root: "../cluster"` resolves to <mount>/cluster, so the
	// image must carry it or the subagent boots with no skills/persona.
	for _, needed := range []string{"COPY .agents/", "COPY AGENTS.md", "COPY AGENTS.d/", "COPY upstream/", "COPY cluster/"} {
		if !strings.Contains(string(dockerfile), needed) {
			t.Errorf("content.Dockerfile missing %q; the mounted tree would be incomplete", needed)
		}
	}

	// The initcontainer-copy overlay (fallback for clusters below the
	// image-volume floor) fills the same mount by `cp`-ing the tree out of
	// the content image. Its source list MUST mirror the Dockerfile COPY
	// layers above, or the daemon boots against an incomplete tree there
	// even though the image-volume overlay is fine. In particular, dropping
	// /cluster crashes startup (subagent root path not found), not just a
	// degraded subagent — this guards that regression.
	initPatch, err := os.ReadFile(filepath.Join("deploy", "overlays", "initcontainer-copy", "patch-content-via-initcontainer.yaml"))
	if err != nil {
		t.Fatalf("read initcontainer-copy patch: %v", err)
	}
	for _, src := range []string{"- /.agents", "- /AGENTS.md", "- /AGENTS.d", "- /upstream", "- /cluster"} {
		if !strings.Contains(string(initPatch), src+"\n") {
			t.Errorf("initcontainer-copy patch does not cp %q; its emptyDir would be missing that tree (must mirror content.Dockerfile COPY layers)", strings.TrimPrefix(src, "- "))
		}
	}
}

// This daemon is headless (--no-repl) and runs auto-continue by default, so
// nobody is tailing stdout when a tool loop starts and auto-continue keeps
// re-driving the interrupted turn. The default --watchdog=warn only LOGS a
// runaway; only --watchdog=enforce (#623) trips a kind=watchdog turn-error and
// refuses new turns, which is what actually stops the re-drive loop. Live UAT
// hit exactly this: a runaway burned tokens under warn mode. Pin the flag so
// the recipe can't silently regress to warn.
func TestDeployDaemonEnforcesWatchdog(t *testing.T) {
	daemon, err := os.ReadFile(filepath.Join("deploy", "base", "50-deployment-daemon.yaml"))
	if err != nil {
		t.Fatalf("read daemon manifest: %v", err)
	}
	// Match the arg-list item form (`- "--watchdog=enforce"`), not the bare
	// string — the explanatory comment above the arg also contains
	// "--watchdog=enforce", so a substring check would pass on the prose even
	// if the actual flag were removed.
	if !strings.Contains(string(daemon), "- \"--watchdog=enforce\"") {
		t.Errorf("daemon must pass --watchdog=enforce as an arg; without it a headless auto-continue daemon re-drives a runaway tool loop indefinitely (only warn-logs it)")
	}
}

// --- deploy RBAC parsing (issue #618) ---------------------------------------

type policyRule struct {
	APIGroups     []string `yaml:"apiGroups"`
	Resources     []string `yaml:"resources"`
	ResourceNames []string `yaml:"resourceNames"`
	Verbs         []string `yaml:"verbs"`
}

type rbacObject struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace"`
	} `yaml:"metadata"`
	Rules    []policyRule `yaml:"rules"`
	Subjects []struct {
		Kind      string `yaml:"kind"`
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace"`
	} `yaml:"subjects"`
	RoleRef struct {
		Kind string `yaml:"kind"`
		Name string `yaml:"name"`
	} `yaml:"roleRef"`
}

func loadRBAC(t *testing.T, name string) rbacObject {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("deploy", "base", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	var obj rbacObject
	if err := yaml.Unmarshal(body, &obj); err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return obj
}

// grants reports whether the role grants verb on (group, resource).
func (r rbacObject) grants(group, resource, verb string) bool {
	for _, rule := range r.Rules {
		if !sliceContains(rule.APIGroups, group) || !sliceContains(rule.Resources, resource) {
			continue
		}
		if sliceContains(rule.Verbs, verb) {
			return true
		}
	}
	return false
}

// TestDeployWatcherClusterRoleEnrichmentComplete pins the #618 fix: the
// vendored watcher ClusterRole must grant the reads lookout enrichment
// performs, or every incident inject regresses to `enrichment_error
// stage=resolve` (the M4-drill / #618 symptom). It also pins the two
// invariants the grant must NOT cross: the recipe's suffixed object name
// (so it coexists with gke-troubleshoot-agent) and read-only verbs only
// (a watcher that could write would break the propose-only posture).
//
// This is the bug-fix test — it FAILS against the pre-#618 minimal role
// (events + pods:get only), which granted no `list` and none of the
// workload/secrets/log reads asserted below.
func TestDeployWatcherClusterRoleEnrichmentComplete(t *testing.T) {
	role := loadRBAC(t, "12-clusterrole-watcher.yaml")

	if role.Kind != "ClusterRole" {
		t.Fatalf("12-clusterrole-watcher.yaml kind = %q, want ClusterRole", role.Kind)
	}
	// Suffixed name so it coexists with gke-troubleshoot-agent's bare
	// "lookout-watch" ClusterRole (cluster-scoped objects share a
	// namespace); the binding (13) roleRef must match this.
	if role.Metadata.Name != "lookout-watch-kube-platform" {
		t.Errorf("ClusterRole name = %q, want lookout-watch-kube-platform", role.Metadata.Name)
	}

	// The reads enrichment (DESIGN.md §7.6) needs on both paths. Each of
	// these is absent from the pre-#618 minimal role, so this slice is the
	// bug-fix assertion.
	type need struct{ group, resource, verb string }
	for _, n := range []need{
		{"", "pods", "list"},                           // scoped-list fallback (old role had get only)
		{"", "pods/log", "get"},                        // triage logs distillation
		{"apps", "deployments", "list"},                // scoped-list fallback
		{"apps", "deployments", "get"},                 // live-path top-owner GET
		{"apps", "replicasets", "get"},                 // live-path top-owner GET
		{"apps", "statefulsets", "get"},                // live-path top-owner GET
		{"apps", "daemonsets", "list"},                 // scoped-list fallback
		{"batch", "jobs", "list"},                      // scoped-list fallback + workload source
		{"", "services", "list"},                       // edges section
		{"", "configmaps", "list"},                     // edges section
		{"", "secrets", "list"},                        // expiry source + edges (§11 tradeoff)
		{"rbac.authorization.k8s.io", "roles", "list"}, // edges RBAC checks
	} {
		if !role.grants(n.group, n.resource, n.verb) {
			t.Errorf("ClusterRole does not grant %q on {group:%q resource:%q}; enrichment would fail",
				n.verb, n.group, n.resource)
		}
	}

	// Read-only: no write verb anywhere, and secrets granted `list` only
	// (never get/watch — no resident informer cache of secret material).
	writeVerbs := map[string]bool{"create": true, "update": true, "patch": true, "delete": true, "deletecollection": true}
	for _, rule := range role.Rules {
		for _, v := range rule.Verbs {
			if writeVerbs[v] {
				t.Errorf("ClusterRole grants write verb %q on %v; the watcher must be read-only", v, rule.Resources)
			}
			if sliceContains(rule.Resources, "secrets") && v != "list" {
				t.Errorf("ClusterRole grants %q on secrets; only `list` is allowed (§11)", v)
			}
		}
	}
}

// TestDeployWatcherCapacityAndNetworkPolicy pins the optimal-config
// additions (#618): the kube-system capacity Role/RoleBinding and the
// default-deny NetworkPolicy that mitigates the cluster-wide secrets
// grant. It checks they are wired into the base kustomization and that
// the binding's subject + roleRef stay self-consistent across files.
func TestDeployWatcherCapacityAndNetworkPolicy(t *testing.T) {
	kustomize, err := os.ReadFile(filepath.Join("deploy", "base", "kustomization.yaml"))
	if err != nil {
		t.Fatalf("read base kustomization: %v", err)
	}
	for _, f := range []string{
		"14-role-watcher-capacity.yaml",
		"15-rolebinding-watcher-capacity.yaml",
		"16-networkpolicy-watcher.yaml",
	} {
		if !strings.Contains(string(kustomize), f) {
			t.Errorf("base kustomization does not list %q", f)
		}
	}

	// The capacity Role/RoleBinding (14/15) live in kube-system, but every
	// other resource belongs to kube-platform. The base MUST namespace them
	// via the unsetOnly NamespaceTransformer, NOT the `namespace:` shorthand:
	// the shorthand clobbers explicit namespaces and would silently rewrite
	// 14/15 to kube-platform, where the cluster-autoscaler-status ConfigMap
	// they read does not exist — the capacity source would 403 at runtime and
	// never surface on disk. This guards that fix.
	if strings.Contains(string(kustomize), "\nnamespace: kube-platform") {
		t.Error("base kustomization uses the `namespace:` shorthand; it clobbers the kube-system capacity RBAC into kube-platform — use the unsetOnly NamespaceTransformer")
	}
	if !strings.Contains(string(kustomize), "namespace-transformer.yaml") {
		t.Error("base kustomization does not reference namespace-transformer.yaml")
	}
	nst, err := os.ReadFile(filepath.Join("deploy", "base", "namespace-transformer.yaml"))
	if err != nil {
		t.Fatalf("read namespace-transformer.yaml: %v", err)
	}
	if !strings.Contains(string(nst), "unsetOnly: true") {
		t.Error("namespace-transformer.yaml must set unsetOnly: true to preserve the kube-system capacity RBAC's explicit namespace")
	}

	capRole := loadRBAC(t, "14-role-watcher-capacity.yaml")
	if capRole.Kind != "Role" || capRole.Metadata.Namespace != "kube-system" {
		t.Errorf("capacity Role kind/ns = %q/%q, want Role/kube-system", capRole.Kind, capRole.Metadata.Namespace)
	}
	if !capRole.grants("", "configmaps", "get") || !capRole.grants("", "configmaps", "list") {
		t.Error("capacity Role must grant get+list on configmaps (cluster-autoscaler-status poll)")
	}

	capBind := loadRBAC(t, "15-rolebinding-watcher-capacity.yaml")
	if capBind.Kind != "RoleBinding" || capBind.Metadata.Namespace != "kube-system" {
		t.Errorf("capacity RoleBinding kind/ns = %q/%q, want RoleBinding/kube-system", capBind.Kind, capBind.Metadata.Namespace)
	}
	if capBind.RoleRef.Name != capRole.Metadata.Name {
		t.Errorf("capacity RoleBinding roleRef %q != Role name %q", capBind.RoleRef.Name, capRole.Metadata.Name)
	}
	if len(capBind.Subjects) != 1 ||
		capBind.Subjects[0].Name != "lookout-watch" ||
		capBind.Subjects[0].Namespace != "kube-platform" {
		t.Errorf("capacity RoleBinding subject = %+v, want SA lookout-watch/kube-platform", capBind.Subjects)
	}

	np := loadRBAC(t, "16-networkpolicy-watcher.yaml")
	if np.Kind != "NetworkPolicy" || np.Metadata.Namespace != "kube-platform" {
		t.Errorf("NetworkPolicy kind/ns = %q/%q, want NetworkPolicy/kube-platform", np.Kind, np.Metadata.Namespace)
	}
}

// TestDeployWatcherImageFloor pins the lookout image at v0.18.0 (#618,
// #621) across every place it is declared — the base Deployment and both
// overlays — so a bump in one spot can't silently leave another behind.
// The naming this recipe uses came from v0.17.0, which retired the
// k8s-event-watcher transition naming (lookout#205/#206), which is why
// every resource here is named lookout-watch. v0.14.0 remains the
// capability floor — it carries per-resource-Forbidden tolerance
// (k8s-lookout#192): a narrowed role degrades to a partial bundle
// instead of hard-failing enrichment.
//
// DELIBERATELY FROZEN — this pin does NOT track lookout's latest.
// This recipe is a portability case study (#704): its value is that it
// is a fixed, reproducible port of kube-agents content onto core-agent,
// so the version skew against examples/gke-troubleshoot-agent (which
// DOES track latest) is intentional and is not drift to "fix". Do not
// bump this tag as part of a routine lookout upgrade; bump it only with
// a reason of its own, and re-vendor the RBAC (12/14/15/16) against the
// new release when you do.
//
// The vendored RBAC (12/14/15/16) is byte-identical across v0.17.0 and
// v0.18.0 — that release changed only the watcher binary, so bumping the
// image needs no re-vendor.
func TestDeployWatcherImageFloor(t *testing.T) {
	const wantTag = "v0.18.0"
	const lookout = "ghcr.io/go-steer/lookout"

	base, err := os.ReadFile(filepath.Join("deploy", "base", "51-deployment-watcher.yaml"))
	if err != nil {
		t.Fatalf("read watcher manifest: %v", err)
	}
	if !strings.Contains(string(base), "image: "+lookout+":"+wantTag) {
		t.Errorf("base 51-deployment-watcher.yaml does not pin %s:%s", lookout, wantTag)
	}

	for _, overlay := range []string{"example", "initcontainer-copy"} {
		path := filepath.Join("deploy", "overlays", overlay, "kustomization.yaml")
		got := kustomizeImageTag(t, path, lookout)
		if got != wantTag {
			t.Errorf("overlay %s pins %s newTag %q, want %q", overlay, lookout, got, wantTag)
		}
	}
}

// kustomizeImageTag returns the newTag pinned for image in a kustomization
// file's images: block ("" if the image or its newTag is absent).
func kustomizeImageTag(t *testing.T, path, image string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	lines := strings.Split(string(body), "\n")
	for i, line := range lines {
		if !strings.Contains(line, "name: "+image) {
			continue
		}
		for j := i + 1; j < len(lines); j++ {
			l := strings.TrimSpace(lines[j])
			if strings.HasPrefix(l, "- name:") {
				break // next image entry; no newTag found for this one
			}
			if strings.HasPrefix(l, "newTag:") {
				return strings.Trim(strings.TrimSpace(strings.TrimPrefix(l, "newTag:")), `"`)
			}
		}
	}
	return ""
}

func sliceContains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// mutatingCLI matches a CLI invocation that a READ-ONLY endpoint cannot
// serve, whatever its tool list turns out to contain: it writes, executes,
// or opens an interactive channel. Membership is decided by the verb, not
// by any claim about which MCP tools exist — that is the whole point, and
// it is why the README's surviving argument against a translation overlay
// needs no live dial to stand up.
//
// `get-credentials` is in the set because it writes a kubeconfig (and is
// inert here anyway, with no kubectl to consume it); `curl -i` is the
// POST probe that runs inside a `kubectl exec`.
var mutatingCLI = regexp.MustCompile(
	`kubectl +(-n +[a-z0-9-]+ +)?(apply|create|delete|patch|edit|scale|autoscale|label|annotate|exec|port-forward|cp|drain|cordon|uncordon|rollout|set|taint)\b` +
		`|gcloud +[a-z-]+ +[a-z-]+ +(update|create|delete|resize|upgrade|get-credentials)\b` +
		`|gcloud +container +backup-restore +[a-z-]+ +create\b` +
		`|gcloud +node-pools +create\b` +
		`|gcloud +iam +service-accounts +add-iam-policy-binding\b` +
		`|curl +-i`)

// publishedCounts are the recipecheck finding counts the README's
// "What does not execute" section and the Astro site's examples page state
// as prose. They are asserted against the live checker below.
//
// Why this test exists: #674 resolved the vendored-content executability
// gap as accept-and-disclose, and a disclosure whose numbers are wrong is
// worse than none. Nine figures across two documents had no guard but the
// recipecheck waiver's own WaiveMinFindings, which are deliberately floors
// (90/68 against 120/68) — they catch a tree going dark, which was #766,
// and by construction cannot catch a re-sync that moves a count. That is
// #766's own lesson ("an assertion whose subject can silently disappear is
// worse than no assertion") reappearing one level up, in prose.
//
// Deliberately NOT guarded: CHANGELOG.md. A changelog entry is a
// point-in-time record that gets frozen into a release section at tag
// time; pinning it would mean editing published release notes whenever the
// snapshot is re-synced.
type findingCounts struct {
	total, upstream, cluster    int
	bash, kubectl, gcloud, curl int
	alert, doubleUnderscore     int
	cli, mutatingCLI            int
}

var publishedCounts = findingCounts{
	total: 188, upstream: 120, cluster: 68,
	bash: 84, kubectl: 59, gcloud: 39, curl: 1,
	alert: 3, doubleUnderscore: 2,
	cli: 99, mutatingCLI: 40,
}

// TestPublishedFindingCountsMatchTheDocs fails when the recipe's prose
// disagrees with the checker it cites (#674).
//
// Two halves, and both matter: the counts have to be true of the tree
// today, AND the documents have to actually carry them. A re-sync of
// upstream/ that moves a number should fail here naming the files to
// update, rather than leaving three documents quietly asserting a
// measurement nobody re-took.
func TestPublishedFindingCountsMatchTheDocs(t *testing.T) {
	// ".." is the examples/ tree — the same root allrecipes_test.go walks,
	// so the Recipe.Name and the relative Finding.File paths match the ones
	// the waiver's globs are written against.
	recipes, err := recipecheck.Discover("..")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	var recipe recipecheck.Recipe
	for _, r := range recipes {
		if r.Name == "kube-platform-agent/.agents" {
			recipe = r
		}
	}
	if recipe.Dir == "" {
		t.Fatalf("recipecheck.Discover did not find kube-platform-agent/.agents in %v", recipes)
	}

	// An empty Policy waives nothing; waiving only marks a finding, so the
	// SET is identical to what allrecipes_test.go sees. Deriving the counts
	// here rather than importing that policy keeps this test independent of
	// the waiver's own bookkeeping.
	findings, err := recipecheck.Check(recipe, recipecheck.Policy{})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}

	var got findingCounts
	for _, f := range findings {
		got.total++
		switch {
		case strings.HasPrefix(f.File, "../upstream/"):
			got.upstream++
		case strings.HasPrefix(f.File, "../cluster/"):
			got.cluster++
		default:
			t.Errorf("finding in an unexpected tree: %s", f)
		}
		switch f.Name {
		case "bash":
			got.bash++
		case "kubectl":
			got.kubectl++
		case "gcloud":
			got.gcloud++
		case "curl":
			got.curl++
		case "alert":
			got.alert++
		case "acme__fleet":
			got.doubleUnderscore++
		default:
			t.Errorf("finding names something the README does not account for: %s", f)
		}
		if f.Name == "kubectl" || f.Name == "gcloud" || f.Name == "curl" {
			got.cli++
			line, readErr := sourceLine(filepath.Join(recipe.Dir, f.File), f.Line)
			if readErr != nil {
				t.Fatalf("read %s:%d: %v", f.File, f.Line, readErr)
			}
			if mutatingCLI.MatchString(line) {
				got.mutatingCLI++
			}
		}
	}

	if got != publishedCounts {
		t.Errorf("recipecheck finding counts moved.\n got: %+v\nwant: %+v\n"+
			"Update publishedCounts here AND the prose in README.md "+
			"(\"What does not execute\") and "+
			"docs/site/src/content/docs/examples/index.md that quotes it.", got, publishedCounts)
	}

	// The documents must carry the numbers, not merely be consistent with
	// them in spirit. Phrases are matched against a whitespace-collapsed
	// copy so that reflowing a paragraph doesn't fail the build; changing a
	// figure does.
	//
	// Occurrences are COUNTED, not merely detected. A figure quoted in n
	// places has to be right in all n: the README states its totals three
	// times (the freeze banner up top, the measurement paragraph, and the
	// line under the table), and presence-matching lets a re-sync fix two
	// of them, go green, and leave the banner — the paragraph most readers
	// actually get to — asserting last quarter's number. An added or
	// dropped mention is a deliberate edit, so failing on it is correct;
	// the fix is to move `want` here in the same commit.
	for _, doc := range []struct {
		path    string
		phrases []phraseCount
	}{
		{
			path: "README.md",
			phrases: []phraseCount{
				{"183 of the 188 findings", 1, "banner (total, shell-gap share)"},
				{"**188 findings** today — 120 in `upstream/skills/`", 1, "total, upstream"},
				{"68 in `cluster/skills/`", 1, "cluster"},
				{"| a ` ```bash ` fence | 84 |", 1, "bash"},
				{"| `kubectl` | 59 |", 1, "kubectl"},
				{"| `gcloud` | 39 |", 1, "gcloud"},
				{"| `curl` | 1 |", 1, "curl"},
				{"| `alert` | 3 |", 1, "alert"},
				{"| `acme__fleet` | 2 |", 1, "doubleUnderscore"},
				{"183 of the 188", 2, "total, shell-gap share (banner + under the table)"},
				{"40 of those 99 CLI steps", 1, "mutatingCLI, cli"},
			},
		},
		{
			path: filepath.Join("..", "..", "docs", "site", "src", "content", "docs", "examples", "index.md"),
			phrases: []phraseCount{
				{"183 of the 188 findings", 1, "total, shell-gap share"},
				{"40 of the 99", 1, "mutatingCLI, cli"},
			},
		},
	} {
		// Relative: `go test` runs a test binary with the package's own
		// source directory as its working directory, whatever directory
		// `go test` itself was invoked from. Verified from both the repo
		// root and this package.
		body, err := os.ReadFile(doc.path)
		if err != nil {
			t.Fatalf("read %s: %v", doc.path, err)
		}
		flat := strings.Join(strings.Fields(string(body)), " ")
		for _, p := range doc.phrases {
			got := strings.Count(flat, strings.Join(strings.Fields(p.text), " "))
			switch got {
			case p.want:
			case 0:
				t.Errorf("%s: %q is gone (wanted %d occurrence(s)). Either a "+
					"published figure went stale, or the sentence was reworded — "+
					"if the latter, re-pin the new wording here. It states: %s.",
					doc.path, p.text, p.want, p.pins)
			default:
				t.Errorf("%s: %q appears %d time(s), want %d. A mention was added "+
					"or dropped, or one of several copies of this figure was "+
					"updated and the others were not — every copy has to move "+
					"together. It states: %s.",
					doc.path, p.text, got, p.want, p.pins)
			}
		}
	}
}

// phraseCount is one published figure, and how many places the document is
// expected to say it. `pins` names the publishedCounts field(s) the phrase
// quotes, so a failure says which line of the struct to reconcile.
type phraseCount struct {
	text string
	want int
	pins string
}

// sourceLine returns the 1-indexed line of a file.
func sourceLine(path string, n int) (string, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	lines := strings.Split(string(body), "\n")
	if n < 1 || n > len(lines) {
		return "", fmt.Errorf("line %d out of range (%d lines)", n, len(lines))
	}
	return lines[n-1], nil
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
