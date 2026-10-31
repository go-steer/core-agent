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

// Command slim is the reference slim-embed consumer for core-agent (#1397).
//
// It exercises the minimal library path — ADK v2 + core-models +
// pkg/agent + pkg/models + pkg/permissions + pkg/tools (GateToolset) +
// pkg/usage — without pulling in the TUI (core-tui, charmbracelet), the
// MCP SDK, Prometheus, the OpenTelemetry SDK/exporters, SQLite drivers,
// or the CLI/daemon composition packages (cmd/core-agent, pkg/compose,
// pkg/runner, pkg/attachadapter, pkg/telemetry).
//
// Presubmit gate: dev/ci/presubmits/verify-slim-deps runs
// `go list -deps ./examples/embed/slim` against the denylist and
// dev/ci/presubmits/examples-smoke executes this binary offline.
package main

import (
	"context"
	"fmt"
	"iter"
	"log"
	"os"
	"strings"
	"sync/atomic"

	adkagent "google.golang.org/adk/v2/agent"
	adktool "google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"

	"github.com/go-steer/core-models/llm"

	"github.com/go-steer/core-agent/v3/pkg/agent"
	"github.com/go-steer/core-agent/v3/pkg/models"
	"github.com/go-steer/core-agent/v3/pkg/permissions"
	"github.com/go-steer/core-agent/v3/pkg/tools"
	"github.com/go-steer/core-agent/v3/pkg/usage"
)

const scriptedModelName = "gemini-2.5-flash"

type statusInput struct {
	Service string `json:"service" jsonschema:"Service identifier to inspect."`
}

type statusOutput struct {
	Service string `json:"service"`
	Healthy bool   `json:"healthy"`
	Detail  string `json:"detail"`
}

// staticToolset is a minimal adktool.Toolset so the example can wrap a
// consumer toolset with tools.GateToolset without importing pkg/mcp or
// pkg/skills.
type staticToolset struct {
	name  string
	tools []adktool.Tool
}

func (s *staticToolset) Name() string { return s.name }

func (s *staticToolset) Tools(adkagent.ReadonlyContext) ([]adktool.Tool, error) {
	return s.tools, nil
}

// scriptedLLM implements github.com/go-steer/core-models/llm.LLM for an
// offline two-step turn:
//  1. Call 1 emits a get_service_status tool call for "payments-api".
//  2. Call 2 inspects the tool response and returns the final diagnosis.
type scriptedLLM struct {
	calls atomic.Int32
}

func (m *scriptedLLM) Name() string { return scriptedModelName }

func (m *scriptedLLM) GenerateContent(_ context.Context, _ *llm.Request, _ bool) iter.Seq2[*llm.Response, error] {
	call := m.calls.Add(1)
	return func(yield func(*llm.Response, error) bool) {
		if call == 1 {
			fc := &genai.Part{
				FunctionCall: &genai.FunctionCall{
					ID:   "call-status-1",
					Name: "get_service_status",
					Args: map[string]any{"service": "payments-api"},
				},
			}
			yield(&llm.Response{
				Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{fc}},
				TurnComplete: true,
				UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
					PromptTokenCount:     120,
					CandidatesTokenCount: 25,
					TotalTokenCount:      145,
				},
			}, nil)
			return
		}
		text := genai.NewContentFromText(
			"payments-api is healthy (3/3 replicas ready).",
			genai.RoleModel,
		)
		yield(&llm.Response{
			Content:      text,
			TurnComplete: true,
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
				PromptTokenCount:     180,
				CandidatesTokenCount: 20,
				TotalTokenCount:      200,
			},
		}, nil)
	}
}

func main() {
	if err := run(context.Background()); err != nil {
		log.Fatalf("slim: %v", err)
	}
}

func run(ctx context.Context) error {
	var toolInvocations atomic.Int32
	statusTool, err := functiontool.New(functiontool.Config{
		Name:        "get_service_status",
		Description: "Return replica readiness for a named service.",
	}, func(_ adkagent.Context, in statusInput) (statusOutput, error) {
		toolInvocations.Add(1)
		return statusOutput{
			Service: in.Service,
			Healthy: true,
			Detail:  "3/3 replicas ready",
		}, nil
	})
	if err != nil {
		return fmt.Errorf("build tool: %w", err)
	}

	// Gate in allowlist mode: ops:get_service_status* is permitted;
	// anything outside the allowlist is denied without prompting.
	policy, err := permissions.NewPolicy([]string{"ops:get_service_status*"}, nil)
	if err != nil {
		return fmt.Errorf("build policy: %w", err)
	}
	gate := permissions.New(permissions.Options{
		Mode:   permissions.ModeAllow,
		Policy: policy,
	})

	// Verify the gate blocks unallowlisted calls in the same namespace.
	if err := gate.CheckToolCallWithArgs(
		ctx,
		"ops",
		"delete_service",
		"delete_service {\"service\":\"payments-api\"}",
		map[string]any{"service": "payments-api"},
	); err == nil {
		return fmt.Errorf("expected gate to deny unallowlisted ops:delete_service call")
	}

	gatedToolset := tools.GateToolset(&staticToolset{
		name:  "ops",
		tools: []adktool.Tool{statusTool},
	}, gate, "ops")

	tracker := usage.NewTracker()
	llmModel := models.Adapt(&scriptedLLM{})

	a, err := agent.New(
		llmModel,
		agent.WithMode(agent.ModeAutonomous),
		agent.WithExtraInstruction("Inspect service readiness before reporting."),
		agent.WithGate(gate),
		agent.WithUsageTracker(tracker),
		agent.WithToolsets([]adktool.Toolset{gatedToolset}),
	)
	if err != nil {
		return fmt.Errorf("agent.New: %w", err)
	}

	rate := usage.PriceFor(scriptedModelName, nil)
	var (
		tap       usage.TurnTap
		finalText strings.Builder
	)
	for ev, err := range a.Run(ctx, "Check payments-api readiness.") {
		if err != nil {
			return fmt.Errorf("agent.Run: %w", err)
		}
		tap.Observe(ev)
		if turnUsage, ok := tap.Commit(ev); ok {
			tracker.AppendUsage(scriptedModelName, turnUsage, rate)
		}
		if ev == nil || ev.Content == nil || ev.Content.Role != genai.RoleModel {
			continue
		}
		for _, p := range ev.Content.Parts {
			if p.Text != "" && !ev.Partial {
				finalText.WriteString(p.Text)
			}
		}
	}

	if got := toolInvocations.Load(); got != 1 {
		return fmt.Errorf("tool invocations = %d, want 1", got)
	}
	reply := strings.TrimSpace(finalText.String())
	if !strings.Contains(reply, "payments-api is healthy") {
		return fmt.Errorf("unexpected model reply %q", reply)
	}
	totals := tracker.Totals()
	if totals.Turns != 2 || totals.InputTokens != 300 || totals.OutputTokens != 45 {
		return fmt.Errorf("unexpected usage totals: %+v", totals)
	}
	if totals.CostUSD <= 0 {
		return fmt.Errorf("expected positive cost from built-in pricing catalog, got %f", totals.CostUSD)
	}

	fmt.Fprintf(os.Stdout, "reply: %s\n", reply)
	fmt.Fprintf(os.Stdout, "turns=%d in=%d out=%d cost=$%.6f\n",
		totals.Turns, totals.InputTokens, totals.OutputTokens, totals.CostUSD)
	return nil
}
