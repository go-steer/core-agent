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

package main

import (
	"context"
	"iter"
	"strings"
	"sync"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/pkg/agent"
	"github.com/go-steer/core-agent/v2/pkg/agent/background"
	"github.com/go-steer/core-agent/v2/pkg/compose"
	"github.com/go-steer/core-agent/v2/pkg/models/mock"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

// bgStubTool is a no-op tool with a controlled name, standing in for the
// session's ordinary built-ins.
func bgStubTool(t *testing.T, name string) tool.Tool {
	t.Helper()
	type empty struct{}
	tl, err := functiontool.New(
		functiontool.Config{Name: name, Description: "stub"},
		func(_ tool.Context, _ empty) (empty, error) { return empty{}, nil },
	)
	if err != nil {
		t.Fatalf("functiontool.New(%q): %v", name, err)
	}
	return tl
}

// spawnToolDescription returns the model-facing description of the named
// tool. Since #640 the spawn_agent declaration carries its manager's
// configured-subagent roster, which makes it an observable fingerprint of
// WHICH manager a spawn tool is bound to.
func spawnToolDescription(t *testing.T, tools []tool.Tool, name string) string {
	t.Helper()
	for _, tl := range tools {
		if tl.Name() != name {
			continue
		}
		d, ok := tl.(interface {
			Declaration() *genai.FunctionDeclaration
		})
		if !ok {
			t.Fatalf("tool %q does not expose Declaration()", name)
		}
		decl := d.Declaration()
		if decl == nil {
			t.Fatalf("tool %q has a nil declaration", name)
		}
		return decl.Description
	}
	t.Fatalf("tool %q not found in %v", name, toolNamesOf(tools))
	return ""
}

func toolNamesOf(ts []tool.Tool) []string {
	out := make([]string, 0, len(ts))
	for _, tl := range ts {
		out = append(out, tl.Name())
	}
	return out
}

// testRecipe builds a recipe against a daemon-wide manager whose roster
// is EMPTY, plus the daemon-bound spawn tools that manager produced —
// the exact shape main.go bakes into SessionFactoryDeps.BuiltinTools.
func testRecipe(t *testing.T, templates []background.SubagentTemplate) (sessionBackgroundRecipe, []tool.Tool) {
	t.Helper()
	prov := mock.NewEcho()
	daemonMgr, err := background.NewManager(background.WithProvider(prov, "echo"))
	if err != nil {
		t.Fatalf("NewManager(daemon): %v", err)
	}
	t.Cleanup(func() { _ = daemonMgr.Close() })
	daemonSpawn := background.NewSpawnTools(daemonMgr)
	if len(daemonSpawn) == 0 {
		t.Fatal("NewSpawnTools returned no tools")
	}
	r := sessionBackgroundRecipe{
		provider:       prov,
		smallModelID:   "echo-small",
		syncWait:       time.Minute,
		spawnToolNames: make(map[string]struct{}, len(daemonSpawn)),
		templates:      templates,
		live:           newSessionManagerSet(),
	}
	for _, tl := range daemonSpawn {
		r.spawnToolNames[tl.Name()] = struct{}{}
	}
	return r, daemonSpawn
}

func clusterTemplate() background.SubagentTemplate {
	return background.SubagentTemplate{
		Name:         "cluster",
		Description:  "GKE cluster diagnostics",
		ModelID:      "echo",
		ModelFactory: func(ctx context.Context) (adkmodel.LLM, error) { return mock.NewEcho().Model(ctx, "echo") },
	}
}

// TestSessionBackgroundRecipe_Inert: --no-background-agents leaves the
// recipe zero, and a zero recipe must not wire anything — daemon-created
// sessions then behave exactly as they did before #637.
func TestSessionBackgroundRecipe_Inert(t *testing.T) {
	t.Parallel()
	var r sessionBackgroundRecipe
	if r.factory() != nil {
		t.Error("zero recipe produced a factory; --no-background-agents would still wire subagents")
	}
}

// TestSessionBackgroundRecipe_BindsSpawnToolsToTheSessionManager is the
// host half of the #637 fix. The daemon-wide spawn tools baked into the
// shared builtin list must be swapped for ones bound to the session's own
// manager; otherwise the session's spawns keep routing to the daemon
// manager (wrong parent triple, wrong gate, shared alert queue) while its
// /subagents catalog reports the session's own roster — the two surfaces
// disagreeing again, just in the other direction.
func TestSessionBackgroundRecipe_BindsSpawnToolsToTheSessionManager(t *testing.T) {
	t.Parallel()
	r, daemonSpawn := testRecipe(t, []background.SubagentTemplate{clusterTemplate()})

	// The daemon manager carries no roster, so its spawn_agent
	// description does not mention "cluster".
	if got := spawnToolDescription(t, daemonSpawn, "spawn_agent"); strings.Contains(got, "cluster") {
		t.Fatalf("daemon spawn_agent already lists cluster; the fingerprint this test relies on is invalid")
	}

	sessionTools := append([]tool.Tool{bgStubTool(t, "read_file")}, daemonSpawn...)
	sub, err := r.factory()(compose.SessionScope{
		SessionID: "sid-1",
		Gate:      permissions.New(permissions.Options{}),
		ModelName: "echo",
		Tools:     sessionTools,
	})
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	t.Cleanup(sub.Close)

	if sub.Manager == nil {
		t.Fatal("factory returned no manager")
	}
	if got := len(sub.Tools); got != len(sessionTools) {
		t.Errorf("tool count = %d, want %d (each daemon spawn tool replaced 1:1) — got %v",
			got, len(sessionTools), toolNamesOf(sub.Tools))
	}
	for name := range r.spawnToolNames {
		n := 0
		for _, tl := range sub.Tools {
			if tl.Name() == name {
				n++
			}
		}
		if n != 1 {
			t.Errorf("tool %q appears %d times, want exactly 1 (a duplicate means the daemon-bound one survived)", name, n)
		}
	}
	if got := spawnToolDescription(t, sub.Tools, "spawn_agent"); !strings.Contains(got, "cluster") {
		t.Errorf("session spawn_agent description does not list the session roster — it is still bound to the daemon manager:\n%s", got)
	}
	cat := sub.Manager.ListSubagentCatalog()
	if len(cat) != 1 || cat[0].Name != "cluster" {
		t.Errorf("ListSubagentCatalog() = %+v, want the declarative roster [cluster]", cat)
	}
}

// TestSessionBackgroundRecipe_ManagerPerCall: every session gets its own
// manager, so live-subagent lists, alert queues, and gates never cross.
func TestSessionBackgroundRecipe_ManagerPerCall(t *testing.T) {
	t.Parallel()
	r, daemonSpawn := testRecipe(t, []background.SubagentTemplate{clusterTemplate()})
	factory := r.factory()

	scope := compose.SessionScope{
		Gate:      permissions.New(permissions.Options{}),
		ModelName: "echo",
		Tools:     daemonSpawn,
	}
	scope.SessionID = "sid-a"
	a, err := factory(scope)
	if err != nil {
		t.Fatalf("factory(sid-a): %v", err)
	}
	t.Cleanup(a.Close)
	scope.SessionID = "sid-b"
	b, err := factory(scope)
	if err != nil {
		t.Fatalf("factory(sid-b): %v", err)
	}
	t.Cleanup(b.Close)

	if a.Manager == b.Manager {
		t.Error("two sessions share one background manager")
	}
	if a.Close == nil || b.Close == nil {
		t.Error("factory returned a manager with no Close; its subagent goroutines would outlive the session")
	}
}

// TestSessionBackgroundRecipe_SpawnToolsAreNotSpawnable: the catalog a
// spawn may draw tools from excludes the spawn tools themselves, matching
// the daemon manager (whose catalog was snapshotted before its own spawn
// tools were appended). A subagent that could spawn would sidestep the
// depth cap's intent.
func TestSessionBackgroundRecipe_SpawnToolsAreNotSpawnable(t *testing.T) {
	t.Parallel()
	r, daemonSpawn := testRecipe(t, nil)

	sub, err := r.factory()(compose.SessionScope{
		SessionID: "sid-cat",
		Gate:      permissions.New(permissions.Options{}),
		ModelName: "echo",
		Tools:     append([]tool.Tool{bgStubTool(t, "read_file")}, daemonSpawn...),
	})
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	t.Cleanup(sub.Close)

	mgr, ok := sub.Manager.(*background.Manager)
	if !ok {
		t.Fatalf("manager is %T, want *background.Manager", sub.Manager)
	}
	// Spawning with an explicit tool grant is the only way to observe the
	// catalog: an unknown name is rejected pre-flight.
	_, err = mgr.Spawn(context.Background(), "", background.Spec{
		Name:         "probe",
		SystemPrompt: "probe",
		Goal:         "probe",
		Tools:        []string{"spawn_agent"},
	})
	if err == nil || !strings.Contains(err.Error(), "unknown tool") {
		t.Errorf("Spawn with tools=[spawn_agent] error = %v, want an unknown-tool rejection", err)
	}
	if _, err := mgr.Spawn(context.Background(), "", background.Spec{
		Name:         "probe",
		SystemPrompt: "probe",
		Goal:         "probe",
		Tools:        []string{"read_file"},
	}); err == nil || strings.Contains(err.Error(), "unknown tool") {
		t.Errorf("Spawn with tools=[read_file] error = %v, want the session's ordinary built-ins to be in the catalog", err)
	}
}

// declToolsLLM captures the tool declarations it is offered on its first
// turn, then ends the run by returning plain text. The captured
// spawn_agent description is the fingerprint of WHICH manager that tool
// is bound to: since #640 the declaration renders its own manager's live
// roster.
type declToolsLLM struct {
	mu       sync.Mutex
	spawnDoc string
	sawSpawn bool
}

func (*declToolsLLM) Name() string { return "decl-tools" }

func (l *declToolsLLM) GenerateContent(_ context.Context, req *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	if req != nil && req.Config != nil {
		l.mu.Lock()
		for _, gt := range req.Config.Tools {
			for _, fd := range gt.FunctionDeclarations {
				if fd.Name == "spawn_agent" {
					l.sawSpawn, l.spawnDoc = true, fd.Description
				}
			}
		}
		l.mu.Unlock()
	}
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		content := &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "done"}}}
		yield(&adkmodel.LLMResponse{Content: content, FinishReason: genai.FinishReasonStop, TurnComplete: true}, nil)
	}
}

