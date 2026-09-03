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

// The plan surface has to report what actually happened (#747): which
// tools the plan unblocked, who wrote each artifact, and whose plan
// /replan archives. All three were found in the 2026-08-14 GKE UAT,
// where a parent and its declarative subagent each recorded a plan in
// one incident under a recipe that had disabled every mutating tool.

package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	adkagent "google.golang.org/adk/agent"
	"google.golang.org/adk/memory"
	"google.golang.org/adk/session"
	"google.golang.org/adk/tool/toolconfirmation"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

// planToolCtx is a tool.Context that reports an agent and a session,
// which is the whole point: the handler now reads both. Full-interface
// satisfaction is deliberate — an ADK bump that adds a method should
// break the stub rather than silently drift.
type planToolCtx struct {
	context.Context
	agent   string
	session string
	// invocation is the per-turn ID the repeat guard keys off (#906).
	// Empty means "the one turn these tests don't care about", which
	// keeps every pre-#906 fixture reading the same.
	invocation string
}

func (c *planToolCtx) UserContent() *genai.Content { return nil }
func (c *planToolCtx) InvocationID() string {
	if c.invocation == "" {
		return "test-invocation"
	}
	return c.invocation
}
func (c *planToolCtx) AgentName() string                    { return c.agent }
func (c *planToolCtx) ReadonlyState() session.ReadonlyState { return nil }
func (c *planToolCtx) UserID() string                       { return "test-user" }
func (c *planToolCtx) AppName() string                      { return "test-app" }
func (c *planToolCtx) SessionID() string                    { return c.session }
func (c *planToolCtx) Branch() string                       { return "" }
func (c *planToolCtx) Artifacts() adkagent.Artifacts        { return nil }
func (c *planToolCtx) State() session.State                 { return nil }
func (c *planToolCtx) FunctionCallID() string               { return "call-1" }
func (c *planToolCtx) Actions() *session.EventActions       { return &session.EventActions{} }
func (c *planToolCtx) ToolConfirmation() *toolconfirmation.ToolConfirmation {
	return nil
}
func (c *planToolCtx) RequestConfirmation(string, any) error { return nil }
func (c *planToolCtx) SearchMemory(context.Context, string) (*memory.SearchResponse, error) {
	return nil, nil
}

// recordPlanAs runs the handler with an attributed context, the way a
// real invocation reaches it.
func recordPlanAs(t *testing.T, gate *permissions.Gate, agentsDir, agent, sessionID, plan string) recordPlanResult {
	t.Helper()
	fn := recordPlanFunc(gate, agentsDir)
	ctx := &planToolCtx{Context: context.Background(), agent: agent, session: sessionID}
	res, err := fn(ctx, recordPlanArgs{Plan: plan})
	if err != nil {
		t.Fatalf("record_plan as %s: %v", agent, err)
	}
	return res
}

// ---------------------------------------------------------------
// Defect 1: the result names the gate it actually armed.
// ---------------------------------------------------------------

