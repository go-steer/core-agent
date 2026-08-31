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
	"errors"
	"slices"
	"testing"
	"time"

	"google.golang.org/adk/tool"

	"github.com/go-steer/core-agent/v2/pkg/agent"
	"github.com/go-steer/core-agent/v2/pkg/attach"
	"github.com/go-steer/core-agent/v2/pkg/auth"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

// stubManager is a minimal agent.SubagentManager. It records the parent
// agent.New stamped on it, which is the property the shared-manager
// design could not deliver: with one daemon-wide manager the LAST
// constructed session wins the back-reference and every session's
// subagents branch off that one session's triple.
type stubManager struct {
	catalog []attach.SubagentCatalogInfo
	parent  *agent.Agent
}

func (m *stubManager) AttachParent(a *agent.Agent)               { m.parent = a }
func (m *stubManager) PrependPendingAlerts(prompt string) string { return prompt }
func (m *stubManager) ListSubagents() []attach.AgentInfo         { return nil }
func (m *stubManager) ListSubagentCatalog() []attach.SubagentCatalogInfo {
	return m.catalog
}

func (m *stubManager) SpawnSubagent(context.Context, attach.SubagentSpec) (attach.SubagentSpawnResponse, error) {
	return attach.SubagentSpawnResponse{}, errors.New("stubManager: not spawnable")
}

func (m *stubManager) StopSubagent(string) (bool, error) { return false, nil }

// attachToolNames projects the operator-facing tool list down to names,
// so a tool-replacement assertion reads off the same surface an operator
// would see on GET /sessions/<sid>/tools.
func attachToolNames(infos []attach.ToolInfo) []string {
	out := make([]string, 0, len(infos))
	for _, i := range infos {
		out = append(out, i.Name)
	}
	return out
}

// TestReproduceAgent_WiresPerSessionSubagents is the regression gate for
// #637: a session built by the multi-session factory reported an empty
// operator /subagents catalog and advertised no "subagent" capability,
// because ReproduceAgent wired no BackgroundManager at all — even though
// the daemon-wide spawn tools baked into BuiltinTools let the model
// delegate by reference. The catalog and the delegation surface
// disagreed about what the session could do.
func TestReproduceAgent_WiresPerSessionSubagents(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	mgr := &stubManager{catalog: []attach.SubagentCatalogInfo{{Name: "cluster", Description: "GKE diagnostics"}}}
	deps := SessionFactoryDeps{
		DaemonCtx:    ctx,
		Model:        stubLLM{},
		Template:     permissions.New(permissions.Options{}),
		BuiltinTools: []tool.Tool{stubTool(t, "read_file"), stubTool(t, "spawn_agent")},
		SessionBackground: func(scope SessionScope) (SessionSubagents, error) {
			if scope.Gate == nil {
				t.Errorf("scope.Gate is nil — a session's subagents would run outside its own permission posture")
			}
			if scope.SessionID != "sid-bg" {
				t.Errorf("scope.SessionID = %q, want %q", scope.SessionID, "sid-bg")
			}
			if scope.ModelName != "stub" {
				t.Errorf("scope.ModelName = %q, want %q", scope.ModelName, "stub")
			}
			// The daemon-bound spawn tool is present for the factory to
			// strip; hand back a session-bound replacement.
			if !slices.Contains(toolNames(scope.Tools), "spawn_agent") {
				t.Errorf("scope.Tools = %v, want the daemon-bound spawn_agent to strip", toolNames(scope.Tools))
			}
			return SessionSubagents{
				Manager: mgr,
				Tools:   []tool.Tool{stubTool(t, "read_file"), stubTool(t, "session_spawn_agent")},
				Close:   func() {},
			}, nil
		},
	}

	ad, cancelSess, err := ReproduceAgent(deps, auth.Anonymous, "sid-bg", "created")
	if err != nil {
		t.Fatalf("ReproduceAgent: %v", err)
	}
	t.Cleanup(cancelSess)

	cat := ad.AttachSubagentCatalog()
	if len(cat) != 1 || cat[0].Name != "cluster" {
		t.Errorf("AttachSubagentCatalog() = %+v, want the configured roster [cluster]", cat)
	}
	rep := ad.AttachCapabilities()
	if !rep.Specialists {
		t.Errorf("AttachCapabilities().Specialists = false, want true")
	}
	if !slices.Contains(rep.SlashCommands, "subagent") {
		t.Errorf("AttachCapabilities().SlashCommands = %v, want it to include \"subagent\"", rep.SlashCommands)
	}
	// The parent back-reference must be THIS session's agent: it is what
	// gives a spawned subagent the right (app, user, sid) triple and the
	// right eventlog branch.
	if mgr.parent != ad.Agent() {
		t.Errorf("manager parent = %p, want this session's agent %p", mgr.parent, ad.Agent())
	}
	// The factory's replacement list wins: leaving the daemon-bound
	// spawn tool in place would route this session's spawns back to the
	// shared manager.
	names := attachToolNames(ad.AttachTools())
	if slices.Contains(names, "spawn_agent") || !slices.Contains(names, "session_spawn_agent") {
		t.Errorf("session tools = %v, want the daemon-bound spawn_agent replaced", names)
	}
}

