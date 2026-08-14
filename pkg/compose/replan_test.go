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

package compose

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/attach"
	"github.com/go-steer/core-agent/v2/pkg/auth"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

// writeSessionPlan drops an active plan artifact attributed the way
// record_plan attributes one, so RevokePlanBy's owner match runs
// against real frontmatter rather than an anonymous file.
func writeSessionPlan(t *testing.T, agentsDir string, seq int, agent, sessionID string) string {
	t.Helper()
	plansDir := filepath.Join(agentsDir, "plans")
	if err := os.MkdirAll(plansDir, 0o755); err != nil {
		t.Fatalf("mkdir plans: %v", err)
	}
	path := filepath.Join(plansDir, "plan-"+strconv.Itoa(seq)+".md")
	body := "---\nplan: " + strconv.Itoa(seq) + "\nagent: " + strconv.Quote(agent) +
		"\nsession: " + strconv.Quote(sessionID) + "\n---\n\nplan body\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// A hub session must be able to revoke its OWN plan and only its own.
//
// /replan returned 501 "capability not registered" for every
// hub-created session until #763: attachProviderOpts deferred the
// Replanner closure, which was survivable while nothing ran
// plan_mode=required under the hub. The kube-platform recipe does, so
// the plan-first gate could be armed and never revoked by the session
// holding the plan — the live GKE finding this test pins.
//
// The second half is the multi-tenant hazard the single-session path
// can't exhibit: <agentsDir>/plans/ is process-global while the gate is
// per-session, so an owner-blind revoke from one tenant would archive
// another tenant's artifact.
func TestReproduceAgent_ReplanRevokesOnlyTheSessionsOwnPlan(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	project := t.TempDir()
	writeSessionPlan(t, project, 1, "core_agent", "sid-a")
	writeSessionPlan(t, project, 2, "core_agent", "sid-b")

	deps := SessionFactoryDeps{
		DaemonCtx:   ctx,
		Model:       stubLLM{},
		Template:    permissions.New(permissions.Options{RequirePlanArtifact: true}),
		ProjectRoot: project,
		AgentsDir:   project,
	}
	newSession := func(sid string) attachReplanner {
		t.Helper()
		ad, cancelAg, err := ReproduceAgent(deps, auth.Anonymous, sid, "created")
		if err != nil {
			t.Fatalf("ReproduceAgent(%s): %v", sid, err)
		}
		t.Cleanup(cancelAg)
		return ad
	}

	// The capability report is what a TUI reads before offering the
	// command, so an unadvertised /replan is invisible even once wired.
	if !slices.Contains(newSession("sid-caps").AttachCapabilities().SlashCommands, "replan") {
		t.Error("hub session does not advertise the replan slash command")
	}

	// A third tenant with no plan of its own must decline rather than
	// take the newest artifact, and must say whose plan it left alone.
	c := newSession("sid-c")
	resp, err := c.AttachReplan(ctx, attach.ReplanRequest{})
	if err != nil {
		t.Fatalf("sid-c AttachReplan: %v (a 501 here means the hub never wired WithReplanner)", err)
	}
	if resp.PlanWasActive || resp.ArchivedPath != "" {
		t.Fatalf("sid-c archived a plan it did not record: %+v", resp)
	}
	// ActivePlans is newest-first, so the artifact named is plan-2.
	if !strings.Contains(resp.Message, "plan-2.md") || !strings.Contains(resp.Message, "sid-b") {
		t.Errorf("decline message does not report the artifact it left alone, with attribution: %q", resp.Message)
	}

	// The owning session revokes its own plan — not the highest
	// sequence, which belongs to sid-b.
	a := newSession("sid-a")
	resp, err = a.AttachReplan(ctx, attach.ReplanRequest{})
	if err != nil {
		t.Fatalf("sid-a AttachReplan: %v", err)
	}
	if !resp.PlanWasActive || filepath.Base(resp.ArchivedPath) != "plan-1-revoked.md" {
		t.Fatalf("sid-a revoked the wrong plan: %+v", resp)
	}
	// plan_mode is required, so the message must promise the denial the
	// per-session sub-gate will actually enforce.
	if !strings.Contains(resp.Message, "will be denied") {
		t.Errorf("required-mode /replan did not reach the session sub-gate: %q", resp.Message)
	}
	if _, err := os.Stat(filepath.Join(project, "plans", "plan-2.md")); err != nil {
		t.Errorf("sid-a's /replan disturbed sid-b's plan: %v", err)
	}

	b := newSession("sid-b")
	resp, err = b.AttachReplan(ctx, attach.ReplanRequest{})
	if err != nil {
		t.Fatalf("sid-b AttachReplan: %v", err)
	}
	if filepath.Base(resp.ArchivedPath) != "plan-2-revoked.md" {
		t.Fatalf("sid-b revoked the wrong plan: %+v", resp)
	}
}

// attachReplanner is the slice of the adapter this test drives.
type attachReplanner interface {
	AttachReplan(context.Context, attach.ReplanRequest) (attach.ReplanResponse, error)
	AttachCapabilities() attach.CapabilityReport
}
