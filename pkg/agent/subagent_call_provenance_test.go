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

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/adk/tool"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/pkg/eventlog"
)

// callToolWith is callTool with arguments, which is what makes a
// recorded call worth citing: "it read something" is not evidence,
// "it read pods in namespace shop" is.
func callToolWith(name string, args map[string]any) *adkmodel.LLMResponse {
	return &adkmodel.LLMResponse{
		Content: &genai.Content{
			Role:  genai.RoleModel,
			Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{Name: name, Args: args}}},
		},
		TurnComplete: true,
		// Usage metadata is what makes the turn countable: the budget
		// caps are driven off the usage tap, so a scripted turn without
		// it never trips a MaxTurns fixture.
		UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
			PromptTokenCount:     100,
			CandidatesTokenCount: 10,
		},
	}
}

// delegationResponse runs one parent turn that delegates to child and
// returns the raw tool-result map the parent's model saw. The map
// rather than its "result" string: the whole point of this change is
// the fields BESIDE the prose.
func delegationResponse(t *testing.T, child *Agent, h *eventlog.Handle) map[string]any {
	t.Helper()
	parent, err := New(&fnCallThenDoneLLM{target: child.AgentName()},
		WithName("parent"),
		WithEventLog(h),
		WithSession("u", "parent"),
		WithSubagents([]*Agent{child}),
	)
	if err != nil {
		t.Fatalf("New parent: %v", err)
	}
	var got []map[string]any
	for ev, err := range parent.Run(context.Background(), "go") {
		if err != nil {
			t.Fatalf("parent.Run: %v", err)
		}
		if ev == nil || ev.Content == nil {
			continue
		}
		for _, p := range ev.Content.Parts {
			if p != nil && p.FunctionResponse != nil && p.FunctionResponse.Name == child.AgentName() {
				got = append(got, p.FunctionResponse.Response)
			}
		}
	}
	if len(got) != 1 {
		t.Fatalf("got %d subagent tool results, want 1", len(got))
	}
	return got[0]
}

// TestSubagentTool_HandsBackTheCallsItMade is #1014 at the synchronous
// door. The parent is graded on grounding its claims, the child hands
// back prose, and prose cannot be cited — so across the fifteen
// archived GKE drill runs the parent re-issued 48% of the reads its
// child had already done. It no longer has to: the calls come back as
// the runtime observed them.
func TestSubagentTool_HandsBackTheCallsItMade(t *testing.T) {
	t.Parallel()
	h := newTestEventLog(t)

	child, err := New(&meteredLLM{
		name: "child-model",
		script: [][]*adkmodel.LLMResponse{
			{callToolWith("ping", map[string]any{"namespace": "shop"})},
			{usedText("shop is unschedulable", 10, 5)},
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

	resp := delegationResponse(t, child, h)

	calls, ok := resp["calls"].([]any)
	if !ok || len(calls) != 1 {
		t.Fatalf("calls = %#v, want the one call the child made", resp["calls"])
	}
	call, _ := calls[0].(map[string]any)
	if call["tool"] != "ping" {
		t.Errorf("recorded tool = %v, want %q", call["tool"], "ping")
	}
	args, _ := call["args"].(map[string]any)
	if args["namespace"] != "shop" {
		t.Errorf("recorded args = %#v, want the namespace the child asked about", call["args"])
	}
	note, _ := resp["calls_note"].(string)
	if !strings.Contains(note, "Cite them") {
		t.Errorf("calls_note = %q, want the sentence that tells the parent it may cite them", note)
	}
	if res, _ := resp["result"].(string); !strings.Contains(res, "shop is unschedulable") {
		t.Errorf("result = %q; provenance is supposed to travel BESIDE the findings, not replace them", res)
	}
}

// TestSubagentTool_ACappedDelegationStillHandsBackItsCalls. The partial
// path is where provenance matters most: a budget-capped subagent
// returns the least prose, so it is the case where a parent has most
// reason to go re-read for itself. Both return sites share one helper
// precisely so this path cannot be the one that gets missed.
func TestSubagentTool_ACappedDelegationStillHandsBackItsCalls(t *testing.T) {
	t.Parallel()
	h := newTestEventLog(t)

	child, err := New(&meteredLLM{
		name: "child-model",
		script: [][]*adkmodel.LLMResponse{
			{callToolWith("ping", map[string]any{"namespace": "shop"})},
			{callToolWith("ping", map[string]any{"namespace": "cart"})},
			{usedText("never reached", 10, 5)},
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

	resp := delegationResponse(t, child, h)

	if res, _ := resp["result"].(string); !strings.Contains(res, "partial result") {
		t.Fatalf("fixture did not hit the cap; result = %q", res)
	}
	calls, ok := resp["calls"].([]any)
	if !ok || len(calls) != 2 {
		t.Fatalf("a capped delegation returned %#v, want the two calls it made before the cap", resp["calls"])
	}
}

// TestSubagentTool_ASilentChildReturnsTheOldShape. A subagent that
// called no tools must serialize exactly as it did before this change:
// an empty array plus a sentence explaining the empty array is pure
// prompt noise on every delegation that never needed the fix.
func TestSubagentTool_ASilentChildReturnsTheOldShape(t *testing.T) {
	t.Parallel()
	h := newTestEventLog(t)

	child, err := New(&meteredLLM{
		name:   "child-model",
		script: [][]*adkmodel.LLMResponse{{usedText("nothing to look at", 10, 5)}},
	},
		WithName("child"),
		WithEventLog(h),
		WithSession("u", "child"),
	)
	if err != nil {
		t.Fatalf("New child: %v", err)
	}

	resp := delegationResponse(t, child, h)
	for _, field := range []string{"calls", "calls_note", "calls_truncated"} {
		if v, present := resp[field]; present {
			t.Errorf("a child that called nothing still emitted %q = %#v", field, v)
		}
	}
}