// TestReproduceAgent_AgentAndSubagentsShareOneSessionGate pins the
// link that carries per-session permission isolation into every tool
// call: the sub-gate handed to the session's subagent factory has to
// be the same object handed to agent.New, because agent.Run stamps
// THAT gate onto the turn context and resolveSessionGate reads it back
// off there. Tool wrappers are constructed once at daemon startup
// against the template gate, so this stamp is the only thing that
// routes a tenant's call to that tenant's mode, approvals, prompter
// and plan-first state (#825).
//
// Two sessions must also get two distinct sub-gates, or every symptom
// DeriveForSession exists to prevent comes back with the derivation
// still nominally in place.
func TestReproduceAgent_AgentAndSubagentsShareOneSessionGate(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	template := permissions.New(permissions.Options{})
	var scoped []*permissions.Gate
	deps := SessionFactoryDeps{
		DaemonCtx: ctx,
		Model:     stubLLM{},
		Template:  template,
		SessionBackground: func(scope SessionScope) (SessionSubagents, error) {
			scoped = append(scoped, scope.Gate)
			return SessionSubagents{Manager: &stubManager{}, Close: func() {}}, nil
		},
	}

	adA, cancelA, err := ReproduceAgent(deps, auth.Anonymous, "sid-a", "created")
	if err != nil {
		t.Fatalf("ReproduceAgent(sid-a): %v", err)
	}
	t.Cleanup(cancelA)
	adB, cancelB, err := ReproduceAgent(deps, auth.Anonymous, "sid-b", "created")
	if err != nil {
		t.Fatalf("ReproduceAgent(sid-b): %v", err)
	}
	t.Cleanup(cancelB)

	if len(scoped) != 2 {
		t.Fatalf("SessionBackground called %d times, want 2", len(scoped))
	}
	gateA, gateB := adA.Agent().Gate(), adB.Agent().Gate()
	if gateA == nil || gateB == nil {
		t.Fatal("a session agent was wired with no gate — every tool call would fall back to the daemon template")
	}
	if gateA != scoped[0] || gateB != scoped[1] {
		t.Errorf("agent gate / subagent scope gate disagree (%p vs %p, %p vs %p): a subagent would "+
			"enforce a different session's posture than its parent", gateA, scoped[0], gateB, scoped[1])
	}
	if gateA == gateB {
		t.Error("both sessions share one sub-gate — approvals, mode and plan-first state would cross tenants")
	}
	if gateA == template || gateB == template {
		t.Error("a session was handed the daemon template gate itself, not a derived sub-gate")
	}
}

// TestReproduceAgent_SubagentManagersArePerSession pins the scoping
// decision itself. Two sessions must get two managers, each stamped with
// its OWN parent — the concrete failure a shared daemon-wide manager
// produces, where whichever agent.New ran last owns the back-reference
// for everybody.
func TestReproduceAgent_SubagentManagersArePerSession(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	var built []*stubManager
	deps := SessionFactoryDeps{
		DaemonCtx: ctx,
		Model:     stubLLM{},
		Template:  permissions.New(permissions.Options{}),
		SessionBackground: func(scope SessionScope) (SessionSubagents, error) {
			m := &stubManager{catalog: []attach.SubagentCatalogInfo{{Name: scope.SessionID}}}
			built = append(built, m)
			return SessionSubagents{Manager: m, Close: func() {}}, nil
		},
	}

	adA, cancelA, err := ReproduceAgent(deps, auth.Anonymous, "sid-a", "created")
	if err != nil {
		t.Fatalf("ReproduceAgent(sid-a): %v", err)
	}
	t.Cleanup(cancelA)
	adB, cancelB, err := ReproduceAgent(deps, auth.Anonymous, "sid-b", "created")
	if err != nil {
		t.Fatalf("ReproduceAgent(sid-b): %v", err)
	}
	t.Cleanup(cancelB)

	if len(built) != 2 {
		t.Fatalf("SessionBackground called %d times, want 2", len(built))
	}
	if built[0] == built[1] {
		t.Fatalf("both sessions share one manager — per-session invariant broken")
	}
	if built[0].parent != adA.Agent() || built[1].parent != adB.Agent() {
		t.Errorf("managers stamped with the wrong parents: %p/%p, want %p/%p",
			built[0].parent, built[1].parent, adA.Agent(), adB.Agent())
	}
	if got := adA.AttachSubagentCatalog(); len(got) != 1 || got[0].Name != "sid-a" {
		t.Errorf("sid-a catalog = %+v, want its own roster", got)
	}
	if got := adB.AttachSubagentCatalog(); len(got) != 1 || got[0].Name != "sid-b" {
		t.Errorf("sid-b catalog = %+v, want its own roster", got)
	}
}

