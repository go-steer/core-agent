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
	"sync/atomic"
	"time"

	"github.com/go-steer/core-agent/v2/internal/pricing"
	"github.com/go-steer/core-agent/v2/pkg/config"
)

// Pricing is the per-million-token rate for one model. Fields are USD
// per million tokens (the same unit upstream providers publish public
// list rates in). CachedInputPerMTok is the reduced rate applied to
// prompt-cache-hit input tokens — Gemini charges 25% of the base input
// rate for both implicit and explicit caches. A zero Pricing carries
// no useful pricing — callers should distinguish "rate unknown" from
// "free" (e.g. echo models). See pricing.Rates / pricing.Catalog for
// the layered resolution behind PriceFor.
type Pricing struct {
	InputPerMTok       float64
	CachedInputPerMTok float64
	OutputPerMTok      float64
	// UpdatedAt is when the rate was last verified against its
	// source. Threads through from internal/pricing.Rates so /pricing
	// can surface staleness. Zero when unknown.
	UpdatedAt time.Time
}

// IsZero reports whether the rates carry no useful pricing.
// CachedInputPerMTok isn't part of the check: a row that carries only
// a cache rate but no base input/output rates is still "unpriced".
func (p Pricing) IsZero() bool { return p.InputPerMTok == 0 && p.OutputPerMTok == 0 }

// globalCatalog is the package-level pricing catalog consulted by
// PriceFor. main.go installs this once at startup via SetCatalog
// after assembling the file-based layers (.agents/pricing.json +
// ~/.core-agent/pricing.json + builtin). Tests + library consumers
// that don't call SetCatalog get a builtin-only catalog the first
// time PriceFor is called.
//
// Stored as atomic.Pointer so PR B's daily refresh can swap the
// catalog atomically without locking every per-turn cost lookup.
var globalCatalog atomic.Pointer[pricing.Catalog]

// SetCatalog installs the catalog PriceFor consults. Safe to call
// from any goroutine; lookups in flight see either the old or new
// catalog atomically, never a torn read.
func SetCatalog(c *pricing.Catalog) { globalCatalog.Store(c) }

// KnownModelsCount returns the total number of models across every
// layer of the installed pricing catalog (cfg override + project file
// + user manual + user external + builtin). Returns 0 when no catalog
// is installed. Used by the attach /pricing endpoint's snapshot so
// operators can see how many models the daemon knows about at a
// glance — the previous default of hard-coded 0 was actively
// misleading during the v2.7.0-dev.3 demo drive.
func KnownModelsCount() int {
	c := globalCatalog.Load()
	if c == nil {
		return 0
	}
	counts := c.Counts()
	return counts.CfgOverride + counts.ProjectFile + counts.UserManual +
		counts.UserExternal + counts.Builtin
}

// PriceForWithSource is PriceFor + the catalog layer name that served
// the rate (pricing.SourceCfgOverride / SourceProjectFile /
// SourceUserManual / SourceUserExternal / SourceBuiltin). Empty source
// when no rate was found. Used by /pricing so operators can spot when
// a rate came from a stale builtin instead of the freshly-refreshed
// LiteLLM external catalog they were expecting.
//
// The cfg override path (used only when no globalCatalog is installed)
// reports source SourceCfgOverride when the model resolves through it.
func PriceForWithSource(modelID string, cfg *config.Config) (Pricing, string) {
	if c := globalCatalog.Load(); c != nil {
		r, src, _ := c.LookupWithSource(modelID)
		return ratesToPricing(r), src
	}
	c, _ := pricing.NewCatalog(pricing.Options{
		CfgOverride: cfgToOverride(cfg),
	})
	r, src, _ := c.LookupWithSource(modelID)
	return ratesToPricing(r), src
}

// PriceFor returns the Pricing for modelID. Resolution chain (first
// exact match wins; longest-prefix fallback at the end):
//
//  1. cfg.Model.Pricing[modelID]                — operator override
//  2. .agents/pricing.json models[modelID]      — project file
//  3. ~/.core-agent/pricing.json                — user file (manual + external)
//  4. compiled-in builtin                       — fallback
//  5. longest-prefix match across (1)..(4)      — suffix variants
//  6. Pricing{}                                  — rate unknown
//
// cfg is consulted via the catalog (if installed via SetCatalog) or
// via an on-the-fly lookup when no catalog is installed. The
// no-catalog path covers tests + library use that doesn't go
// through cmd/core-agent's startup.
func PriceFor(modelID string, cfg *config.Config) Pricing {
	if c := globalCatalog.Load(); c != nil {
		r, _ := c.Lookup(modelID)
		return ratesToPricing(r)
	}
	// Catalog not installed (test / library). Build a one-shot
	// catalog from cfg + builtin so the answer is consistent with
	// what SetCatalog'd consumers would get.
	c, _ := pricing.NewCatalog(pricing.Options{
		CfgOverride: cfgToOverride(cfg),
	})
	r, _ := c.Lookup(modelID)
	return ratesToPricing(r)
}

// ratesToPricing projects internal/pricing.Rates into the public
// Pricing shape. Split out so PriceFor's two code paths stay in
// lockstep as new rate fields land.
func ratesToPricing(r pricing.Rates) Pricing {
	return Pricing{
		InputPerMTok:       r.InputPerMTok,
		CachedInputPerMTok: r.CachedInputPerMTok,
		OutputPerMTok:      r.OutputPerMTok,
		UpdatedAt:          r.UpdatedAt,
	}
}

// cfgToOverride extracts the cfg.Model.Pricing map into the
// internal/pricing wire shape. nil-safe.
func cfgToOverride(cfg *config.Config) map[string]pricing.ModelRates {
	if cfg == nil || len(cfg.Model.Pricing) == 0 {
		return nil
	}
	out := make(map[string]pricing.ModelRates, len(cfg.Model.Pricing))
	for k, v := range cfg.Model.Pricing {
		out[k] = pricing.ModelRates{
			InputPerMTok:       v.InputPerMTok,
			CachedInputPerMTok: v.CachedInputPerMTok,
			OutputPerMTok:      v.OutputPerMTok,
		}
	}
	return out
}

// CostUSD returns the dollar cost of (input, output) tokens at p.
// Treats every input token as uncached — see CostUSDWithCache for the
// cached-vs-uncached split.
func (p Pricing) CostUSD(inputTokens, outputTokens int) float64 {
	const million = 1_000_000.0
	return (float64(inputTokens)/million)*p.InputPerMTok +
		(float64(outputTokens)/million)*p.OutputPerMTok
}

// CostUSDWithCache returns the dollar cost with cache-hit tokens billed
// at CachedInputPerMTok. When CachedInputPerMTok is zero (rate unknown)
// cached tokens fall back to InputPerMTok so the estimate never
// silently drops to zero cost for cached input.
func (p Pricing) CostUSDWithCache(uncachedInputTokens, cachedInputTokens, outputTokens int) float64 {
	const million = 1_000_000.0
	cachedRate := p.CachedInputPerMTok
	if cachedRate == 0 {
		cachedRate = p.InputPerMTok
	}
	return (float64(uncachedInputTokens)/million)*p.InputPerMTok +
		(float64(cachedInputTokens)/million)*cachedRate +
		(float64(outputTokens)/million)*p.OutputPerMTok
}
