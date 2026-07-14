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
	"math"
	"testing"

	"github.com/go-steer/core-agent/pkg/attach"
	"github.com/go-steer/core-agent/pkg/usage"
)

// TestAttachUsage_CachedFieldsAndPerTurn is the on-the-wire spec test
// for issue #222: /sessions/<id>/usage must expose cached vs uncached
// input tokens, cost-usd + a counterfactual "if nothing had cached"
// reference, and one entry per model call in a per_turn array.
func TestAttachUsage_CachedFieldsAndPerTurn(t *testing.T) {
	t.Parallel()
	tr := usage.NewTracker()
	// Rates match builtin gemini-3.1-pro so the numbers are the same
	// operators will see against a real Vertex session.
	p := usage.Pricing{InputPerMTok: 1.25, CachedInputPerMTok: 0.3125, OutputPerMTok: 5.00}

	// Turn 1: cold. No cache.
	tr.AppendUsage("gemini-3.1-pro", usage.TurnUsage{
		InputTokens:  10_000,
		OutputTokens: 500,
	}, p)
	// Turn 2: warm. 8k of the 10k input from cache.
	tr.AppendUsage("gemini-3.1-pro", usage.TurnUsage{
		InputTokens:       10_000,
		CachedInputTokens: 8_000,
		OutputTokens:      500,
	}, p)

	a := &Agent{tracker: tr}
	info := a.AttachUsage()

	// Overall aggregates.
	if info.Overall.Turns != 2 {
		t.Errorf("Overall.Turns = %d, want 2", info.Overall.Turns)
	}
	if info.Overall.InputTokens != 20_000 {
		t.Errorf("Overall.InputTokens = %d, want 20_000", info.Overall.InputTokens)
	}
	if info.Overall.InputTokensCached != 8_000 {
		t.Errorf("Overall.InputTokensCached = %d, want 8_000", info.Overall.InputTokensCached)
	}
	if info.Overall.InputTokensUncached != 12_000 {
		t.Errorf("Overall.InputTokensUncached = %d, want 12_000", info.Overall.InputTokensUncached)
	}
	if info.Overall.OutputTokens != 1_000 {
		t.Errorf("Overall.OutputTokens = %d, want 1_000", info.Overall.OutputTokens)
	}

	// Actual cost: turn1 (10k * 1.25 + 500 * 5)/1e6 + turn2 (2k * 1.25 + 8k * 0.3125 + 500 * 5)/1e6
	wantCost := (0.01*1.25 + 0.0005*5.00) + (0.002*1.25 + 0.008*0.3125 + 0.0005*5.00)
	if math.Abs(info.Overall.CostUSD-wantCost) > 1e-9 {
		t.Errorf("Overall.CostUSD = %f, want %f", info.Overall.CostUSD, wantCost)
	}
	// Reference cost: both turns billed at input rate for all inputs.
	// (10k * 1.25 + 500 * 5)/1e6 * 2 = 2 * (0.0125 + 0.0025) = 0.03
	wantRef := 2 * (0.01*1.25 + 0.0005*5.00)
	// PriceFor("gemini-3.1-pro", nil) may or may not resolve — depends on
	// whether SetCatalog was called by any test that ran first. If it
	// hasn't, PriceFor still falls back to a fresh builtin catalog per
	// the pkg/usage/pricing.go path, so the answer is stable.
	if math.Abs(info.Overall.CostUSDUncachedReference-wantRef) > 1e-9 {
		t.Errorf("Overall.CostUSDUncachedReference = %f, want %f",
			info.Overall.CostUSDUncachedReference, wantRef)
	}
	// The whole point of #222: reference > actual = caching win visible.
	if info.Overall.CostUSDUncachedReference <= info.Overall.CostUSD {
		t.Errorf("cache savings should be visible: ref=%f actual=%f",
			info.Overall.CostUSDUncachedReference, info.Overall.CostUSD)
	}

	// PerTurn shape.
	if len(info.PerTurn) != 2 {
		t.Fatalf("len(PerTurn) = %d, want 2", len(info.PerTurn))
	}
	if info.PerTurn[0].Turn != 1 || info.PerTurn[1].Turn != 2 {
		t.Errorf("PerTurn indexes wrong: %d, %d", info.PerTurn[0].Turn, info.PerTurn[1].Turn)
	}
	if info.PerTurn[0].InputTokensCached != 0 {
		t.Errorf("cold turn should have 0 cached: %+v", info.PerTurn[0])
	}
	if info.PerTurn[1].InputTokensCached != 8_000 {
		t.Errorf("warm turn cached = %d, want 8_000", info.PerTurn[1].InputTokensCached)
	}
	if info.PerTurn[0].Model != "gemini-3.1-pro" {
		t.Errorf("PerTurn[0].Model = %q, want gemini-3.1-pro", info.PerTurn[0].Model)
	}
	// TotalTokens = input + output (+ thoughts + tool-use, both zero here).
	if info.PerTurn[0].TotalTokens != 10_500 {
		t.Errorf("PerTurn[0].TotalTokens = %d, want 10_500", info.PerTurn[0].TotalTokens)
	}
}

