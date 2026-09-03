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
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/pricing"
)

// These tests install a process-global catalog, so none of them are
// parallel and each restores the no-catalog state the rest of the
// package assumes.

// catalogWith builds a one-model catalog through the override layer —
// the same layer SetCatalog folds cfg.Model.Pricing into, so this is
// the shape a real /pricing set produces.
func catalogWith(t *testing.T, model string, in, out float64) *pricing.Catalog {
	t.Helper()
	c, err := pricing.NewCatalog(pricing.Options{
		CfgOverride: map[string]pricing.ModelRates{
			model: {InputPerMTok: in, OutputPerMTok: out},
		},
	})
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	return c
}

// The whole point of the helper: once a catalog is installed it is the
// definition of the rate, and a value captured before the swap loses.
func TestPriceForRefreshed_InstalledCatalogBeatsTheCapturedRate(t *testing.T) {
	SetCatalog(nil)
	t.Cleanup(func() { SetCatalog(nil) })

	stale := Pricing{InputPerMTok: 1, OutputPerMTok: 2}
	SetCatalog(catalogWith(t, "m", 10, 40))

	got := PriceForRefreshed("m", stale)
	if got.InputPerMTok != 10 || got.OutputPerMTok != 40 {
		t.Errorf("PriceForRefreshed = in %v / out %v, want the installed catalog's 10 / 40 — a captured rate must not outlive a /pricing refresh",
			got.InputPerMTok, got.OutputPerMTok)
	}
}

// A second swap has to win too. The failure this guards against is a
// helper that reads the catalog once and memoizes, which would look
// correct in the single-swap test above.
func TestPriceForRefreshed_TracksEverySwap(t *testing.T) {
	SetCatalog(nil)
	t.Cleanup(func() { SetCatalog(nil) })

	stale := Pricing{InputPerMTok: 1}
	SetCatalog(catalogWith(t, "m", 10, 40))
	if got := PriceForRefreshed("m", stale).InputPerMTok; got != 10 {
		t.Fatalf("after first swap: %v, want 10", got)
	}
	SetCatalog(catalogWith(t, "m", 99, 400))
	if got := PriceForRefreshed("m", stale).InputPerMTok; got != 99 {
		t.Errorf("after second swap: %v, want 99", got)
	}
}

// Library and test callers never call SetCatalog. Their explicit rate
// has to survive, or embedding core-agent would silently reprice every
// turn against a builtin table the host never opted into.
func TestPriceForRefreshed_NoCatalogKeepsTheCallersRate(t *testing.T) {
	SetCatalog(nil)
	t.Cleanup(func() { SetCatalog(nil) })

	fallback := Pricing{InputPerMTok: 7, OutputPerMTok: 9}
	got := PriceForRefreshed("gemini-3.7-flash", fallback)
	if got != fallback {
		t.Errorf("PriceForRefreshed with no catalog = %+v, want the caller's %+v", got, fallback)
	}
}

// A model the installed catalog has never heard of resolves Unpriced
// rather than falling back to the captured rate. That is deliberate and
// worth pinning: the catalog is authoritative once installed, and
// silently substituting a stale rate for an unknown model is how a
// wrong number gets into the ledger looking confident. Unpriced is the
// signal downstream aggregation already understands.
func TestPriceForRefreshed_UnknownModelUnderACatalogIsUnpriced(t *testing.T) {
	SetCatalog(nil)
	t.Cleanup(func() { SetCatalog(nil) })

	SetCatalog(catalogWith(t, "m", 10, 40))
	got := PriceForRefreshed("no-such-model-anywhere", Pricing{InputPerMTok: 7})
	if !got.Unpriced {
		t.Errorf("Unpriced = false for a model absent from the installed catalog (got %+v)", got)
	}
	if got.InputPerMTok == 7 {
		t.Errorf("InputPerMTok fell back to the captured 7; the installed catalog is authoritative")
	}
}
