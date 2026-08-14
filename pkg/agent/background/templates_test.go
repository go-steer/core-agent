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
	"errors"
	"iter"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/adk/tool"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/pkg/agent"
	"github.com/go-steer/core-agent/v2/pkg/models/mock"
)

// tmplFactory builds a template ModelFactory that records the asked-for
// model id on prov, mirroring the synchronous declarative builder's
// factory (a captured provider + model name).
func tmplFactory(prov *recordingProvider, id string) func(context.Context) (adkmodel.LLM, error) {
	return func(c context.Context) (adkmodel.LLM, error) { return prov.Model(c, id) }
}

func newTemplateManager(t *testing.T, prov *recordingProvider, templates []SubagentTemplate, opts ...ManagerOption) *Manager {
	t.Helper()
	base := []ManagerOption{WithProvider(prov, "parent-model")}
	mgr, err := NewManager(append(base, opts...)...)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := mgr.SetSubagentTemplates(templates); err != nil {
		t.Fatalf("SetSubagentTemplates: %v", err)
	}
	return mgr
}

func TestSetSubagentTemplates_Validation(t *testing.T) {
	t.Parallel()
	prov := &recordingProvider{llm: &stopRaceLLM{}}
	ok := func() *Manager {
		m, err := NewManager(WithProvider(prov, "parent-model"),
			WithPredefinedSpecs([]Spec{{Name: "catalog", SystemPrompt: "p"}}))
		if err != nil {
			t.Fatalf("NewManager: %v", err)
		}
		return m
	}
	goodFactory := tmplFactory(prov, "m")

	cases := []struct {
		name string
		ts   []SubagentTemplate
	}{
		{"branch-unsafe name", []SubagentTemplate{{Name: "has.dot", ModelFactory: goodFactory}}},
		{"empty name", []SubagentTemplate{{Name: "", ModelFactory: goodFactory}}},
		{"nil factory", []SubagentTemplate{{Name: "cluster"}}},
		{"duplicate names", []SubagentTemplate{
			{Name: "dup", ModelFactory: goodFactory},
			{Name: "dup", ModelFactory: goodFactory},
		}},
		{"collides with predefined", []SubagentTemplate{{Name: "catalog", ModelFactory: goodFactory}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := ok().SetSubagentTemplates(c.ts); err == nil {
				t.Errorf("SetSubagentTemplates(%s): want error, got nil", c.name)
			}
		})
	}

	// A failed call must not mutate the registered set.
	m := ok()
	if err := m.SetSubagentTemplates([]SubagentTemplate{{Name: "cluster", ModelFactory: goodFactory}}); err != nil {
		t.Fatalf("first SetSubagentTemplates: %v", err)
	}
	if err := m.SetSubagentTemplates([]SubagentTemplate{{Name: "bad.name", ModelFactory: goodFactory}}); err == nil {
		t.Fatal("expected error on bad update")
	}
	if !m.hasTemplate("cluster") {
		t.Error("a failed update wiped the existing templates")
	}
}

func TestTemplateNames_Sorted(t *testing.T) {
	t.Parallel()
	prov := &recordingProvider{llm: &stopRaceLLM{}}
	mgr := newTemplateManager(t, prov, []SubagentTemplate{
		{Name: "zebra", ModelFactory: tmplFactory(prov, "m")},
		{Name: "alpha", ModelFactory: tmplFactory(prov, "m")},
	})
	got := mgr.TemplateNames()
	if len(got) != 2 || got[0] != "alpha" || got[1] != "zebra" {
		t.Errorf("TemplateNames() = %v, want [alpha zebra]", got)
	}
}

