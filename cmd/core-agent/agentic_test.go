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

package main

import (
	"strings"
	"testing"
	"time"

	"github.com/go-steer/core-agent/pkg/agent"
	"github.com/go-steer/core-agent/pkg/usage"
)

func TestRenderContextStats_FreshSession(t *testing.T) {
	t.Parallel()
	out := renderContextStats(agent.ContextStats{}, 0)
	if !strings.Contains(out, "Compactions:  none yet") {
		t.Errorf("fresh-session output missing 'Compactions: none yet':\n%s", out)
	}
	if !strings.Contains(out, "Checkpoints:  none yet") {
		t.Errorf("fresh-session output missing 'Checkpoints: none yet':\n%s", out)
	}
	if !strings.Contains(out, "Subtasks:     none yet") {
		t.Errorf("fresh-session output missing 'Subtasks: none yet':\n%s", out)
	}
	// TotalSummaryChars row is hidden when zero — verify it
	// stays hidden.
	if strings.Contains(out, "Summarized:") {
		t.Errorf("fresh-session shouldn't show 'Summarized:' row:\n%s", out)
	}
}

func TestRenderContextStats_PopulatedSession(t *testing.T) {
	t.Parallel()
	s := agent.ContextStats{
		CompactionCount:     2,
		LastCompactionFocus: "auth module",
		LastCompactionTime:  time.Now().Add(-5 * time.Minute),
		CheckpointCount:     3,
		LastCheckpointNote:  "finished surveying messageKinds for the v3 design",
		LastCheckpointTime:  time.Now().Add(-30 * time.Second),
		TotalSummaryChars:   12345,
		SubtaskCount:        4,
		SubtaskInputTokens:  20000,
		SubtaskOutputTokens: 1500,
		SubtaskCostUSD:      0.0234,
	}
	out := renderContextStats(s, 0)

	for _, want := range []string{
		"Compactions:  2",
		"focus: auth module",
		"Checkpoints:  3",
		"note: finished surveying messageKinds",
		"Summarized:   12345 chars",
		"Subtasks:     4",
		"20000 in / 1500 out",
		"$0.0234",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("populated output missing %q:\n%s", want, out)
		}
	}
}

func TestRenderContextStats_TruncatesLongCheckpointNote(t *testing.T) {
	t.Parallel()
	longNote := strings.Repeat("x", 200)
	s := agent.ContextStats{
		CheckpointCount:    1,
		LastCheckpointNote: longNote,
		LastCheckpointTime: time.Now(),
	}
	out := renderContextStats(s, 0)
	if !strings.Contains(out, "...") {
		t.Errorf("expected long note to be truncated with '...', got:\n%s", out)
	}
	if strings.Contains(out, longNote) {
		t.Errorf("expected long note to be truncated, but full string appeared")
	}
}

