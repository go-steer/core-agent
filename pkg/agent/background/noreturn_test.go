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
	"testing"
	"time"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/pkg/agent/autonomous"
)

// askTheOperatorLLM is the 2026-08-18 GKE UAT shape: the subagent
// produces a plausible-sounding report and ends by asking the operator
// whether to continue. There is no operator. Nothing will answer.
const askTheOperator = "I completed a comprehensive audit of your Autopilot cluster. " +
	"Please let me know if you would like me to continue and audit any of the other clusters, or if you are ready to wrap up!"

type askTheOperatorLLM struct{}

func (*askTheOperatorLLM) Name() string { return "ask-the-operator" }

func (*askTheOperatorLLM) GenerateContent(_ context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		content := &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: askTheOperator}}}
		yield(&adkmodel.LLMResponse{Content: content, FinishReason: genai.FinishReasonStop, TurnComplete: true}, nil)
	}
}

// TestSync_NoReturnIsNotDressedAsAnAnswer is the #710 acceptance test.
//
// Fails on pre-fix code: a bounded subagent that stops talking without
// calling its return tool came back as stop_reason "natural" with the
// question sitting in `output`, which is the same object a subagent
// that deliberately returned a curated result produces. The parent in
// the filed session read it as a finished report, and only recovered by
// redoing the investigation itself — after paying $1.33 for the
// delegation.
func TestSync_NoReturnIsNotDressedAsAnAnswer(t *testing.T) {
	t.Parallel()
	prov := &recordingProvider{llm: &askTheOperatorLLM{}}
	mgr := newTemplateManager(t, prov, []SubagentTemplate{{
		Name:         "cluster",
		Instruction:  "triage",
		ModelFactory: tmplFactory(prov, "cluster-model"),
		ModelID:      "cluster-model",
	}}, WithDefaultBudgets(Budgets{MaxTurns: 4}), WithSyncWaitTimeout(10*time.Second))
	attachEchoParent(t, mgr)
	defer mgr.Close()

	h, err := mgr.SpawnTemplate(context.Background(), "", "cluster", RefOverrides{Goal: "diagnose emailservice"}, "")
	if err != nil {
		t.Fatalf("SpawnTemplate: %v", err)
	}
	res := mgr.awaitResult(context.Background(), h)

	if res.StopReason != StopNoReturn {
		t.Errorf("stop_reason = %q, want %q — nothing asserted this answers the goal", res.StopReason, StopNoReturn)
	}
	if res.Guidance == "" {
		t.Error("no guidance on a result the parent must not build on; an enum two fields down is what already failed")
	}
	// The text still has to reach the parent. Discarding it is #691's
	// mistake — the parent then re-derives work it already paid for.
	if !strings.Contains(res.Output, "comprehensive audit") {
		t.Errorf("output = %q, want the subagent's text kept", res.Output)
	}
}

// The contrast, on the same synchronous path: a subagent that calls its
// return tool is finished, says so, and gets no second-guessing.
func TestSync_ReturnedResultIsNaturalAndUnannotated(t *testing.T) {
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

	h, err := mgr.SpawnTemplate(context.Background(), "", "cluster", RefOverrides{Goal: "diagnose emailservice"}, "")
	if err != nil {
		t.Fatalf("SpawnTemplate: %v", err)
	}
	res := mgr.awaitResult(context.Background(), h)

	if res.StopReason != StopNatural {
		t.Errorf("stop_reason = %q, want %q", res.StopReason, StopNatural)
	}
	if res.Guidance != "" {
		t.Errorf("guidance = %q, want none — a finished result needs no caveat, and a caveat on every result is a caveat on none", res.Guidance)
	}
}

// Every class that is not "natural" has to carry a line telling the
// parent what to do about it. Without this, a class added later
// silently inherits the empty string and re-opens #710 for that case.
func TestStopGuidance_EveryNonNaturalClassSaysSomething(t *testing.T) {
	t.Parallel()
	for _, class := range []StopClass{StopNoReturn, StopMaxSteps, StopBudget, StopDeferred, StopStopped, StopError} {
		if stopGuidance(class) == "" {
			t.Errorf("stop class %q has no parent-facing guidance", class)
		}
	}
	if g := stopGuidance(StopNatural); g != "" {
		t.Errorf("stopGuidance(natural) = %q, want empty", g)
	}
}

// The async twin. A wait that times out delivers the same outcome as a
// [Background reports] alert instead, so the annotation has to survive
// that crossing — the #691 rule that anything only the sync path
// renders is unreachable exactly when the subagent took long enough to
// be worth waiting for.
func TestTerminalAlertText_NoReturnIsAnnotated(t *testing.T) {
	t.Parallel()
	kind, text := terminalAlertText(StatusCompleted, autonomous.RunResult{FinalText: askTheOperator}, nil)
	if kind != "completed" {
		t.Errorf("kind = %q, want completed — the loop did end", kind)
	}
	if !strings.Contains(text, "stop_reason: no_return") {
		t.Errorf("alert text = %q, want the no_return trailer", text)
	}
	if !strings.Contains(text, "comprehensive audit") {
		t.Errorf("alert text = %q, want the subagent's text kept", text)
	}
}
