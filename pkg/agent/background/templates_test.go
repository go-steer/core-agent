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
	"testing"
	"time"

	adkmodel "google.golang.org/adk/model"

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

func TestCatalog_TemplatesAndPredefined(t *testing.T) {
	t.Parallel()
	prov := &recordingProvider{llm: &stopRaceLLM{}}
	mgr := newTemplateManager(t, prov, []SubagentTemplate{
		{Name: "zebra", Description: "zzz", ModelID: "z-model", Root: "../zebra", ModelFactory: tmplFactory(prov, "z-model")},
		{Name: "alpha", Description: "aaa", ModelFactory: tmplFactory(prov, "a-model")},
	}, WithPredefinedSpecs([]Spec{{Name: "researcher", SystemPrompt: "p", ModelID: "r-model"}}))

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
