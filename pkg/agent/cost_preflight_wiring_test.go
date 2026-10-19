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
	"sync"
	"testing"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v3/pkg/usage"
)

// The preflight regression tests drive preflightCostCeilingRequest and
// turnPreflight directly. These go through New and Run, so they fail if
// the wiring those tests assume is ever dropped.

// ctxCapturingLLM answers every call with a short reply and keeps the
// context of the last one, which carries the turn's cost check.
type ctxCapturingLLM struct {
	mu    sync.Mutex
	calls int
	ctx   context.Context
}

func (*ctxCapturingLLM) Name() string { return pricedModel }

func (l *ctxCapturingLLM) GenerateContent(ctx context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	l.mu.Lock()
	l.calls++
	l.ctx = ctx
	l.mu.Unlock()
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		yield(&adkmodel.LLMResponse{
			Content:       genai.NewContentFromText("done", genai.RoleModel),
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 10, CandidatesTokenCount: 1},
			TurnComplete:  true,
		}, nil)
	}
}

func (l *ctxCapturingLLM) last() (int, context.Context) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.calls, l.ctx
}

// TestRun_PreflightEndsWhenRunReturns pins Run's end of turnPreflight:
// the check a turn installs must be over once Run's iterator is done. A
// background subagent keeps the turn's context values after the turn
// (background/spawn.go, contextWithoutCancel), so a check Run never ends
// screens that subagent's requests against the finished turn's ceiling,
// trips the idle parent and advances its streak.
func TestRun_PreflightEndsWhenRunReturns(t *testing.T) {
	t.Parallel()
	llm := &ctxCapturingLLM{}
	a, err := New(llm,
		WithSession("u-preflight-end", "s-preflight-end"),
		WithUsageTracker(usage.NewTracker()),
		WithCostCeiling(CostCeiling{MaxTurnUSD: 0.05}),
	)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	var rec terminalRecorder
	rec.attachTo(a)
	for range a.Run(context.Background(), "spawn the reviewer") {
	}
	calls, turnCtx := llm.last()
	if calls != 1 || turnCtx == nil {
		t.Fatalf("provider calls = %d, want 1 with a captured context", calls)
	}
	check := costPreflightFromContext(turnCtx)
	if check == nil {
		t.Fatal("the turn's model call carried no cost check; this test would prove nothing")
	}

	// What a background subagent still running from that turn sends.
	if _, err := check(preflightCall{req: bigPreflightRequest(800_000)}); err != nil {
		t.Errorf("a request after Run returned was refused by the finished turn's ceiling: %v", err)
	}
	if trips := rec.guardrailTrips(); len(trips) != 0 {
		t.Errorf("guardrail trips = %+v, want none: the turn the check belongs to is over", trips)
	}
}

// TestRun_PreflightPricesTheAgentsOwnRequestsAtItsWindow pins New's
// owner wiring: the model wrapper must stamp this agent's requests with
// a.preflightOwner, or the check reads each of them as another agent's
// and prices it from the serialized request alone. After a 400K-token
// call, the agent's next request continues that conversation, so it is
// priced at 400K and refused under a ceiling below that, never sent.
func TestRun_PreflightPricesTheAgentsOwnRequestsAtItsWindow(t *testing.T) {
	t.Parallel()
	const window = 400_000
	price := usage.PriceFor(pricedModel, nil)
	ceiling := price.CostUSD(window, 0) / 2
	tr := usage.NewTracker()
	tr.AppendUsage(pricedModel, usage.TurnUsage{InputTokens: window, OutputTokens: 100}, price)
	llm := &ctxCapturingLLM{}
	a, err := New(llm,
		WithSession("u-preflight-owner", "s-preflight-owner"),
		WithUsageTracker(tr),
		WithCostCeiling(CostCeiling{MaxTurnUSD: ceiling}),
	)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	for range a.Run(context.Background(), "continue") {
	}
	if calls, _ := llm.last(); calls != 0 {
		t.Errorf("provider calls = %d, want 0: the agent's own request was priced as another agent's, from its serialized size, and sent past a $%.4f ceiling its %d-token window exceeds", calls, ceiling, window)
	}
}
