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

package permissions

import (
	"context"
	"strings"
	"testing"
)

// Plan-first denies mutating tools before record_plan is called,
// even under ModeYolo (the design's headline composition guarantee:
// "yolo + require_plan_artifact" = no actions before plan).
func TestPlanFirst_DeniesMutatingToolsBeforePlanRecorded(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		mode Mode
	}{
		{"ask", ModeAsk},
		{"yolo", ModeYolo},
		{"acceptEdits", ModeAcceptEdits},
	}
	for _, tc := range cases {
		t.Run(string(tc.mode), func(t *testing.T) {
			t.Parallel()
			g := New(Options{Mode: tc.mode, RequirePlanArtifact: true})
			// write_file is the canonical mutating-tool case.
			err := g.CheckFileWrite(context.Background(), "write_file", "/tmp/x")
			if err == nil {
				t.Fatalf("expected plan-first denial, got nil")
			}
			if !strings.Contains(err.Error(), "record_plan") {
				t.Errorf("error should mention record_plan: %v", err)
			}
		})
	}
}

func TestPlanFirst_DeniesBashBeforePlan(t *testing.T) {
	t.Parallel()
	g := New(Options{Mode: ModeYolo, RequirePlanArtifact: true})
	err := g.CheckBash(context.Background(), "git status")
	if err == nil {
		t.Fatal("expected plan-first denial for bash before plan")
	}
	if !strings.Contains(err.Error(), "record_plan") {
		t.Errorf("bash denial should mention record_plan: %v", err)
	}
}

// Read tools are exempt — research must happen BEFORE the plan,
// or the workflow deadlocks.
func TestPlanFirst_AllowsReadTools(t *testing.T) {
	t.Parallel()
	g := New(Options{Mode: ModeYolo, RequirePlanArtifact: true})
	readTools := []string{"read_file", "read_many_files", "stat", "list_dir", "glob", "grep", "json_query", "fetch_url", "todo"}
	for _, name := range readTools {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := g.CheckGeneric(context.Background(), name, "/tmp/x"); err != nil {
				t.Errorf("%s should NOT be plan-gated, got: %v", name, err)
			}
		})
	}
}

// record_plan itself is exempt — the escape valve from plan-first
// gating can't be plan-gated. ModeYolo isolates the plan-first
// pre-check from mode-based prompting (in production, record_plan's
// handler bypasses the gate entirely; the exempt-list entry is
// defensive for downstream callers that might wire it differently).
func TestPlanFirst_AllowsRecordPlanItself(t *testing.T) {
	t.Parallel()
	g := New(Options{Mode: ModeYolo, RequirePlanArtifact: true})
	if err := g.CheckGeneric(context.Background(), "record_plan", "any-key"); err != nil {
		t.Errorf("record_plan should be exempt from plan-first gating, got: %v", err)
	}
}

// Skill introspection is exempt at namespace level — list_skills /
// load_skill / load_skill_resource are all registered through
// GateToolset(ts, gate, "skill") in pkg/skills/load.go, so the gate
// sees "skill" as the tool name for every underlying call. Read-only
// discovery of what the agent has available is a precondition for
// planning, not a mutating action; gating it deadlocks the workflow.
// (Observed live during the v2.6 GKE-troubleshoot demo drive — see
// issue #213.)
func TestPlanFirst_AllowsSkillIntrospection(t *testing.T) {
	t.Parallel()
	g := New(Options{Mode: ModeYolo, RequirePlanArtifact: true})
	if err := g.CheckGeneric(context.Background(), "skill", "list_skills"); err != nil {
		t.Errorf("skill namespace should be exempt from plan-first gating, got: %v", err)
	}
	// Repeat with a load_skill-shaped key to confirm the exemption is
	// namespace-level, not keyed on the human-readable summary.
	if err := g.CheckGeneric(context.Background(), "skill", "load_skill(name=k8s-triage)"); err != nil {
		t.Errorf("skill namespace should be exempt regardless of key content, got: %v", err)
	}
}

// list_agents and check_agent are read-only subagent introspection
// registered directly by pkg/agent/background_tools.go — not via the
// namespace wrapper. Exempt them individually.
func TestPlanFirst_AllowsSubagentIntrospection(t *testing.T) {
	t.Parallel()
	g := New(Options{Mode: ModeYolo, RequirePlanArtifact: true})
	for _, name := range []string{"list_agents", "check_agent"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := g.CheckGeneric(context.Background(), name, "any"); err != nil {
				t.Errorf("%s should be exempt from plan-first gating, got: %v", name, err)
			}
		})
	}
}

