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

package usage

import (
	"encoding/json"
	"testing"

	"google.golang.org/adk/v2/session"
)

// A side call (the auto-mode approver, #1175) is spend, not a
// measurement of the conversation: it counts in the totals the cost
// ceilings read, and leaves everything that reads Last — the context
// window, the context fill, the compaction tier — on the conversation's
// own model call.
func TestAppendSideUsage_CountsCostButIsNotTheLastTurn(t *testing.T) {
	t.Parallel()
	tr := NewTracker()
	fired := 0
	tr.SetOnAppend(func() { fired++ })
	tr.Append("claude-opus-4-7", 50000, 100, Pricing{InputPerMTok: 1})
	tr.AddPendingContextBytes(4000)
	before := tr.ContextWindowUsed()

	side := tr.AppendSideUsage("gemini-3.5-flash", TurnUsage{InputTokens: 900, OutputTokens: 30}, Pricing{InputPerMTok: 1, OutputPerMTok: 1})
	if !side.Side || side.CostUSD <= 0 {
		t.Fatalf("side turn = %+v, want Side and a cost", side)
	}

	last, ok := tr.Last()
	if !ok || last.Model != "claude-opus-4-7" || last.InputTokens != 50000 {
		t.Errorf("Last() = %+v, want the conversation's own turn", last)
	}
	if got := tr.ContextWindowSize(); got != 1_000_000 {
		t.Errorf("ContextWindowSize() = %d, want the conversation model's 1_000_000", got)
	}
	if got := tr.ContextWindowUsed(); got != before {
		t.Errorf("ContextWindowUsed() = %d after a side call, want %d unchanged", got, before)
	}
	if got := tr.PendingContextBytes(); got != 4000 {
		t.Errorf("PendingContextBytes() = %d, want 4000: a side call measures nothing", got)
	}
	tot := tr.Totals()
	if tot.Turns != 2 || tot.InputTokens != 50900 || tot.CostUSD <= 0.05 {
		t.Errorf("Totals() = %+v, want both calls counted", tot)
	}
	if by := tr.TotalsByModel()["gemini-3.5-flash"]; by.Turns != 1 || by.CostUSD != side.CostUSD {
		t.Errorf("TotalsByModel()[side model] = %+v, want the side call", by)
	}
	if fired != 1 {
		t.Errorf("onAppend fired %d times, want 1 (not for the side call)", fired)
	}
}

func TestLast_OnlySideCalls(t *testing.T) {
	t.Parallel()
	tr := NewTracker()
	tr.AppendSideUsage("m", TurnUsage{InputTokens: 1}, Pricing{})
	if _, ok := tr.Last(); ok {
		t.Error("Last() found a turn when only side calls were recorded")
	}
}

// A side call's spend survives a restart: the row its usage is recorded
// on replays through AppendSideUsage, so the session's cost — and the
// cost ceilings that read it — come back whole, and Last is still the
// conversation's own turn. The numbers are put through a JSON round
// trip first, as a persisted row's are.
func TestRebuild_ReplaysSideUsage(t *testing.T) {
	t.Parallel()
	raw, err := json.Marshal(SideUsageMetadata("judge-model", TurnUsage{InputTokens: 900, OutputTokens: 30}))
	if err != nil {
		t.Fatal(err)
	}
	var meta map[string]any
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatal(err)
	}
	row := &session.Event{}
	row.CustomMetadata = meta
	events := []*session.Event{mkEvent(100, 150, false, "main-model"), row}
	tr := NewTracker()
	if err := RebuildTrackerFromEvents(t.Context(), tr, seqFromEvents(events), "main-model",
		func(string) Pricing { return Pricing{InputPerMTok: 1, OutputPerMTok: 1} }); err != nil {
		t.Fatal(err)
	}
	if by := tr.TotalsByModel()["judge-model"]; by.InputTokens != 900 || by.OutputTokens != 30 || by.CostUSD <= 0 {
		t.Errorf("replayed side usage = %+v, want the approver call's tokens and a cost", by)
	}
	if last, _ := tr.Last(); last.Model != "main-model" {
		t.Errorf("Last() after rebuild = %+v, want the conversation's turn", last)
	}
}
