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
	"strings"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/adk/tool"

	"github.com/go-steer/core-agent/v2/pkg/eventlog"
	"github.com/go-steer/core-agent/v2/pkg/usage"
)

// pricedModel is a model ID the builtin pricing catalog knows, so a
// scripted turn produces a real dollar figure for the cost cap to
// evaluate. usage.PriceFor falls back to a one-shot builtin catalog when
// none is installed, which is the case in a library test.
//
// At $0.30/Mtok input, 100k input tokens is $0.03 a turn.
const (
	pricedModel     = "gemini-2.5-flash"
	tokensPerCent3  = 100_000 // $0.03 of input on pricedModel
	costPerTurnUSD3 = 0.03
)

// runBoundedDelegation is runParentDelegating with a budget on the
// subagent, returning the tool result the parent's model saw. The result
// is the point of these tests: a capped delegation must hand back what it
// has, labelled, rather than an error or a silent truncation.
func runBoundedDelegation(t *testing.T, child *Agent, h *eventlog.Handle, tracker *usage.Tracker) string {
	t.Helper()
	opts := []Option{
		WithName("parent"),
		WithEventLog(h),
		WithSession("u", "parent"),
		WithSubagents([]*Agent{child}),
	}
	if tracker != nil {
		opts = append(opts, WithUsageTracker(tracker))
	}
	parent, err := New(&fnCallThenDoneLLM{target: child.AgentName()}, opts...)
	if err != nil {
		t.Fatalf("New parent: %v", err)
	}
	var results []string
	for ev, err := range parent.Run(context.Background(), "go") {
		if err != nil {
			t.Fatalf("parent.Run: %v", err)
		}
		if ev == nil || ev.Content == nil {
			continue
		}
		for _, p := range ev.Content.Parts {
			if p != nil && p.FunctionResponse != nil {
				if r, ok := p.FunctionResponse.Response["result"].(string); ok {
					results = append(results, r)
				}
			}
		}
	}
	if len(results) != 1 {
		t.Fatalf("got %d subagent tool results, want 1: %q", len(results), results)
	}
	return results[0]
}

// TestSubagentTool_TurnBudgetStopsTheDelegation: the runaway shape #713
// describes is a subagent that keeps taking cheap turns. ADK's
// runner.RunConfig has no turn cap — only StreamingMode and
// SaveInputBlobsAsArtifacts — so the cap is counted in the tool handler,
// and without it a declared max_turns would be config an operator can
// write and the substrate ignores.
func TestSubagentTool_TurnBudgetStopsTheDelegation(t *testing.T) {
	t.Parallel()
	h := newTestEventLog(t)
	tracker := usage.NewTracker()

	child, err := New(&meteredLLM{
		name: "child-model",
		script: [][]*adkmodel.LLMResponse{
			{callTool("ping", 100, 10)},
			{callTool("ping", 100, 10)},
			{callTool("ping", 100, 10)},
			{usedText("finally done", 100, 10)},
		},
	},
		WithName("child"),
		WithEventLog(h),
		WithSession("u", "child"),
		WithTools([]tool.Tool{pingTool(t)}),
		WithSubagentBudgets(SubagentBudgets{MaxTurns: 2}),
	)
	if err != nil {
		t.Fatalf("New child: %v", err)
	}
	got := runBoundedDelegation(t, child, h, tracker)

	if tot := tracker.Totals(); tot.Turns != 2 {
		t.Errorf("turns = %d, want 2 (the cap is checked after the turn that trips it is billed)", tot.Turns)
	}
	if !strings.Contains(got, "partial result") || !strings.Contains(got, "turn budget of 2") {
		t.Errorf("result does not name the cap that stopped it:\n%s", got)
	}
	// The subagent never reached its fourth scripted turn, so the text
	// that only that turn produces must not be in the result — otherwise
	// the cap didn't actually stop anything.
	if strings.Contains(got, "finally done") {
		t.Errorf("delegation ran past its turn cap:\n%s", got)
	}
}