// attachSyncParent wires a parent that carries mgr AND exposes names as
// synchronous subagent tools (agent.WithSubagents) — the shape main.go
// builds for the daemon's own agent, where a declarative subagent is
// reachable both ways.
func attachSyncParent(t *testing.T, mgr *Manager, names ...string) *agent.Agent {
	t.Helper()
	kids := make([]*agent.Agent, 0, len(names))
	for _, n := range names {
		kid, err := agent.New(echoLLM(t), agent.WithName(n))
		if err != nil {
			t.Fatalf("agent.New(%s): %v", n, err)
		}
		kids = append(kids, kid)
	}
	parent, err := agent.New(echoLLM(t), agent.WithBackgroundManager(mgr), agent.WithSubagents(kids))
	if err != nil {
		t.Fatalf("agent.New(parent): %v", err)
	}
	if got := parent.SubagentNames(); len(got) != len(names) {
		t.Fatalf("parent SubagentNames() = %v, want %v — the fixture no longer wires sync tools", got, names)
	}
	return parent
}

func echoLLM(t *testing.T) adkmodel.LLM {
	t.Helper()
	llm, err := mock.NewEcho().Model(context.Background(), "echo")
	if err != nil {
		t.Fatalf("mock echo Model: %v", err)
	}
	return llm
}

func TestCatalog_TemplatesAndPredefined(t *testing.T) {
	t.Parallel()
	prov := &recordingProvider{llm: &stopRaceLLM{}}
	mgr := newTemplateManager(t, prov, []SubagentTemplate{
		{Name: "zebra", Description: "zzz", ModelID: "z-model", Root: "../zebra", ModelFactory: tmplFactory(prov, "z-model")},
		{Name: "alpha", Description: "aaa", ModelFactory: tmplFactory(prov, "a-model")},
	}, WithPredefinedSpecs([]Spec{{Name: "researcher", SystemPrompt: "p", ModelID: "r-model"}}))
	attachSyncParent(t, mgr, "alpha", "zebra")

	cat := mgr.Catalog()
	if len(cat) != 3 {
		t.Fatalf("Catalog() len = %d, want 3 (2 templates + 1 predefined)", len(cat))
	}

	// Templates come first, sorted by name; predefined follow.
	if cat[0].Name != "alpha" || cat[1].Name != "zebra" || cat[2].Name != "researcher" {
		t.Fatalf("Catalog() order = [%s %s %s], want [alpha zebra researcher]", cat[0].Name, cat[1].Name, cat[2].Name)
	}

	// Templates are sync+async and carry description/model/root.
	alpha := cat[0]
	if alpha.Description != "aaa" || len(alpha.Modes) != 2 || alpha.Modes[0] != "sync" || alpha.Modes[1] != "async" {
		t.Errorf("alpha = %+v, want description=aaa modes=[sync async]", alpha)
	}
	zebra := cat[1]
	if zebra.Model != "z-model" || zebra.Root != "../zebra" {
		t.Errorf("zebra = %+v, want model=z-model root=../zebra", zebra)
	}

	// Predefined specs are async-only, no sync tool, no root.
	pre := cat[2]
	if pre.Model != "r-model" || pre.Root != "" || len(pre.Modes) != 1 || pre.Modes[0] != "async" {
		t.Errorf("researcher = %+v, want model=r-model root=\"\" modes=[async]", pre)
	}

	// ListSubagentCatalog (the interface seam) returns identical data.
	if via := mgr.ListSubagentCatalog(); len(via) != len(cat) || via[0].Name != cat[0].Name {
		t.Errorf("ListSubagentCatalog() disagrees with Catalog(): %v vs %v", via, cat)
	}
}

