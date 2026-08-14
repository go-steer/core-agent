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

	coretools "github.com/go-steer/core-agent/v2/pkg/tools"
)

const (
	boundedRCA     = "RCA: emailservice image tag does-not-exist:v0-demo-break is invalid; revert to emailservice:v0.10.5"
	boundedDegrade = "standing by in a healthy, inactive state"
)

// answerThenDegradeLLM reproduces the 2026-08-13 GKE UAT shape without
// any tool call at all: turn 1 is the correct answer, and every
// "continue" after it degrades into a status line. The subagent has
// nothing left to do after turn 1 — it just isn't allowed to stop.
type answerThenDegradeLLM struct {
	turn atomic.Int32
}

func (*answerThenDegradeLLM) Name() string { return "answer-then-degrade" }

func (l *answerThenDegradeLLM) GenerateContent(_ context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	text := boundedDegrade
	if l.turn.Add(1) == 1 {
		text = boundedRCA
	}
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		content := &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: text}}}
		yield(&adkmodel.LLMResponse{Content: content, FinishReason: genai.FinishReasonStop, TurnComplete: true}, nil)
	}
}

// toolThenAnswerLLM uses a tool before answering: the ordinary shape of
// real work. Turn 1 calls the tool; when the tool result comes back it
// produces the answer, still inside the same runOneTurn.
type toolThenAnswerLLM struct {
	calls atomic.Int32
}

func (*toolThenAnswerLLM) Name() string { return "tool-then-answer" }

func (l *toolThenAnswerLLM) GenerateContent(_ context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	n := l.calls.Add(1)
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		var content *genai.Content
		if n == 1 {
			fc := &genai.FunctionCall{Name: "report_alert", Args: map[string]any{"text": "looking"}}
			content = &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: fc}}}
		} else {
			content = &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: boundedRCA}}}
		}
		yield(&adkmodel.LLMResponse{Content: content, FinishReason: genai.FinishReasonStop, TurnComplete: true}, nil)
	}
}

// returnThenDegradeLLM is answerThenDegradeLLM with an actual return
// call on turn 1, using the alias the 2026-08-14 UAT model reached for.
// Every later turn degrades, so a run that does not stop at the return
// call hands back the status line instead of the findings.
type returnThenDegradeLLM struct {
	turn atomic.Int32
}

func (*returnThenDegradeLLM) Name() string { return "return-then-degrade" }

func (l *returnThenDegradeLLM) GenerateContent(_ context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	first := l.turn.Add(1) == 1
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		content := &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: boundedDegrade}}}
		if first {
			fc := &genai.FunctionCall{Name: "mark_task_done", Args: map[string]any{"result": boundedRCA}}
			content = &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: fc}}}
		}
		yield(&adkmodel.LLMResponse{Content: content, FinishReason: genai.FinishReasonStop, TurnComplete: true}, nil)
	}
}

