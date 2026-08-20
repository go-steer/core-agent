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

package background

import (
	"context"
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

// The acceptance tests for #758: the act of spawning a subagent goes
// through the parent's permission gate, not just what the subagent
// goes on to do once it exists.
//
// Every case here passes on pre-fix code in the sense that the spawn
// succeeds — which is the bug. What fails pre-fix is the assertion
// that a gate the operator armed had any effect at all.
//
// Modes are ModeYolo throughout so the tests exercise plan-first and
// the deny policy without needing a prompter: both are consulted
// before the mode is, which is itself the property under test for the
// plan-first cases (an operator who set plan_mode=required meant it
// even in yolo).

func gatedTemplateManager(t *testing.T, g *permissions.Gate) *Manager {
	t.Helper()
	prov := &recordingProvider{llm: &stopRaceLLM{}}
	mgr := newTemplateManager(t, prov, []SubagentTemplate{
		{Name: "cluster", ModelFactory: tmplFactory(prov, "m"), ModelID: "m"},
		{Name: "reviewer", ModelFactory: tmplFactory(prov, "m"), ModelID: "m"},
	}, WithGate(g), WithAllowAdhoc(true))
	attachEchoParent(t, mgr)
	t.Cleanup(func() { _ = mgr.Close() })
	return mgr
}

func spawnVia(t *testing.T, mgr *Manager, args map[string]any) map[string]any {
	t.Helper()
	return runToolJSON(t, NewSpawnAgentTool(mgr), context.Background(), args)
}

// TestSpawnAgent_PlanFirstDeniesUntilPlanRecorded is the issue's
// headline: under plan_mode=required, record_plan's own description
// tells the model spawn_agent will be denied before a plan exists.
// Pre-fix that was a claim about nothing.
func TestSpawnAgent_PlanFirstDeniesUntilPlanRecorded(t *testing.T) {
	t.Parallel()
	g := permissions.New(permissions.Options{
		Mode:                permissions.ModeYolo,
		RequirePlanArtifact: true,
	})
	mgr := gatedTemplateManager(t, g)

	resp := spawnVia(t, mgr, map[string]any{"agent": "cluster", "goal": "triage the crashloop"})
	got := errorText(t, resp)
	if !strings.Contains(got, "plan-first mode requires record_plan") {
		t.Fatalf("spawn before record_plan: error = %q, want the plan-first denial — "+
			"the gate the operator armed did not see the spawn", got)
	}
	if _, ok := mgr.Get("cluster"); ok {
		t.Fatal("a denied spawn registered a subagent: the refusal has to happen before the launch, not after it")
	}

	// The escape valve works: once record_plan has flipped the flag,
	// the same call goes through.
	g.MarkPlanRecorded()
	resp = spawnVia(t, mgr, map[string]any{"agent": "cluster", "goal": "triage the crashloop"})
	if got := errorText(t, resp); got != "" {
		t.Fatalf("spawn after record_plan: error = %q, want none", got)
	}
}

// TestSpawnAgent_DenyPolicyIsPerSubagent pins the gate key: it is the
// subagent being launched, so an operator can withhold one entry from
// the catalog without withholding delegation.
func TestSpawnAgent_DenyPolicyIsPerSubagent(t *testing.T) {
	t.Parallel()
	pol, err := permissions.NewPolicy(nil, []string{"spawn_agent:cluster"})
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	mgr := gatedTemplateManager(t, permissions.New(permissions.Options{
		Mode:   permissions.ModeYolo,
		Policy: pol,
	}))

	resp := spawnVia(t, mgr, map[string]any{"agent": "cluster", "goal": "g"})
	if got := errorText(t, resp); !strings.Contains(got, "denied by config policy") {
		t.Fatalf("denied subagent: error = %q, want the policy denial", got)
	}
	// The refusal names the subagent the operator denied, so the
	// operator surfaces draw it against the right row (#746).
	if name, _ := resp["name"].(string); name != "cluster" {
		t.Fatalf("refusal name = %q, want %q", name, "cluster")
	}

	resp = spawnVia(t, mgr, map[string]any{"agent": "reviewer", "goal": "g"})
	if got := errorText(t, resp); got != "" {
		t.Fatalf("undenied subagent: error = %q, want none — the deny rule leaked past its key", got)
	}
}

// TestSpawnAgent_AdHocKeyIsPrefixed pins the second half of the key
// design. An ad-hoc name is model-chosen, so a rule naming one is
// worthless; the "ad-hoc:" prefix is what makes "no inline personas"
// expressible as policy.
func TestSpawnAgent_AdHocKeyIsPrefixed(t *testing.T) {
	t.Parallel()
	pol, err := permissions.NewPolicy(nil, []string{"spawn_agent:ad-hoc:*"})
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	mgr := gatedTemplateManager(t, permissions.New(permissions.Options{
		Mode:   permissions.ModeYolo,
		Policy: pol,
	}))

	resp := spawnVia(t, mgr, map[string]any{
		"name": "whatever-it-called-itself", "system_prompt": "p", "goal": "g",
	})
	if got := errorText(t, resp); !strings.Contains(got, "denied by config policy") {
		t.Fatalf("ad-hoc spawn: error = %q, want the policy denial — a name the model picked "+
			"must not be able to route around the rule", got)
	}
	// Catalog spawns are untouched by an ad-hoc rule.
	resp = spawnVia(t, mgr, map[string]any{"agent": "cluster", "goal": "g"})
	if got := errorText(t, resp); got != "" {
		t.Fatalf("catalog spawn under an ad-hoc deny: error = %q, want none", got)
	}
}

// TestStopAgent_IsNotGated pins the deliberate exclusion. The prose
// #758 set out to make true said "spawn_agent family"; stopping is
// de-escalation, and a gate that can refuse it turns a runaway
// subagent into one the model is forbidden to halt.
func TestStopAgent_IsNotGated(t *testing.T) {
	t.Parallel()
	pol, err := permissions.NewPolicy(nil, []string{"*"})
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	// Deny everything, and arm plan-first with no plan recorded: two
	// independent reasons a gated tool would refuse.
	g := permissions.New(permissions.Options{
		Mode:                permissions.ModeYolo,
		Policy:              pol,
		RequirePlanArtifact: true,
	})
	mgr := gatedTemplateManager(t, g)

	// Control: the same gate really does refuse the tool that IS gated,
	// so a passing stop below means "not gated" rather than "gate inert".
	if got := errorText(t, spawnVia(t, mgr, map[string]any{"agent": "cluster", "goal": "g"})); got == "" {
		t.Fatal("deny-all policy did not refuse spawn_agent; the rest of this test proves nothing")
	}

	// The subagent to stop is launched through the Go API, which is the
	// host's door and was never gated — only the model-facing tool is.
	h, err := mgr.SpawnTemplate(context.Background(), "", "cluster", RefOverrides{Goal: "g"}, "")
	if err != nil {
		t.Fatalf("SpawnTemplate: %v", err)
	}
	resp := runToolJSON(t, NewStopAgentTool(mgr), context.Background(),
		map[string]any{"name": h.Name})
	if got := errorText(t, resp); got != "" {
		t.Fatalf("stop_agent under a deny-all gate: error = %q, want none — "+
			"cancelling has to stay reachable when everything else is denied", got)
	}
}

// TestSpawnTools_RegisterWithTheGate covers the #747 half: record_plan
// reports the set the gate is actually asked about, so a spawn door
// that gates but never registers would gate silently.
func TestSpawnTools_RegisterWithTheGate(t *testing.T) {
	t.Parallel()
	g := permissions.New(permissions.Options{
		Mode:                permissions.ModeYolo,
		RequirePlanArtifact: true,
	})
	mgr := gatedTemplateManager(t, g)
	_ = NewSpawnTools(mgr)

	names, known := g.PlanGatedTools()
	if !known {
		t.Fatal("PlanGatedTools: known = false after building the spawn tools")
	}
	var sawSpawn, sawStop bool
	for _, n := range names {
		switch n {
		case SpawnAgentToolName:
			sawSpawn = true
		case StopAgentToolName:
			sawStop = true
		}
	}
	if !sawSpawn {
		t.Errorf("plan-gated set = %v, want it to name %q so record_plan can tell the model what its plan unblocked",
			names, SpawnAgentToolName)
	}
	if sawStop {
		t.Errorf("plan-gated set = %v, must not name %q: it is not gated, and naming it "+
			"is the #215 bug in the direction #747 fixed", names, StopAgentToolName)
	}
}

// countingPrompter approves for the session and counts how often it
// was asked.
type countingPrompter struct {
	calls []permissions.PromptRequest
}

func (p *countingPrompter) AskApproval(_ context.Context, req permissions.PromptRequest) (permissions.Decision, error) {
	p.calls = append(p.calls, req)
	return permissions.DecisionAllowSession, nil
}

// TestSpawnAgent_AskModePromptsOncePerSubagent pins the UX change an
// interactive user actually feels: in the default mode, delegating now
// asks. It has to ask per subagent (the key is the subagent, so
// approving `cluster` is not approving the whole catalog) and it has to
// ask only once (a session grant on the same key holds), or the prompt
// becomes the kind of noise operators disable wholesale.
func TestSpawnAgent_AskModePromptsOncePerSubagent(t *testing.T) {
	t.Parallel()
	p := &countingPrompter{}
	mgr := gatedTemplateManager(t, permissions.New(permissions.Options{
		Mode:     permissions.ModeAsk,
		Prompter: p,
	}))

	for i := 0; i < 2; i++ {
		if got := errorText(t, spawnVia(t, mgr, map[string]any{"agent": "cluster", "goal": "g"})); got != "" {
			t.Fatalf("approved spawn %d: error = %q, want none", i, got)
		}
	}
	if len(p.calls) != 1 {
		t.Fatalf("prompts for the same subagent = %d, want 1 — the session grant did not hold", len(p.calls))
	}
	if got := p.calls[0].ToolName; got != SpawnAgentToolName {
		t.Errorf("prompt tool = %q, want %q", got, SpawnAgentToolName)
	}
	if got := p.calls[0].Detail; !strings.Contains(got, "cluster") {
		t.Errorf("prompt detail = %q, want it to name the subagent being launched", got)
	}

	if got := errorText(t, spawnVia(t, mgr, map[string]any{"agent": "reviewer", "goal": "g"})); got != "" {
		t.Fatalf("second subagent: error = %q, want none", got)
	}
	if len(p.calls) != 2 {
		t.Fatalf("prompts after a different subagent = %d, want 2 — approving one entry "+
			"of the catalog must not approve the rest", len(p.calls))
	}
}

// TestSpawnRemoteAgent_IsGatedToo closes the escape hatch. Gating only
// the in-process door would leave a parent one tool call away from the
// same fan-out on a substrate its gate cannot reach at all once the
// work is running.
func TestSpawnRemoteAgent_IsGatedToo(t *testing.T) {
	t.Parallel()
	pol, err := permissions.NewPolicy(nil, []string{"spawn_remote_agent:ad-hoc:*"})
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	mgr := gatedTemplateManager(t, permissions.New(permissions.Options{
		Mode:   permissions.ModeYolo,
		Policy: pol,
	}))
	spawner := &fakeRemoteSpawner{}
	tl, err := NewSpawnRemoteAgentTool(spawner, mgr)
	if err != nil {
		t.Fatalf("NewSpawnRemoteAgentTool: %v", err)
	}

	resp := runToolJSON(t, tl, context.Background(), map[string]any{
		"name": "far-worker", "system_prompt": "p", "goal": "g",
	})
	if got := errorText(t, resp); !strings.Contains(got, "denied by config policy") {
		t.Fatalf("remote spawn: error = %q, want the policy denial", got)
	}
	// The refusal has to precede the launch, not annotate it: a remote
	// agent that started is spending on another machine.
	spawner.mu.Lock()
	launched := len(spawner.handles)
	spawner.mu.Unlock()
	if launched != 0 {
		t.Fatalf("denied remote spawn still launched %d agent(s)", launched)
	}

	names, known := mgr.gate.PlanGatedTools()
	if !known {
		t.Fatal("PlanGatedTools: known = false after building the remote spawn tool")
	}
	var saw bool
	for _, n := range names {
		if n == spawnRemoteAgentToolName {
			saw = true
		}
	}
	if !saw {
		t.Errorf("plan-gated set = %v, want it to name %q", names, spawnRemoteAgentToolName)
	}
}

// TestSpawnAgent_NoGateIsNotADenial: a host that wired no gate has
// nothing else gated either, so refusing here would be the only
// enforcement in a build that asked for none.
func TestSpawnAgent_NoGateIsNotADenial(t *testing.T) {
	t.Parallel()
	mgr := gatedTemplateManager(t, nil)
	if got := errorText(t, spawnVia(t, mgr, map[string]any{"agent": "cluster", "goal": "g"})); got != "" {
		t.Fatalf("ungated build: error = %q, want none", got)
	}
}
