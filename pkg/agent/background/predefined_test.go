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
	"sync"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/model"

	"github.com/go-steer/core-agent/v2/pkg/agent"
	"github.com/go-steer/core-agent/v2/pkg/models/mock"
)

// recordingProvider records every model id Model() is asked to build so
// a test can assert per-spec model resolution actually reaches the
// provider factory. The returned LLM terminates the autonomous loop in
// one turn (reuses stopRaceLLM from manager_test.go).
type recordingProvider struct {
	mu    sync.Mutex
	asked []string
	llm   adkmodel.LLM
}

func (p *recordingProvider) Name() string { return "recording" }
func (p *recordingProvider) Model(_ context.Context, id string) (adkmodel.LLM, error) {
	p.mu.Lock()
	p.asked = append(p.asked, id)
	p.mu.Unlock()
	return p.llm, nil
}

func (p *recordingProvider) askedFor(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.asked {
		if a == id {
			return true
		}
	}
	return false
}

func newRosterManager(t *testing.T, specs []Spec, opts ...ManagerOption) *Manager {
	t.Helper()
	base := []ManagerOption{
		WithProvider(mock.NewEcho(), "parent-model"),
		WithPredefinedSpecs(specs),
	}
	mgr, err := NewManager(append(base, opts...)...)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return mgr
}

func TestNewManager_RejectsDuplicatePredefinedName(t *testing.T) {
	t.Parallel()
	_, err := NewManager(
		WithProvider(mock.NewEcho(), "echo"),
		WithPredefinedSpecs([]Spec{
			{Name: "dup", SystemPrompt: "a"},
			{Name: "dup", SystemPrompt: "b"},
		}),
	)
	if err == nil {
		t.Fatal("expected error for duplicate predefined name")
	}
}

func TestNewManager_PredefinedRequiresSystemPromptButNotGoal(t *testing.T) {
	t.Parallel()
	// Missing SystemPrompt → error.
	if _, err := NewManager(
		WithProvider(mock.NewEcho(), "echo"),
		WithPredefinedSpecs([]Spec{{Name: "x"}}),
	); err == nil {
		t.Error("expected error when predefined spec has no SystemPrompt")
	}
	// Goal may be empty at registration (supplied per spawn).
	if _, err := NewManager(
		WithProvider(mock.NewEcho(), "echo"),
		WithPredefinedSpecs([]Spec{{Name: "x", SystemPrompt: "persona"}}),
	); err != nil {
		t.Errorf("predefined spec without a Goal should be valid: %v", err)
	}
	// Branch-unsafe name → error.
	if _, err := NewManager(
		WithProvider(mock.NewEcho(), "echo"),
		WithPredefinedSpecs([]Spec{{Name: "has.dot", SystemPrompt: "p"}}),
	); err == nil {
		t.Error("expected error for branch-unsafe predefined name")
	}
}

func TestResolvePredefinedSpec_UnknownReference(t *testing.T) {
	t.Parallel()
	mgr := newRosterManager(t, nil)
	_, err := mgr.resolvePredefinedSpec("nope", RefOverrides{})
	if !errors.Is(err, ErrUnknownSubagent) {
		t.Errorf("err = %v, want ErrUnknownSubagent", err)
	}
}

func TestResolvePredefinedSpec_InheritsTemplateAndReplacesGoal(t *testing.T) {
	t.Parallel()
	mgr := newRosterManager(t, []Spec{{
		Name:         "cluster",
		SystemPrompt: "triage clusters",
		Goal:         "default goal",
		ModelID:      "cluster-model",
		Tools:        []string{"read_file", "grep"},
		Budgets:      Budgets{MaxTurns: 20},
	}})

	spec, err := mgr.resolvePredefinedSpec("cluster", RefOverrides{Goal: "look at prod"})
	if err != nil {
		t.Fatalf("resolvePredefinedSpec: %v", err)
	}
	if spec.SystemPrompt != "triage clusters" {
		t.Errorf("SystemPrompt = %q, want inherited persona", spec.SystemPrompt)
	}
	if spec.Goal != "look at prod" {
		t.Errorf("Goal = %q, want the override", spec.Goal)
	}
	if spec.ModelID != "cluster-model" {
		t.Errorf("ModelID = %q, want inherited cluster-model", spec.ModelID)
	}
	if spec.Budgets.MaxTurns != 20 {
		t.Errorf("MaxTurns = %d, want inherited 20", spec.Budgets.MaxTurns)
	}
	// An empty goal override keeps the template's goal.
	spec2, err := mgr.resolvePredefinedSpec("cluster", RefOverrides{})
	if err != nil {
		t.Fatalf("resolvePredefinedSpec: %v", err)
	}
	if spec2.Goal != "default goal" {
		t.Errorf("Goal = %q, want template default when not overridden", spec2.Goal)
	}
}

