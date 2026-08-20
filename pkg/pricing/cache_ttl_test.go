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

package pricing

import (
	"math"
	"strings"
	"testing"
)

// ttlRates is a deliberately round set: base 10, 5m writes at 1.25x,
// 1h writes at 2x — the same multipliers Anthropic publishes, so a
// wrong bucket shows up as a wrong multiple rather than a near miss.
var ttlRates = Rates{
	InputPerMTok:                10,
	CachedInputPerMTok:          1,
	CacheCreationInputPerMTok:   12.5,
	CacheCreation1hInputPerMTok: 20,
	OutputPerMTok:               50,
}

func closeEnoughUSD(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// The whole point of the field: a 1-hour write costs 60% more than a
// 5-minute one, and pricing it at the 5-minute rate is the 37.5%
// undercount #770 was filed for.
func TestCostUSDWithCacheTTLs_PricesTheOneHourShareAtItsOwnRate(t *testing.T) {
	t.Parallel()
	const million = 1_000_000

	all5m := ttlRates.CostUSDWithCacheTTLs(0, 0, million, 0, 0)
	if want := 12.5; !closeEnoughUSD(all5m, want) {
		t.Errorf("1M writes all at 5m = %v, want %v", all5m, want)
	}
	all1h := ttlRates.CostUSDWithCacheTTLs(0, 0, million, million, 0)
	if want := 20.0; !closeEnoughUSD(all1h, want) {
		t.Errorf("1M writes all at 1h = %v, want %v", all1h, want)
	}
	if all1h <= all5m {
		t.Fatalf("1h (%v) is not dearer than 5m (%v); the split bought nothing", all1h, all5m)
	}

	// Half and half lands exactly between — the 1h count is a subset
	// subtracted from the 5m remainder, not an extra charge added on
	// top. A double-count would read 12.5+10 = 22.5 here.
	half := ttlRates.CostUSDWithCacheTTLs(0, 0, million, million/2, 0)
	if want := 6.25 + 10.0; !closeEnoughUSD(half, want) {
		t.Errorf("500K at each TTL = %v, want %v (subset, not addend)", half, want)
	}
}

// A catalog row that carries a 5-minute write rate and no 1-hour one is
// far likelier to be missing a field than to bill 1h writes at base
// input. Falling back to base would UNDERCHARGE by more than not
// splitting at all.
func TestCostUSDWithCacheTTLs_UnknownOneHourRateFallsBackToFiveMinute(t *testing.T) {
	t.Parallel()
	r := ttlRates
	r.CacheCreation1hInputPerMTok = 0

	got := r.CostUSDWithCacheTTLs(0, 0, 1_000_000, 1_000_000, 0)
	if want := 12.5; !closeEnoughUSD(got, want) {
		t.Errorf("1h writes with no 1h rate = %v, want the 5m rate %v", got, want)
	}
	if closeEnoughUSD(got, 10) {
		t.Error("fell back to base input, which understates a cache write")
	}
}

// Both write rates unknown still degrades to base input rather than to
// free — the pre-#263 number, which is wrong but not zero.
func TestCostUSDWithCacheTTLs_BothWriteRatesUnknownFallBackToInput(t *testing.T) {
	t.Parallel()
	r := Rates{InputPerMTok: 10, OutputPerMTok: 50}
	got := r.CostUSDWithCacheTTLs(0, 0, 1_000_000, 1_000_000, 0)
	if want := 10.0; !closeEnoughUSD(got, want) {
		t.Errorf("got %v, want base input %v", got, want)
	}
}

// The 1h count arrives from a provider response and crosses a JSON
// sidecar, so a caller can hand over a subset larger than the set. It
// can only ever shrink to the total — never inflate the bill, never
// drive the 5m remainder negative.
func TestCostUSDWithCacheTTLs_ClampsTheSubsetToTheWriteBucket(t *testing.T) {
	t.Parallel()
	over := ttlRates.CostUSDWithCacheTTLs(0, 0, 1_000_000, 5_000_000, 0)
	if want := 20.0; !closeEnoughUSD(over, want) {
		t.Errorf("1h count above the total = %v, want the whole bucket at 1h %v", over, want)
	}
	under := ttlRates.CostUSDWithCacheTTLs(0, 0, 1_000_000, -7, 0)
	if want := 12.5; !closeEnoughUSD(under, want) {
		t.Errorf("negative 1h count = %v, want the whole bucket at 5m %v", under, want)
	}
}

// CostUSDWithCacheWrites is on the stability-promise surface and now
// delegates. Every existing caller must get the number it got before:
// zero 1h tokens, whole bucket at the 5-minute rate.
func TestCostUSDWithCacheWrites_UnchangedByTheTTLSplit(t *testing.T) {
	t.Parallel()
	const million = 1_000_000.0
	got := ttlRates.CostUSDWithCacheWrites(1_000_000, 2_000_000, 3_000_000, 4_000_000)
	want := (1_000_000/million)*ttlRates.InputPerMTok +
		(2_000_000/million)*ttlRates.CachedInputPerMTok +
		(3_000_000/million)*ttlRates.CacheCreationInputPerMTok +
		(4_000_000/million)*ttlRates.OutputPerMTok
	if !closeEnoughUSD(got, want) {
		t.Errorf("CostUSDWithCacheWrites = %v, want %v", got, want)
	}
}

// ModelRates and Rates must stay field-for-field convertible — the
// documented invariant behind (ModelRates).Rates(). A new field added
// to one and not the other, or added in a different position, is a
// compile error there and a wrong-value error here.
func TestModelRates_CarriesTheOneHourRateThroughTheConversion(t *testing.T) {
	t.Parallel()
	m := ModelRates{
		InputPerMTok:                1,
		CachedInputPerMTok:          2,
		CacheCreationInputPerMTok:   3,
		CacheCreation1hInputPerMTok: 4,
		OutputPerMTok:               5,
	}
	if got := m.Rates(); got.CacheCreation1hInputPerMTok != 4 || got.OutputPerMTok != 5 {
		t.Errorf("Rates() = %+v; the field order of the two structs has drifted", got)
	}
}

// The builtin table is the zero-config baseline, so the rate has to be
// IN it, not merely supported by the type. Anthropic publishes 1h
// writes at exactly 2x base input; that ratio is the check, since it
// survives a catalog regen that moves the absolute numbers.
func TestBuiltin_AnthropicRowsCarryTheOneHourWriteRate(t *testing.T) {
	t.Parallel()
	seen := 0
	for name, r := range builtin {
		if !strings.HasPrefix(name, "claude-") || r.CacheCreationInputPerMTok == 0 {
			continue
		}
		if r.CacheCreation1hInputPerMTok == 0 {
			t.Errorf("%s: has a 5m write rate but no 1h one", name)
			continue
		}
		seen++
		if ratio := r.CacheCreation1hInputPerMTok / r.InputPerMTok; math.Abs(ratio-2) > 0.01 {
			t.Errorf("%s: 1h write rate is %.2fx base input, want 2x", name, ratio)
		}
	}
	if seen == 0 {
		t.Fatal("no Anthropic row carries a 1h write rate; the regen did not pick the field up")
	}
}
