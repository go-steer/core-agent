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
	"strings"
	"testing"

	adkagent "google.golang.org/adk/agent"
	"google.golang.org/adk/memory"
	"google.golang.org/adk/session"
	adktool "google.golang.org/adk/tool"
	"google.golang.org/adk/tool/toolconfirmation"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/tools"
)

// probeTool is a read-only tool for the waiter to poll.
type probeTool struct{ payload string }

func (p *probeTool) Name() string        { return "probe" }
func (p *probeTool) Description() string { return "test probe" }
func (p *probeTool) IsLongRunning() bool { return false }
func (p *probeTool) ReadOnlyHint() bool  { return true }
func (p *probeTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{Name: "probe"}
}

func (p *probeTool) Run(adktool.Context, any) (map[string]any, error) {
	return map[string]any{"state": p.payload}, nil
}

// The catalog binding is the wiring an embedder would otherwise have
// to remember (#648). It happens inside New, so every construction
// site gets it — and this test fails with "no tool catalog is bound"
// if that call is dropped.
func TestNew_BindsTheToolCatalogToWaitAndVerify(t *testing.T) {
	t.Parallel()
	waiter, err := tools.NewWaitAndVerifyTool(config.DefaultConfig(), tools.WaitAndVerifyOptions{})
	if err != nil {
		t.Fatalf("NewWaitAndVerifyTool: %v", err)
	}
	a, err := New(minimalLLM{}, WithTools([]adktool.Tool{waiter, &probeTool{payload: "ready"}}))
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}

	var registered adktool.Tool
	for _, tl := range a.Tools() {
		if tl.Name() == tools.WaitAndVerifyToolName {
			registered = tl
		}
	}
	if registered == nil {
		t.Fatal("wait_and_verify is not in the agent's tool list")
	}
	runnable, ok := registered.(interface {
		Run(adktool.Context, any) (map[string]any, error)
	})
	if !ok {
		t.Fatalf("registered wait_and_verify (%T) is not callable", registered)
	}

	out, err := runnable.Run(&wiringToolCtx{Context: context.Background()}, map[string]any{
		"tool":            "probe",
		"expect_contains": "ready",
	})
	if err != nil {
		t.Fatalf("wait_and_verify against a bound catalog: %v", err)
	}
	if verified, _ := out["verified"].(bool); !verified {
		t.Errorf("result = %v, want verified against the sibling tool", out)
	}
}

// The refusal has to hold through the agent's wrapper stack too — the
// serializer and the instrumenter must not mask the read-only
// classification the waiter checks.
func TestNew_WaitAndVerifyStillRefusesAMutatingSibling(t *testing.T) {
	t.Parallel()
	waiter, err := tools.NewWaitAndVerifyTool(config.DefaultConfig(), tools.WaitAndVerifyOptions{})
	if err != nil {
		t.Fatalf("NewWaitAndVerifyTool: %v", err)
	}
	a, err := New(minimalLLM{}, WithTools([]adktool.Tool{waiter, &mutatingProbe{}}))
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	var runnable interface {
		Run(adktool.Context, any) (map[string]any, error)
	}
	for _, tl := range a.Tools() {
		if tl.Name() == tools.WaitAndVerifyToolName {
			runnable, _ = tl.(interface {
				Run(adktool.Context, any) (map[string]any, error)
			})
		}
	}
	if runnable == nil {
		t.Fatal("wait_and_verify is not callable in the agent's tool list")
	}
	_, err = runnable.Run(&wiringToolCtx{Context: context.Background()}, map[string]any{
		"tool":            "apply_manifest",
		"expect_contains": "ok",
	})
	if err == nil || !strings.Contains(err.Error(), "not classified read-only") {
		t.Fatalf("want a refusal through the wrapper stack, got %v", err)
	}
}

type mutatingProbe struct{}

func (m *mutatingProbe) Name() string        { return "apply_manifest" }
func (m *mutatingProbe) Description() string { return "test mutator" }
func (m *mutatingProbe) IsLongRunning() bool { return false }
func (m *mutatingProbe) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{Name: "apply_manifest"}
}

func (m *mutatingProbe) Run(adktool.Context, any) (map[string]any, error) {
	return map[string]any{"applied": true}, nil
}

// wiringToolCtx is a minimal tool.Context for driving a registered
// tool's Run directly. Full-interface satisfaction is deliberate: an
// ADK bump that adds a method should break the stub rather than
// silently drift.
type wiringToolCtx struct {
	context.Context
}

func (c *wiringToolCtx) UserContent() *genai.Content          { return nil }
func (c *wiringToolCtx) InvocationID() string                 { return "test-invocation" }
func (c *wiringToolCtx) AgentName() string                    { return "test-agent" }
func (c *wiringToolCtx) ReadonlyState() session.ReadonlyState { return nil }
func (c *wiringToolCtx) UserID() string                       { return "test-user" }
func (c *wiringToolCtx) AppName() string                      { return "test-app" }
func (c *wiringToolCtx) SessionID() string                    { return "test-session" }
func (c *wiringToolCtx) Branch() string                       { return "" }
func (c *wiringToolCtx) Artifacts() adkagent.Artifacts        { return nil }
func (c *wiringToolCtx) State() session.State                 { return nil }
func (c *wiringToolCtx) FunctionCallID() string               { return "call-1" }
func (c *wiringToolCtx) Actions() *session.EventActions       { return &session.EventActions{} }
func (c *wiringToolCtx) ToolConfirmation() *toolconfirmation.ToolConfirmation {
	return nil
}
func (c *wiringToolCtx) RequestConfirmation(string, any) error { return nil }
func (c *wiringToolCtx) SearchMemory(context.Context, string) (*memory.SearchResponse, error) {
	return nil, nil
}
