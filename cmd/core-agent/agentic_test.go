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

	"github.com/go-steer/core-agent/agent"
	"github.com/go-steer/core-agent/usage"
)

func TestRenderContextStats_FreshSession(t *testing.T) {
	t.Parallel()
	out := renderContextStats(agent.ContextStats{})
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
	out := renderContextStats(s)

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
	out := renderContextStats(s)
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
	out := renderContextStats(s)
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
	out := renderContextStats(s)
	if strings.Contains(out, "Models:") {
		t.Errorf("Models row should be hidden when breakdown is empty:\n%s", out)
	}
}