// TestReproduceAgent_ClosesSubagentsOnEvict guards the leak: a spawned
// subagent runs under context.WithoutCancel, so nothing but the
// manager's Close terminates it when its session is evicted.
func TestReproduceAgent_ClosesSubagentsOnEvict(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	closed := make(chan struct{})
	deps := SessionFactoryDeps{
		DaemonCtx: ctx,
		Model:     stubLLM{},
		Template:  permissions.New(permissions.Options{}),
		SessionBackground: func(SessionScope) (SessionSubagents, error) {
			return SessionSubagents{
				Manager: &stubManager{},
				Close:   func() { close(closed) },
			}, nil
		},
	}

	_, cancelSess, err := ReproduceAgent(deps, auth.Anonymous, "sid-evict", "created")
	if err != nil {
		t.Fatalf("ReproduceAgent: %v", err)
	}
	cancelSess()

	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("evicting the session did not close its background manager — its subagent goroutines outlive the session")
	}
}

// TestReproduceAgent_SubagentFactoryErrorAborts: a factory that can't
// build the session's subagents fails the whole construction rather than
// silently handing back a session whose advertised delegation surface
// doesn't exist.
func TestReproduceAgent_SubagentFactoryErrorAborts(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	deps := SessionFactoryDeps{
		DaemonCtx: ctx,
		Model:     stubLLM{},
		Template:  permissions.New(permissions.Options{}),
		SessionBackground: func(SessionScope) (SessionSubagents, error) {
			return SessionSubagents{}, errors.New("boom")
		},
	}

	if _, _, err := ReproduceAgent(deps, auth.Anonymous, "sid-err", "created"); err == nil {
		t.Fatal("ReproduceAgent succeeded with a failing SessionBackground, want an error")
	}
}

// TestReproduceAgent_NoSubagentFactory keeps the seam opt-in: a host that
// predates the field (or ran --no-background-agents) gets a session with
// no manager and no "subagent" capability, exactly as before.
func TestReproduceAgent_NoSubagentFactory(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ad, cancelSess, err := ReproduceAgent(SessionFactoryDeps{
		DaemonCtx: ctx,
		Model:     stubLLM{},
		Template:  permissions.New(permissions.Options{}),
	}, auth.Anonymous, "sid-none", "created")
	if err != nil {
		t.Fatalf("ReproduceAgent: %v", err)
	}
	t.Cleanup(cancelSess)

	if ad.Agent().BackgroundManager() != nil {
		t.Errorf("BackgroundManager() != nil with no SessionBackground factory")
	}
	if ad.AttachCapabilities().Specialists {
		t.Errorf("Specialists = true with no manager wired")
	}
}

// declaredSubagent builds the shared inner *agent.Agent a declarative
// subagent is, as cmd/core-agent's buildDeclaredSubagents produces it:
// a name, a model, a persona, and nothing session-shaped. Every
// per-session value the delegation needs — gate, session triple,
// eventlog service, usage tracker — is supplied by the PARENT at the
// point WithSubagents materializes the tool, which is why one inner
// agent can back every tenant without a second set of MCP servers.
func declaredSubagent(t *testing.T, name string) *agent.Agent {
	t.Helper()
	sa, err := agent.New(stubLLM{}, agent.WithName(name), agent.WithInstruction("triage"))
	if err != nil {
		t.Fatalf("agent.New(%q): %v", name, err)
	}
	return sa
}

// subagentTool returns the named tool off an agent's resolved surface.
func subagentTool(t *testing.T, a *agent.Agent, name string) tool.Tool {
	t.Helper()
	for _, tl := range a.Tools() {
		if tl != nil && tl.Name() == name {
			return tl
		}
	}
	t.Fatalf("agent has no %q tool; surface = %v", name, toolNames(a.Tools()))
	return nil
}

