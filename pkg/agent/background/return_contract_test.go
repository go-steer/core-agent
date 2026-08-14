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
	"iter"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/genai"
)

// returningLLM calls the named return tool once with the given payload,
// then narrates itself forever. The narration is the point: it
// reproduces the 2026-08-13 GKE UAT shape where the subagent produced a
// correct RCA and then, re-driven by "continue", degraded into
// "standing by in a healthy, inactive state" — which was what the
// parent actually received.
type returningLLM struct {
	toolName string
	args     map[string]any
	turn     atomic.Int32
}

func (*returningLLM) Name() string { return "returning" }

func (l *returningLLM) GenerateContent(_ context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		if l.turn.Add(1) == 1 {
			fc := &genai.FunctionCall{Name: l.toolName, Args: l.args}
			content := &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: fc}}}
			yield(&adkmodel.LLMResponse{Content: content, FinishReason: genai.FinishReasonStop, TurnComplete: true}, nil)
			return
		}
		content := &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "standing by in a healthy, inactive state"}}}
		yield(&adkmodel.LLMResponse{Content: content, FinishReason: genai.FinishReasonStop, TurnComplete: true}, nil)
	}
}

// TestSubagentReturn_EveryAliasEndsTheRunWithItsPayload is the #728
// regression at the level that matters: a subagent that reaches for ANY
// of the four names it might plausibly reach for must end the run and
// hand its findings to the parent.
//
// Runs in ModeStanding because since #730 that is the mode the return
// tool belongs to: a bounded delegation registers no return tool at all
// (it ends by running out of tool calls), so there is no alias set to
// get wrong. The aliases still matter for a standing worker, whose only
// way out short of a budget IS the tool.
//
// Fails on pre-fix code for three of the four:
//
//   - report_completed pushed a "completed" alert and returned ok
//     WITHOUT ending the run, so the loop re-drove the model and the
//     status line above became the returned value (status "deferred",
//     output "standing by...").
//   - mark_task_done was not registered on subagents at all, so the call
//     errored with tool-not-found and the run went the same way.
//   - return_result did not exist.
//
// Only report_done worked, and only if the model happened to pick it out
// of three near-synonyms.
func TestSubagentReturn_EveryAliasEndsTheRunWithItsPayload(t *testing.T) {
	t.Parallel()
	const findings = "RCA: emailservice image tag does-not-exist:v0-demo-break is invalid; revert to emailservice:v0.10.5"

	cases := []struct {
		tool string
		args map[string]any
	}{
		{"return_result", map[string]any{"result": findings}},
		{"report_done", map[string]any{"state": "done", "detail": findings}},
		{"report_completed", map[string]any{"result": findings}},
		{"mark_task_done", map[string]any{"detail": findings}},
	}
	for _, tc := range cases {
		t.Run(tc.tool, func(t *testing.T) {
			t.Parallel()
			prov := &recordingProvider{llm: &returningLLM{toolName: tc.tool, args: tc.args}}
			mgr := newTemplateManager(t, prov, []SubagentTemplate{{
				Name:         "cluster",
				Instruction:  "triage",
				ModelFactory: tmplFactory(prov, "cluster-model"),
				ModelID:      "cluster-model",
				Mode:         ModeStanding,
			}}, WithDefaultBudgets(Budgets{MaxTurns: 4}), WithSyncWaitTimeout(10*time.Second))
			attachEchoParent(t, mgr)
			defer mgr.Close()

			h, err := mgr.SpawnTemplate(context.Background(), "", "cluster", RefOverrides{Goal: "triage emailservice"}, "")
			if err != nil {
				t.Fatalf("SpawnTemplate: %v", err)
			}
			res := mgr.awaitResult(context.Background(), h)

			if res.Status != "completed" {
				t.Errorf("status = %q, want completed — %s must terminate the run, not ack and let it re-drive", res.Status, tc.tool)
			}
			if !strings.Contains(res.Output, "does-not-exist:v0-demo-break") {
				t.Errorf("output = %q, want the findings passed to %s", res.Output, tc.tool)
			}
		})
	}
}

