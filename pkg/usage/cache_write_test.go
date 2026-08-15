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

// Cost accounting for the cache-WRITE bucket (#263). Anthropic bills
// cache_creation_input_tokens at a premium over base input (1.25x on
// the 5-minute TTL); before this the provider folded them into the
// uncached remainder and every cache-warming turn was billed at 1x.

package usage

import (
	"encoding/json"
	"math"
	"testing"

	"google.golang.org/genai"
)

// opus5 mirrors pkg/pricing/builtin.go's claude-opus-5 row: $5/MTok
// input, $0.50 cache read (0.1x), $6.25 cache write (1.25x), $25
// output. Written out rather than looked up so the test pins the
// arithmetic, not the catalog.
var opus5 = Pricing{
	InputPerMTok:              5,
	CachedInputPerMTok:        0.5,
	CacheCreationInputPerMTok: 6.25,
	OutputPerMTok:             25,
}

const epsilon = 1e-9

// warmingTurn is one cache-warming turn as Anthropic reports it,
// already mapped into genai's shape by the provider: PromptTokenCount
// is the sum of all three input buckets, CachedContentTokenCount is
// just the reads, and the writes ride the CustomMetadata sidecar.
func warmingTurn() (*genai.GenerateContentResponseUsageMetadata, map[string]any) {
	return &genai.GenerateContentResponseUsageMetadata{
			PromptTokenCount:        1_000 + 20_000 + 4_000,
			CachedContentTokenCount: 20_000,
			CandidatesTokenCount:    500,
		}, map[string]any{
			CacheCreationTokensMetadataKey: int64(4_000),
		}
}

// TestAppendUsage_BillsCacheWritesAtThePremiumRate is the regression
// test for #263. The turn writes 4k tokens of cache; at Opus 5 rates
// that costs $6.25/MTok, not the $5/MTok the uncached bucket pays.
//
// Pre-fix the sidecar was unreadable and the 4k landed in `uncached`,
// producing $0.0475 — which this test also pins explicitly, so a
// regression can't pass by coincidence.
func TestAppendUsage_BillsCacheWritesAtThePremiumRate(t *testing.T) {
	t.Parallel()
	meta, custom := warmingTurn()
	u := TurnUsageFromMetadata(meta, custom)

	if u.CacheCreationInputTokens != 4_000 {
		t.Fatalf("CacheCreationInputTokens = %d, want 4000 (read from the CustomMetadata sidecar)",
			u.CacheCreationInputTokens)
	}
	if got := u.UncachedInputTokens(); got != 1_000 {
		t.Errorf("UncachedInputTokens() = %d, want 1000 (prompt - reads - writes)", got)
	}

	tr := NewTracker()
	turn := tr.AppendUsage("claude-opus-5", u, opus5)

	//  1,000 uncached * $5.00/MTok = $0.0050
	// 20,000 reads    * $0.50/MTok = $0.0100
	//  4,000 writes   * $6.25/MTok = $0.0250
	//    500 output   * $25.0/MTok = $0.0125
	const want = 0.0050 + 0.0100 + 0.0250 + 0.0125
	// The pre-fix number, named so a failure says WHICH wrong answer it
	// got: 5,000 uncached (writes folded in) * $5.00 = $0.0250, plus the
	// same reads and output.
	const undercount = 0.0250 + 0.0100 + 0.0125
	if math.Abs(turn.CostUSD-want) > epsilon {
		hint := ""
		if math.Abs(turn.CostUSD-undercount) <= epsilon {
			hint = " — that's the pre-#263 undercount: cache writes are still billed at the input rate"
		}
		t.Errorf("CostUSD = %.6f, want %.6f%s", turn.CostUSD, want, hint)
	}

	if turn.CacheCreationInputTokens != 4_000 {
		t.Errorf("Turn.CacheCreationInputTokens = %d, want 4000", turn.CacheCreationInputTokens)
	}
	if got := turn.UncachedInputTokens(); got != 1_000 {
		t.Errorf("Turn.UncachedInputTokens() = %d, want 1000", got)
	}
	if tot := tr.Totals(); tot.CacheCreationInputTokens != 4_000 || tot.UncachedInputTokens() != 1_000 {
		t.Errorf("Totals cache-write/uncached = %d/%d, want 4000/1000",
			tot.CacheCreationInputTokens, tot.UncachedInputTokens())
	}
}

