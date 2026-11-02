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

func TestRunRecipe_SubagentSkillsScopingAndRootedSubagent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	dotAgents := filepath.Join(dir, ".agents")
	alphaDir := filepath.Join(dotAgents, "skills", "alpha-skill")
	betaDir := filepath.Join(dotAgents, "skills", "beta-skill")
	rootBundleDir := filepath.Join(dotAgents, "roots", "auditor")
	rootSkillDir := filepath.Join(rootBundleDir, "skills", "auditor-only")
	for _, d := range []string{alphaDir, betaDir, rootSkillDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("MkdirAll %s: %v", d, err)
		}
	}

	if err := os.WriteFile(filepath.Join(alphaDir, "SKILL.md"), []byte("---\nname: alpha-skill\ndescription: Alpha skill.\n---\nAlpha body.\n"), 0o644); err != nil {
		t.Fatalf("WriteFile alpha SKILL.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(betaDir, "SKILL.md"), []byte("---\nname: beta-skill\ndescription: Beta skill.\n---\nBeta body.\n"), 0o644); err != nil {
		t.Fatalf("WriteFile beta SKILL.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootBundleDir, "AGENTS.md"), []byte("Rooted auditor persona from roots/auditor/AGENTS.md."), 0o644); err != nil {
		t.Fatalf("WriteFile root AGENTS.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootSkillDir, "SKILL.md"), []byte("---\nname: auditor-only\ndescription: Auditor skill.\n---\nAuditor skill body.\n"), 0o644); err != nil {
		t.Fatalf("WriteFile auditor-only SKILL.md: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dotAgents, "config.json"), []byte(`{
  "version": 1,
  "model": { "name": "gemini-2.5-flash" },
  "permissions": { "mode": "yolo" },
  "subagents": [
    {
      "name": "scoped_sub",
      "description": "Subagent scoped to alpha-skill only.",
      "skills": ["alpha-skill"],
      "instructions": "Scoped subagent."
    },
    {
      "name": "rooted_sub",
      "description": "Rooted subagent with its own AGENTS.md and skills.",
      "root": "roots/auditor"
    }
  ]
}`), 0o644); err != nil {
		t.Fatalf("WriteFile config.json: %v", err)
	}

	var rootedSysInst string
	var scopedListSkillsResp string
	var rootedListSkillsResp string

	llm := &composeTestLLM{
		steps: []func(*adkmodel.LLMRequest) (*adkmodel.LLMResponse, error){
			// Step 0 (root): calls scoped_sub.
			func(_ *adkmodel.LLMRequest) (*adkmodel.LLMResponse, error) {
				return &adkmodel.LLMResponse{
					Content: &genai.Content{
						Role: genai.RoleModel,
						Parts: []*genai.Part{
							{FunctionCall: &genai.FunctionCall{ID: "c1", Name: "scoped_sub", Args: map[string]any{"request": "list your skills"}}},
						},
					},
					TurnComplete: true,
				}, nil
			},
			// Step 1 (scoped_sub): calls list_skills.
			func(_ *adkmodel.LLMRequest) (*adkmodel.LLMResponse, error) {
				return &adkmodel.LLMResponse{
					Content: &genai.Content{
						Role: genai.RoleModel,
						Parts: []*genai.Part{
							{FunctionCall: &genai.FunctionCall{ID: "s1", Name: "list_skills", Args: map[string]any{}}},
						},
					},
					TurnComplete: true,
				}, nil
			},
			// Step 2 (scoped_sub): observes list_skills response and returns.
			func(req *adkmodel.LLMRequest) (*adkmodel.LLMResponse, error) {
				scopedListSkillsResp = flattenFunctionResponses(req)
				return &adkmodel.LLMResponse{
					Content:      genai.NewContentFromText("scoped done", genai.RoleModel),
					TurnComplete: true,
				}, nil
			},
			// Step 3 (root): calls rooted_sub.
			func(_ *adkmodel.LLMRequest) (*adkmodel.LLMResponse, error) {
				return &adkmodel.LLMResponse{
					Content: &genai.Content{
						Role: genai.RoleModel,
						Parts: []*genai.Part{
							{FunctionCall: &genai.FunctionCall{ID: "c2", Name: "rooted_sub", Args: map[string]any{"request": "list your skills"}}},
						},
					},
					TurnComplete: true,
				}, nil
			},
			// Step 4 (rooted_sub): captures system instruction and calls list_skills.
			func(req *adkmodel.LLMRequest) (*adkmodel.LLMResponse, error) {
				if req.Config != nil && req.Config.SystemInstruction != nil {
					for _, p := range req.Config.SystemInstruction.Parts {
						if p != nil && p.Text != "" {
							rootedSysInst += p.Text + "\n"
						}
					}
				}
				return &adkmodel.LLMResponse{
					Content: &genai.Content{
						Role: genai.RoleModel,
						Parts: []*genai.Part{
							{FunctionCall: &genai.FunctionCall{ID: "r1", Name: "list_skills", Args: map[string]any{}}},
						},
					},
					TurnComplete: true,
				}, nil
			},
			// Step 5 (rooted_sub): observes list_skills response and returns.
			func(req *adkmodel.LLMRequest) (*adkmodel.LLMResponse, error) {
				rootedListSkillsResp = flattenFunctionResponses(req)
				return &adkmodel.LLMResponse{
					Content:      genai.NewContentFromText("rooted done", genai.RoleModel),
					TurnComplete: true,
				}, nil
			},
			// Step 6 (root): final answer.
			func(_ *adkmodel.LLMRequest) (*adkmodel.LLMResponse, error) {
				return &adkmodel.LLMResponse{
					Content:      genai.NewContentFromText("both subagents verified", genai.RoleModel),
					TurnComplete: true,
				}, nil
			},
		},
	}

	res, err := RunRecipe(context.Background(), dir, RunConfig{
		Model: llm,
	}, "Verify subagent skill scoping and rooted subagent.")
	if err != nil {
		t.Fatalf("RunRecipe: %v", err)
	}
	if !strings.Contains(res.Text, "both subagents verified") {
		t.Errorf("Text = %q, want 'both subagents verified'", res.Text)
	}
	if !strings.Contains(scopedListSkillsResp, "alpha-skill") || strings.Contains(scopedListSkillsResp, "beta-skill") {
		t.Errorf("scoped_sub list_skills = %s; want alpha-skill and NOT beta-skill", scopedListSkillsResp)
	}
	if !strings.Contains(rootedSysInst, "Rooted auditor persona from roots/auditor/AGENTS.md.") {
		t.Errorf("rooted_sub system instruction missing root AGENTS.md; got:\n%s", rootedSysInst)
	}
	if !strings.Contains(rootedListSkillsResp, "auditor-only") || strings.Contains(rootedListSkillsResp, "alpha-skill") {
		t.Errorf("rooted_sub list_skills = %s; want auditor-only and NOT alpha-skill", rootedListSkillsResp)
	}
}

func flattenFunctionResponses(req *adkmodel.LLMRequest) string {
	if req == nil {
		return ""
	}
	var sb strings.Builder
	for _, c := range req.Contents {
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			if p != nil && p.FunctionResponse != nil {
				for k, v := range p.FunctionResponse.Response {
					sb.WriteString(k)
					sb.WriteString("=")
					sb.WriteString(stringifyAny(v))
					sb.WriteString(" ")
				}
			}
		}
	}
	return sb.String()
}

func stringifyAny(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []any:
		var parts []string
		for _, elem := range x {
			parts = append(parts, stringifyAny(elem))
		}
		return strings.Join(parts, ",")
	case map[string]any:
		var parts []string
		for k, val := range x {
			parts = append(parts, k+":"+stringifyAny(val))
		}
		return strings.Join(parts, ",")
	default:
		return ""
	}
}
