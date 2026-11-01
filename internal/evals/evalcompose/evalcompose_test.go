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

package evalcompose

import (
	"context"
	"errors"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"
)

func TestMeterAllowAndObserve(t *testing.T) {
	t.Parallel()
	scopes := MeterScopes([]Spec{
		{
			Name:   "analyst",
			Budget: SpecialistBudget{MaxTurns: 2, MaxCostUSD: 0.50},
		},
	}, "", "echo")
	m := NewMeter(BudgetConfig{
		Limits:      Limits{MaxTurns: 5, MaxCostUSD: 2.0, RatePer1K: 1.0},
		Scopes:      scopes,
		FinalReport: true,
	})

	if err := m.Allow("analyst"); err != nil {
		t.Fatalf("Allow before spend = %v, want nil", err)
	}
	if m.AllowFinalReport("analyst") {
		t.Fatal("AllowFinalReport before any spend = true, want false")
	}

	ev1 := &session.Event{Author: "analyst"}
	ev1.UsageMetadata = &genai.GenerateContentResponseUsageMetadata{TotalTokenCount: 250}
	if err := m.Observe(ev1); err != nil {
		t.Fatalf("Observe(ev1) = %v, want nil", err)
	}

	ev2 := &session.Event{Author: "analyst"}
	ev2.UsageMetadata = &genai.GenerateContentResponseUsageMetadata{TotalTokenCount: 250}
	if err := m.Observe(ev2); err != nil {
		t.Fatalf("Observe(ev2) = %v, want nil", err)
	}

	if err := m.Allow("analyst"); !errors.Is(err, ErrBudgetRefused) {
		t.Fatalf("Allow after hitting scope cap = %v, want ErrBudgetRefused", err)
	}
	if !m.AllowFinalReport("analyst") {
		t.Fatal("first AllowFinalReport = false, want true")
	}
	if m.AllowFinalReport("analyst") {
		t.Fatal("second AllowFinalReport = true, want false (latched)")
	}
}

func TestLoopGuardArgsEquivalenceAndResultChangeReset(t *testing.T) {
	t.Parallel()
	if !argsEquivalent(`{"path":"./a/b/main.go"}`, `{"path":"a/b/main.go"}`) {
		t.Fatal("expected cleaned relative paths to be equivalent")
	}
	if !argsEquivalent(`{"file":"/workspace/pkg/main.go"}`, `{"file":"pkg/main.go"}`) {
		t.Fatal("expected suffix-equivalent paths to be equivalent")
	}
	if argsEquivalent(`{"file":"a/main.go"}`, `{"file":"b/main.go"}`) {
		t.Fatal("expected distinct directory prefixes not to be equivalent")
	}

	g := &loopGuardState{
		cfg:       LoopGuardConfig{RepeatThreshold: 2, RepeatStopAfter: 1},
		capCalls:  10,
		thresh:    2,
		stopAfter: 1,
		byInv:     map[string]*invLoopState{},
	}
	st := g.stateFor("inv-1")
	st.lastTool = "get_pod"
	st.lastArgs = `{"name":"api"}`
	st.result = `{"phase":"Pending"}`
	st.length = 2
	st.notes["c-2"] = "loop warning"

	// When the tool result changes, the streak resets to 1 and the note is dropped.
	digest := serializeLoopArgs(map[string]any{"phase": "Running"})
	if st.result != digest {
		st.result = digest
		st.length = 1
		delete(st.notes, "c-2")
	}
	if st.length != 1 {
		t.Fatalf("st.length = %d, want 1", st.length)
	}
}

type dummyToolset struct {
	name  string
	tools []tool.Tool
}

func (d dummyToolset) Name() string { return d.name }
func (d dummyToolset) Tools(adkagent.ReadonlyContext) ([]tool.Tool, error) {
	return d.tools, nil
}

func TestFilterToolsetsAndBuildRoot(t *testing.T) {
	t.Parallel()
	t1, err := functiontool.New(functiontool.Config{Name: "read_pod", Description: "read"}, func(_ adkagent.Context, _ struct{}) (map[string]any, error) {
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t2, err := functiontool.New(functiontool.Config{Name: "delete_pod", Description: "delete"}, func(_ adkagent.Context, _ struct{}) (map[string]any, error) {
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	offered := []tool.Toolset{dummyToolset{name: "gke", tools: []tool.Tool{t1, t2}}}

	// Empty MCP slice denies all toolsets.
	denied := filterToolsets(Spec{Name: "s1", Tools: ToolAllowlist{MCP: []MCPAllowlist{}}}, offered)
	if len(denied) != 0 {
		t.Fatalf("len(denied) = %d, want 0", len(denied))
	}

	// Scoped MCP allowlist keeps only read_pod.
	scoped := filterToolsets(Spec{
		Name: "s2",
		Tools: ToolAllowlist{MCP: []MCPAllowlist{
			{Server: "gke", Tools: []string{"read_pod"}},
		}},
	}, offered)
	if len(scoped) != 1 {
		t.Fatalf("len(scoped) = %d, want 1", len(scoped))
	}
	gotTools, err := scoped[0].Tools(nil)
	if err != nil || len(gotTools) != 1 || gotTools[0].Name() != "read_pod" {
		t.Fatalf("scoped tools = %v, err = %v", gotTools, err)
	}

	m, err := BuildModel(context.Background(), "", "echo", ModelOptions{})
	if err != nil {
		t.Fatal(err)
	}
	root, _, err := BuildRoot(context.Background(), RootConfig{
		Bundle:    Bundle{Name: "test-wl", Specialists: []string{"diag"}},
		Specs:     []Spec{{Name: "diag", Description: "diagnoser", Mode: ModeTask}},
		Model:     m,
		ModelName: "echo",
		Toolsets:  offered,
	})
	if err != nil || root == nil {
		t.Fatalf("BuildRoot coordinator: root=%v, err=%v", root, err)
	}
}
