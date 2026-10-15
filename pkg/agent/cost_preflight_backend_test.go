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
	"testing"

	adkmodel "google.golang.org/adk/v2/model"

	"github.com/go-steer/core-agent/v3/pkg/usage"
)

// openaiOnlyModel is priced only as "openai/<id>" in the shared catalog,
// so it reads as unpriced on any other backend — the sharpest case for
// pricing a request on the wrong backend.
const openaiOnlyModel = "gpt-5-mini"

func requireOpenAIOnlyPrice(t *testing.T) {
	t.Helper()
	if !usage.PriceFor(openaiOnlyModel, nil).Unpriced {
		t.Skipf("%q is priced on the process backend; the fixture no longer isolates the billing backend", openaiOnlyModel)
	}
	if usage.PriceOn("openai", openaiOnlyModel).Unpriced {
		t.Fatalf("%q has no openai row; pick another OpenAI-only model", openaiOnlyModel)
	}
}

// TestCostCeilingPreflight_PricesOnTheAgentsBillingBackend: an agent
// billed on openai runs an OpenAI-only model under a per-turn ceiling.
// Priced on the process backend the request reads as unpriced and is
// refused before it is sent.
func TestCostCeilingPreflight_PricesOnTheAgentsBillingBackend(t *testing.T) {
	t.Parallel()
	requireOpenAIOnlyPrice(t)
	model := &meteredLLM{
		name:   openaiOnlyModel,
		script: [][]*adkmodel.LLMResponse{{usedText("done", 100, 10)}},
	}
	a, err := New(model,
		WithName("openai-agent"),
		WithEventLog(newTestEventLog(t)),
		WithSession("u", "openai-backend"),
		WithBillingBackend("openai"),
		WithUsageTracker(usage.NewTracker()),
		WithCostCeiling(CostCeiling{MaxTurnUSD: 1}),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for range a.Run(context.Background(), "go") {
	}
	model.mu.Lock()
	calls := model.calls
	model.mu.Unlock()
	if calls != 1 {
		t.Errorf("provider calls = %d, want 1: the request is priced on the agent's openai backend and is well inside the ceiling", calls)
	}
}

// TestCostCeilingPreflight_ParentPricesChildOnChildBackend: the parent's
// per-turn ceiling also screens its subagent's requests. A child billed
// on openai must be priced there, not on the parent's backend.
func TestCostCeilingPreflight_ParentPricesChildOnChildBackend(t *testing.T) {
	t.Parallel()
	requireOpenAIOnlyPrice(t)
	h := newTestEventLog(t)
	model := &meteredLLM{
		name:   openaiOnlyModel,
		script: [][]*adkmodel.LLMResponse{{usedText("done", 100, 10)}},
	}
	child, err := New(model,
		WithName("child"),
		WithEventLog(h),
		WithSession("u", "openai-child"),
		WithBillingBackend("openai"),
	)
	if err != nil {
		t.Fatalf("New child: %v", err)
	}
	parent, err := New(pricedFnCallThenDoneLLM{&fnCallThenDoneLLM{target: child.AgentName()}},
		WithName("parent"),
		WithEventLog(h),
		WithSession("u", "gemini-parent"),
		WithUsageTracker(usage.NewTracker()),
		WithCostCeiling(CostCeiling{MaxTurnUSD: 1}),
		WithSubagents([]*Agent{child}),
	)
	if err != nil {
		t.Fatalf("New parent: %v", err)
	}
	for range parent.Run(context.Background(), "go") {
	}
	model.mu.Lock()
	calls := model.calls
	model.mu.Unlock()
	if calls != 1 {
		t.Errorf("child provider calls = %d, want 1: the parent's preflight must price the child's request on the child's openai backend", calls)
	}
}