// TestCostUSDForTurn_MatchesTheTracker pins that the tracker-less
// fallbacks in pkg/agent (subtask.go's SkipParentUsage branch,
// autonomous.go's no-tracker branch) price a turn identically to
// Tracker.AppendUsage. They used to open-code their own arithmetic and
// drifted — one billed the whole prompt at the base input rate.
func TestCostUSDForTurn_MatchesTheTracker(t *testing.T) {
	t.Parallel()
	meta, custom := warmingTurn()
	u := TurnUsageFromMetadata(meta, custom)

	viaTracker := NewTracker().AppendUsage("claude-opus-5", u, opus5).CostUSD
	viaPricing := opus5.CostUSDForTurn(u)
	if math.Abs(viaTracker-viaPricing) > epsilon {
		t.Errorf("CostUSDForTurn = %.6f, tracker = %.6f; the two must agree", viaPricing, viaTracker)
	}

	// CostUSD (the no-cache-awareness helper) bills everything at the
	// input rate — strictly more than the split. Pinning the ordering
	// keeps a future "simplification" from collapsing them.
	if flat := opus5.CostUSD(u.InputTokens, u.OutputTokens); flat <= viaPricing {
		t.Errorf("flat CostUSD = %.6f, want > cache-split cost %.6f", flat, viaPricing)
	}
}

// TestTurnUsageFromMetadata_SidecarShapes covers every numeric shape
// the sidecar value arrives in. Freshly stamped it's an int64; after a
// JSON round-trip through the eventlog it's a float64; a decoder
// configured with UseNumber yields json.Number. All three must read the
// same, or cost silently reverts to the undercount on reload.
func TestTurnUsageFromMetadata_SidecarShapes(t *testing.T) {
	t.Parallel()
	meta := &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 10_000}
	for name, val := range map[string]any{
		"int":         4_000,
		"int32":       int32(4_000),
		"int64":       int64(4_000),
		"float64":     float64(4_000),
		"json.Number": json.Number("4000"),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := TurnUsageFromMetadata(meta, map[string]any{CacheCreationTokensMetadataKey: val})
			if got.CacheCreationInputTokens != 4_000 {
				t.Errorf("CacheCreationInputTokens = %d, want 4000", got.CacheCreationInputTokens)
			}
		})
	}
}

// TestTurnUsageFromMetadata_SidecarSurvivesJSONRoundTrip walks the
// actual persistence shape: the provider stamps an int64, the eventlog
// serializes the map, and Rebuild reads it back. A reloaded session
// must report the same cost the live run did.
func TestTurnUsageFromMetadata_SidecarSurvivesJSONRoundTrip(t *testing.T) {
	t.Parallel()
	meta, custom := warmingTurn()

	raw, err := json.Marshal(custom)
	if err != nil {
		t.Fatalf("marshal CustomMetadata: %v", err)
	}
	var reloaded map[string]any
	if err := json.Unmarshal(raw, &reloaded); err != nil {
		t.Fatalf("unmarshal CustomMetadata: %v", err)
	}

	live := opus5.CostUSDForTurn(TurnUsageFromMetadata(meta, custom))
	after := opus5.CostUSDForTurn(TurnUsageFromMetadata(meta, reloaded))
	if math.Abs(live-after) > epsilon {
		t.Errorf("cost after eventlog round-trip = %.6f, want %.6f (live)", after, live)
	}
}

// TestTurnUsageFromMetadata_BadSidecarDegradesToZero pins that a
// missing, malformed, or nonsensical sidecar reads as "no cache
// writes" rather than panicking or inventing a charge.
func TestTurnUsageFromMetadata_BadSidecarDegradesToZero(t *testing.T) {
	t.Parallel()
	meta := &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 10_000}
	for name, custom := range map[string]map[string]any{
		"nil map":      nil,
		"empty map":    {},
		"absent key":   {"something_else": int64(4_000)},
		"string value": {CacheCreationTokensMetadataKey: "4000"},
		"nil value":    {CacheCreationTokensMetadataKey: nil},
		"negative":     {CacheCreationTokensMetadataKey: int64(-4_000)},
		"unparseable":  {CacheCreationTokensMetadataKey: json.Number("not-a-number")},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := TurnUsageFromMetadata(meta, custom).CacheCreationInputTokens; got != 0 {
				t.Errorf("CacheCreationInputTokens = %d, want 0", got)
			}
		})
	}
}