// TestReproduceAgent_WiresTheSynchronousSubagentTool is the regression
// gate for #741 part 1.
//
// A declarative subagent is built into two doors: an asynchronous one
// (spawn_agent {agent: "cluster"}, via the manager's templates) and a
// synchronous one (a tool literally named "cluster", via
// agent.WithSubagents). ReproduceAgent wired only the first, so every
// POST /sessions session could reach a declarative subagent by
// reference only while the daemon's own `default` session could call it
// directly — GET /sessions/default/tools listed `cluster`, GET
// /sessions/<sid>/tools did not.
//
// The fix is not the per-session agent rebuild the issue feared. New()
// resolves subagent tools AFTER every option has settled, so handing it
// the shared inner agent yields a tool already bound to this session's
// gate, triple, service and tracker.
func TestReproduceAgent_WiresTheSynchronousSubagentTool(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	deps := SessionFactoryDeps{
		DaemonCtx:    ctx,
		Model:        stubLLM{},
		Template:     permissions.New(permissions.Options{}),
		BuiltinTools: []tool.Tool{stubTool(t, "read_file")},
		Subagents:    []*agent.Agent{declaredSubagent(t, "cluster")},
	}

	ad, cancelSess, err := ReproduceAgent(deps, auth.Anonymous, "sid-sync", "created")
	if err != nil {
		t.Fatalf("ReproduceAgent: %v", err)
	}
	t.Cleanup(cancelSess)

	infos := ad.AttachTools()
	names := attachToolNames(infos)
	if !slices.Contains(names, "cluster") {
		t.Fatalf("session tools = %v, want the synchronous \"cluster\" subagent tool — "+
			"without it this tenant can only reach the subagent by reference, while the "+
			"daemon's own session can call it directly", names)
	}
	// GET /tools classifies it as a subagent rather than an anonymous
	// "other", which is what lets an operator tell `cluster` apart from a
	// built-in of the same name.
	for _, i := range infos {
		if i.Name == "cluster" && i.Source != attach.ToolSourceSubagent {
			t.Errorf("cluster tool source = %q, want %q", i.Source, attach.ToolSourceSubagent)
		}
	}
	// SubagentNames is not decoration: Manager.Catalog's syncSubagentNames
	// reads it to decide whether GET /sessions/<sid>/subagents may claim
	// "sync" for a template (#743). Wiring the tool without it would ship
	// the part-2 disagreement inverted — a callable tool the operator
	// catalog reports as async-only.
	if got := ad.Agent().SubagentNames(); !slices.Contains(got, "cluster") {
		t.Errorf("SubagentNames() = %v, want [cluster] — /subagents would under-report the "+
			"sync door that /tools now carries", got)
	}
}

// TestReproduceAgent_SubagentToolIsBoundPerSession pins the property
// that makes sharing one inner agent across tenants safe. The inner
// *agent.Agent is shared deliberately — rebuilding it per session would
// multiply MCP server processes by session count, which is why #741
// read as an architectural change. What must NOT be shared is the tool
// wrapping it: it captures the parent's gate, session triple, service
// and tracker at construction, so two sessions handed one tool instance
// would file both tenants' delegated events under one session row and
// bill both to one ledger.
func TestReproduceAgent_SubagentToolIsBoundPerSession(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	shared := declaredSubagent(t, "cluster")
	deps := SessionFactoryDeps{
		DaemonCtx: ctx,
		Model:     stubLLM{},
		Template:  permissions.New(permissions.Options{}),
		Subagents: []*agent.Agent{shared},
	}

	adA, cancelA, err := ReproduceAgent(deps, auth.Anonymous, "sid-a", "created")
	if err != nil {
		t.Fatalf("ReproduceAgent(sid-a): %v", err)
	}
	t.Cleanup(cancelA)
	adB, cancelB, err := ReproduceAgent(deps, auth.Anonymous, "sid-b", "created")
	if err != nil {
		t.Fatalf("ReproduceAgent(sid-b): %v", err)
	}
	t.Cleanup(cancelB)

	toolA, toolB := subagentTool(t, adA.Agent(), "cluster"), subagentTool(t, adB.Agent(), "cluster")
	if toolA == toolB {
		t.Error("both sessions share one subagent tool instance: it carries the parent's gate, " +
			"session triple and tracker, so one tenant's delegation would run under the other's")
	}
	// Both sessions must still reach the SAME inner agent — the whole
	// point of doing this at the tool layer instead of rebuilding.
	if len(deps.Subagents) != 1 || deps.Subagents[0] != shared {
		t.Error("ReproduceAgent mutated deps.Subagents; the inner agent must be shared, not rebuilt")
	}
}
