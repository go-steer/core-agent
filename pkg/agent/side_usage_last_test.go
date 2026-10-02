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

	"github.com/go-steer/core-agent/v2/pkg/usage"
)

// #1191. Tracker.Last() stands for the conversation: the context gauge
// reads its input count, the window is looked up by its model, the
// compaction tier is classified from its model, and the #975 estimate of
// tool results appended since the parent's last call is cleared by it.
//
// An agentic_* subtask turn and a digest-summarizer call are model calls
// made mid-turn on someone else's behalf. Recorded as ordinary turns,
// each one became Last(): the gauge reported the subtask's prompt as the
// conversation's fill, the window and tier came from a cheaper model,
// and the pending tool-result estimate was thrown away — under-reporting
// fill in exactly the direction where compaction fails to fire before
// the provider rejects the request. The soak recipe sets
// agentic_wrap_llm, so this is the A1 path, not a corner.
//
// The spend still counts: both must still move Totals, which is what
// the cost ceilings read.

const (
	conversationModel = "claude-opus-5"
	conversationFill  = 90_000
	pendingToolBytes  = 40_000
)

// seedConversation lands one parent turn and a large tool result after
// it, the state a subtask or digest call interrupts.
func seedConversation(t *testing.T) *usage.Tracker {
	t.Helper()
	tr := usage.NewTracker()
	tr.AppendUsage(conversationModel, usage.TurnUsage{InputTokens: conversationFill, OutputTokens: 100}, usage.Pricing{InputPerMTok: 1, OutputPerMTok: 1})
	tr.AddPendingContextBytes(pendingToolBytes)
	return tr
}

func assertStillTheConversation(t *testing.T, tr *usage.Tracker, inputBefore int, who string) {
	t.Helper()
	last, ok := tr.Last()
	if !ok || last.Model != conversationModel || last.InputTokens != conversationFill {
		t.Errorf("after a %s call, Last() = {%s, %d in}; want the conversation's {%s, %d in}", who, last.Model, last.InputTokens, conversationModel, conversationFill)
	}
	used, estimated := tr.ContextWindowUsedEstimated()
	if !estimated || used <= conversationFill {
		t.Errorf("after a %s call the context gauge reads %d (estimated=%v); want more than %d — the tool result appended since the parent's last call was dropped from the estimate", who, used, estimated, conversationFill)
	}
	// Tokens rather than dollars, so the assertion does not depend on
	// the test model carrying a price.
	if tr.Totals().InputTokens <= inputBefore {
		t.Errorf("the %s call's usage did not reach Totals (%d input tokens before, %d after), so the cost ceilings would not see its spend", who, inputBefore, tr.Totals().InputTokens)
	}
}

func TestASubtaskTurnDoesNotStandInForTheConversation(t *testing.T) {
	t.Parallel()
	tr := seedConversation(t)
	before := tr.Totals().InputTokens
	a, err := New(&captureLLM{response: "ok", inputTokens: 500, outputTokens: 50}, WithUsageTracker(tr))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.RunSubtask(context.Background(), SubtaskSpec{Name: "s", SystemPrompt: "x", UserMessage: "y"}); err != nil {
		t.Fatal(err)
	}
	assertStillTheConversation(t, tr, before, "subtask")
}

func TestADigestCallDoesNotStandInForTheConversation(t *testing.T) {
	t.Parallel()
	tr := seedConversation(t)
	before := tr.Totals().InputTokens
	a, err := New(&captureLLM{response: "ok"}, WithUsageTracker(tr))
	if err != nil {
		t.Fatal(err)
	}
	a.chargeDigestSubagent(recordFromSidecar(t, thinkingSidecar(4_000, 120, 0)))
	assertStillTheConversation(t, tr, before, "digest")
}
