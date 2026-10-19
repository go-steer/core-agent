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
	"errors"
	"iter"
	"strings"
	"testing"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v3/pkg/models"
	"github.com/go-steer/core-agent/v3/pkg/usage"
)

// The cost preflight's review on #1327 reproduced each of these with a
// probe that failed on the PR as submitted. Every one of them makes the
// per-turn ceiling over-count or trip the wrong turn, and a preflight trip
// advances the guardrail streak, so three false trips halt a session.

func preflightTestAgent(ceiling float64) (*Agent, *usage.Tracker, *terminalRecorder) {
	tr := usage.NewTracker()
	a := &Agent{tracker: tr, modelName: pricedModel, costCeiling: CostCeiling{MaxTurnUSD: ceiling}}
	rec := &terminalRecorder{}
	rec.attachTo(a)
	return a, tr, rec
}

func smallPreflightRequest() *adkmodel.LLMRequest {
	return &adkmodel.LLMRequest{Model: pricedModel, Contents: []*genai.Content{genai.NewContentFromText("read foo.go", genai.RoleUser)}}
}

func bigPreflightRequest(chars int) *adkmodel.LLMRequest {
	return &adkmodel.LLMRequest{Model: pricedModel, Contents: []*genai.Content{genai.NewContentFromText(strings.Repeat("x", chars), genai.RoleUser)}}
}

func retainedEstimates(a *Agent) (int, float64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var usd float64
	for _, e := range a.turnPreflightEstimates {
		usd += e.usd
	}
	return len(a.turnPreflightEstimates), usd
}

// A child check that refuses means the request is never sent, so nothing
// will settle the parent's estimate. The parent used to keep it anyway,
// counting a phantom $0.04 against the rest of its turn.
func TestPreflight_ChildRefusalLeavesNoParentEstimate(t *testing.T) {
	t.Parallel()
	a, _, _ := preflightTestAgent(1.0)
	a.snapshotTurnStartCost()
	ctx := withCostPreflight(context.Background(), a.preflightCostCeilingRequest)
	ctx = withCostPreflight(ctx, func(preflightCall) (preflightRelease, error) {
		return nil, &costPreflightError{reason: "child budget"}
	})
	if _, err := costPreflightFromContext(ctx)(preflightCall{req: bigPreflightRequest(400_000)}); err == nil {
		t.Fatal("want the child's refusal")
	}
	if n, usd := retainedEstimates(a); n != 0 {
		t.Errorf("a refused child request left %d parent estimate(s) ($%.6f) that no usage will ever settle", n, usd)
	}
}

type failingPreflightLLM struct{}

func (failingPreflightLLM) Name() string { return pricedModel }
func (failingPreflightLLM) GenerateContent(context.Context, *adkmodel.LLMRequest, bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) { yield(nil, errors.New("503 unavailable")) }
}

// A provider error before any usage means none is on its way to the
// tracker; the estimate for that request has nothing to settle it.
func TestPreflight_FailedRequestLeavesNoEstimate(t *testing.T) {
	t.Parallel()
	a, _, _ := preflightTestAgent(1.0)
	a.snapshotTurnStartCost()
	ctx := withCostPreflight(context.Background(), a.preflightCostCeilingRequest)
	for range newRoleSanitizingLLM(failingPreflightLLM{}, "").GenerateContent(ctx, smallPreflightRequest(), false) {
	}
	if n, usd := retainedEstimates(a); n != 0 {
		t.Errorf("a failed request (no usage) left %d retained estimate(s) ($%.6f)", n, usd)
	}
	a.mu.Lock()
	reserved := a.turnPreflightReservedUSD
	a.mu.Unlock()
	if reserved != 0 {
		t.Errorf("reservation after the failed request = $%.6f, want 0", reserved)
	}
}

// A side call (summarizer, session title, approver, agentic subtask, MCP
// digest) is not screened. It records through AppendSideUsage, which
// settlement does not count, so a screened side call's estimate stayed
// pending all turn while its real cost was also counted: the same spend
// twice.
func TestPreflight_SideCallsAreNotScreened(t *testing.T) {
	t.Parallel()
	screened := 0
	ctx := withCostPreflight(context.Background(), func(preflightCall) (preflightRelease, error) {
		screened++
		return nil, nil
	})
	model := newRoleSanitizingLLM(failingPreflightLLM{}, "")
	for range model.GenerateContent(models.AsSideCall(ctx, "summarizer"), smallPreflightRequest(), false) {
	}
	if screened != 0 {
		t.Errorf("a side call was screened %d time(s), want 0", screened)
	}
	for range model.GenerateContent(ctx, smallPreflightRequest(), false) {
	}
	if screened != 1 {
		t.Errorf("the agent's own call was screened %d time(s), want 1: only side calls skip the check", screened)
	}
}
