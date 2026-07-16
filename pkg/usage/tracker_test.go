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
	"math"
	"testing"

	"github.com/go-steer/core-agent/internal/pricing"
	"github.com/go-steer/core-agent/pkg/config"
)

func TestPricing_CostMath(t *testing.T) {
	t.Parallel()
	p := Pricing{InputPerMTok: 1.25, OutputPerMTok: 5.00}
	got := p.CostUSD(1_000_000, 500_000)
	want := 1.25 + 2.50
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("CostUSD = %f, want %f", got, want)
	}
}

func TestPriceFor_BuiltinExact(t *testing.T) {
	t.Parallel()
	p := PriceFor("gemini-3.5-flash", nil)
	if p.IsZero() {
		t.Fatalf("expected non-zero pricing for known model")
	}
}

func TestPriceFor_PrefixMatch(t *testing.T) {
	t.Parallel()
	p := PriceFor("gemini-3.5-flash-05-15", nil)
	if p.IsZero() {
		t.Fatalf("date-suffixed variant should still match the family")
	}
}

func TestPriceFor_ConfigOverrideWins(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultConfig()
	cfg.Model.Name = "gemini-3.5-flash"
	cfg.Model.Pricing = config.PricingMap{
		"gemini-3.5-flash": {InputPerMTok: 99, OutputPerMTok: 999},
	}
	p := PriceFor("gemini-3.5-flash", cfg)
	if p.InputPerMTok != 99 || p.OutputPerMTok != 999 {
		t.Errorf("config override ignored: got %+v", p)
	}
}

// TestPriceFor_ConfigOverrideAppliesToMultipleModels exercises the
// map-shaped Pricing — operators who route across several models
// (via /model switches mid-session) need rates for each. The pre-
// PR-A shape only matched cfg.Model.Name; this regression test
// catches an accidental revert to single-model matching.
func TestPriceFor_ConfigOverrideAppliesToMultipleModels(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultConfig()
	cfg.Model.Name = "primary-model"
	cfg.Model.Pricing = config.PricingMap{
		"primary-model":   {InputPerMTok: 1.0, OutputPerMTok: 2.0},
		"secondary-model": {InputPerMTok: 3.0, OutputPerMTok: 4.0},
	}
	primary := PriceFor("primary-model", cfg)
	if primary.InputPerMTok != 1.0 || primary.OutputPerMTok != 2.0 {
		t.Errorf("primary override missing: got %+v", primary)
	}
	// The point: the secondary entry should ALSO resolve even
	// though cfg.Model.Name doesn't match its key.
	secondary := PriceFor("secondary-model", cfg)
	if secondary.InputPerMTok != 3.0 || secondary.OutputPerMTok != 4.0 {
		t.Errorf("secondary override missing: got %+v", secondary)
	}
}

func TestPriceFor_UnknownModelIsZero(t *testing.T) {
	t.Parallel()
	if !PriceFor("openai-gpt-9000", nil).IsZero() {
		t.Errorf("expected zero pricing for unknown model")
	}
}

// TestKnownModelsCount pins the accessor behavior — the /pricing
// endpoint's snapshot uses this to report how many models the
// operator's catalog knows about. Zero when no catalog installed
// (fallback / library mode); non-zero when SetCatalog was called
// (typical daemon startup).
//
// NOT parallel: mutates package-global catalog state (defer-restores
// so the change doesn't leak to other tests).
func TestKnownModelsCount(t *testing.T) {
	prev := globalCatalog.Load()
	t.Cleanup(func() { globalCatalog.Store(prev) })

	// No catalog installed → 0.
	globalCatalog.Store(nil)
	if got := KnownModelsCount(); got != 0 {
		t.Errorf("KnownModelsCount with no catalog = %d, want 0", got)
	}

	// Builtin-only catalog (what NewCatalog produces with empty opts).
	c, err := pricing.NewCatalog(pricing.Options{})
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	globalCatalog.Store(c)
	if got := KnownModelsCount(); got == 0 {
		t.Errorf("KnownModelsCount with builtin catalog = 0, want > 0")
	}
}

func TestTracker_AppendAndTotals(t *testing.T) {
	t.Parallel()
	tr := NewTracker()
	p := Pricing{InputPerMTok: 1.0, OutputPerMTok: 2.0}

	tr.Append("m", 100_000, 50_000, p)
	tr.Append("m", 200_000, 100_000, p)

	tot := tr.Totals()
	if tot.Turns != 2 {
		t.Errorf("turns = %d, want 2", tot.Turns)
	}
	if tot.InputTokens != 300_000 || tot.OutputTokens != 150_000 {
		t.Errorf("token sums wrong: %+v", tot)
	}
	wantCost := (0.3 * 1.0) + (0.15 * 2.0)
	if math.Abs(tot.CostUSD-wantCost) > 1e-9 {
		t.Errorf("cost = %f, want %f", tot.CostUSD, wantCost)
	}
	last, ok := tr.Last()
	if !ok || last.InputTokens != 200_000 {
		t.Errorf("Last wrong: %+v ok=%v", last, ok)
	}
	if got := len(tr.All()); got != 2 {
		t.Errorf("All() len = %d", got)
	}
}

