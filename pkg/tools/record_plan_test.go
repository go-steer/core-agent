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

package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/adk/tool"

	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

func newCfgWithPlanFirst(on bool) *config.Config {
	cfg := config.DefaultConfig()
	cfg.Permissions.RequirePlanArtifact = on
	return cfg
}

func hasTool(tools []tool.Tool, name string) bool {
	for _, t := range tools {
		if t == nil {
			continue
		}
		if t.Name() == name {
			return true
		}
	}
	return false
}

// invokeRecordPlan is a thin helper that runs the record_plan
// function directly (bypassing the tool.Tool wrapper) so tests can
// assert on the returned struct without needing to drive an LLM
// fake. We invoke the bare functiontool.Func[recordPlanArgs,
// recordPlanResult] returned by recordPlanFunc.
func invokeRecordPlan(t *testing.T, gate *permissions.Gate, agentsDir, plan string) (recordPlanResult, error) {
	t.Helper()
	fn := recordPlanFunc(gate, agentsDir)
	// tool.Context's zero value is fine — the handler doesn't touch it.
	return fn(tool.Context(nil), recordPlanArgs{Plan: plan})
}

func TestRecordPlan_WritesArtifactAndFlipsGate(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	gate := permissions.New(permissions.Options{Mode: permissions.ModeYolo, RequirePlanArtifact: true})
	if gate.IsPlanRecorded() {
		t.Fatal("fresh gate: plan should not be recorded")
	}

	res, err := invokeRecordPlan(t, gate, dir, "## Goal\nDo X.")
	if err != nil {
		t.Fatal(err)
	}
	if res.Sequence != 1 {
		t.Errorf("first plan should be seq 1, got %d", res.Sequence)
	}
	if !strings.HasSuffix(res.Path, "plan-1.md") {
		t.Errorf("path should end with plan-1.md, got %s", res.Path)
	}
	if !gate.IsPlanRecorded() {
		t.Error("gate should be flipped after record_plan")
	}
	body, err := os.ReadFile(res.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "## Goal") {
		t.Errorf("artifact missing plan content: %q", body)
	}
	// POSIX-clean trailing newline.
	if !strings.HasSuffix(string(body), "\n") {
		t.Errorf("artifact should end with newline, got %q", body)
	}
}

// TestMarkPlanRecorded_RoutesThroughSessionGate is the regression
// test for #214. Before the fix, recordPlanFunc called
// `gate.MarkPlanRecorded()` on the closure-captured TEMPLATE gate,
// but every permission check goes through the per-SESSION sub-gate
// (via resolveSessionGate(ctx) in pkg/permissions/gate.go). The two
// have independent planRecorded fields, so record_plan wrote plan
// files but the guard never saw the flip — infinite loop.
//
// This test exercises both paths:
//   - ctx carries a session sub-gate → flip lands on the session
//     sub-gate (template stays false)
//   - ctx has no session sub-gate → fall back to template
func TestMarkPlanRecorded_RoutesThroughSessionGate(t *testing.T) {
	t.Parallel()

	t.Run("session gate on ctx: session flipped, template untouched", func(t *testing.T) {
		t.Parallel()
		template := permissions.New(permissions.Options{RequirePlanArtifact: true})
		session := template.DeriveForSession("sid-123", nil)

		if template.IsPlanRecorded() {
			t.Fatal("template starts unrecorded")
		}
		if session.IsPlanRecorded() {
			t.Fatal("session starts unrecorded")
		}

		ctx := permissions.WithSessionGate(context.Background(), session)
		markPlanRecorded(ctx, template)

		if !session.IsPlanRecorded() {
			t.Error("session gate: MarkPlanRecorded should have flipped session sub-gate")
		}
		if template.IsPlanRecorded() {
			t.Error("session gate: template must NOT be flipped when session gate is present (multi-session isolation)")
		}
	})

	t.Run("no session gate on ctx: template flipped (fallback path)", func(t *testing.T) {
		t.Parallel()
		template := permissions.New(permissions.Options{RequirePlanArtifact: true})

		markPlanRecorded(context.Background(), template)

		if !template.IsPlanRecorded() {
			t.Error("no session gate: MarkPlanRecorded should have flipped template gate")
		}
	})

	t.Run("empty ctx: template flipped (single-user path)", func(t *testing.T) {
		t.Parallel()
		template := permissions.New(permissions.Options{RequirePlanArtifact: true})

		// context.TODO() carries no session gate — falls through to
		// the template. Mirrors the shape existing tests use via
		// tool.Context(nil), except linter-safe (SA1012 rejects
		// bare nil contexts). SessionGateFromContext handles the
		// nil-context case internally at pkg/permissions/session_context.go
		// so the recordPlanFunc handler stays safe regardless.
		markPlanRecorded(context.TODO(), template)

		if !template.IsPlanRecorded() {
			t.Error("empty ctx: MarkPlanRecorded should have flipped template gate")
		}
	})

	t.Run("nil template + no session gate: no panic", func(t *testing.T) {
		t.Parallel()
		// Defensive coverage — the recordPlanFunc constructor rejects
		// a nil gate at RecordPlan(), so this path shouldn't be hit
		// in production. But the helper must not panic if it ever is
		// (e.g., a future refactor). Just verifies no panic.
		markPlanRecorded(context.Background(), nil)
	})
}

