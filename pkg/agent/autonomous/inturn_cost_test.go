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

package autonomous

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/pkg/agent"
	"github.com/go-steer/core-agent/v2/pkg/usage"
)

// spinCallTurn yields a call to the "spin" tool carrying usage. The
// runner executes the tool and dispatches ANOTHER model call inside
// the same agent.Run turn, so a script of these models the runaway
// tool loop (#144) that the driver's between-turn budget checks never
// get a chance to see.
func spinCallTurn(in int32) scenarioFn {
	return func(_ context.Context, _ *adkmodel.LLMRequest) []stubResp {
		fc := &genai.FunctionCall{Name: "spin", Args: map[string]any{}}
		content := &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: fc}}}
		return []stubResp{
			{resp: &adkmodel.LLMResponse{
				Content:      content,
				TurnComplete: true,
				FinishReason: genai.FinishReasonStop,
				UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
					PromptTokenCount: in,
					TotalTokenCount:  in,
				},
			}},
		}
	}
}

// buildSpinAgent wires the stub LLM with a no-op "spin" tool
// alongside the driver's own extras, so a scripted loop of spin calls
// keeps one agent.Run turn going indefinitely.
func buildSpinAgent(llm *stubLLM, name string, calls *atomic.Int32) func([]tool.Tool) (*agent.Agent, error) {
	return func(extras []tool.Tool) (*agent.Agent, error) {
		type empty struct{}
		spin, err := functiontool.New(
			functiontool.Config{Name: "spin", Description: "no-op that keeps the turn going"},
			func(_ tool.Context, _ empty) (empty, error) {
				calls.Add(1)
				return empty{}, nil
			})
		if err != nil {
			return nil, err
		}
		return agent.New(llm,
			agent.WithName(name),
			agent.WithSession("u-test", "s-test-"+name),
			agent.WithTools(append(append([]tool.Tool(nil), extras...), spin)),
			agent.WithInstruction("test agent; call spin forever."),
		)
	}
}

// A tool loop inside ONE turn must not spend past WithMaxCost.
//
// Regression for #729: the driver only compared spend against the
// bound at a turn boundary, and a tool loop never reaches one — so a
// $1.50 cap let a single turn burn $6 (every scripted call) before
// the loop check ran once and stopped the run it had already paid
// for. The bound is money, and money is spent inside the turn.
func TestRunAutonomous_StopsMidTurnOnMaxCost(t *testing.T) {
	t.Parallel()
	var toolCalls atomic.Int32
	llm := &stubLLM{scenarios: []scenarioFn{
		spinCallTurn(1_000_000), // $1 -> under
		spinCallTurn(1_000_000), // $2 -> crosses $1.50, must be the last
		spinCallTurn(1_000_000),
		spinCallTurn(1_000_000),
		spinCallTurn(1_000_000),
		spinCallTurn(1_000_000),
	}}
	pricing := usage.Pricing{InputPerMTok: 1.0}
	res, err := Run(context.Background(),
		buildSpinAgent(llm, "inturn-cost", &toolCalls),
		"spin",
		WithPricing(pricing),
		WithMaxCost(1.5),
		WithMaxTurns(4))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Reason != StopReasonMaxCost {
		t.Errorf("Reason = %q, want %q", res.Reason, StopReasonMaxCost)
	}
	if res.Turns != 1 {
		t.Errorf("Turns = %d, want 1 (the bound must trip inside the first turn)", res.Turns)
	}
	// Two model calls: the one that put spend at $1 and the one that
	// crossed to $2. Anything more means the driver kept paying after
	// the bound was already breached.
	if got := atomic.LoadInt32(&llm.calls); got != 2 {
		t.Errorf("model calls = %d, want 2 (the loop must stop at the call that crosses the bound)", got)
	}
	if res.CostUSD != 2 {
		t.Errorf("CostUSD = %v, want 2 (the crossing call is paid for, nothing after it)", res.CostUSD)
	}
}

// The mid-turn bound is the RUN's bound, not a per-turn one: spend
// carried in from earlier turns counts toward it. Without the prior
// cost baseline threaded into runOneTurn, each turn would get a fresh
// $1.50 to spend and a capped run would never actually stop.
func TestRunAutonomous_MidTurnCostCountsPriorTurns(t *testing.T) {
	t.Parallel()
	var toolCalls atomic.Int32
	llm := &stubLLM{scenarios: []scenarioFn{
		textTurn("turn one", 1_000_000, 0), // $1, turn ends under the bound
		spinCallTurn(1_000_000),            // $2 total -> crosses inside turn 2
		spinCallTurn(1_000_000),
		spinCallTurn(1_000_000),
	}}
	pricing := usage.Pricing{InputPerMTok: 1.0}
	res, err := Run(context.Background(),
		buildSpinAgent(llm, "inturn-cost-prior", &toolCalls),
		"spin",
		WithPricing(pricing),
		WithMaxCost(1.5),
		WithMaxTurns(4))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Reason != StopReasonMaxCost {
		t.Errorf("Reason = %q, want %q", res.Reason, StopReasonMaxCost)
	}
	if res.Turns != 2 {
		t.Errorf("Turns = %d, want 2", res.Turns)
	}
	if got := atomic.LoadInt32(&llm.calls); got != 2 {
		t.Errorf("model calls = %d, want 2", got)
	}
}