// TestCatalog_ModesFollowTheParentToolSurface: "sync" is a claim about
// the parent, not about the template. A manager whose parent carries it
// (WithBackgroundManager) but was never given the synchronous subagent
// tools (WithSubagents) must report async-only — that parent's model
// cannot call the subagent, only spawn it by reference. This is every
// multi-session session: compose.ReproduceAgent wires the manager and
// nothing else, so the old hardcoded sync+async had /subagents
// advertising a tool /tools did not list (#741).
func TestCatalog_ModesFollowTheParentToolSurface(t *testing.T) {
	t.Parallel()
	roster := func() []SubagentTemplate {
		prov := &recordingProvider{llm: &stopRaceLLM{}}
		return []SubagentTemplate{{Name: "cluster", ModelFactory: tmplFactory(prov, "c-model")}}
	}

	t.Run("async only when the parent has no sync tool", func(t *testing.T) {
		t.Parallel()
		prov := &recordingProvider{llm: &stopRaceLLM{}}
		mgr := newTemplateManager(t, prov, roster())
		attachEchoParent(t, mgr) // WithBackgroundManager, no WithSubagents
		cat := mgr.Catalog()
		if len(cat) != 1 {
			t.Fatalf("Catalog() = %+v, want 1 entry", cat)
		}
		if !slices.Equal(cat[0].Modes, []string{"async"}) {
			t.Errorf("cluster modes = %v, want [async] — the parent was never offered a cluster tool", cat[0].Modes)
		}
	})

	t.Run("sync when the parent exposes it", func(t *testing.T) {
		t.Parallel()
		prov := &recordingProvider{llm: &stopRaceLLM{}}
		mgr := newTemplateManager(t, prov, roster())
		attachSyncParent(t, mgr, "cluster")
		cat := mgr.Catalog()
		if len(cat) != 1 {
			t.Fatalf("Catalog() = %+v, want 1 entry", cat)
		}
		if !slices.Equal(cat[0].Modes, []string{"sync", "async"}) {
			t.Errorf("cluster modes = %v, want [sync async]", cat[0].Modes)
		}
	})

	t.Run("a sync tool that is not a template does not invent one", func(t *testing.T) {
		t.Parallel()
		prov := &recordingProvider{llm: &stopRaceLLM{}}
		mgr := newTemplateManager(t, prov, roster())
		attachSyncParent(t, mgr, "reviewer")
		cat := mgr.Catalog()
		if len(cat) != 1 || cat[0].Name != "cluster" {
			t.Fatalf("Catalog() = %+v, want just the cluster template", cat)
		}
		if !slices.Equal(cat[0].Modes, []string{"async"}) {
			t.Errorf("cluster modes = %v, want [async] — a differently-named sync tool must not mark it sync", cat[0].Modes)
		}
	})

	t.Run("no parent attached", func(t *testing.T) {
		t.Parallel()
		prov := &recordingProvider{llm: &stopRaceLLM{}}
		mgr := newTemplateManager(t, prov, roster())
		if cat := mgr.Catalog(); len(cat) != 1 || !slices.Equal(cat[0].Modes, []string{"async"}) {
			t.Errorf("Catalog() = %+v, want cluster async-only before any agent.New wires a parent", cat)
		}
	})
}

func TestCatalog_Empty(t *testing.T) {
	t.Parallel()
	prov := &recordingProvider{llm: &stopRaceLLM{}}
	mgr := newTemplateManager(t, prov, nil)
	if cat := mgr.Catalog(); cat == nil || len(cat) != 0 {
		t.Errorf("Catalog() = %v, want non-nil empty slice", cat)
	}
}

func TestSpawnTemplate_UnknownReference(t *testing.T) {
	t.Parallel()
	prov := &recordingProvider{llm: &stopRaceLLM{}}
	mgr := newTemplateManager(t, prov, nil)
	_, err := mgr.SpawnTemplate(context.Background(), "", "nope", RefOverrides{Goal: "g"}, "")
	if !errors.Is(err, ErrUnknownSubagent) {
		t.Errorf("err = %v, want ErrUnknownSubagent", err)
	}
}

func TestSpawnTemplate_RejectsNonNarrowingOverrides(t *testing.T) {
	t.Parallel()
	prov := &recordingProvider{llm: &stopRaceLLM{}}
	mgr := newTemplateManager(t, prov, []SubagentTemplate{
		{Name: "cluster", ModelFactory: tmplFactory(prov, "cluster-model")},
	}, WithSmallModelID("small-model"))

	// Goal is required (a template carries none).
	if _, err := mgr.SpawnTemplate(context.Background(), "", "cluster", RefOverrides{}, ""); err == nil {
		t.Error("missing goal: want error, got nil")
	}
	// Tool narrowing is not supported for templates.
	if _, err := mgr.SpawnTemplate(context.Background(), "", "cluster", RefOverrides{Goal: "g", Tools: []string{"read_file"}}, ""); err == nil {
		t.Error("tools override: want error, got nil")
	}
	// A specific model is rejected (D2).
	if _, err := mgr.SpawnTemplate(context.Background(), "", "cluster", RefOverrides{Goal: "g", Model: "gemini-3.5-pro"}, ""); !errors.Is(err, ErrModelNotOverridable) {
		t.Errorf("specific model err = %v, want ErrModelNotOverridable", err)
	}
}

