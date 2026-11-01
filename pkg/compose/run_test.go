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

package compose

import (
	"context"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

type composeTestLLM struct {
	mu          sync.Mutex
	step        int
	steps       []func(*adkmodel.LLMRequest) (*adkmodel.LLMResponse, error)
	lastSysInst string
	lastTools   []string
	reply       string
}

func (m *composeTestLLM) Name() string { return "gemini-2.5-flash" }

func (m *composeTestLLM) GenerateContent(_ context.Context, req *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		m.mu.Lock()
		if req != nil && req.Config != nil {
			if req.Config.SystemInstruction != nil {
				var parts []string
				for _, p := range req.Config.SystemInstruction.Parts {
					if p != nil && p.Text != "" {
						parts = append(parts, p.Text)
					}
				}
				m.lastSysInst = strings.Join(parts, "\n")
			}
			var toolNames []string
			for _, gt := range req.Config.Tools {
				if gt == nil {
					continue
				}
				for _, fd := range gt.FunctionDeclarations {
					if fd != nil {
						toolNames = append(toolNames, fd.Name)
					}
				}
			}
			m.lastTools = toolNames
		}
		var fn func(*adkmodel.LLMRequest) (*adkmodel.LLMResponse, error)
		if m.step < len(m.steps) {
			fn = m.steps[m.step]
			m.step++
		}
		reply := m.reply
		m.mu.Unlock()

		if fn != nil {
			resp, err := fn(req)
			yield(resp, err)
			return
		}
		yield(&adkmodel.LLMResponse{
			Content:      genai.NewContentFromText(reply, genai.RoleModel),
			TurnComplete: true,
		}, nil)
	}
}

func TestRunRecipe_ExpandsInstructionsViaInstructionPackage(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	agentsD := filepath.Join(dir, "AGENTS.d")
	if err := os.MkdirAll(agentsD, 0o755); err != nil {
		t.Fatalf("MkdirAll AGENTS.d: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("Primary recipe rule.\n@include rules/extra.md"), 0o644); err != nil {
		t.Fatalf("WriteFile AGENTS.md: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "rules"), 0o755); err != nil {
		t.Fatalf("MkdirAll rules: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rules", "extra.md"), []byte("Included rule from rules/extra.md."), 0o644); err != nil {
		t.Fatalf("WriteFile extra.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentsD, "01-overlay.md"), []byte("Overlay rule from AGENTS.d."), 0o644); err != nil {
		t.Fatalf("WriteFile 01-overlay.md: %v", err)
	}

	llm := &composeTestLLM{reply: `{"status":"ready"}`}
	res, err := RunWorkload(context.Background(), dir, RunConfig{
		Model: llm,
	}, "Check workload readiness.")
	if err != nil {
		t.Fatalf("RunWorkload: %v", err)
	}
	if res.Output["status"] != "ready" {
		t.Errorf("Output[status] = %v, want ready", res.Output["status"])
	}
	for _, want := range []string{
		"Primary recipe rule.",
		"Included rule from rules/extra.md.",
		"Overlay rule from AGENTS.d.",
	} {
		if !strings.Contains(llm.lastSysInst, want) {
			t.Errorf("system instruction missing %q; got:\n%s", want, llm.lastSysInst)
		}
	}
}

func TestRunRecipe_WiresSkillsAndExpandsSubagentIncludes(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	dotAgents := filepath.Join(dir, ".agents")
	skillDir := filepath.Join(dotAgents, "skills", "triage-runbook")
	subDir := filepath.Join(dotAgents, "subagents")
	rulesDir := filepath.Join(dir, "rules")
	for _, d := range []string{skillDir, subDir, rulesDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("MkdirAll %s: %v", d, err)
		}
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(`---
name: triage-runbook
description: Standard triage steps.
---
Always check error rates first.
`), 0o644); err != nil {
		t.Fatalf("WriteFile SKILL.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(rulesDir, "specialist.md"), []byte("Specialist included directive: verify all pod logs."), 0o644); err != nil {
		t.Fatalf("WriteFile specialist.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(subDir, "specialist.md"), []byte(`---
description: Specialist subagent.
mode: single_turn
---
You are the specialist.
@include rules/specialist.md
`), 0o644); err != nil {
		t.Fatalf("WriteFile subagents/specialist.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dotAgents, "config.json"), []byte(`{
  "version": 1,
  "model": {"name": "gemini-2.5-flash"},
  "permissions": {"mode": "yolo"}
}`), 0o644); err != nil {
		t.Fatalf("WriteFile config.json: %v", err)
	}

	var rootTools []string
	var subSysInst string
	llm := &composeTestLLM{
		steps: []func(*adkmodel.LLMRequest) (*adkmodel.LLMResponse, error){
			func(req *adkmodel.LLMRequest) (*adkmodel.LLMResponse, error) {
				for _, gt := range req.Config.Tools {
					for _, fd := range gt.FunctionDeclarations {
						rootTools = append(rootTools, fd.Name)
					}
				}
				return &adkmodel.LLMResponse{
					Content: &genai.Content{
						Role: genai.RoleModel,
						Parts: []*genai.Part{
							{FunctionCall: &genai.FunctionCall{
								ID:   "call-spec",
								Name: "specialist",
								Args: map[string]any{"request": "Check pod logs"},
							}},
						},
					},
					TurnComplete: true,
				}, nil
			},
			func(req *adkmodel.LLMRequest) (*adkmodel.LLMResponse, error) {
				if req.Config != nil && req.Config.SystemInstruction != nil {
					for _, p := range req.Config.SystemInstruction.Parts {
						if p != nil && p.Text != "" {
							subSysInst += p.Text + "\n"
						}
					}
				}
				return &adkmodel.LLMResponse{
					Content:      genai.NewContentFromText("All pod logs verified clean.", genai.RoleModel),
					TurnComplete: true,
				}, nil
			},
			func(_ *adkmodel.LLMRequest) (*adkmodel.LLMResponse, error) {
				return &adkmodel.LLMResponse{
					Content:      genai.NewContentFromText("Done: all pod logs verified clean.", genai.RoleModel),
					TurnComplete: true,
				}, nil
			},
		},
	}

	res, err := RunRecipe(context.Background(), dir, RunConfig{
		Model: llm,
	}, "Run specialist check.")
	if err != nil {
		t.Fatalf("RunRecipe: %v", err)
	}
	if !strings.Contains(res.Text, "all pod logs verified clean") {
		t.Errorf("Text = %q, want pod logs summary", res.Text)
	}
	hasSkillTool := false
	for _, name := range rootTools {
		if name == "list_skills" || name == "load_skill" {
			hasSkillTool = true
			break
		}
	}
	if !hasSkillTool {
		t.Errorf("root tools %v missing skill toolset (list_skills/load_skill)", rootTools)
	}
	if !strings.Contains(subSysInst, "Specialist included directive: verify all pod logs.") {
		t.Errorf("subagent system instruction missing @include content; got:\n%s", subSysInst)
	}
}