// TestBounded_StopsAtTheAnswerInsteadOfDegrading is the #730 acceptance
// test, and the whole train's reason for existing.
//
// Fails on pre-fix code: a bounded delegation ran on the standing-worker
// loop, so after producing a correct answer with no tool calls left it
// was re-driven with "continue" up to MaxTurns. Each re-drive overwrote
// FinalText, so the parent received the LAST thing the subagent said
// ("standing by in a healthy, inactive state") instead of the answer,
// and paid for four turns to get the worse result. Pre-fix this asserts
// output == the degraded status line, status "deferred", 4 turns.
func TestBounded_StopsAtTheAnswerInsteadOfDegrading(t *testing.T) {
	t.Parallel()
	prov := &recordingProvider{llm: &answerThenDegradeLLM{}}
	mgr := newTemplateManager(t, prov, []SubagentTemplate{{
		Name:         "cluster",
		Instruction:  "triage",
		ModelFactory: tmplFactory(prov, "cluster-model"),
		ModelID:      "cluster-model",
	}}, WithDefaultBudgets(Budgets{MaxTurns: 4}), WithSyncWaitTimeout(10*time.Second))
	attachEchoParent(t, mgr)
	defer mgr.Close()

	h, err := mgr.SpawnTemplate(context.Background(), "", "cluster", RefOverrides{Goal: "triage emailservice"}, "")
	if err != nil {
		t.Fatalf("SpawnTemplate: %v", err)
	}
	res := mgr.awaitResult(context.Background(), h)

	if res.Status != "completed" {
		t.Errorf("status = %q, want completed — running out of tool calls IS completion for a bounded delegation", res.Status)
	}
	if res.StopReason != StopNatural {
		t.Errorf("stop_reason = %q, want %q", res.StopReason, StopNatural)
	}
	if !strings.Contains(res.Output, "does-not-exist:v0-demo-break") {
		t.Errorf("output = %q, want the answer from turn 1", res.Output)
	}
	if strings.Contains(res.Output, boundedDegrade) {
		t.Errorf("output = %q: the loop re-drove the subagent past its own answer", res.Output)
	}
	if r := h.Result(); r == nil {
		t.Fatal("no RunResult")
	} else if r.Turns != 1 {
		t.Errorf("turns = %d, want 1 — the answer was ready after the first turn", r.Turns)
	}
}

// TestBounded_ToolUsingTurnStillTerminates guards the reading of
// "no tool calls" that makes the stop condition work at all.
//
// The obvious implementation — count every functionCall part seen
// during the turn — never fires for a delegation that does any work,
// because using a tool at some point is the normal case. What matters
// is whether the model's LAST response asked for one. This test uses a
// tool and then answers, and must still stop on the same turn.
func TestBounded_ToolUsingTurnStillTerminates(t *testing.T) {
	t.Parallel()
	prov := &recordingProvider{llm: &toolThenAnswerLLM{}}
	mgr := newTemplateManager(t, prov, []SubagentTemplate{{
		Name:         "cluster",
		Instruction:  "triage",
		ModelFactory: tmplFactory(prov, "cluster-model"),
		ModelID:      "cluster-model",
	}}, WithDefaultBudgets(Budgets{MaxTurns: 6}), WithSyncWaitTimeout(10*time.Second))
	attachEchoParent(t, mgr)
	defer mgr.Close()

	h, err := mgr.SpawnTemplate(context.Background(), "", "cluster", RefOverrides{Goal: "triage emailservice"}, "")
	if err != nil {
		t.Fatalf("SpawnTemplate: %v", err)
	}
	res := mgr.awaitResult(context.Background(), h)

	if res.StopReason != StopNatural {
		t.Errorf("stop_reason = %q, want %q — the tool call was mid-turn, the turn still ended with an answer", res.StopReason, StopNatural)
	}
	if !strings.Contains(res.Output, "does-not-exist:v0-demo-break") {
		t.Errorf("output = %q, want the answer", res.Output)
	}
	if r := h.Result(); r == nil {
		t.Fatal("no RunResult")
	} else if r.Turns != 1 {
		t.Errorf("turns = %d, want 1", r.Turns)
	}
}