func TestRecordPlan_RejectsEmptyPlan(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	gate := permissions.New(permissions.Options{RequirePlanArtifact: true})
	cases := []string{"", "   ", "\n\n\t\n"}
	for _, c := range cases {
		_, err := invokeRecordPlan(t, gate, dir, c)
		if err == nil {
			t.Errorf("expected error for empty plan %q, got nil", c)
		}
		if !strings.Contains(err.Error(), "required") {
			t.Errorf("error should mention required: %v", err)
		}
	}
	if gate.IsPlanRecorded() {
		t.Error("gate should NOT flip on empty-plan rejection")
	}
}

func TestRecordPlan_RevisionsIncrementSeq(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	gate := permissions.New(permissions.Options{RequirePlanArtifact: true})

	for i := 1; i <= 3; i++ {
		res, err := invokeRecordPlan(t, gate, dir, "plan rev")
		if err != nil {
			t.Fatal(err)
		}
		if res.Sequence != i {
			t.Errorf("revision %d: got seq %d, want %d", i, res.Sequence, i)
		}
	}
}

func TestRecordPlan_CreatesPlansDirIfMissing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	plansDir := filepath.Join(dir, recordPlanDir)
	if _, err := os.Stat(plansDir); !os.IsNotExist(err) {
		t.Fatalf("expected plansDir to be absent, got %v", err)
	}
	gate := permissions.New(permissions.Options{RequirePlanArtifact: true})
	if _, err := invokeRecordPlan(t, gate, dir, "first plan"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(plansDir); err != nil {
		t.Errorf("plansDir should exist after record_plan: %v", err)
	}
}

func TestRecordPlan_RequiresAgentsDir(t *testing.T) {
	t.Parallel()
	_, err := RecordPlan(permissions.New(permissions.Options{}), "")
	if err == nil {
		t.Fatal("expected error when agentsDir is empty")
	}
	if !strings.Contains(err.Error(), "agentsDir") {
		t.Errorf("error should mention agentsDir: %v", err)
	}
}

func TestRecordPlan_RequiresGate(t *testing.T) {
	t.Parallel()
	_, err := RecordPlan(nil, t.TempDir())
	if err == nil {
		t.Fatal("expected error when gate is nil")
	}
	if !strings.Contains(err.Error(), "gate") {
		t.Errorf("error should mention gate: %v", err)
	}
}

func TestNextPlanSeq_EmptyDirReturnsOne(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	seq, err := nextPlanSeq(filepath.Join(dir, "nonexistent"))
	if err != nil {
		t.Fatal(err)
	}
	if seq != 1 {
		t.Errorf("empty dir should return seq 1, got %d", seq)
	}
}

func TestNextPlanSeq_IgnoresUnrelatedFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Drop a mix of plan and non-plan files into the directory.
	for _, name := range []string{
		"plan-1.md", "plan-2.md", "plan-5.md",
		"plan-3-revoked.md", // archived plan; max-seq considers it
		"README.md",         // unrelated
		"plan.md",           // wrong pattern (no seq)
		"plan-x.md",         // wrong seq (non-numeric)
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	seq, err := nextPlanSeq(dir)
	if err != nil {
		t.Fatal(err)
	}
	if seq != 6 {
		t.Errorf("expected seq 6 (max=5 + 1), got %d", seq)
	}
}