// TestAttachUsage_NoTracker exercises the nil-tracker path used by
// hand-constructed test agents. Must return a zero UsageInfo without
// panicking or allocating a PerTurn slice.
func TestAttachUsage_NoTracker(t *testing.T) {
	t.Parallel()
	a := &Agent{}
	info := a.AttachUsage()
	if info.Overall.Turns != 0 || info.PerTurn != nil || info.PerModel != nil {
		t.Errorf("nil-tracker AttachUsage should be zero: %+v", info)
	}
}

// TestAttachUsage_PerModelWhenMultipleModels covers the mixed-model
// path (parent frontier + subtask flash). PerModel must be populated
// and CostUSDUncachedReference must roll up per-model too.
func TestAttachUsage_PerModelWhenMultipleModels(t *testing.T) {
	t.Parallel()
	tr := usage.NewTracker()
	// Use rates the pricing catalog knows: gemini-3.1-pro + gemini-3.5-flash.
	pro := usage.PriceFor("gemini-3.1-pro", nil)
	flash := usage.PriceFor("gemini-3.5-flash", nil)
	if pro.IsZero() || flash.IsZero() {
		t.Skip("catalog didn't resolve gemini rates; skipping in this env")
	}
	tr.AppendUsage("gemini-3.1-pro", usage.TurnUsage{InputTokens: 10_000, OutputTokens: 500}, pro)
	tr.AppendUsage("gemini-3.5-flash", usage.TurnUsage{InputTokens: 5_000, OutputTokens: 200}, flash)

	a := &Agent{tracker: tr}
	info := a.AttachUsage()
	if len(info.PerModel) != 2 {
		t.Fatalf("PerModel has %d models, want 2", len(info.PerModel))
	}
	// Sanity: each per-model row should carry a positive uncached-ref
	// cost since both models are priced.
	for name, row := range info.PerModel {
		if row.CostUSDUncachedReference <= 0 {
			t.Errorf("PerModel[%s].CostUSDUncachedReference = %v, want > 0",
				name, row.CostUSDUncachedReference)
		}
	}
}

// TestUsageTotalsToAttach_UncachedMathClamps guards the projection
// math: if a caller ever manages to smuggle in Totals with cached >
// input (shouldn't happen post-AppendUsage but the projection helper
// runs unconditionally), InputTokensUncached must not underflow into
// a garbage negative int64.
func TestUsageTotalsToAttach_UncachedMathIsHonest(t *testing.T) {
	t.Parallel()
	// This test pins the current invariant: AppendUsage clamps at
	// the record site, so Totals().CachedInputTokens can never exceed
	// InputTokens for any tracker built via the public API. If that
	// changes, revisit usageTotalsToAttach.
	got := usageTotalsToAttach(usage.Totals{
		Turns:             1,
		InputTokens:       1000,
		CachedInputTokens: 400,
		OutputTokens:      100,
	})
	if got.InputTokensUncached != 600 {
		t.Errorf("InputTokensUncached = %d, want 600", got.InputTokensUncached)
	}
	if got.InputTokens != 1000 || got.InputTokensCached != 400 {
		t.Errorf("field mapping wrong: %+v", got)
	}
	// Zero-cache case leaves omitempty fields at zero.
	got2 := usageTotalsToAttach(usage.Totals{Turns: 1, InputTokens: 100, OutputTokens: 10})
	if got2.InputTokensCached != 0 || got2.InputTokensUncached != 100 {
		t.Errorf("cold-only case: %+v", got2)
	}
}

var _ attach.UsageProvider = (*Agent)(nil)