// TestSubagentReturn_ToolsAreOfferedUnderEveryAlias pins the declared
// surface: a model reaching for any of these names must find it, since
// picking the wrong one is the failure #728 exists to remove. Standing
// mode, for the reason above: bounded delegations declare none of them.
func TestSubagentReturn_ToolsAreOfferedUnderEveryAlias(t *testing.T) {
	t.Parallel()
	capture := &declCapturingLLM{}
	prov := &recordingProvider{llm: capture}
	mgr := newTemplateManager(t, prov, []SubagentTemplate{{
		Name:         "cluster",
		Instruction:  "triage",
		ModelFactory: tmplFactory(prov, "cluster-model"),
		ModelID:      "cluster-model",
		Mode:         ModeStanding,
	}}, WithDefaultBudgets(Budgets{MaxTurns: 1}))
	attachEchoParent(t, mgr)
	defer mgr.Close()

	h, err := mgr.SpawnTemplate(context.Background(), "", "cluster", RefOverrides{Goal: "g"}, "")
	if err != nil {
		t.Fatalf("SpawnTemplate: %v", err)
	}
	waitDone(t, h)

	want := append([]string{"return_result"}, subagentReturnToolAliases...)
	for _, name := range want {
		if _, ok := capture.declaration(name); !ok {
			t.Errorf("spawned subagent was not offered %q", name)
		}
	}
	// report_alert stays a distinct tool: a mid-run alert is genuinely
	// a different message from a return, and folding it in would let a
	// progress note end the run.
	if _, ok := capture.declaration("report_alert"); !ok {
		t.Error("report_alert must remain available for mid-run reporting")
	}
}

// TestSubagentReturn_ContractIsInTheInstruction is the #727 regression.
// The return contract has to reach the model on paths where no tool
// description does — a natural stop, a budget cap, a watchdog halt —
// because on all of those the subagent's last assistant text is what the
// parent receives.
//
// Fails on pre-fix code: the framing lived only in the done tool's
// description (#641), so a subagent that never read or called that tool
// never learned its output was a return value.
//
// Standing mode, so the contract names the return tool; the bounded
// form of the same assertion is in bounded_test.go.
func TestSubagentReturn_ContractIsInTheInstruction(t *testing.T) {
	t.Parallel()
	capture := &declCapturingLLM{}
	prov := &recordingProvider{llm: capture}
	mgr := newTemplateManager(t, prov, []SubagentTemplate{{
		Name:         "cluster",
		Instruction:  "triage",
		ModelFactory: tmplFactory(prov, "cluster-model"),
		ModelID:      "cluster-model",
		Mode:         ModeStanding,
	}}, WithDefaultBudgets(Budgets{MaxTurns: 1}))
	attachEchoParent(t, mgr)
	defer mgr.Close()

	h, err := mgr.SpawnTemplate(context.Background(), "", "cluster", RefOverrides{Goal: "g"}, "")
	if err != nil {
		t.Fatalf("SpawnTemplate: %v", err)
	}
	waitDone(t, h)

	sys := capture.systemInstruction()
	if sys == "" {
		t.Fatal("captured no system instruction")
	}
	for _, want := range []string{
		"value returned to it",
		"return_result",
		"LAST message is the findings",
	} {
		if !strings.Contains(sys, want) {
			t.Errorf("system instruction missing %q; got:\n%s", want, sys)
		}
	}
	// The spec's own persona must survive alongside the contract —
	// WithExtraInstruction appends, it does not replace.
	if !strings.Contains(sys, "triage") {
		t.Error("the subagent's own persona was lost")
	}
}

// TestSubagentReturn_ContractSurvivesReplaceSystemPrompt covers the
// other launch path (an ad-hoc Spawn from a catalog Spec) in its most
// hostile arrangement: replace_system_prompt swaps out instruction
// layers 1–3 wholesale. The return contract is a property of being a
// delegation, not of the harness baseline, so it has to hold for a
// subagent running a bare operator-authored prompt too. Standing mode,
// so the tool-naming form of the contract is the one under test.
func TestSubagentReturn_ContractSurvivesReplaceSystemPrompt(t *testing.T) {
	t.Parallel()
	capture := &declCapturingLLM{}
	prov := &recordingProvider{llm: capture}
	mgr, err := NewManager(WithProvider(prov, "parent-model"),
		WithDefaultBudgets(Budgets{MaxTurns: 1}))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	attachEchoParent(t, mgr)
	defer mgr.Close()

	h, err := mgr.Spawn(context.Background(), "", Spec{
		Name:                "adhoc",
		SystemPrompt:        "you are a bare prompt",
		ReplaceSystemPrompt: true,
		Goal:                "g",
		Mode:                ModeStanding,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	waitDone(t, h)

	sys := capture.systemInstruction()
	if !strings.Contains(sys, "you are a bare prompt") {
		t.Errorf("replace_system_prompt did not take effect; got:\n%s", sys)
	}
	if !strings.Contains(sys, "return_result") {
		t.Errorf("return contract missing under replace_system_prompt; got:\n%s", sys)
	}
	if _, ok := capture.declaration("return_result"); !ok {
		t.Error("ad-hoc spawned subagent was not offered return_result")
	}
}
