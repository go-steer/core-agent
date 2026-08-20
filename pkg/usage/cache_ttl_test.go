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
	"math"
	"testing"

	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/pkg/config"
)

// ttlPricing uses the published Anthropic multipliers over a round base
// so a bucket landing on the wrong rate shows up as a wrong multiple.
var ttlPricing = Pricing{
	InputPerMTok:                10,
	CachedInputPerMTok:          1,
	CacheCreationInputPerMTok:   12.5,
	CacheCreation1hInputPerMTok: 20,
	OutputPerMTok:               50,
}

func nearUSD(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// The reason the field exists: a turn whose writes went to a 1-hour
// breakpoint costs 60% more than the same turn at 5 minutes, and the
// ledger — which also backs --max-session-cost-usd — has to say so.
func TestCostUSDForTurn_PricesTheOneHourShareSeparately(t *testing.T) {
	t.Parallel()
	base := TurnUsage{InputTokens: 1_000_000, CacheCreationInputTokens: 1_000_000}

	at5m := ttlPricing.CostUSDForTurn(base)
	oneHour := base
	oneHour.CacheCreation1hInputTokens = 1_000_000
	at1h := ttlPricing.CostUSDForTurn(oneHour)

	if !nearUSD(at5m, 12.5) {
		t.Errorf("5m turn = %v, want 12.5", at5m)
	}
	if !nearUSD(at1h, 20) {
		t.Errorf("1h turn = %v, want 20", at1h)
	}
	if at1h <= at5m {
		t.Fatalf("1h turn (%v) priced no higher than the 5m one (%v)", at1h, at5m)
	}
}

// The subset is clamped into the write bucket it lives in — a provider
// or a JSON sidecar reporting more 1h writes than writes can only ever
// shrink to the total, never bill tokens the turn never wrote.
func TestTurnUsage_ClampedBoundsTheOneHourSubset(t *testing.T) {
	t.Parallel()
	got := TurnUsage{
		InputTokens:                1000,
		CacheCreationInputTokens:   400,
		CacheCreation1hInputTokens: 900,
	}.Clamped()
	if got.CacheCreation1hInputTokens != 400 {
		t.Errorf("1h subset = %d, want it clamped to the 400-token write bucket", got.CacheCreation1hInputTokens)
	}

	neg := TurnUsage{
		InputTokens:                1000,
		CacheCreationInputTokens:   400,
		CacheCreation1hInputTokens: -5,
	}.Clamped()
	if neg.CacheCreation1hInputTokens != 0 {
		t.Errorf("negative 1h subset = %d, want 0", neg.CacheCreation1hInputTokens)
	}

	// Clamping the write bucket down must drag the subset with it —
	// otherwise a contradictory triple leaves the dearest rate applied
	// to tokens the clamp just removed.
	cascade := TurnUsage{
		InputTokens:                100,
		CachedInputTokens:          100,
		CacheCreationInputTokens:   50,
		CacheCreation1hInputTokens: 50,
	}.Clamped()
	if cascade.CacheCreationInputTokens != 0 || cascade.CacheCreation1hInputTokens != 0 {
		t.Errorf("clamped = %+v, want both write buckets emptied by the full cache-read", cascade)
	}
}

// The count crosses the provider boundary on CustomMetadata, the one
// per-event map that survives ADK's persist round-trip — so a session
// rebuilt from the eventlog reprices identically to the live run.
func TestTurnUsageFromMetadata_ReadsTheOneHourKey(t *testing.T) {
	t.Parallel()
	um := &genai.GenerateContentResponseUsageMetadata{
		PromptTokenCount:        1000,
		CachedContentTokenCount: 200,
		CandidatesTokenCount:    50,
	}
	for _, tc := range []struct {
		name string
		val  any
		want int
	}{
		{"int64 in-process", int64(300), 300},
		{"float64 after a JSON decode", float64(300), 300},
		{"json.Number", json.Number("300"), 300},
		{"negative", int64(-1), 0},
		{"wrong type", "300", 0},
	} {
		got := TurnUsageFromMetadata(um, map[string]any{
			CacheCreationTokensMetadataKey:   int64(400),
			CacheCreation1hTokensMetadataKey: tc.val,
		})
		if got.CacheCreation1hInputTokens != tc.want {
			t.Errorf("%s: 1h = %d, want %d", tc.name, got.CacheCreation1hInputTokens, tc.want)
		}
		if got.CacheCreationInputTokens != 400 {
			t.Errorf("%s: the total write bucket changed to %d", tc.name, got.CacheCreationInputTokens)
		}
	}

	// A recording made before #770 — or any provider with one TTL —
	// carries no key at all and must price exactly as it did before.
	old := TurnUsageFromMetadata(um, map[string]any{CacheCreationTokensMetadataKey: int64(400)})
	if old.CacheCreation1hInputTokens != 0 {
		t.Errorf("absent key read as %d, want 0", old.CacheCreation1hInputTokens)
	}
	if !nearUSD(ttlPricing.CostUSDForTurn(old), ttlPricing.CostUSDWithCacheWrites(400, 200, 400, 50)) {
		t.Error("a bucket-less turn no longer prices the way it did before the split")
	}
}

// The metadata keys are a cross-package wire contract: pkg/models/*
// spells them literally to avoid depending on the accounting layer, so
// the spelling has to be pinned somewhere both sides can see.
func TestCacheCreation1hTokensMetadataKey_Spelling(t *testing.T) {
	t.Parallel()
	if CacheCreation1hTokensMetadataKey != "cache_creation_1h_input_tokens" {
		t.Errorf("key = %q; pkg/models/anthropic hard-codes the old spelling", CacheCreation1hTokensMetadataKey)
	}
}

// A per-model operator override has to be able to state the 1-hour rate
// too — otherwise an operator on a model the catalog doesn't know can
// correct the 5-minute rate and silently leave the 1-hour one wrong.
func TestPriceFor_ConfigOverrideCarriesTheOneHourRate(t *testing.T) {
	SetCatalog(nil)
	t.Cleanup(func() { SetCatalog(nil) })

	cfg := &config.Config{}
	cfg.Model.Pricing = config.PricingMap{"made-up-model": {
		InputPerMTok:                10,
		CacheCreationInputPerMTok:   12.5,
		CacheCreation1hInputPerMTok: 20,
		OutputPerMTok:               50,
	}}
	if got := PriceFor("made-up-model", cfg).CacheCreation1hInputPerMTok; got != 20 {
		t.Errorf("CacheCreation1hInputPerMTok = %v, want 20 from the cfg override", got)
	}
}

// The digest sidecar is the only channel by which a wrapped MCP call's
// spend reaches the paying session (#717/#771). Its TTL split has to
// survive the hop, or a 1h-configured daemon underbills every digest.
func TestDigestSavingsRecord_SubagentTurnCarriesTheOneHourShare(t *testing.T) {
	t.Parallel()
	rec := DigestSavingsRecord{
		SubagentInputTokens:                1_000_000,
		SubagentCacheCreationInputTokens:   1_000_000,
		SubagentCacheCreation1hInputTokens: 1_000_000,
	}
	if got := rec.SubagentTurn().CacheCreation1hInputTokens; got != 1_000_000 {
		t.Fatalf("SubagentTurn 1h = %d, want 1000000", got)
	}
	if got := ttlPricing.CostUSDForTurn(rec.SubagentTurn()); !nearUSD(got, 20) {
		t.Errorf("digest turn = %v, want the 1h rate 20 (the 5m rate would read 12.5)", got)
	}
}

// AppendUsage is the tracker's single definition of turn cost. The
// ledger it feeds is what maybeEnforceCostCeiling reads, so a 1h turn
// billed at the 5m rate lets a runaway session run 60% further on its
// write spend before the ceiling trips.
func TestAppendUsage_LedgerPricesTheOneHourShare(t *testing.T) {
	t.Parallel()
	tr := NewTracker()
	tr.AppendUsage("m", TurnUsage{
		InputTokens:                1_000_000,
		CacheCreationInputTokens:   1_000_000,
		CacheCreation1hInputTokens: 1_000_000,
	}, ttlPricing)
	if got := tr.Totals().CostUSD; !nearUSD(got, 20) {
		t.Errorf("session cost = %v, want 20", got)
	}
}
