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

package attach

import (
	"strings"
	"testing"
	"time"
)

func TestRenderUsage_Empty(t *testing.T) {
	t.Parallel()
	got := RenderUsage(UsageInfo{})
	if !strings.Contains(got, "no turns recorded") {
		t.Errorf("empty UsageInfo should render the no-turns placeholder, got:\n%s", got)
	}
}

func TestRenderUsage_WithCacheSavings(t *testing.T) {
	t.Parallel()
	info := UsageInfo{
		Overall: UsageTotals{
			Turns:                    2,
			InputTokens:              20_000,
			InputTokensCached:        8_000,
			InputTokensUncached:      12_000,
			OutputTokens:             1_000,
			CostUSD:                  0.0175,
			CostUSDUncachedReference: 0.030,
		},
		PerTurn: []UsageTurn{
			{Turn: 1, At: time.Date(2026, 7, 14, 10, 0, 0, 0, time.UTC), Model: "gemini-3.1-pro",
				InputTokens: 10_000, OutputTokens: 500, CostUSD: 0.015},
			{Turn: 2, At: time.Date(2026, 7, 14, 10, 0, 5, 0, time.UTC), Model: "gemini-3.1-pro",
				InputTokens: 10_000, InputTokensCached: 8_000, OutputTokens: 500, CostUSD: 0.0025},
		},
	}
	got := RenderUsage(info)
	// Cache-savings line must render when reference > actual.
	if !strings.Contains(got, "cache saved") {
		t.Errorf("expected cache-savings line when ref > actual:\n%s", got)
	}
	// Comma grouping on the 20k input total.
	if !strings.Contains(got, "20,000") {
		t.Errorf("expected comma-grouped input tokens:\n%s", got)
	}
	// Per-turn section renders and shows the cached tag on the warm turn.
	if !strings.Contains(got, "#2") || !strings.Contains(got, "8,000 cached") {
		t.Errorf("expected per-turn row with cached tag for turn 2:\n%s", got)
	}
	// Cold turn's row should NOT carry a cached tag.
	line1 := extractLineWith(t, got, "#1 ")
	if strings.Contains(line1, "cached") {
		t.Errorf("cold turn (#1) should not carry a cached tag: %q", line1)
	}
}

func TestRenderUsage_NoSavingsWhenReferenceEqualsActual(t *testing.T) {
	t.Parallel()
	// Sessions with no cache hits: uncached-ref == actual, so the
	// "cache saved" line must be suppressed (nothing to say).
	info := UsageInfo{
		Overall: UsageTotals{
			Turns:                    1,
			InputTokens:              10_000,
			InputTokensUncached:      10_000,
			OutputTokens:             500,
			CostUSD:                  0.015,
			CostUSDUncachedReference: 0.015,
		},
	}
	got := RenderUsage(info)
	if strings.Contains(got, "cache saved") {
		t.Errorf("no-cache session should suppress the cache-savings line:\n%s", got)
	}
}

func TestRenderUsage_PerModelSection(t *testing.T) {
	t.Parallel()
	info := UsageInfo{
		Overall: UsageTotals{Turns: 5, InputTokens: 100_000, OutputTokens: 5_000, CostUSD: 0.15},
		PerModel: map[string]UsageTotals{
			"gemini-3.1-pro": {Turns: 3, InputTokens: 80_000, OutputTokens: 4_000, CostUSD: 0.13},
			"gemini-3-flash": {Turns: 2, InputTokens: 20_000, OutputTokens: 1_000, CostUSD: 0.02},
		},
	}
	got := RenderUsage(info)
	if !strings.Contains(got, "Per model") {
		t.Errorf("expected Per model section header:\n%s", got)
	}
	if !strings.Contains(got, "gemini-3.1-pro") || !strings.Contains(got, "gemini-3-flash") {
		t.Errorf("expected both model rows:\n%s", got)
	}
}

func TestRenderUsage_LongSessionTruncation(t *testing.T) {
	t.Parallel()
	// 50 turns → per-turn section caps at 20 with a "(last 20 of 50)"
	// annotation so scrollback doesn't blow up.
	turns := make([]UsageTurn, 50)
	for i := range turns {
		turns[i] = UsageTurn{Turn: i + 1, InputTokens: 100, OutputTokens: 10}
	}
	got := RenderUsage(UsageInfo{
		Overall: UsageTotals{Turns: 50, InputTokens: 5_000, OutputTokens: 500},
		PerTurn: turns,
	})
	if !strings.Contains(got, "(last 20 of 50)") {
		t.Errorf("expected truncation annotation:\n%s", got)
	}
	// First recorded per-turn row is #31 (50 - 20 + 1), not #1.
	if !strings.Contains(got, "#31 ") {
		t.Errorf("expected first rendered row to be #31 (tail slice):\n%s", got)
	}
	if strings.Contains(got, "#1 ") {
		t.Errorf("did not expect #1 in the tail-truncated view:\n%s", got)
	}
}

func TestCommas(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0"},
		{999, "999"},
		{1_000, "1,000"},
		{1_234_567, "1,234,567"},
		{-42_000, "-42,000"},
	}
	for _, c := range cases {
		if got := commas(c.in); got != c.want {
			t.Errorf("commas(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

// extractLineWith returns the first line in s that contains sub.
// Test helper — fails the test if no match found.
func extractLineWith(t *testing.T, s, sub string) string {
	t.Helper()
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, sub) {
			return line
		}
	}
	t.Fatalf("no line containing %q in:\n%s", sub, s)
	return ""
}