// TestBounded_OffersTheReturnToolAndItsAliases is #745, and it reverses
// decision 3 of #730 on purpose.
//
// #730 dropped the return tool from bounded runs on the argument that
// one exit is simpler than two. Bounded is the DEFAULT for
// declarative-subagent spawns, so that left the #728 alias net covering
// only the path models rarely take: in the 2026-08-14 GKE UAT a bounded
// `cluster` finished its RCA, called mark_task_done, and was told it
// had hallucinated the tool. Two exits are fine when they are ordered —
// a call returns a curated result, silence still ends the run.
//
// Fails on pre-fix code: no bounded spawn was offered any of these.
func TestBounded_OffersTheReturnToolAndItsAliases(t *testing.T) {
	t.Parallel()
	capture := &declCapturingLLM{}
	prov := &recordingProvider{llm: capture}
	mgr := newTemplateManager(t, prov, []SubagentTemplate{{
		Name:         "cluster",
		Instruction:  "triage",
		ModelFactory: tmplFactory(prov, "cluster-model"),
		ModelID:      "cluster-model",
	}}, WithDefaultBudgets(Budgets{MaxTurns: 1}))
	attachEchoParent(t, mgr)
	defer mgr.Close()

	h, err := mgr.SpawnTemplate(context.Background(), "", "cluster", RefOverrides{Goal: "g"}, "")
	if err != nil {
		t.Fatalf("SpawnTemplate: %v", err)
	}
	waitDone(t, h)

	for _, name := range append([]string{"return_result"}, subagentReturnToolAliases...) {
		if _, ok := capture.declaration(name); !ok {
			t.Errorf("bounded delegation was not offered %q; the #728 alias net has to cover the default path", name)
		}
	}
	// report_alert is not a termination gesture and stays.
	if _, ok := capture.declaration("report_alert"); !ok {
		t.Error("report_alert must remain available for mid-run reporting")
	}
}

// TestBounded_ReturnToolCallHandsBackTheResult is the behavioural half:
// the alias the UAT model reached for now ends the run and its payload
// is what the parent receives, instead of a scraped last message.
func TestBounded_ReturnToolCallHandsBackTheResult(t *testing.T) {
	t.Parallel()
	prov := &recordingProvider{llm: &returnThenDegradeLLM{}}
	mgr := newTemplateManager(t, prov, []SubagentTemplate{{
		Name:         "cluster",
		Instruction:  "triage",
		ModelFactory: tmplFactory(prov, "cluster-model"),
		ModelID:      "cluster-model",
	}}, WithDefaultBudgets(Budgets{MaxTurns: 4}), WithSyncWaitTimeout(10*time.Second))
	attachEchoParent(t, mgr)
	defer mgr.Close()

	h, err := mgr.SpawnTemplate(context.Background(), "", "cluster", RefOverrides{Goal: "triage emailservice"}, "")
	if err != nil {
		t.Fatalf("SpawnTemplate: %v", err)
	}
	res := mgr.awaitResult(context.Background(), h)

	if res.Status != "completed" {
		t.Errorf("status = %q, want completed", res.Status)
	}
	if res.StopReason != StopNatural {
		t.Errorf("stop_reason = %q, want %q", res.StopReason, StopNatural)
	}
	if !strings.Contains(res.Output, "does-not-exist:v0-demo-break") {
		t.Errorf("output = %q, want the result the subagent returned", res.Output)
	}
	if strings.Contains(res.Output, boundedDegrade) {
		t.Errorf("output = %q: the return call did not end the run", res.Output)
	}
	if r := h.Result(); r == nil {
		t.Fatal("no RunResult")
	} else if r.Turns != 1 {
		t.Errorf("turns = %d, want 1 — the return call ended the run on the first turn", r.Turns)
	}
}

// TestBounded_ContractNamesTheReturnTool is the instruction half. Now
// that bounded registers the tool, the contract must name it — and the
// last-message clause stays as the backstop for the run that ends
// without one. Telling a subagent to call a tool that isn't registered
// is the #641 failure (reached for mark_task_done, got tool-not-found,
// never recovered); so is withholding one it is going to reach for.
func TestBounded_ContractNamesTheReturnTool(t *testing.T) {
	t.Parallel()
	capture := &declCapturingLLM{}
	prov := &recordingProvider{llm: capture}
	mgr := newTemplateManager(t, prov, []SubagentTemplate{{
		Name:         "cluster",
		Instruction:  "triage",
		ModelFactory: tmplFactory(prov, "cluster-model"),
		ModelID:      "cluster-model",
	}}, WithDefaultBudgets(Budgets{MaxTurns: 1}))
	attachEchoParent(t, mgr)
	defer mgr.Close()

	h, err := mgr.SpawnTemplate(context.Background(), "", "cluster", RefOverrides{Goal: "g"}, "")
	if err != nil {
		t.Fatalf("SpawnTemplate: %v", err)
	}
	waitDone(t, h)

	sys := capture.systemInstruction()
	if !strings.Contains(sys, "`return_result`") {
		t.Errorf("bounded contract does not name the tool the subagent now has; got:\n%s", sys)
	}
	if !strings.Contains(sys, "LAST message") {
		t.Errorf("bounded contract dropped the last-message backstop; got:\n%s", sys)
	}
	if !strings.Contains(sys, "triage") {
		t.Error("the subagent's own persona was lost")
	}
}