// TestSubagentTool_CostBudgetStopsTheDelegation is the cap that could not
// exist before #849: the synchronous door had no cost signal at all, so
// there was nothing to compare a dollar limit against. It accumulates
// across turns rather than tripping on one, which is the case that
// matters — a runaway is many affordable turns.
func TestSubagentTool_CostBudgetStopsTheDelegation(t *testing.T) {
	t.Parallel()
	h := newTestEventLog(t)
	tracker := usage.NewTracker()

	// Sanity-check the fixture before relying on it: an unpriced model
	// would make every turn cost zero and the cap unreachable, and the
	// test would pass by never stopping anything.
	if c := usage.PriceFor(pricedModel, nil).CostUSDForTurn(usage.TurnUsage{InputTokens: tokensPerCent3}); c <= 0 {
		t.Fatalf("fixture model %q prices at %v/turn; the cost cap cannot be exercised", pricedModel, c)
	}

	child, err := New(&meteredLLM{
		name: pricedModel,
		script: [][]*adkmodel.LLMResponse{
			{callTool("ping", tokensPerCent3, 0)},
			{callTool("ping", tokensPerCent3, 0)},
			{usedText("finally done", tokensPerCent3, 0)},
		},
	},
		WithName("child"),
		WithEventLog(h),
		WithSession("u", "child"),
		WithTools([]tool.Tool{pingTool(t)}),
		// Between one turn and two: turn 1 leaves it under, turn 2 trips it.
		WithSubagentBudgets(SubagentBudgets{MaxCostUSD: costPerTurnUSD3 * 1.5}),
	)
	if err != nil {
		t.Fatalf("New child: %v", err)
	}
	got := runBoundedDelegation(t, child, h, tracker)

	if tot := tracker.Totals(); tot.Turns != 2 {
		t.Errorf("turns = %d, want 2 (one turn is under the cap, two are over)", tot.Turns)
	}
	if !strings.Contains(got, "cost budget of") {
		t.Errorf("result does not name the cap that stopped it:\n%s", got)
	}
	if strings.Contains(got, "finally done") {
		t.Errorf("delegation ran past its cost cap:\n%s", got)
	}
}

// TestSubagentTool_WallclockBudgetStopsTheDelegation. Wall-clock is the
// one dimension the runner can enforce, via a derived context — but its
// expiry arrives as an error on the range, so the handler has to tell it
// apart from a real failure. Getting that wrong turns a budget stop into
// a failed tool call, which is the #691 mistake: the parent loses the
// partial and pays twice.
func TestSubagentTool_WallclockBudgetStopsTheDelegation(t *testing.T) {
	t.Parallel()
	h := newTestEventLog(t)

	child, err := New(&meteredLLM{
		name:   "child-model",
		delay:  2 * time.Second,
		script: [][]*adkmodel.LLMResponse{{usedText("finally done", 100, 10)}},
	},
		WithName("child"),
		WithEventLog(h),
		WithSession("u", "child"),
		WithSubagentBudgets(SubagentBudgets{MaxWallclock: 50 * time.Millisecond}),
	)
	if err != nil {
		t.Fatalf("New child: %v", err)
	}
	started := time.Now()
	got := runBoundedDelegation(t, child, h, nil)

	if elapsed := time.Since(started); elapsed > time.Second {
		t.Errorf("delegation took %s; the wall-clock cap did not cut it short", elapsed)
	}
	if !strings.Contains(got, "wall-clock budget of") {
		t.Errorf("result does not name the cap that stopped it:\n%s", got)
	}
	// Nothing was produced, and saying so beats a header describing
	// content that isn't there.
	if !strings.Contains(got, "produced no output") {
		t.Errorf("empty partial is not labelled as empty:\n%s", got)
	}
}

// TestSubagentTool_NoBudgetsRunsToCompletion pins the default: this door
// has always been unbounded, and turning every existing declarative
// subagent into a capped one would truncate deployments that work today.
func TestSubagentTool_NoBudgetsRunsToCompletion(t *testing.T) {
	t.Parallel()
	h := newTestEventLog(t)

	child, err := New(&meteredLLM{
		name: "child-model",
		script: [][]*adkmodel.LLMResponse{
			{callTool("ping", 100, 10)},
			{callTool("ping", 100, 10)},
			{usedText("finally done", 100, 10)},
		},
	},
		WithName("child"),
		WithEventLog(h),
		WithSession("u", "child"),
		WithTools([]tool.Tool{pingTool(t)}),
	)
	if err != nil {
		t.Fatalf("New child: %v", err)
	}
	got := runBoundedDelegation(t, child, h, nil)

	if !strings.Contains(got, "finally done") {
		t.Errorf("uncapped delegation did not run to completion:\n%s", got)
	}
	if strings.Contains(got, "partial result") {
		t.Errorf("uncapped delegation was labelled a partial:\n%s", got)
	}
}

// TestSubagentPartial_KeepsTheWorkAndLeadsWithTheLabel. The header goes
// first because a model skimming a long result reads the head, and
// "this is unfinished" changes how it should read everything after.
func TestSubagentPartial_KeepsTheWorkAndLeadsWithTheLabel(t *testing.T) {
	t.Parallel()
	got := subagentPartial("the findings so far", "cluster", "turn budget of 3")

	if !strings.HasPrefix(got, "[partial result:") {
		t.Errorf("label is not first:\n%s", got)
	}
	if !strings.Contains(got, "the findings so far") {
		t.Errorf("partial work was discarded:\n%s", got)
	}
	if !strings.Contains(got, "cluster") || !strings.Contains(got, "turn budget of 3") {
		t.Errorf("label names neither the subagent nor the cap:\n%s", got)
	}
}