func TestLatestActivePlan_SkipsRevoked(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	plansDir := filepath.Join(dir, recordPlanDir)
	if err := os.MkdirAll(plansDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"plan-1.md", "plan-2.md", "plan-3-revoked.md"} {
		if err := os.WriteFile(filepath.Join(plansDir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	latest := LatestActivePlan(dir)
	if !strings.HasSuffix(latest, "plan-2.md") {
		t.Errorf("latest active should be plan-2.md (plan-3 is revoked), got %s", latest)
	}
}

func TestLatestActivePlan_NoPlansReturnsEmpty(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if latest := LatestActivePlan(dir); latest != "" {
		t.Errorf("empty plans dir should return empty, got %s", latest)
	}
	if latest := LatestActivePlan(""); latest != "" {
		t.Errorf("empty agentsDir should return empty, got %s", latest)
	}
}

func TestRevokeLatestPlan_RenamesAndClearsFlag(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	gate := permissions.New(permissions.Options{RequirePlanArtifact: true})

	if _, err := invokeRecordPlan(t, gate, dir, "plan body"); err != nil {
		t.Fatal(err)
	}
	if !gate.IsPlanRecorded() {
		t.Fatal("gate should be set after record_plan")
	}

	revoked, err := RevokeLatestPlan(gate, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(revoked, "plan-1-revoked.md") {
		t.Errorf("revoked path should end with plan-1-revoked.md, got %s", revoked)
	}
	if gate.IsPlanRecorded() {
		t.Error("gate flag should be cleared after revoke")
	}
	// The original file should no longer exist; the revoked rename
	// should be present.
	plansDir := filepath.Join(dir, recordPlanDir)
	if _, err := os.Stat(filepath.Join(plansDir, "plan-1.md")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("plan-1.md should be gone, got err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(plansDir, "plan-1-revoked.md")); err != nil {
		t.Errorf("plan-1-revoked.md should exist, got err=%v", err)
	}
}

func TestRevokeLatestPlan_NoPlanIsNoOp(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	gate := permissions.New(permissions.Options{RequirePlanArtifact: true})
	// Pretend the flag was set out-of-band.
	gate.MarkPlanRecorded()
	revoked, err := RevokeLatestPlan(gate, dir)
	if err != nil {
		t.Fatal(err)
	}
	if revoked != "" {
		t.Errorf("expected empty revoked path with no plans, got %s", revoked)
	}
	if gate.IsPlanRecorded() {
		t.Error("gate flag should be cleared regardless")
	}
}

func TestRevokeLatestPlan_NextRecordIncrementsPastRevoked(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	gate := permissions.New(permissions.Options{RequirePlanArtifact: true})
	// Record, revoke, record again — second plan should be seq 2,
	// not seq 1 (collision with the revoked file's old seq).
	if _, err := invokeRecordPlan(t, gate, dir, "first"); err != nil {
		t.Fatal(err)
	}
	if _, err := RevokeLatestPlan(gate, dir); err != nil {
		t.Fatal(err)
	}
	res, err := invokeRecordPlan(t, gate, dir, "second")
	if err != nil {
		t.Fatal(err)
	}
	if res.Sequence != 2 {
		t.Errorf("after revoke+rewrite, expected seq 2, got %d", res.Sequence)
	}
}

func TestRecordPlan_BuildRegistersWhenConfigEnables(t *testing.T) {
	t.Parallel()
	// record_plan should appear in the Build registry only when
	// permissions.require_plan_artifact is set AND agentsDir is
	// non-empty. This protects the model from seeing an inert tool.
	dir := t.TempDir()
	gate := permissions.New(permissions.Options{Mode: permissions.ModeYolo, RequirePlanArtifact: true})

	cfgOff := newCfgWithPlanFirst(false)
	cfgOn := newCfgWithPlanFirst(true)

	regOff, err := Build(cfgOff, gate, dir, Default())
	if err != nil {
		t.Fatal(err)
	}
	if hasTool(regOff.Tools, "record_plan") {
		t.Error("record_plan should NOT register when require_plan_artifact is false")
	}

	regOn, err := Build(cfgOn, gate, dir, Default())
	if err != nil {
		t.Fatal(err)
	}
	if !hasTool(regOn.Tools, "record_plan") {
		t.Error("record_plan should register when require_plan_artifact is true")
	}

	regNoDir, err := Build(cfgOn, gate, "", Default())
	if err != nil {
		t.Fatal(err)
	}
	if hasTool(regNoDir.Tools, "record_plan") {
		t.Error("record_plan should NOT register when agentsDir is empty")
	}
}