// Whatever the model established before the bound tripped still has
// to reach the caller — a budget stop returns a bounded result, not
// an empty one (#729). The text collected before the break is the
// run's FinalText.
func TestRunAutonomous_MidTurnCostKeepsFinalText(t *testing.T) {
	t.Parallel()
	var toolCalls atomic.Int32
	llm := &stubLLM{scenarios: []scenarioFn{
		func(_ context.Context, _ *adkmodel.LLMRequest) []stubResp {
			content := &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{
				{Text: "root cause: the deployment references a nonexistent image tag"},
				{FunctionCall: &genai.FunctionCall{Name: "spin", Args: map[string]any{}}},
			}}
			return []stubResp{{resp: &adkmodel.LLMResponse{
				Content:      content,
				TurnComplete: true,
				FinishReason: genai.FinishReasonStop,
				UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
					PromptTokenCount: 2_000_000,
					TotalTokenCount:  2_000_000,
				},
			}}}
		},
		spinCallTurn(1_000_000),
	}}
	res, err := Run(context.Background(),
		buildSpinAgent(llm, "inturn-cost-text", &toolCalls),
		"diagnose",
		WithPricing(usage.Pricing{InputPerMTok: 1.0}),
		WithMaxCost(1.5))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Reason != StopReasonMaxCost {
		t.Fatalf("Reason = %q, want %q", res.Reason, StopReasonMaxCost)
	}
	if res.FinalText == "" {
		t.Fatal("FinalText is empty: the findings the model produced before the bound tripped were dropped")
	}
	if want := "nonexistent image tag"; !strings.Contains(res.FinalText, want) {
		t.Errorf("FinalText = %q, want it to contain %q", res.FinalText, want)
	}
}

// The bound must not cut off the call that hands back the result.
//
// The mid-turn check fires on the model response, and the tool it
// requested runs after — so a naive break on the response carrying
// `return_result` would discard the delegation's answer and report a
// budget stop instead. That is #728's failure arriving through a
// different door, so the loop takes one more step (at most once) when
// the crossing call is the done call.
func TestRunAutonomous_MidTurnCostDoesNotDropTheReturn(t *testing.T) {
	t.Parallel()
	var toolCalls atomic.Int32
	llm := &stubLLM{scenarios: []scenarioFn{
		// One call, over the bound, and it IS the return.
		func(_ context.Context, _ *adkmodel.LLMRequest) []stubResp {
			fc := &genai.FunctionCall{
				Name: "report_done",
				Args: map[string]any{"state": "done", "detail": "image tag does not exist in the registry"},
			}
			content := &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: fc}}}
			return []stubResp{{resp: &adkmodel.LLMResponse{
				Content:      content,
				TurnComplete: true,
				FinishReason: genai.FinishReasonStop,
				UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
					PromptTokenCount: 2_000_000,
					TotalTokenCount:  2_000_000,
				},
			}}}
		},
		textTurn("wrapping up", 0, 0),
	}}
	res, err := Run(context.Background(),
		buildSpinAgent(llm, "inturn-cost-return", &toolCalls),
		"diagnose",
		WithPricing(usage.Pricing{InputPerMTok: 1.0}),
		WithMaxCost(1.5))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Reason != StopReasonCompleted {
		t.Errorf("Reason = %q, want %q (the budget stop swallowed the result)", res.Reason, StopReasonCompleted)
	}
	if want := "image tag does not exist in the registry"; res.DoneDetail != want {
		t.Errorf("DoneDetail = %q, want %q", res.DoneDetail, want)
	}
	// The grace is for the tool, not for another model call: once the
	// done signal lands the run is over, and the scripted follow-up
	// must never be reached.
	if got := atomic.LoadInt32(&llm.calls); got != 1 {
		t.Errorf("model calls = %d, want 1 (the deferral must not buy the model another turn)", got)
	}
}

// No bound configured means no mid-turn interference: a long tool
// loop runs to its scripted end. Guards against the check firing on
// a zero maxCostUSD (every unbudgeted run would stop after one call).
func TestRunAutonomous_NoMaxCostRunsTurnToCompletion(t *testing.T) {
	t.Parallel()
	var toolCalls atomic.Int32
	llm := &stubLLM{scenarios: []scenarioFn{
		spinCallTurn(1_000_000),
		spinCallTurn(1_000_000),
		textTurn("done spinning", 1_000_000, 0),
	}}
	res, err := Run(context.Background(),
		buildSpinAgent(llm, "inturn-cost-off", &toolCalls),
		"spin",
		WithPricing(usage.Pricing{InputPerMTok: 1.0}),
		WithMaxTurns(1))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Reason != StopReasonMaxTurns {
		t.Errorf("Reason = %q, want %q", res.Reason, StopReasonMaxTurns)
	}
	if got := atomic.LoadInt32(&llm.calls); got != 3 {
		t.Errorf("model calls = %d, want 3 (whole turn should run)", got)
	}
	if got := toolCalls.Load(); got != 2 {
		t.Errorf("tool calls = %d, want 2", got)
	}
}