func TestSpawnTemplate_SmallWithoutTierRejected(t *testing.T) {
	t.Parallel()
	prov := &recordingProvider{llm: &stopRaceLLM{}}
	mgr := newTemplateManager(t, prov, []SubagentTemplate{
		{Name: "cluster", ModelFactory: tmplFactory(prov, "cluster-model")},
	}) // no WithSmallModelID
	if _, err := mgr.SpawnTemplate(context.Background(), "", "cluster", RefOverrides{Goal: "g", Model: "small"}, ""); !errors.Is(err, ErrNoSmallModel) {
		t.Errorf("small without tier err = %v, want ErrNoSmallModel", err)
	}
}

// attachParent wires a fresh echo-backed parent so Spawn/SpawnTemplate can
// reach the parent session service — without the recording provider seeing
// the parent's model build (only subagent builds are asserted).
func attachEchoParent(t *testing.T, mgr *Manager) {
	t.Helper()
	parentLLM, err := mock.NewEcho().Model(context.Background(), "echo")
	if err != nil {
		t.Fatalf("parent Model: %v", err)
	}
	if _, err := agent.New(parentLLM, agent.WithBackgroundManager(mgr)); err != nil {
		t.Fatalf("agent.New: %v", err)
	}
}

func waitDone(t *testing.T, h *Handle) {
	t.Helper()
	select {
	case <-h.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("subagent goroutine didn't finish")
	}
}

// TestSpawnTemplate_RunsOnTemplateModel is the option-B end-to-end proof:
// a declarative subagent registered as a template spawns asynchronously
// and builds a fresh LLM from ITS OWN factory (its configured model),
// independent of the manager's/parent's model.
func TestSpawnTemplate_RunsOnTemplateModel(t *testing.T) {
	t.Parallel()
	prov := &recordingProvider{llm: &stopRaceLLM{}}
	mgr := newTemplateManager(t, prov, []SubagentTemplate{{
		Name:         "cluster",
		Instruction:  "triage clusters",
		ModelFactory: tmplFactory(prov, "cluster-model"),
		ModelID:      "cluster-model",
	}}, WithSmallModelID("small-model"), WithDefaultBudgets(Budgets{MaxTurns: 1}))
	attachEchoParent(t, mgr)
	defer mgr.Close()

	h, err := mgr.SpawnTemplate(context.Background(), "", "cluster", RefOverrides{Goal: "look at prod"}, "")
	if err != nil {
		t.Fatalf("SpawnTemplate: %v", err)
	}
	if h.Name != "cluster-1" {
		t.Errorf("auto-derived name = %q, want cluster-1", h.Name)
	}
	waitDone(t, h)
	if !prov.askedFor("cluster-model") {
		t.Errorf("template model not built; asked=%v", prov.asked)
	}
}

// TestSpawnTemplate_SmallDownshiftUsesManagerProvider proves a "small"
// override resolves against the manager's provider + small tier, not the
// template's own model.
func TestSpawnTemplate_SmallDownshiftUsesManagerProvider(t *testing.T) {
	t.Parallel()
	prov := &recordingProvider{llm: &stopRaceLLM{}}
	mgr := newTemplateManager(t, prov, []SubagentTemplate{{
		Name:         "cluster",
		ModelFactory: tmplFactory(prov, "cluster-model"),
		ModelID:      "cluster-model",
	}}, WithSmallModelID("small-model"), WithDefaultBudgets(Budgets{MaxTurns: 1}))
	attachEchoParent(t, mgr)
	defer mgr.Close()

	h, err := mgr.SpawnTemplate(context.Background(), "", "cluster", RefOverrides{Goal: "g", Model: "small"}, "explicit-name")
	if err != nil {
		t.Fatalf("SpawnTemplate small: %v", err)
	}
	if h.Name != "explicit-name" {
		t.Errorf("explicit name = %q, want explicit-name", h.Name)
	}
	waitDone(t, h)
	if !prov.askedFor("small-model") {
		t.Errorf("small tier not built; asked=%v", prov.asked)
	}
	if prov.askedFor("cluster-model") {
		t.Errorf("template model built despite small override; asked=%v", prov.asked)
	}
}