func (l *declToolsLLM) snapshot() (bool, string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.sawSpawn, l.spawnDoc
}

// TestSessionBackgroundRecipe_TemplateSpawnToolsBindToTheSessionManager
// closes the leak one level below TestSessionBackgroundRecipe_BindsSpawn-
// ToolsToTheSessionManager. That test covers the tools handed to the
// session's PARENT; this one covers the tools a declarative subagent
// carries. buildDeclaredSubagents resolves a subagent's Tools ONCE at
// startup, against a builtin list that already holds the daemon manager's
// spawn tools (main.go appends NewSpawnTools before it builds them), and
// the recipe then registers that one shared roster on every session's own
// manager. Unbound, a session's subagent spawns onto the DAEMON manager:
// daemon-wide gate, the daemon's parent instead of the session's, and
// "[Background reports]" landing in another tenant's turn — the four-way
// breakage the per-session manager exists to prevent, reached one level
// down. Observed in the 2026-08-13 GKE UAT, where a session's bg.cluster-1
// spawned bg.cluster-2 onto the daemon's "default" session.
func TestSessionBackgroundRecipe_TemplateSpawnToolsBindToTheSessionManager(t *testing.T) {
	t.Parallel()
	r, daemonSpawn := testRecipe(t, nil)

	// The daemon manager carries no roster of its own, so "cluster" in a
	// spawn_agent description can only have come from the session manager.
	if got := spawnToolDescription(t, daemonSpawn, "spawn_agent"); strings.Contains(got, "cluster") {
		t.Fatalf("daemon spawn_agent already lists cluster; the fingerprint this test relies on is invalid")
	}

	rec := &declToolsLLM{}
	tmpl := clusterTemplate()
	tmpl.ModelFactory = func(context.Context) (adkmodel.LLM, error) { return rec, nil }
	// The daemon-bound tool surface buildDeclaredSubagents bakes in.
	tmpl.Tools = append([]tool.Tool{bgStubTool(t, "read_file")}, daemonSpawn...)
	shared := []background.SubagentTemplate{tmpl}
	r.templates = shared

	sub, err := r.factory()(compose.SessionScope{
		SessionID: "sid-tmpl",
		// Unattended: an ask-mode gate refuses to run at all.
		Gate:      permissions.New(permissions.Options{Mode: permissions.ModeYolo}),
		ModelName: "echo",
		Tools:     append([]tool.Tool{bgStubTool(t, "read_file")}, daemonSpawn...),
	})
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	t.Cleanup(sub.Close)

	mgr, ok := sub.Manager.(*background.Manager)
	if !ok {
		t.Fatalf("manager is %T, want *background.Manager", sub.Manager)
	}
	// A parent agent gives Spawn a session service to branch from.
	if _, err := agent.New(mustEchoModel(t), agent.WithBackgroundManager(mgr)); err != nil {
		t.Fatalf("agent.New: %v", err)
	}

	h, err := mgr.SpawnTemplate(context.Background(), "", "cluster", background.RefOverrides{Goal: "triage"}, "")
	if err != nil {
		t.Fatalf("SpawnTemplate: %v", err)
	}
	select {
	case <-h.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("subagent goroutine didn't finish")
	}

	sawSpawn, doc := rec.snapshot()
	if !sawSpawn {
		t.Fatalf("the spawned subagent was never offered spawn_agent; this fixture no longer exercises the binding")
	}
	if !strings.Contains(doc, "cluster") {
		t.Errorf("the subagent's spawn_agent carries no session roster — it is still bound to the DAEMON manager:\n%s", doc)
	}

	// The roster is shared across every session, so registration must not
	// write through into it.
	if got := spawnToolDescription(t, shared[0].Tools, "spawn_agent"); strings.Contains(got, "cluster") {
		t.Errorf("registration mutated the shared template's Tools in place; the next session inherits this one's manager:\n%s", got)
	}
}