// TestStanding_KeepsRunningPastATextOnlyTurn is the other side of the
// discriminator, and the reason this is opt-in rather than the loop's
// new default. For a worker that watches something, a turn with no tool
// calls means idle, not finished — stopping there would silently
// convert every standing worker into a one-shot.
func TestStanding_KeepsRunningPastATextOnlyTurn(t *testing.T) {
	t.Parallel()
	prov := &recordingProvider{llm: &answerThenDegradeLLM{}}
	mgr := newTemplateManager(t, prov, []SubagentTemplate{{
		Name:         "watcher",
		Instruction:  "watch",
		ModelFactory: tmplFactory(prov, "cluster-model"),
		ModelID:      "cluster-model",
		Mode:         ModeStanding,
	}}, WithDefaultBudgets(Budgets{MaxTurns: 3}), WithSyncWaitTimeout(10*time.Second))
	attachEchoParent(t, mgr)
	defer mgr.Close()

	h, err := mgr.SpawnTemplate(context.Background(), "", "watcher", RefOverrides{Goal: "watch"}, "")
	if err != nil {
		t.Fatalf("SpawnTemplate: %v", err)
	}
	res := mgr.awaitResult(context.Background(), h)

	if res.StopReason != StopMaxSteps {
		t.Errorf("stop_reason = %q, want %q — a standing worker runs until a budget or an explicit done", res.StopReason, StopMaxSteps)
	}
	if r := h.Result(); r == nil {
		t.Fatal("no RunResult")
	} else if r.Turns != 3 {
		t.Errorf("turns = %d, want 3 (the turn cap)", r.Turns)
	}
}

// TestResolveMode covers the ModeAuto rule directly, including the
// override in both directions — the derivation is a default, not a
// constraint.
func TestResolveMode(t *testing.T) {
	t.Parallel()
	sleeper := coretools.SleepScheduler()
	tests := []struct {
		name  string
		mode  Mode
		sched coretools.Scheduler
		want  Mode
	}{
		{"auto without a scheduler is bounded", ModeAuto, nil, ModeBounded},
		{"auto with a scheduler is standing", ModeAuto, sleeper, ModeStanding},
		{"explicit standing survives no scheduler", ModeStanding, nil, ModeStanding},
		{"explicit bounded survives a scheduler", ModeBounded, sleeper, ModeBounded},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := resolveMode(tc.mode, tc.sched); got != tc.want {
				t.Errorf("resolveMode(%q, sched=%t) = %q, want %q", tc.mode, tc.sched != nil, got, tc.want)
			}
		})
	}
}

// TestSpec_RejectsUnknownMode keeps a typo from silently selecting the
// standing loop — the mode that costs money when it's wrong.
func TestSpec_RejectsUnknownMode(t *testing.T) {
	t.Parallel()
	err := validateSpec(Spec{Name: "a", SystemPrompt: "p", Goal: "g", Mode: Mode("one-shot")})
	if err == nil {
		t.Fatal("validateSpec accepted an unknown Mode")
	}
	if !strings.Contains(err.Error(), "one-shot") {
		t.Errorf("error = %v, want it to name the offending value", err)
	}
	if err := validateTemplate(SubagentTemplate{
		Name:         "t",
		ModelFactory: tmplFactory(&recordingProvider{}, "m"),
		Mode:         Mode("oneshot"),
	}); err == nil {
		t.Fatal("validateTemplate accepted an unknown Mode")
	}
}