// TestTurnUsageFromGenaiMetadata_StillWorks pins that the pre-existing
// sidecar-less entry point is unchanged for the Gemini/Vertex path,
// which has no cache-write bucket to report.
func TestTurnUsageFromGenaiMetadata_StillWorks(t *testing.T) {
	t.Parallel()
	if got := TurnUsageFromGenaiMetadata(nil); got != (TurnUsage{}) {
		t.Errorf("nil metadata = %+v, want zero TurnUsage", got)
	}
	got := TurnUsageFromGenaiMetadata(&genai.GenerateContentResponseUsageMetadata{
		PromptTokenCount:        25_000,
		CachedContentTokenCount: 20_000,
		CandidatesTokenCount:    500,
		ThoughtsTokenCount:      42,
		ToolUsePromptTokenCount: 7,
	})
	want := TurnUsage{
		InputTokens: 25_000, CachedInputTokens: 20_000,
		OutputTokens: 500, ThoughtsTokens: 42, ToolUseTokens: 7,
	}
	if got != want {
		t.Errorf("TurnUsage = %+v, want %+v", got, want)
	}
}

// TestClamped_KeepsBucketsDisjoint pins the defensive arithmetic: the
// three input buckets must partition InputTokens, so the uncached
// remainder can never go negative and drive a negative cost.
func TestClamped_KeepsBucketsDisjoint(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name                             string
		in                               TurnUsage
		wantCached, wantWrite, wantUncap int
	}{
		{"normal split",
			TurnUsage{InputTokens: 25_000, CachedInputTokens: 20_000, CacheCreationInputTokens: 4_000},
			20_000, 4_000, 1_000},
		{"writes overflow the remainder",
			TurnUsage{InputTokens: 25_000, CachedInputTokens: 20_000, CacheCreationInputTokens: 9_000},
			20_000, 5_000, 0},
		{"reads alone exceed the prompt",
			TurnUsage{InputTokens: 1_000, CachedInputTokens: 4_000, CacheCreationInputTokens: 500},
			1_000, 0, 0},
		{"negative counters floor at zero",
			TurnUsage{InputTokens: 1_000, CachedInputTokens: -5, CacheCreationInputTokens: -5},
			0, 0, 1_000},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := tc.in.Clamped()
			if c.CachedInputTokens != tc.wantCached || c.CacheCreationInputTokens != tc.wantWrite {
				t.Errorf("clamped cached/write = %d/%d, want %d/%d",
					c.CachedInputTokens, c.CacheCreationInputTokens, tc.wantCached, tc.wantWrite)
			}
			if got := tc.in.UncachedInputTokens(); got != tc.wantUncap {
				t.Errorf("UncachedInputTokens() = %d, want %d", got, tc.wantUncap)
			}
			if cost := opus5.CostUSDForTurn(tc.in); cost < 0 {
				t.Errorf("CostUSDForTurn = %.6f, must never go negative", cost)
			}
		})
	}
}

// TestCostUSDForTurn_UnknownWriteRateFallsBackToInput pins the
// degradation path: a model whose catalog row predates the cache-write
// column bills writes at the base input rate — the old, understated
// number — rather than treating them as free.
func TestCostUSDForTurn_UnknownWriteRateFallsBackToInput(t *testing.T) {
	t.Parallel()
	stale := Pricing{InputPerMTok: 5, CachedInputPerMTok: 0.5, OutputPerMTok: 25} // no write rate
	u := TurnUsage{InputTokens: 25_000, CachedInputTokens: 20_000, CacheCreationInputTokens: 4_000}

	// 1,000 uncached + 4,000 writes both at $5/MTok, 20,000 reads at $0.50.
	const want = (1_000+4_000)*5/1e6 + 20_000*0.5/1e6
	if got := stale.CostUSDForTurn(u); math.Abs(got-want) > epsilon {
		t.Errorf("CostUSDForTurn with unknown write rate = %.6f, want %.6f (input-rate fallback)", got, want)
	}
	// Same rates, same prompt, but with the 4,000 written tokens counted
	// as reads instead. The fallback must land strictly above it: an
	// unknown write rate degrades to the base input rate, never to the
	// cache-read discount.
	asReads := stale.CostUSDForTurn(TurnUsage{InputTokens: 25_000, CachedInputTokens: 24_000})
	if got := stale.CostUSDForTurn(u); got <= asReads {
		t.Errorf("fallback billed %.6f, not more than the bill-writes-as-reads number %.6f", got, asReads)
	}
}