func TestTracker_LastEmpty(t *testing.T) {
	t.Parallel()
	tr := NewTracker()
	if _, ok := tr.Last(); ok {
		t.Errorf("expected !ok on empty tracker")
	}
}

func TestTracker_TotalsByModel(t *testing.T) {
	t.Parallel()
	tr := NewTracker()
	pro := Pricing{InputPerMTok: 3.0, OutputPerMTok: 12.0}
	flash := Pricing{InputPerMTok: 0.15, OutputPerMTok: 0.60}

	// 2 parent turns on pro + 3 subtask turns on flash.
	tr.Append("gemini-3.1-pro", 10_000, 500, pro)
	tr.Append("gemini-2.5-flash", 5_000, 200, flash)
	tr.Append("gemini-3.1-pro", 8_000, 300, pro)
	tr.Append("gemini-2.5-flash", 3_000, 100, flash)
	tr.Append("gemini-2.5-flash", 4_000, 150, flash)

	byModel := tr.TotalsByModel()
	if len(byModel) != 2 {
		t.Fatalf("TotalsByModel returned %d models, want 2", len(byModel))
	}
	pt, ok := byModel["gemini-3.1-pro"]
	if !ok {
		t.Fatalf("pro model missing from TotalsByModel")
	}
	if pt.Turns != 2 || pt.InputTokens != 18_000 || pt.OutputTokens != 800 {
		t.Errorf("pro totals = %+v, want Turns=2 In=18000 Out=800", pt)
	}
	ft, ok := byModel["gemini-2.5-flash"]
	if !ok {
		t.Fatalf("flash model missing from TotalsByModel")
	}
	if ft.Turns != 3 || ft.InputTokens != 12_000 || ft.OutputTokens != 450 {
		t.Errorf("flash totals = %+v, want Turns=3 In=12000 Out=450", ft)
	}
	// Grand total should match plain Totals().
	all := tr.Totals()
	if pt.Turns+ft.Turns != all.Turns {
		t.Errorf("per-model turns (%d+%d) != Totals().Turns (%d)", pt.Turns, ft.Turns, all.Turns)
	}
	if pt.InputTokens+ft.InputTokens != all.InputTokens {
		t.Errorf("per-model input (%d+%d) != Totals().InputTokens (%d)", pt.InputTokens, ft.InputTokens, all.InputTokens)
	}
}

func TestTracker_TotalsByModelEmpty(t *testing.T) {
	t.Parallel()
	tr := NewTracker()
	got := tr.TotalsByModel()
	if got == nil {
		t.Errorf("TotalsByModel should return non-nil empty map for empty tracker")
	}
	if len(got) != 0 {
		t.Errorf("TotalsByModel on empty tracker = %v, want empty", got)
	}
}

func TestPricing_CostUSDWithCache(t *testing.T) {
	t.Parallel()
	// Cached rate honored when set: 800k uncached at $1.25/M + 200k
	// cached at $0.3125/M + 100k output at $5/M = $1.5625 total.
	p := Pricing{InputPerMTok: 1.25, CachedInputPerMTok: 0.3125, OutputPerMTok: 5.00}
	got := p.CostUSDWithCache(800_000, 200_000, 100_000)
	want := 0.8*1.25 + 0.2*0.3125 + 0.1*5.00
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("CostUSDWithCache = %f, want %f", got, want)
	}

	// Fallback: no cached rate → cache hits bill at InputPerMTok
	// (honest "no discount known" signal, not silent freebie).
	p2 := Pricing{InputPerMTok: 1.0, OutputPerMTok: 2.0}
	got2 := p2.CostUSDWithCache(500_000, 500_000, 100_000)
	want2 := 0.5*1.0 + 0.5*1.0 + 0.1*2.0
	if math.Abs(got2-want2) > 1e-9 {
		t.Errorf("CostUSDWithCache (fallback) = %f, want %f", got2, want2)
	}
}