// TestSpawnRef_RoutesTemplateAndPredefined proves the operator-facing
// reference spawn (the /subagent command's backend) routes a declarative
// template through its own model factory and a catalog predefined spec
// through its configured model, and rejects unknown names.
func TestSpawnRef_RoutesTemplateAndPredefined(t *testing.T) {
	t.Parallel()
	prov := &recordingProvider{llm: &stopRaceLLM{}}
	mgr, err := NewManager(
		WithProvider(prov, "parent-model"),
		WithPredefinedSpecs([]Spec{{Name: "catalog", SystemPrompt: "p", ModelID: "catalog-model"}}),
		WithDefaultBudgets(Budgets{MaxTurns: 1}),
	)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := mgr.SetSubagentTemplates([]SubagentTemplate{{
		Name:         "cluster",
		Instruction:  "triage",
		ModelFactory: tmplFactory(prov, "cluster-model"),
		ModelID:      "cluster-model",
	}}); err != nil {
		t.Fatalf("SetSubagentTemplates: %v", err)
	}
	attachEchoParent(t, mgr)
	defer mgr.Close()

	hTmpl, err := mgr.SpawnRef(context.Background(), "", "cluster", "look at prod", RefOverrides{}, "")
	if err != nil {
		t.Fatalf("SpawnRef(template): %v", err)
	}
	if hTmpl.Name != "cluster-1" {
		t.Errorf("template instance name = %q, want cluster-1", hTmpl.Name)
	}
	waitDone(t, hTmpl)

	hSpec, err := mgr.SpawnRef(context.Background(), "", "catalog", "scan", RefOverrides{}, "")
	if err != nil {
		t.Fatalf("SpawnRef(predefined): %v", err)
	}
	if hSpec.Name != "catalog-1" {
		t.Errorf("predefined instance name = %q, want catalog-1", hSpec.Name)
	}
	waitDone(t, hSpec)

	if !prov.askedFor("cluster-model") {
		t.Errorf("template model not built; asked=%v", prov.asked)
	}
	if !prov.askedFor("catalog-model") {
		t.Errorf("predefined model not built; asked=%v", prov.asked)
	}

	if _, err := mgr.SpawnRef(context.Background(), "", "nope", "g", RefOverrides{}, ""); !errors.Is(err, ErrUnknownSubagent) {
		t.Errorf("unknown ref err = %v, want ErrUnknownSubagent", err)
	}
}

func TestReferenceNames_MergesTemplatesAndPredefined(t *testing.T) {
	t.Parallel()
	prov := &recordingProvider{llm: &stopRaceLLM{}}
	mgr, err := NewManager(
		WithProvider(prov, "parent-model"),
		WithPredefinedSpecs([]Spec{{Name: "zeta", SystemPrompt: "p"}}),
	)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := mgr.SetSubagentTemplates([]SubagentTemplate{{Name: "alpha", ModelFactory: tmplFactory(prov, "m")}}); err != nil {
		t.Fatalf("SetSubagentTemplates: %v", err)
	}
	got := mgr.ReferenceNames()
	if len(got) != 2 || got[0] != "alpha" || got[1] != "zeta" {
		t.Errorf("ReferenceNames() = %v, want [alpha zeta]", got)
	}
}

// declRecordingLLM captures the tool declarations it is offered on its
// first turn, then ends the run by returning plain text (a bounded
// delegation terminates when the model stops asking for tools). The
// captured spawn_agent description is the fingerprint of WHICH manager
// that tool is bound to: rosterTool renders the live roster of its own
// manager into the declaration (#640).
type declRecordingLLM struct {
	mu       sync.Mutex
	seen     []string
	spawnDoc string
}