// mcp (the namespace) should NOT be exempt — MCP servers can expose
// mutating tools, so plan-first must still apply. This locks in the
// deliberate asymmetry vs the "skill" namespace above.
func TestPlanFirst_DoesNotExemptMCPNamespace(t *testing.T) {
	t.Parallel()
	g := New(Options{Mode: ModeYolo, RequirePlanArtifact: true})
	err := g.CheckGeneric(context.Background(), "mcp", "gke_patch_k8s_resource")
	if err == nil {
		t.Fatal("mcp namespace must be plan-gated (may include mutating tools); got no error")
	}
	if !strings.Contains(err.Error(), "plan-first mode requires record_plan") {
		t.Errorf("expected plan-first denial for mcp namespace, got: %v", err)
	}
}

func TestPlanFirst_UnblocksAfterMarkPlanRecorded(t *testing.T) {
	t.Parallel()
	g := New(Options{Mode: ModeYolo, RequirePlanArtifact: true})
	if err := g.CheckFileWrite(context.Background(), "write_file", "/tmp/x"); err == nil {
		t.Fatal("write_file should be denied before plan")
	}
	g.MarkPlanRecorded()
	if err := g.CheckFileWrite(context.Background(), "write_file", "/tmp/x"); err != nil {
		t.Errorf("write_file should be allowed after MarkPlanRecorded under yolo, got: %v", err)
	}
}

func TestPlanFirst_ClearReGates(t *testing.T) {
	t.Parallel()
	g := New(Options{Mode: ModeYolo, RequirePlanArtifact: true})
	g.MarkPlanRecorded()
	if err := g.CheckFileWrite(context.Background(), "write_file", "/tmp/x"); err != nil {
		t.Fatalf("write_file should be allowed after MarkPlanRecorded: %v", err)
	}
	g.ClearPlanRecorded()
	if err := g.CheckFileWrite(context.Background(), "write_file", "/tmp/x"); err == nil {
		t.Error("write_file should re-deny after ClearPlanRecorded")
	}
}

func TestPlanFirst_DisabledByDefault(t *testing.T) {
	t.Parallel()
	// No RequirePlanArtifact opt-in → plan-first pre-check is a no-op.
	g := New(Options{Mode: ModeYolo})
	if err := g.CheckFileWrite(context.Background(), "write_file", "/tmp/x"); err != nil {
		t.Errorf("without RequirePlanArtifact, write_file should be allowed under yolo: %v", err)
	}
	if g.PlanRequired() {
		t.Error("PlanRequired() should be false when option not set")
	}
}

func TestPlanFirst_OutOfScopeWriteAlsoGated(t *testing.T) {
	t.Parallel()
	// A clever bypass attempt: write to a path outside the project
	// scope so promptForPath is called instead of gateRequest. The
	// plan-first pre-check is duplicated there to plug that hole.
	scope, _ := NewPathScope("/restricted-root", "", nil)
	g := New(Options{Mode: ModeAsk, Scope: scope, RequirePlanArtifact: true})
	err := g.CheckFileWrite(context.Background(), "write_file", "/elsewhere/x")
	if err == nil {
		t.Fatal("expected plan-first denial for out-of-scope write")
	}
	if !strings.Contains(err.Error(), "record_plan") {
		t.Errorf("error should mention record_plan: %v", err)
	}
}

func TestPlanFirst_IsPlanRecordedReportsCurrent(t *testing.T) {
	t.Parallel()
	g := New(Options{RequirePlanArtifact: true})
	if g.IsPlanRecorded() {
		t.Error("fresh gate: IsPlanRecorded should be false")
	}
	g.MarkPlanRecorded()
	if !g.IsPlanRecorded() {
		t.Error("after MarkPlanRecorded: IsPlanRecorded should be true")
	}
	g.ClearPlanRecorded()
	if g.IsPlanRecorded() {
		t.Error("after ClearPlanRecorded: IsPlanRecorded should be false")
	}
}

// MCP-shaped tools (any tool name not in the exempt list) get plan-
// gated by default. This is Q1's resolution: "gate everything by
// default; per-server allowlist later if it bites".
func TestPlanFirst_GatesUnknownToolByName(t *testing.T) {
	t.Parallel()
	g := New(Options{Mode: ModeYolo, RequirePlanArtifact: true})
	mcpishNames := []string{"gke.list_clusters", "linear.get_issue", "custom.do_thing", "spawn_agent"}
	for _, name := range mcpishNames {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := g.CheckGeneric(context.Background(), name, "args"); err == nil {
				t.Errorf("%s should be plan-gated (not in exempt list)", name)
			}
		})
	}
}