func TestResolvePredefinedSpec_ModelOverride(t *testing.T) {
	t.Parallel()
	specs := []Spec{{Name: "cluster", SystemPrompt: "p", ModelID: "cluster-model"}}

	// "small" with a small tier configured → downshift.
	mgr := newRosterManager(t, specs, WithSmallModelID("small-model"))
	spec, err := mgr.resolvePredefinedSpec("cluster", RefOverrides{Model: "small"})
	if err != nil {
		t.Fatalf("resolve small: %v", err)
	}
	if spec.ModelID != "small-model" {
		t.Errorf("ModelID = %q, want small-model", spec.ModelID)
	}

	// inherit keeps the spec's configured model.
	spec, err = mgr.resolvePredefinedSpec("cluster", RefOverrides{Model: "inherit"})
	if err != nil {
		t.Fatalf("resolve inherit: %v", err)
	}
	if spec.ModelID != "cluster-model" {
		t.Errorf("ModelID = %q, want cluster-model on inherit", spec.ModelID)
	}

	// A specific model is rejected (D2).
	if _, err := mgr.resolvePredefinedSpec("cluster", RefOverrides{Model: "gemini-3.5-pro"}); !errors.Is(err, ErrModelNotOverridable) {
		t.Errorf("specific model override err = %v, want ErrModelNotOverridable", err)
	}

	// "small" without a configured small tier → ErrNoSmallModel.
	mgrNoSmall := newRosterManager(t, specs)
	if _, err := mgrNoSmall.resolvePredefinedSpec("cluster", RefOverrides{Model: "small"}); !errors.Is(err, ErrNoSmallModel) {
		t.Errorf("small without config err = %v, want ErrNoSmallModel", err)
	}
}

func TestResolvePredefinedSpec_ToolsNarrowOnly(t *testing.T) {
	t.Parallel()
	mgr := newRosterManager(t, []Spec{{
		Name:         "cluster",
		SystemPrompt: "p",
		Tools:        []string{"read_file", "grep"},
		Extras:       []string{"kubectl_get"},
	}})

	// A subset is accepted and becomes the whole grant.
	spec, err := mgr.resolvePredefinedSpec("cluster", RefOverrides{Tools: []string{"read_file", "kubectl_get"}})
	if err != nil {
		t.Fatalf("narrow subset: %v", err)
	}
	if len(spec.Tools) != 2 || spec.Extras != nil {
		t.Errorf("narrowed grant = tools:%v extras:%v, want [read_file kubectl_get] / nil", spec.Tools, spec.Extras)
	}

	// A tool the spec doesn't grant is rejected (no widening).
	if _, err := mgr.resolvePredefinedSpec("cluster", RefOverrides{Tools: []string{"bash"}}); !errors.Is(err, ErrToolNotGranted) {
		t.Errorf("widening err = %v, want ErrToolNotGranted", err)
	}

	// No tools override keeps the full grant.
	spec, err = mgr.resolvePredefinedSpec("cluster", RefOverrides{})
	if err != nil {
		t.Fatalf("no override: %v", err)
	}
	if len(spec.Tools) != 2 || len(spec.Extras) != 1 {
		t.Errorf("unmodified grant = tools:%v extras:%v, want full", spec.Tools, spec.Extras)
	}
}

