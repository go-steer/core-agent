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

package coretuiremote

import (
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/attach"
)

// TestRenderPricingInfo_ShowsEveryRateThatCharges pins that /pricing's
// rate card names the cache-WRITE premium (#263). An operator who sees
// an unexpected cost and runs /pricing to check has no other way to
// confirm that a configured `cache_creation_input_per_mtok` took
// effect — a rate that does charging but never appears on the card is
// the same unenforced-claim shape this milestone exists to remove.
func TestRenderPricingInfo_ShowsEveryRateThatCharges(t *testing.T) {
	t.Parallel()
	got := renderPricingInfo(attach.PricingInfo{
		Source:       "builtin",
		KnownModels:  12,
		CurrentModel: "claude-opus-5",
		Current: &attach.ModelPricing{
			InputUSDPerMTok:      5,
			OutputUSDPerMTok:     25,
			CachedUSDPerMTok:     0.5,
			CacheWriteUSDPerMTok: 6.25,
		},
	})
	for _, want := range []string{"$5.00 in", "$25.00 out", "$0.5000 cache-read", "$6.2500 cache-write"} {
		if !strings.Contains(got, want) {
			t.Errorf("rate card missing %q:\n%s", want, got)
		}
	}
}

// TestRenderPricingInfo_OmitsUnsetCacheRates pins that a Gemini-shaped
// card (no write rate published) doesn't grow a "$0.0000 cache-write"
// line, which would read as "writes are free" rather than "this
// provider doesn't bill writes per token".
func TestRenderPricingInfo_OmitsUnsetCacheRates(t *testing.T) {
	t.Parallel()
	got := renderPricingInfo(attach.PricingInfo{
		Source:       "builtin",
		CurrentModel: "gemini-3.5-flash",
		Current: &attach.ModelPricing{
			InputUSDPerMTok:  1.5,
			OutputUSDPerMTok: 9,
			CachedUSDPerMTok: 0.15,
		},
	})
	if strings.Contains(got, "cache-write") {
		t.Errorf("no write rate set, but the card shows one:\n%s", got)
	}
	if !strings.Contains(got, "cache-read") {
		t.Errorf("read rate should still render:\n%s", got)
	}
}