// The UAT frame exactly: plan_mode=required, every mutating built-in
// disabled by the recipe, and one MCP server whose read surface is
// what plan-first was really holding back. Pre-fix the model was told
// "Mutating tools are now unblocked for this session" — a category the
// build had emptied — and never told about the `gke` toolset at all.
func TestRecordPlanMessage_NamesTheToolsThisBuildGates(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	gate := permissions.New(permissions.Options{Mode: permissions.ModeYolo, RequirePlanArtifact: true})

	b := Default()
	for _, name := range []string{"bash", "write_file", "edit_file", "delete_file"} {
		if err := b.Disable(name); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Build(config.DefaultConfig(), gate, dir, b); err != nil {
		t.Fatal(err)
	}
	// The MCP namespace registers as its own toolset, which is how the
	// gate learns the name planFirstDenial will see for every gke tool.
	gate.RegisterPlanGatedTools("mcp")

	msg := recordPlanAs(t, gate, dir, "core_agent", "s1", "## Goal\nInvestigate.").Message

	if strings.Contains(msg, "Mutating tools are now unblocked") {
		t.Errorf("still asserting the pre-fix category: %q", msg)
	}
	if !strings.Contains(msg, "mcp") {
		t.Errorf("message never names the surface the plan actually unblocked: %q", msg)
	}
	for _, gone := range []string{"bash", "write_file", "edit_file", "delete_file"} {
		if strings.Contains(msg, gone) {
			t.Errorf("message names %q, which this build disabled: %q", gone, msg)
		}
	}
}

// Advisory mode blocks nothing, so it must not claim an unblock. Same
// bug the #215 description split fixed one surface earlier; the result
// path kept the single unconditional string.
func TestRecordPlanMessage_AdvisoryModeClaimsNoUnblock(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	gate := permissions.New(permissions.Options{Mode: permissions.ModeYolo})
	if gate.PlanRequired() {
		t.Fatal("advisory fixture armed the gate")
	}
	if _, err := Build(config.DefaultConfig(), gate, dir, Default()); err != nil {
		t.Fatal(err)
	}

	msg := recordPlanAs(t, gate, dir, "core_agent", "s1", "## Goal\nAudit.").Message

	if strings.Contains(msg, "unblocked") {
		t.Errorf("advisory mode claims an unblock: %q", msg)
	}
	if !strings.Contains(msg, "advisory") {
		t.Errorf("advisory mode never says so: %q", msg)
	}
}

// A build that registered nothing plan-gated says that plainly rather
// than naming a set. The mirror of ActiveSearchBinaries' contract.
func TestRecordPlanMessage_EmptyGatedSetSaysNothingWasBlocked(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	gate := permissions.New(permissions.Options{Mode: permissions.ModeYolo, RequirePlanArtifact: true})
	gate.RegisterPlanGatedTools() // host spoke; nothing plan-gated

	msg := recordPlanAs(t, gate, dir, "core_agent", "s1", "plan").Message

	if !strings.Contains(msg, "no plan-gated tools") {
		t.Errorf("empty gated set is not reported: %q", msg)
	}
}

// A host that never told the gate anything gets prose that declines to
// enumerate. "Unknown" must not collapse into "none" — that would be
// the same unenforceable claim pointed the other way.
func TestRecordPlanMessage_UnknownCatalogDoesNotEnumerate(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	gate := permissions.New(permissions.Options{Mode: permissions.ModeYolo, RequirePlanArtifact: true})
	if _, known := gate.PlanGatedTools(); known {
		t.Fatal("fixture: a fresh gate should not claim to know its catalog")
	}

	msg := recordPlanAs(t, gate, dir, "core_agent", "s1", "plan").Message

	if strings.Contains(msg, "no plan-gated tools") {
		t.Errorf("unknown catalog reported as an empty one: %q", msg)
	}
	if !strings.Contains(msg, "unblocked") {
		t.Errorf("required mode should still report an unblock: %q", msg)
	}
}

// The gate owns the exempt filter, so a host can hand it the whole
// catalog without knowing which names plan-first actually denies.
func TestRegisterPlanGatedTools_DropsExemptNames(t *testing.T) {
	t.Parallel()
	gate := permissions.New(permissions.Options{RequirePlanArtifact: true})
	gate.RegisterPlanGatedTools("read_file", "record_plan", "skill", "grep", "fetch_url")

	got, known := gate.PlanGatedTools()
	if !known {
		t.Fatal("registration did not mark the catalog known")
	}
	if len(got) != 1 || got[0] != "fetch_url" {
		t.Errorf("plan-gated set = %v, want [fetch_url]", got)
	}
}

// Sessions created through POST /sessions derive from the template, so
// the catalog has to survive derivation or record_plan goes vague in
// exactly the deployments where someone else reads the artifact.
func TestPlanGatedTools_SurvivesSessionDerivation(t *testing.T) {
	t.Parallel()
	template := permissions.New(permissions.Options{RequirePlanArtifact: true})
	template.RegisterPlanGatedTools("bash", "mcp")

	got, known := template.DeriveForSession("s-1", nil).PlanGatedTools()
	if !known {
		t.Fatal("derived sub-gate lost the catalog")
	}
	if strings.Join(got, ",") != "bash,mcp" {
		t.Errorf("derived set = %v, want [bash mcp]", got)
	}
}

// ---------------------------------------------------------------
// Defect 2: the artifact says who wrote it.
// ---------------------------------------------------------------

func TestRecordPlan_ArtifactCarriesItsAuthor(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	gate := permissions.New(permissions.Options{Mode: permissions.ModeYolo, RequirePlanArtifact: true})

	res := recordPlanAs(t, gate, dir, "cluster", "sess-42", "## Goal\nDiagnose the image pull failure.")

	raw, err := os.ReadFile(res.Path)
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	if !strings.HasPrefix(body, "---\n") {
		t.Fatalf("artifact has no frontmatter — nothing on disk says who wrote it:\n%s", body)
	}
	agent, sessionID := parsePlanFrontmatter(raw)
	if agent != "cluster" || sessionID != "sess-42" {
		t.Errorf("frontmatter = agent %q session %q, want cluster/sess-42:\n%s", agent, sessionID, body)
	}
	if !strings.Contains(body, "## Goal") {
		t.Errorf("frontmatter ate the plan body:\n%s", body)
	}
	// The model's own transcript should carry the same attribution.
	if res.Agent != "cluster" || res.Session != "sess-42" {
		t.Errorf("result = agent %q session %q, want cluster/sess-42", res.Agent, res.Session)
	}
}

// A handler driven without an invocation context (library callers, the
// existing unit tests) must not emit half-written frontmatter.
func TestRecordPlan_NoContextWritesNoAttribution(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	gate := permissions.New(permissions.Options{Mode: permissions.ModeYolo, RequirePlanArtifact: true})

	res, err := invokeRecordPlan(t, gate, dir, "plan body")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(res.Path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "agent:") || strings.Contains(string(raw), "session:") {
		t.Errorf("emitted empty attribution keys:\n%s", raw)
	}
	if a, s := parsePlanFrontmatter(raw); a != "" || s != "" {
		t.Errorf("parsed attribution %q/%q from an unattributed plan", a, s)
	}
}

// Agent names come from operator config, so the frontmatter has to
// survive a value that would break a bare YAML scalar.
func TestPlanFrontmatter_QuotesAwkwardNames(t *testing.T) {
	t.Parallel()
	fm := planFrontmatter(3, PlanOwner{Agent: "team: platform", Session: "s#1"})
	agent, sessionID := parsePlanFrontmatter([]byte(fm + "body\n"))
	if agent != "team: platform" || sessionID != "s#1" {
		t.Errorf("round-trip = %q/%q, want %q/%q:\n%s", agent, sessionID, "team: platform", "s#1", fm)
	}
}

// ---------------------------------------------------------------
// Defect 3: /replan archives the operator's plan, not the newest.
// ---------------------------------------------------------------

// The UAT shape: the parent plans, then its subagent plans. Pre-fix
// /replan archived plan-2 — the specialist's investigation notes — and
// left the parent's delegation plan, the one the operator was
// rejecting, active on disk.
func TestRevokePlanBy_ArchivesTheParentsPlanNotTheSubagents(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	gate := permissions.New(permissions.Options{Mode: permissions.ModeYolo, RequirePlanArtifact: true})

	parent := recordPlanAs(t, gate, dir, "core_agent", "s1", "## Goal\nDelegate to the cluster specialist.")
	sub := recordPlanAs(t, gate, dir, "cluster", "s1", "## Goal\nRead the pod events.")
	if filepath.Base(parent.Path) != "plan-1.md" || filepath.Base(sub.Path) != "plan-2.md" {
		t.Fatalf("fixture: got %s then %s", parent.Path, sub.Path)
	}

	archived, err := RevokePlanBy(gate, dir, PlanOwner{Agent: "core_agent", Session: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(archived, "plan-1-revoked.md") {
		t.Errorf("archived %s, want plan-1-revoked.md — /replan took the subagent's plan", archived)
	}
	if _, err := os.Stat(sub.Path); err != nil {
		t.Errorf("the subagent's plan was disturbed: %v", err)
	}
	if gate.IsPlanRecorded() {
		t.Error("gate flag should be clear after revoke")
	}
}

// Only the subagent planned. Archiving its notes because they are the
// only thing on disk is the same wrong guess; the gate flag still
// clears, so the parent is still forced through record_plan.
func TestRevokePlanBy_LeavesAnotherAgentsPlanAlone(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	gate := permissions.New(permissions.Options{Mode: permissions.ModeYolo, RequirePlanArtifact: true})

	sub := recordPlanAs(t, gate, dir, "cluster", "s1", "## Goal\nRead the pod events.")

	archived, err := RevokePlanBy(gate, dir, PlanOwner{Agent: "core_agent", Session: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if archived != "" {
		t.Errorf("archived %s, which core_agent did not write", archived)
	}
	if _, err := os.Stat(sub.Path); err != nil {
		t.Errorf("the subagent's plan was archived anyway: %v", err)
	}
	if gate.IsPlanRecorded() {
		t.Error("gate flag should clear even when there was nothing to archive")
	}
}

// A plans directory written before frontmatter existed still revokes,
// or an upgrade mid-incident would read as "you have no plan".
func TestRevokePlanBy_FallsBackForUnattributedPlans(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	plansDir := filepath.Join(dir, recordPlanDir)
	if err := os.MkdirAll(plansDir, 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(plansDir, "plan-1.md")
	if err := os.WriteFile(legacy, []byte("## Goal\nOld plan.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gate := permissions.New(permissions.Options{RequirePlanArtifact: true})

	archived, err := RevokePlanBy(gate, dir, PlanOwner{Agent: "core_agent", Session: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(archived, "plan-1-revoked.md") {
		t.Errorf("archived %q, want the pre-frontmatter plan", archived)
	}
}

// The un-scoped spelling is load-bearing for library callers, so its
// newest-wins behavior has to be exactly what it always was.
func TestRevokeLatestPlan_ZeroOwnerStillTakesTheNewest(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	gate := permissions.New(permissions.Options{Mode: permissions.ModeYolo, RequirePlanArtifact: true})

	recordPlanAs(t, gate, dir, "core_agent", "s1", "first")
	recordPlanAs(t, gate, dir, "cluster", "s1", "second")

	archived, err := RevokeLatestPlan(gate, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(archived, "plan-2-revoked.md") {
		t.Errorf("archived %q, want plan-2-revoked.md", archived)
	}
}

// Concurrent tenants interleave into one process-global sequence, so
// the session has to discriminate when the agent name does not.
func TestRevokePlanBy_SeparatesConcurrentSessions(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	gate := permissions.New(permissions.Options{Mode: permissions.ModeYolo, RequirePlanArtifact: true})

	mine := recordPlanAs(t, gate, dir, "core_agent", "tenant-a", "plan A")
	theirs := recordPlanAs(t, gate, dir, "core_agent", "tenant-b", "plan B")

	archived, err := RevokePlanBy(gate, dir, PlanOwner{Agent: "core_agent", Session: "tenant-a"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(archived, filepath.Base(mine.Path[:len(mine.Path)-len(".md")])+"-revoked.md") {
		t.Errorf("archived %q, want tenant-a's %q", archived, mine.Path)
	}
	if _, err := os.Stat(theirs.Path); err != nil {
		t.Errorf("tenant-b's plan was archived: %v", err)
	}
}

// ActivePlans is the surface a host renders "who planned what" from.
func TestActivePlans_ReportsAttributionNewestFirst(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	gate := permissions.New(permissions.Options{Mode: permissions.ModeYolo, RequirePlanArtifact: true})
	recordPlanAs(t, gate, dir, "core_agent", "s1", "first")
	recordPlanAs(t, gate, dir, "cluster", "s1", "second")

	got := ActivePlans(dir)
	if len(got) != 2 {
		t.Fatalf("active plans = %+v, want two", got)
	}
	if got[0].Sequence != 2 || got[0].Agent != "cluster" {
		t.Errorf("newest = %+v, want seq 2 by cluster", got[0])
	}
	if got[1].Sequence != 1 || got[1].Agent != "core_agent" {
		t.Errorf("oldest = %+v, want seq 1 by core_agent", got[1])
	}
}

// ---------------------------------------------------------------
// The wiring that makes the message true in the real binary.
// ---------------------------------------------------------------

// Build is the only thing that knows which built-ins were registered,
// so it is the only thing that can tell the gate. A disabled tool must
// not appear, or the message goes back to naming an empty category.
func TestBuild_TellsTheGateWhatItRegistered(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	gate := permissions.New(permissions.Options{Mode: permissions.ModeYolo, RequirePlanArtifact: true})

	b := Default()
	if err := b.Disable("bash"); err != nil {
		t.Fatal(err)
	}
	if _, err := Build(config.DefaultConfig(), gate, dir, b); err != nil {
		t.Fatal(err)
	}

	got, known := gate.PlanGatedTools()
	if !known {
		t.Fatal("Build never told the gate its catalog")
	}
	set := map[string]bool{}
	for _, n := range got {
		set[n] = true
	}
	if set["bash"] {
		t.Errorf("disabled bash reported as plan-gated: %v", got)
	}
	if !set["write_file"] {
		t.Errorf("registered write_file missing from the plan-gated set: %v", got)
	}
	if set["read_file"] || set["record_plan"] {
		t.Errorf("exempt tools leaked into the plan-gated set: %v", got)
	}
}

// A namespaced toolset is checked under its namespace, so that is the
// name the gate has to learn — otherwise the entire MCP surface stays
// invisible to the message.
func TestGateToolset_RegistersItsNamespace(t *testing.T) {
	t.Parallel()
	gate := permissions.New(permissions.Options{RequirePlanArtifact: true})
	GateToolset(&stubToolset{name: "gke"}, gate, "mcp")
	GateToolset(&stubToolset{name: "skills"}, gate, "skill")

	got, known := gate.PlanGatedTools()
	if !known {
		t.Fatal("GateToolset never registered a namespace")
	}
	if len(got) != 1 || got[0] != "mcp" {
		t.Errorf("plan-gated set = %v, want [mcp] (skill is exempt)", got)
	}
}