// TestSessionBackgroundRecipe_CatalogModesMatchTheSessionToolSurface is
// the operator-visible half of the same split. A multi-session session is
// handed the manager (so spawn_agent works) but never the synchronous
// subagent tools — compose.ReproduceAgent does not call
// agent.WithSubagents — so its /subagents listing must report async-only.
// The daemon's own parent, built from the SAME shared roster but with the
// sync tool wired, still reports sync+async. Before this, Catalog
// hardcoded sync+async for every template, so the 2026-08-13 GKE UAT had
// a session advertising a "cluster" tool its /tools listing did not carry
// and its model was never offered (#741).
func TestSessionBackgroundRecipe_CatalogModesMatchTheSessionToolSurface(t *testing.T) {
	t.Parallel()
	r, daemonSpawn := testRecipe(t, []background.SubagentTemplate{clusterTemplate()})

	sub, err := r.factory()(compose.SessionScope{
		SessionID: "sid-modes",
		Gate:      permissions.New(permissions.Options{}),
		ModelName: "echo",
		Tools:     append([]tool.Tool{bgStubTool(t, "read_file")}, daemonSpawn...),
	})
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	t.Cleanup(sub.Close)

	// The session parent's shape, as ReproduceAgent builds it.
	if _, err := agent.New(mustEchoModel(t), agent.WithBackgroundManager(sub.Manager.(*background.Manager))); err != nil {
		t.Fatalf("agent.New(session parent): %v", err)
	}
	cat := sub.Manager.ListSubagentCatalog()
	if len(cat) != 1 || cat[0].Name != "cluster" {
		t.Fatalf("session catalog = %+v, want [cluster]", cat)
	}
	if got := strings.Join(cat[0].Modes, "+"); got != "async" {
		t.Errorf("session cluster modes = %q, want \"async\" — this session's model has no cluster tool to call", got)
	}

	// Same roster, daemon-shaped parent: the sync tool IS wired there.
	daemonMgr, err := background.NewManager(background.WithProvider(mock.NewEcho(), "echo"))
	if err != nil {
		t.Fatalf("NewManager(daemon): %v", err)
	}
	t.Cleanup(func() { _ = daemonMgr.Close() })
	if err := daemonMgr.SetSubagentTemplates([]background.SubagentTemplate{clusterTemplate()}); err != nil {
		t.Fatalf("SetSubagentTemplates: %v", err)
	}
	kid, err := agent.New(mustEchoModel(t), agent.WithName("cluster"))
	if err != nil {
		t.Fatalf("agent.New(cluster): %v", err)
	}
	if _, err := agent.New(mustEchoModel(t),
		agent.WithBackgroundManager(daemonMgr),
		agent.WithSubagents([]*agent.Agent{kid}),
	); err != nil {
		t.Fatalf("agent.New(daemon parent): %v", err)
	}
	dcat := daemonMgr.ListSubagentCatalog()
	if len(dcat) != 1 {
		t.Fatalf("daemon catalog = %+v, want [cluster]", dcat)
	}
	if got := strings.Join(dcat[0].Modes, "+"); got != "sync+async" {
		t.Errorf("daemon cluster modes = %q, want \"sync+async\" — the daemon parent does carry the tool", got)
	}
}