func TestTracker_AppendUsage_CachedFields(t *testing.T) {
	t.Parallel()
	tr := NewTracker()
	// gemini-3.1-pro rates: input $1.25, cached $0.3125, output $5.
	p := Pricing{InputPerMTok: 1.25, CachedInputPerMTok: 0.3125, OutputPerMTok: 5.00}

	// One warm turn with 200k of 1M input from cache + 50k output +
	// 10k thinking + 5k tool-use.
	tr.AppendUsage("gemini-3.1-pro", TurnUsage{
		InputTokens:       1_000_000,
		CachedInputTokens: 200_000,
		OutputTokens:      50_000,
		ThoughtsTokens:    10_000,
		ToolUseTokens:     5_000,
	}, p)
	// One cold turn: no cache hits.
	tr.AppendUsage("gemini-3.1-pro", TurnUsage{
		InputTokens:  500_000,
		OutputTokens: 20_000,
	}, p)

	tot := tr.Totals()
	if tot.Turns != 2 {
		t.Errorf("Turns = %d, want 2", tot.Turns)
	}
	if tot.InputTokens != 1_500_000 {
		t.Errorf("InputTokens = %d, want 1_500_000", tot.InputTokens)
	}
	if tot.CachedInputTokens != 200_000 {
		t.Errorf("CachedInputTokens = %d, want 200_000", tot.CachedInputTokens)
	}
	if tot.OutputTokens != 70_000 {
		t.Errorf("OutputTokens = %d, want 70_000", tot.OutputTokens)
	}
	if tot.ThoughtsTokens != 10_000 || tot.ToolUseTokens != 5_000 {
		t.Errorf("thoughts/tool-use rollup wrong: %+v", tot)
	}
	// Warm turn: 800k uncached * 1.25 + 200k cached * 0.3125 + 50k * 5 = 1.3125
	// Cold turn: 500k * 1.25 + 20k * 5 = 0.725
	wantCost := (0.8*1.25 + 0.2*0.3125 + 0.05*5.00) + (0.5*1.25 + 0.02*5.00)
	if math.Abs(tot.CostUSD-wantCost) > 1e-9 {
		t.Errorf("CostUSD = %f, want %f", tot.CostUSD, wantCost)
	}
}

func TestTracker_AppendUsage_ClampsCachedOverInput(t *testing.T) {
	t.Parallel()
	// Provider quirk: the issue observed cached > prompt occasionally.
	// AppendUsage must clamp so downstream uncached math stays >= 0.
	tr := NewTracker()
	p := Pricing{InputPerMTok: 1.0, CachedInputPerMTok: 0.25, OutputPerMTok: 2.0}

	tr.AppendUsage("m", TurnUsage{
		InputTokens:       100_000,
		CachedInputTokens: 250_000, // over-reported
		OutputTokens:      1_000,
	}, p)

	got, ok := tr.Last()
	if !ok {
		t.Fatal("expected one recorded turn")
	}
	if got.CachedInputTokens != 100_000 {
		t.Errorf("CachedInputTokens = %d, want clamped to 100_000", got.CachedInputTokens)
	}
	// Uncached = 0, all input at cached rate: 100k * 0.25 + 1k * 2 = 0.027
	wantCost := 0.1*0.25 + 0.001*2.0
	if math.Abs(got.CostUSD-wantCost) > 1e-9 {
		t.Errorf("clamped-turn CostUSD = %f, want %f", got.CostUSD, wantCost)
	}
}

func TestTracker_AppendBackCompat(t *testing.T) {
	t.Parallel()
	// The old Append(model, in, out, p) signature is now a shim over
	// AppendUsage. Verify legacy callers still land a well-formed
	// Turn with zero cached/thoughts (i.e. cost matches CostUSD, not
	// CostUSDWithCache-with-savings).
	tr := NewTracker()
	p := Pricing{InputPerMTok: 1.0, CachedInputPerMTok: 0.25, OutputPerMTok: 2.0}
	tr.Append("m", 1_000_000, 100_000, p)
	got, ok := tr.Last()
	if !ok {
		t.Fatal("expected one recorded turn")
	}
	if got.CachedInputTokens != 0 || got.ThoughtsTokens != 0 {
		t.Errorf("legacy Append should zero cached/thoughts: %+v", got)
	}
	wantCost := 1.0*1.0 + 0.1*2.0 // 1.20 — no cache discount
	if math.Abs(got.CostUSD-wantCost) > 1e-9 {
		t.Errorf("legacy Append cost = %f, want %f", got.CostUSD, wantCost)
	}
}

func TestTurnUsageFromGenaiMetadata_NilSafe(t *testing.T) {
	t.Parallel()
	got := TurnUsageFromGenaiMetadata(nil)
	if got != (TurnUsage{}) {
		t.Errorf("nil metadata should yield zero TurnUsage, got %+v", got)
	}
}