func (*declRecordingLLM) Name() string { return "decl-recording" }

func (l *declRecordingLLM) GenerateContent(_ context.Context, req *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	if req != nil && req.Config != nil {
		l.mu.Lock()
		for _, gt := range req.Config.Tools {
			for _, fd := range gt.FunctionDeclarations {
				l.seen = append(l.seen, fd.Name)
				if fd.Name == "spawn_agent" {
					l.spawnDoc = fd.Description
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

func (l *declRecordingLLM) snapshot() ([]string, string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.seen), l.spawnDoc
}

// spawnDeclaration renders the spawn_agent declaration out of a resolved
// tool list — the same roster fingerprint declRecordingLLM captures, read
// directly rather than through a run.
func spawnDocFromTools(t *testing.T, tools []tool.Tool) string {
	t.Helper()
	for _, tl := range tools {
		if tl == nil || tl.Name() != "spawn_agent" {
			continue
		}
		rn, ok := tl.(runnableTool)
		if !ok {
			t.Fatalf("spawn_agent is %T, not runnable", tl)
		}
		decl := rn.Declaration()
		if decl == nil {
			t.Fatal("spawn_agent Declaration() = nil")
		}
		return decl.Description
	}
	t.Fatal("no spawn_agent in tool list")
	return ""
}

// TestSetSubagentTemplates_RebindsSpawnToolsToThisManager is the
// multi-session tenancy gate. A declarative subagent's Tools are resolved
// once at daemon startup and therefore carry the DAEMON manager's spawn
// tools; the same roster is then registered on every session's own
// manager. Unless installation rebinds them, a session's subagent spawns
// onto the daemon manager — wrong gate, wrong parent, wrong tenant's alert
// channel (the 2026-08-13 GKE UAT: a session's bg.cluster-1 put
// bg.cluster-2 on the daemon's "default" session).
func TestSetSubagentTemplates_RebindsSpawnToolsToThisManager(t *testing.T) {
	t.Parallel()
	rec := &declRecordingLLM{}
	prov := &recordingProvider{llm: rec}

	// The daemon manager's roster names only "daemon-only", so its spawn
	// tools' declarations are distinguishable from the session's.
	daemon := newTemplateManager(t, prov, nil,
		WithPredefinedSpecs([]Spec{{Name: "daemon-only", SystemPrompt: "p", Description: "the daemon's own catalog entry"}}))
	defer daemon.Close()

	// One shared roster, resolved against the daemon's tool surface —
	// exactly what cmd/core-agent hands every session's manager.
	shared := []SubagentTemplate{{
		Name:         "cluster",
		Description:  "read-only GKE triage for one cluster",
		Instruction:  "triage",
		ModelFactory: tmplFactory(prov, "cluster-model"),
		ModelID:      "cluster-model",
		Tools:        NewSpawnTools(daemon),
	}}
	session := newTemplateManager(t, prov, shared, WithDefaultBudgets(Budgets{MaxTurns: 1}))
	attachEchoParent(t, session)
	defer session.Close()

	h, err := session.SpawnTemplate(context.Background(), "", "cluster", RefOverrides{Goal: "g"}, "")
	if err != nil {
		t.Fatalf("SpawnTemplate: %v", err)
	}
	waitDone(t, h)

	seen, spawnDoc := rec.snapshot()
	if !slices.Contains(seen, "spawn_agent") {
		t.Fatalf("the spawned subagent was never offered spawn_agent (saw %v) — this fixture no longer exercises the rebind", seen)
	}
	if !strings.Contains(spawnDoc, "cluster — read-only GKE triage for one cluster") {
		t.Errorf("the subagent's spawn_agent is still bound to the DAEMON manager — its declaration carries no session roster:\n%s", spawnDoc)
	}
	if strings.Contains(spawnDoc, "daemon-only") {
		t.Errorf("the subagent's spawn_agent still advertises the daemon catalog:\n%s", spawnDoc)
	}

	// Registration must not write through into the caller's slice: it is
	// shared with the daemon manager and every other session.
	if doc := spawnDocFromTools(t, shared[0].Tools); !strings.Contains(doc, "daemon-only") {
		t.Errorf("SetSubagentTemplates mutated the shared template's Tools in place; the daemon's own roster is gone:\n%s", doc)
	}
}

// TestRebindSpawnTools_PreservesOperatorScoping: the rebind replaces
// spawn tools that are already present, and adds none. A subagent whose
// `tools` allowlist excluded spawn_agent must not acquire one by being
// registered on a manager.
func TestRebindSpawnTools_PreservesOperatorScoping(t *testing.T) {
	t.Parallel()
	prov := &recordingProvider{llm: &stopRaceLLM{}}
	daemon := newTemplateManager(t, prov, nil)
	defer daemon.Close()
	session := newTemplateManager(t, prov, nil,
		WithPredefinedSpecs([]Spec{{Name: "session-only", SystemPrompt: "p"}}))
	defer session.Close()

	t.Run("no spawn tools stays that way", func(t *testing.T) {
		in := []tool.Tool{newNamedStubTool(t, "read_file")}
		out := rebindSpawnTools(in, session.ownSpawnTools())
		if len(out) != 1 || out[0].Name() != "read_file" {
			t.Fatalf("out = %v, want the input unchanged", toolNames(out))
		}
	})

	t.Run("partial grant is not widened", func(t *testing.T) {
		in := []tool.Tool{newNamedStubTool(t, "read_file"), NewStopAgentTool(daemon)}
		out := rebindSpawnTools(in, session.ownSpawnTools())
		if got := toolNames(out); !slices.Equal(got, []string{"read_file", "stop_agent"}) {
			t.Fatalf("out = %v, want [read_file stop_agent] — order and membership preserved", got)
		}
	})

	t.Run("empty and nil pass through", func(t *testing.T) {
		if out := rebindSpawnTools(nil, session.ownSpawnTools()); out != nil {
			t.Errorf("rebindSpawnTools(nil) = %v, want nil", toolNames(out))
		}
	})

	t.Run("spawn_agent is rebound", func(t *testing.T) {
		in := NewSpawnTools(daemon)
		out := rebindSpawnTools(in, session.ownSpawnTools())
		if got := toolNames(out); !slices.Equal(got, []string{"spawn_agent", "stop_agent"}) {
			t.Fatalf("out = %v, want [spawn_agent stop_agent]", got)
		}
		if doc := spawnDocFromTools(t, out); !strings.Contains(doc, "session-only") {
			t.Errorf("rebound spawn_agent does not carry the session roster:\n%s", doc)
		}
		if doc := spawnDocFromTools(t, in); strings.Contains(doc, "session-only") {
			t.Error("rebindSpawnTools mutated its input slice")
		}
	})
}

func toolNames(ts []tool.Tool) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Name())
	}
	return out
}

// TestSpawnAgentTool_RoutingRationale documents why NewSpawnAgentTool must
// gate on hasTemplate before the catalog path: a template name is invisible
// to resolvePredefinedSpec, so the catalog branch alone would reject it as
// unknown. The hasTemplate gate is what routes an {agent: "<template>"}
// reference to SpawnTemplate instead.
func TestSpawnAgentTool_RoutingRationale(t *testing.T) {
	t.Parallel()
	prov := &recordingProvider{llm: &stopRaceLLM{}}
	mgr := newTemplateManager(t, prov, []SubagentTemplate{{
		Name:         "cluster",
		Instruction:  "triage",
		ModelFactory: tmplFactory(prov, "cluster-model"),
		ModelID:      "cluster-model",
	}})

	if !mgr.hasTemplate("cluster") {
		t.Fatal("template not registered")
	}
	// The catalog path cannot resolve a template name — hence the gate.
	if _, _, err := buildSpawnSpec(mgr, spawnAgentArgs{Agent: "cluster", Goal: "g"}); !errors.Is(err, ErrUnknownSubagent) {
		t.Errorf("buildSpawnSpec(template name) err = %v, want ErrUnknownSubagent", err)
	}
}
