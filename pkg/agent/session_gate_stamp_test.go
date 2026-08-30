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
	"sync/atomic"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

// Run stamps this agent's gate onto the turn context, and that stamp
// is the whole mechanism of per-session permission isolation in a
// multi-session daemon: MCP toolsets and the built-in tool registry
// are constructed once at startup against the daemon's template gate,
// so the only thing that redirects a tenant's tool call to that
// tenant's mode, approvals, prompter and plan-first state is
// resolveSessionGate reading this value back off the context.
//
// It was unpinned until #825 went looking for a bug on the other end
// of the same chain. Deleting the stamp broke no test: every symptom
// showed up somewhere else entirely — plan-first unsatisfiable for
// every tenant, one tenant's approvals answering for the next,
// ask-mode prompts routed to a broker nobody is subscribed to — with
// nothing pointing back here. See
// pkg/agent/background/session_gate_test.go for the far end.

// gateReadingLLM calls the probe tool once, then finishes.
type gateReadingLLM struct {
	calls atomic.Int32
	tool  string
}

func (*gateReadingLLM) Name() string { return "gate-reader" }

func (l *gateReadingLLM) GenerateContent(_ context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	n := l.calls.Add(1)
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		if n == 1 {
			yield(&adkmodel.LLMResponse{Content: &genai.Content{
				Role:  genai.RoleModel,
				Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{Name: l.tool, Args: map[string]any{}}}},
			}}, nil)
			return
		}
		yield(&adkmodel.LLMResponse{
			Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "done"}}},
			TurnComplete: true,
		}, nil)
	}
}

// runGateProbe runs one turn against an agent built with opts and
// reports the session gate its tool saw on the context.
func runGateProbe(t *testing.T, opts ...Option) (*permissions.Gate, bool) {
	t.Helper()
	const name = "gate_probe"
	seen := make(chan *permissions.Gate, 1)
	type empty struct{}
	probe, err := functiontool.New(
		functiontool.Config{Name: name, Description: "report the session gate on ctx"},
		func(ctx tool.Context, _ empty) (empty, error) {
			g, _ := permissions.SessionGateFromContext(ctx)
			select {
			case seen <- g:
			default:
			}
			return empty{}, nil
		},
	)
	if err != nil {
		t.Fatalf("functiontool.New: %v", err)
	}

	a, err := New(&gateReadingLLM{tool: name}, append([]Option{WithTools([]tool.Tool{probe})}, opts...)...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, err := range a.Run(context.Background(), "go") {
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	}
	select {
	case g := <-seen:
		return g, true
	case <-time.After(time.Second):
		return nil, false
	}
}

// TestRun_StampsTheAgentsGateOnTheTurnContext is the head of the
// chain. Without it, a tool constructed against the daemon's template
// gate has no way to learn whose session it is running for.
func TestRun_StampsTheAgentsGateOnTheTurnContext(t *testing.T) {
	t.Parallel()
	template := permissions.New(permissions.Options{Mode: permissions.ModeYolo})
	sessionGate := template.DeriveForSession("alice", nil)

	got, ran := runGateProbe(t, WithGate(sessionGate))
	if !ran {
		t.Fatal("the tool never ran")
	}
	if got != sessionGate {
		t.Fatalf("session gate on the tool context = %p, want the agent's own gate %p — "+
			"a tool built against the daemon template gate cannot reach this session's "+
			"mode, approvals, prompter or plan-first state without it", got, sessionGate)
	}
}

// TestRun_NoGateLeavesTheContextClean pins the other half: a
// single-user host that wired no gate must not have a typed-nil
// stamped on its contexts, which SessionGateFromContext would then
// have to defend against on every check.
func TestRun_NoGateLeavesTheContextClean(t *testing.T) {
	t.Parallel()
	got, ran := runGateProbe(t)
	if !ran {
		t.Fatal("the tool never ran")
	}
	if got != nil {
		t.Fatalf("session gate on the tool context = %p, want none for an agent with no gate", got)
	}
}
