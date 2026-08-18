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