func TestRenderContextStats_ModelBreakdownSortsByCost(t *testing.T) {
	t.Parallel()
	// Multi-model breakdown should appear, sorted by descending
	// cost so the priciest model leads the row.
	s := agent.ContextStats{
		ModelBreakdown: map[string]usage.Totals{
			"gemini-2.5-flash":              {Turns: 3, InputTokens: 12000, OutputTokens: 450, CostUSD: 0.0085},
			"gemini-3.1-pro-preview":        {Turns: 2, InputTokens: 18000, OutputTokens: 800, CostUSD: 0.101},
			"unused-model-zero-cost-tiebrk": {Turns: 1, InputTokens: 100, OutputTokens: 10, CostUSD: 0.0001},
		},
	}
	out := renderContextStats(s, 0)
	if !strings.Contains(out, "Models:") {
		t.Errorf("expected Models row in output:\n%s", out)
	}
	// Pro should appear before flash (higher cost).
	proIdx := strings.Index(out, "gemini-3.1-pro-preview")
	flashIdx := strings.Index(out, "gemini-2.5-flash")
	if proIdx < 0 || flashIdx < 0 {
		t.Fatalf("both model names should appear: %s", out)
	}
	if proIdx > flashIdx {
		t.Errorf("expected pro (higher cost) before flash, got order pro@%d flash@%d:\n%s", proIdx, flashIdx, out)
	}
	// Cost figures must appear verbatim.
	for _, want := range []string{"$0.1010", "$0.0085", "2 turns", "3 turns"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestRenderContextStats_ModelBreakdownHiddenWhenEmpty(t *testing.T) {
	t.Parallel()
	// Single-model session (or no tracker wired): ModelBreakdown
	// is empty/nil → no Models row → /context stays clean.
	s := agent.ContextStats{
		SubtaskCount: 1,
		// ModelBreakdown nil
	}
	out := renderContextStats(s, 0)
	if strings.Contains(out, "Models:") {
		t.Errorf("Models row should be hidden when breakdown is empty:\n%s", out)
	}
}

// TestRenderContextStats_DigestSavingsBlockRendersWithPricing pins the
// #223 Phase 4 contract: when the MCP wrap has fired AND parent input
// pricing is available, /context surfaces a "Digest savings" block
// with token counts + dollar figures. Regression signal: if this
// fails, the cost-reduction infra's operator-visible proof-point
// disappears from /context.
func TestRenderContextStats_DigestSavingsBlockRendersWithPricing(t *testing.T) {
	t.Parallel()
	s := agent.ContextStats{
		DigestSavings: usage.DigestSavingsTotals{
			StructuralCalls:          3,
			StructuralTokensSaved:    12_000,
			AgenticCalls:             1,
			AgenticTokensSaved:       8_000,
			AgenticSubagentInTokens:  1_500,
			AgenticSubagentOutTokens: 400,
			AgenticSubagentCostUSD:   0.0006,
			PassthroughCalls:         2,
		},
	}
	// Parent at $15/M input (roughly Opus rate) → structural saves
	// 12k * 15 / 1M = $0.18; agentic saves 8k * 15 / 1M - 0.0006 =
	// $0.12 - $0.0006 = $0.1194.
	out := renderContextStats(s, 15.0)
	for _, want := range []string{
		"Digest savings (vs. no-digest baseline)",
		"Structural: 3 calls, 12000 tokens saved (~$0.1800)",
		"Agentic:    1 calls, 8000 tokens saved (~$0.1194 net after $0.0006 subagent cost)",
		"Passthrough: 2 calls under threshold",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("digest-savings output missing %q:\n%s", want, out)
		}
	}
}

// TestRenderContextStats_DigestSavingsWithoutPricing pins the "unknown
// rate" degradation: when the parent isn't priced, the block still
// renders token counts but omits dollar figures (better than hiding
// the block entirely — operators still see the wrap layer's activity).
func TestRenderContextStats_DigestSavingsWithoutPricing(t *testing.T) {
	t.Parallel()
	s := agent.ContextStats{
		DigestSavings: usage.DigestSavingsTotals{
			StructuralCalls:       1,
			StructuralTokensSaved: 500,
		},
	}
	out := renderContextStats(s, 0)
	if !strings.Contains(out, "Structural: 1 calls, 500 tokens saved") {
		t.Errorf("expected token count row when rate is 0:\n%s", out)
	}
	if strings.Contains(out, "~$") {
		t.Errorf("no dollar figure should appear when parent rate is 0:\n%s", out)
	}
}

// TestRenderContextStats_DigestSavingsHiddenWhenNoActivity pins the
// clean-fresh-session invariant: no MCP calls → no Digest savings
// block. Otherwise /context on a session with no MCP servers shows a
// zero-block that carries no signal.
func TestRenderContextStats_DigestSavingsHiddenWhenNoActivity(t *testing.T) {
	t.Parallel()
	out := renderContextStats(agent.ContextStats{}, 15.0)
	if strings.Contains(out, "Digest savings") {
		t.Errorf("empty digest-savings totals should hide the block:\n%s", out)
	}
}
