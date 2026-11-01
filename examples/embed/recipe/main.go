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

// Command recipe is the reference one-call recipe embedder for core-agent
// (#1404).
//
// Compared to examples/embed/slim (which wires models.Adapt, permissions.Gate,
// tools.GateToolset, usage.TurnTap, and agent.New step by step), this example
// uses agent.RunRecipe to load a recipe directory (.agents/config.json,
// .agents/subagents/*.md, and AGENTS.md), gate custom tools, execute a
// single-turn structured subagent alongside a custom tool, and return a typed
// OneShotResult in a single call — while remaining inside the slim dependency
// boundary enforced by dev/tools/verify-slim-deps.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"sync"

	"github.com/go-steer/core-models/llm"
	adkagent "google.golang.org/adk/v2/agent"
	adktool "google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v3/pkg/agent"
	"github.com/go-steer/core-agent/v3/pkg/models"
)

type statusArgs struct {
	Service string `json:"service" jsonschema:"Name of the service to inspect."`
}

type statusResult struct {
	Service string `json:"service"`
	Healthy bool   `json:"healthy"`
	Latency string `json:"latency"`
}

// scriptedCoreModel implements github.com/go-steer/core-models/llm.LLM for an
// offline, deterministic three-step recipe turn:
//  1. Root agent calls the custom get_service_status tool.
//  2. Root agent delegates to the Markdown-authored reviewer subagent
//     (mode: single_turn, output_schema: ../schemas/verdict.json).
//  3. Reviewer subagent returns a schema-validated JSON object, and root agent
//     emits the final JSON report.
type scriptedCoreModel struct {
	mu   sync.Mutex
	step int
}

func (m *scriptedCoreModel) Name() string { return "gemini-2.5-flash" }

func (m *scriptedCoreModel) GenerateContent(_ context.Context, _ *llm.Request, _ bool) iter.Seq2[*llm.Response, error] {
	return func(yield func(*llm.Response, error) bool) {
		m.mu.Lock()
		m.step++
		step := m.step
		m.mu.Unlock()

		switch step {
		case 1:
			yield(&llm.Response{
				Content: &genai.Content{
					Role: genai.RoleModel,
					Parts: []*genai.Part{
						{
							FunctionCall: &genai.FunctionCall{
								ID:   "call-status-1",
								Name: "get_service_status",
								Args: map[string]any{"service": "checkout"},
							},
						},
					},
				},
				UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
					PromptTokenCount:        1_000,
					CachedContentTokenCount: 400,
					CandidatesTokenCount:    50,
					TotalTokenCount:         1_050,
				},
				TurnComplete: true,
			}, nil)
		case 2:
			yield(&llm.Response{
				Content: &genai.Content{
					Role: genai.RoleModel,
					Parts: []*genai.Part{
						{
							FunctionCall: &genai.FunctionCall{
								ID:   "call-reviewer-1",
								Name: "reviewer",
								Args: map[string]any{"request": "Evaluate checkout service health (healthy=true, latency=9ms)."},
							},
						},
					},
				},
				UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
					PromptTokenCount:        1_200,
					CachedContentTokenCount: 800,
					CandidatesTokenCount:    40,
					TotalTokenCount:         1_240,
				},
				TurnComplete: true,
			}, nil)
		case 3:
			// Subagent (mode: single_turn, output_schema: ../schemas/verdict.json).
			yield(&llm.Response{
				Content: genai.NewContentFromText(
					`{"verdict":"healthy","confidence":99}`,
					genai.RoleModel,
				),
				UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
					PromptTokenCount:     500,
					CandidatesTokenCount: 25,
					TotalTokenCount:      525,
				},
				TurnComplete: true,
			}, nil)
		default:
			yield(&llm.Response{
				Content: genai.NewContentFromText(
					`{"service":"checkout","verdict":"healthy","latency":"9ms"}`,
					genai.RoleModel,
				),
				UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
					PromptTokenCount:        1_400,
					CachedContentTokenCount: 1_000,
					CandidatesTokenCount:    35,
					TotalTokenCount:         1_435,
				},
				TurnComplete: true,
			}, nil)
		}
	}
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "embed/recipe: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	ctx := context.Background()

	recipeDir, err := writeSampleRecipeDir()
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(recipeDir) }()

	statusTool, err := functiontool.New(functiontool.Config{
		Name:        "get_service_status",
		Description: "Return health and latency for a named service.",
	}, func(_ adkagent.Context, args statusArgs) (statusResult, error) {
		return statusResult{
			Service: args.Service,
			Healthy: true,
			Latency: "9ms",
		}, nil
	})
	if err != nil {
		return fmt.Errorf("build tool: %w", err)
	}

	res, err := agent.RunRecipe(ctx, recipeDir, agent.OneShotConfig{
		Model:     models.Adapt(&scriptedCoreModel{}),
		Tools:     []adktool.Tool{statusTool},
		SessionID: "recipe-demo-session",
	}, "Check the checkout service and review its health.")
	if err != nil {
		return fmt.Errorf("RunRecipe: %w", err)
	}

	if res.Output["verdict"] != "healthy" {
		return fmt.Errorf("unexpected output verdict: %v", res.Output)
	}
	if res.Usage.Turns != 4 {
		return fmt.Errorf("Usage.Turns = %d, want 4 (3 root + 1 subagent)", res.Usage.Turns)
	}

	out, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(out))
	return nil
}

func writeSampleRecipeDir() (string, error) {
	dir, err := os.MkdirTemp("", "core-agent-embed-recipe-*")
	if err != nil {
		return "", err
	}
	subagentsDir := filepath.Join(dir, ".agents", "subagents")
	schemasDir := filepath.Join(dir, ".agents", "schemas")
	if err := os.MkdirAll(subagentsDir, 0o755); err != nil {
		return "", err
	}
	if err := os.MkdirAll(schemasDir, 0o755); err != nil {
		return "", err
	}

	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte(
		"You are an SRE triage recipe. Inspect service status and consult the reviewer subagent before reporting.\n",
	), 0o644); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, ".agents", "config.json"), []byte(`{
  "version": 1,
  "model": { "name": "gemini-2.5-flash" },
  "permissions": {
    "mode": "allow",
    "allow": [
      "get_service_status:*",
      "spawn_agent:*"
    ]
  }
}
`), 0o644); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(schemasDir, "verdict.json"), []byte(`{
  "type": "object",
  "properties": {
    "verdict": { "type": "string" },
    "confidence": { "type": "integer" }
  },
  "required": ["verdict", "confidence"]
}
`), 0o644); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(subagentsDir, "reviewer.md"), []byte(`---
description: Single-turn health verdict classifier.
mode: single_turn
output_schema: ../schemas/verdict.json
---
Classify the service health report and return a JSON object matching the output schema.
`), 0o644); err != nil {
		return "", err
	}
	return dir, nil
}
