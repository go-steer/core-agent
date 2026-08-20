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

package agent

import (
	"context"
	"iter"
	"strings"
	"testing"

	adkagent "google.golang.org/adk/agent"
	"google.golang.org/adk/memory"
	adkmodel "google.golang.org/adk/model"
	"google.golang.org/adk/session"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/toolconfirmation"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

// answeringLLM lets the allowed half of these tests reach the end of a
// subagent run; minimalLLM deliberately refuses to be invoked, which
// would be indistinguishable from a denial here.
type answeringLLM struct{}

func (answeringLLM) Name() string { return "answering" }

func (answeringLLM) GenerateContent(context.Context, *adkmodel.LLMRequest, bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		yield(&adkmodel.LLMResponse{
			Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "done"}}},
			FinishReason: genai.FinishReasonStop,
			TurnComplete: true,
		}, nil)
	}
}

// gateToolCtx is a minimal tool.Context for driving a subagent tool's
// Run directly. Full-interface satisfaction is deliberate: an ADK bump
// that adds a method should break the stub rather than silently drift.
type gateToolCtx struct{ context.Context }

func (c *gateToolCtx) UserContent() *genai.Content          { return nil }
func (c *gateToolCtx) InvocationID() string                 { return "test-invocation" }
func (c *gateToolCtx) AgentName() string                    { return "test-agent" }
func (c *gateToolCtx) ReadonlyState() session.ReadonlyState { return nil }
func (c *gateToolCtx) UserID() string                       { return "test-user" }
func (c *gateToolCtx) AppName() string                      { return "test-app" }
func (c *gateToolCtx) SessionID() string                    { return "test-session" }
func (c *gateToolCtx) Branch() string                       { return "" }
func (c *gateToolCtx) Artifacts() adkagent.Artifacts        { return nil }
func (c *gateToolCtx) State() session.State                 { return nil }
func (c *gateToolCtx) FunctionCallID() string               { return "call-1" }
func (c *gateToolCtx) Actions() *session.EventActions       { return &session.EventActions{} }
func (c *gateToolCtx) ToolConfirmation() *toolconfirmation.ToolConfirmation {
	return nil
}
func (c *gateToolCtx) RequestConfirmation(string, any) error { return nil }
func (c *gateToolCtx) SearchMemory(context.Context, string) (*memory.SearchResponse, error) {
	return nil, nil
}

// runSubagentTool invokes the tool the way the ADK flow does and
// returns the Go error, which is where a gate denial lands.
func runSubagentTool(t *testing.T, tl tool.Tool) error {
	t.Helper()
	runner, ok := tl.(interface {
		Run(tool.Context, any) (map[string]any, error)
	})
	if !ok {
		t.Fatalf("%s is not runnable", tl.Name())
	}
	_, err := runner.Run(&gateToolCtx{Context: context.Background()},
		map[string]any{"request": "look into the crashloop"})
	return err
}

func gatedSubagentTool(t *testing.T, g *permissions.Gate) tool.Tool {
	t.Helper()
	h, cleanup := openTestEventLog(t)
	t.Cleanup(cleanup)
	inner, err := New(answeringLLM{}, WithName("cluster"), WithEventLog(h), WithSession("u", "s1"))
	if err != nil {
		t.Fatalf("New inner: %v", err)
	}
	tl, err := NewSubagentTool(SubagentOptions{Inner: inner, Gate: g})
	if err != nil {
		t.Fatalf("NewSubagentTool: %v", err)
	}
	return tl
}

// The second door #758 had to close. A declarative subagent is
// reachable two ways — `spawn_agent {agent: "cluster"}` and, because
// WithSubagents also registers it, a direct `cluster(request: …)` call.
// Gating only the first would have left plan-first a suggestion: the
// model picks whichever door the schema nudges it toward.
func TestSubagentTool_PlanFirstDeniesTheSynchronousDoor(t *testing.T) {
	t.Parallel()
	g := permissions.New(permissions.Options{
		Mode:                permissions.ModeYolo,
		RequirePlanArtifact: true,
	})
	tl := gatedSubagentTool(t, g)

	err := runSubagentTool(t, tl)
	if err == nil || !strings.Contains(err.Error(), "plan-first mode requires record_plan") {
		t.Fatalf("call before record_plan: err = %v, want the plan-first denial", err)
	}
	// The denial names the bucket, not the subagent's own tool name —
	// that is what makes one rule cover both doors.
	if !strings.Contains(err.Error(), "spawn_agent") {
		t.Errorf("denial = %q, want it attributed to the spawn_agent bucket", err)
	}

	g.MarkPlanRecorded()
	if err := runSubagentTool(t, tl); err != nil {
		t.Fatalf("call after record_plan: err = %v, want the gate to stand aside", err)
	}
}

// One deny rule closes both doors. An operator who wrote
// `spawn_agent:cluster` meant that cluster does not run, not that it
// does not run asynchronously.
func TestSubagentTool_OneDenyRuleCoversBothDoors(t *testing.T) {
	t.Parallel()
	pol, err := permissions.NewPolicy(nil, []string{"spawn_agent:cluster"})
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	tl := gatedSubagentTool(t, permissions.New(permissions.Options{
		Mode:   permissions.ModeYolo,
		Policy: pol,
	}))
	if err := runSubagentTool(t, tl); err == nil || !strings.Contains(err.Error(), "denied by config policy") {
		t.Fatalf("denied subagent via the synchronous door: err = %v, want the policy denial", err)
	}
}

// WithSubagents is where these tools are constructed, so it is the only
// place a gate can reach them — the wiring, not just the handler.
func TestWithSubagents_ThreadsTheGateIntoTheSubagentTools(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestEventLog(t)
	defer cleanup()
	inner, err := New(answeringLLM{}, WithName("cluster"), WithEventLog(h), WithSession("u", "s1"))
	if err != nil {
		t.Fatalf("New inner: %v", err)
	}
	g := permissions.New(permissions.Options{
		Mode:                permissions.ModeYolo,
		RequirePlanArtifact: true,
	})
	parent, err := New(answeringLLM{}, WithEventLog(h), WithSession("u", "s0"),
		WithGate(g), WithSubagents([]*Agent{inner}))
	if err != nil {
		t.Fatalf("New parent: %v", err)
	}
	var tl tool.Tool
	for _, candidate := range parent.Tools() {
		if candidate.Name() == "cluster" {
			tl = candidate
		}
	}
	if tl == nil {
		t.Fatal("parent has no cluster subagent tool")
	}
	// Assert the denial text, not merely that something failed: an
	// unrelated run error would otherwise pass this test on pre-fix code.
	if err := runSubagentTool(t, tl); err == nil ||
		!strings.Contains(err.Error(), "plan-first mode requires record_plan") {
		t.Fatalf("subagent tool built by WithSubagents, plan-first armed with no plan recorded: "+
			"err = %v, want the plan-first denial — the parent's gate never reached the tool it constructed", err)
	}
}

// A host that wired no gate has nothing gated anywhere; refusing here
// would be the only enforcement in a build that asked for none.
func TestSubagentTool_NilGateIsNotADenial(t *testing.T) {
	t.Parallel()
	if err := runSubagentTool(t, gatedSubagentTool(t, nil)); err != nil {
		t.Fatalf("ungated build: err = %v, want none", err)
	}
}