func TestResolvePredefinedSpec_BudgetsTightenOnly(t *testing.T) {
	t.Parallel()
	mgr := newRosterManager(t, []Spec{{
		Name:         "cluster",
		SystemPrompt: "p",
		Budgets: Budgets{
			MaxTurns:     50,
			MaxCost:      2.0,
			MaxWallclock: 10 * time.Minute,
		},
	}})

	spec, err := mgr.resolvePredefinedSpec("cluster", RefOverrides{
		Budgets: Budgets{
			MaxTurns:     10,               // tighter → wins
			MaxCost:      5.0,              // looser → ignored
			MaxWallclock: 30 * time.Minute, // looser → ignored
		},
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if spec.Budgets.MaxTurns != 10 {
		t.Errorf("MaxTurns = %d, want tighter 10", spec.Budgets.MaxTurns)
	}
	if spec.Budgets.MaxCost != 2.0 {
		t.Errorf("MaxCost = %v, want spec's 2.0 (looser override ignored)", spec.Budgets.MaxCost)
	}
	if spec.Budgets.MaxWallclock != 10*time.Minute {
		t.Errorf("MaxWallclock = %v, want spec's 10m (looser override ignored)", spec.Budgets.MaxWallclock)
	}
}

func TestTightenInt(t *testing.T) {
	t.Parallel()
	cases := []struct{ base, ov, want int }{
		{50, 10, 10}, // tighter wins
		{50, 90, 50}, // looser ignored
		{50, 0, 50},  // absent override keeps base
		{0, 10, 10},  // uncapped base → override caps
		{0, 0, 0},    // both uncapped
	}
	for _, c := range cases {
		if got := tightenInt(c.base, c.ov); got != c.want {
			t.Errorf("tightenInt(%d,%d) = %d, want %d", c.base, c.ov, got, c.want)
		}
	}
}

func TestNextInstanceName(t *testing.T) {
	t.Parallel()
	mgr := newRosterManager(t, nil)
	if got := mgr.nextInstanceName("cluster", ""); got != "cluster-1" {
		t.Errorf("first auto name = %q, want cluster-1", got)
	}
	if got := mgr.nextInstanceName("cluster", ""); got != "cluster-2" {
		t.Errorf("second auto name = %q, want cluster-2", got)
	}
	if got := mgr.nextInstanceName("triage", ""); got != "triage-1" {
		t.Errorf("per-spec counter leaked: %q, want triage-1", got)
	}
	if got := mgr.nextInstanceName("cluster", "prod-cluster"); got != "prod-cluster" {
		t.Errorf("explicit name = %q, want prod-cluster", got)
	}
}

func TestBuildSpawnSpec_AdhocGate(t *testing.T) {
	t.Parallel()
	// Ad-hoc off (default): an inline-persona spawn is refused.
	mgr := newRosterManager(t, nil)
	_, _, err := buildSpawnSpec(mgr, spawnAgentArgs{Name: "adhoc", SystemPrompt: "p", Goal: "g"})
	if !errors.Is(err, ErrAdhocDisabled) {
		t.Errorf("ad-hoc off err = %v, want ErrAdhocDisabled", err)
	}

	// Ad-hoc on: the inline spec is built.
	mgrOn := newRosterManager(t, nil, WithAllowAdhoc(true))
	spec, name, err := buildSpawnSpec(mgrOn, spawnAgentArgs{Name: "adhoc", SystemPrompt: "p", Goal: "g"})
	if err != nil {
		t.Fatalf("ad-hoc on: %v", err)
	}
	if name != "adhoc" || spec.SystemPrompt != "p" {
		t.Errorf("ad-hoc spec = %q/%q, want adhoc/p", name, spec.SystemPrompt)
	}
}

func TestBuildSpawnSpec_ReferenceAutoNamesAndIgnoresInlinePersona(t *testing.T) {
	t.Parallel()
	mgr := newRosterManager(t, []Spec{{Name: "cluster", SystemPrompt: "triage"}})
	// A reference works even with ad-hoc off, and auto-derives an instance
	// name. An inline system_prompt is ignored (the spec's persona is fixed).
	spec, name, err := buildSpawnSpec(mgr, spawnAgentArgs{
		Agent:        "cluster",
		SystemPrompt: "IGNORED",
		Goal:         "check prod",
	})
	if err != nil {
		t.Fatalf("reference spawn: %v", err)
	}
	if name != "cluster-1" {
		t.Errorf("auto name = %q, want cluster-1", name)
	}
	if spec.SystemPrompt != "triage" {
		t.Errorf("SystemPrompt = %q, want the spec's fixed persona (inline ignored)", spec.SystemPrompt)
	}
	if spec.Goal != "check prod" {
		t.Errorf("Goal = %q, want check prod", spec.Goal)
	}
}

// TestSpawn_ReferencedSpecRunsOnConfiguredModel is the end-to-end proof
// that a reference spawn's per-spec model actually reaches the provider
// factory — the core #626 enabler that lets a triage/cluster subagent
// run on its own (often cheaper) model rather than the parent's.
func TestSpawn_ReferencedSpecRunsOnConfiguredModel(t *testing.T) {
	t.Parallel()
	prov := &recordingProvider{llm: &stopRaceLLM{}}
	mgr, err := NewManager(
		WithProvider(prov, "parent-model"),
		WithSmallModelID("small-model"),
		WithDefaultBudgets(Budgets{MaxTurns: 1}),
		WithPredefinedSpecs([]Spec{{
			Name: "cluster", SystemPrompt: "triage", ModelID: "cluster-model",
		}}),
	)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	// Parent built from a separate echo LLM so the only recording-provider
	// Model() call is the one Spawn makes for the subagent.
	parentLLM, err := mock.NewEcho().Model(context.Background(), "echo")
	if err != nil {
		t.Fatalf("parent Model: %v", err)
	}
	if _, err := agent.New(parentLLM, agent.WithBackgroundManager(mgr)); err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	defer mgr.Close()

	spec, _, err := buildSpawnSpec(mgr, spawnAgentArgs{Agent: "cluster", Goal: "look at prod"})
	if err != nil {
		t.Fatalf("buildSpawnSpec: %v", err)
	}
	h, err := mgr.Spawn(context.Background(), "", spec)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	select {
	case <-h.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("subagent goroutine didn't finish")
	}
	if !prov.askedFor("cluster-model") {
		t.Errorf("provider was not asked to build cluster-model; asked=%v", prov.asked)
	}
}