func mustEchoModel(t *testing.T) adkmodel.LLM {
	t.Helper()
	m, err := mock.NewEcho().Model(context.Background(), "echo")
	if err != nil {
		t.Fatalf("mock echo Model: %v", err)
	}
	return m
}

// TestSessionManagerSet_ClosesLiveManagersOnShutdown: the daemon's own
// manager gets a bounded cancel-and-drain on the way out; a session's
// must too, or a SIGTERM tears its in-flight subagents down mid-tool-call
// instead of cancelling them. Eviction deregisters, so the set tracks
// live sessions rather than every session the daemon ever created.
func TestSessionManagerSet_ClosesLiveManagersOnShutdown(t *testing.T) {
	t.Parallel()
	r, daemonSpawn := testRecipe(t, nil)
	factory := r.factory()

	scope := compose.SessionScope{
		Gate:      permissions.New(permissions.Options{}),
		ModelName: "echo",
		Tools:     daemonSpawn,
	}
	scope.SessionID = "sid-live"
	live, err := factory(scope)
	if err != nil {
		t.Fatalf("factory(sid-live): %v", err)
	}
	scope.SessionID = "sid-evicted"
	evicted, err := factory(scope)
	if err != nil {
		t.Fatalf("factory(sid-evicted): %v", err)
	}

	evicted.Close()
	if got := r.live.len(); got != 1 {
		t.Errorf("live set holds %d managers after one eviction, want 1 — evicted sessions leak", got)
	}

	r.live.closeAll()
	if got := r.live.len(); got != 0 {
		t.Errorf("live set holds %d managers after closeAll, want 0", got)
	}
	// Close is idempotent, so an eviction racing shutdown is safe.
	live.Close()

	mgr, ok := live.Manager.(*background.Manager)
	if !ok {
		t.Fatalf("manager is %T, want *background.Manager", live.Manager)
	}
	if _, err := mgr.Spawn(context.Background(), "", background.Spec{
		Name:         "post-shutdown",
		SystemPrompt: "probe",
		Goal:         "probe",
	}); err == nil {
		t.Error("Spawn succeeded on a manager the shutdown drain should have closed")
	}
}
