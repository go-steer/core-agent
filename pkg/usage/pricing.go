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

	"github.com/go-steer/core-models/profile"

	"github.com/go-steer/core-agent/v3/pkg/config"
	"github.com/go-steer/core-agent/v3/pkg/pricing"
)

// Pricing is the per-million-token rate for one model. Fields are USD
// per million tokens (the same unit upstream providers publish public
// list rates in). CachedInputPerMTok is the reduced rate applied to
// prompt-cache-hit input tokens — Gemini charges 25% of the base input
// rate for both implicit and explicit caches.
// CacheCreationInputPerMTok is the PREMIUM rate applied to input tokens
// that write a cache entry (Anthropic's cache_creation_input_tokens,
// 1.25x base input); zero for providers that don't bill writes
// separately. Like pricing.Rates.CacheCreationInputPerMTok it holds the
// 5-minute-TTL rate; CacheCreation1hInputPerMTok holds the 1-hour one
// (2x base input), and falls back to the 5-minute rate when zero.
// A zero Pricing carries
// no useful pricing — callers should distinguish "rate unknown" from
// "free" (e.g. echo models). See pricing.Rates / pricing.Catalog for
// the layered resolution behind PriceFor.
type Pricing struct {
	InputPerMTok                float64
	CachedInputPerMTok          float64
	CacheCreationInputPerMTok   float64
	CacheCreation1hInputPerMTok float64
	OutputPerMTok               float64
	// UpdatedAt is when the rate was last verified against its
	// source. Threads through from pkg/pricing.Rates so /pricing
	// can surface staleness. Zero when unknown.
	UpdatedAt time.Time
	// Unpriced is true when no catalog layer had a rate for the model
	// (pricing.Catalog.Lookup returned found=false). It disambiguates
	// "rate unknown" (Unpriced=true, cost should render "$—") from a
	// genuinely free model (Unpriced=false, all rates zero). Without
	// this flag a $0 cost is indistinguishable from an unknown-price
	// model in Totals, per-model breakdowns, and the
	// core_agent.session.cost_usd metric — see #368.
	Unpriced bool
	// LongContext, when set, is the model's above-threshold price
	// (Gemini's 200K, Claude Sonnet 4.5's 200K on Vertex AI, OpenAI's
	// 272K). A turn whose whole prompt is over the threshold bills every
	// token at it, output included — see pricing.Rates.At. Nil for a
	// model with one price at every size.
	LongContext *pricing.LongContext
	// Backend is the backend the rate was looked up for, as BackendFor
	// or PriceOn's caller named it: "" when none was known, which
	// prices from the bare rows. Tracker copies it onto each Turn, so a
	// later re-pricing of the turn (the /usage savings reference) asks
	// the same backend the ledger did, even for a subagent whose
	// provider isn't its parent's.
	Backend string
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

// globalBackend is the backend PriceFor prices against when its caller
// can't name one: a nil cfg, or a cfg whose model.provider is empty
// (auto-detected). cmd/core-agent installs it with SetBackend once the
// provider is resolved. Empty means "unknown", which prices from the
// bare rows exactly as before backends existed.
var globalBackend atomic.Pointer[string]

// SetBackend installs the backend PriceFor uses when its caller names
// none — a core-models backend name ("gemini", "vertex", "anthropic",
// "anthropic-vertex", "openai", "xai", "vertex-maas", or a profile's
// own), as BackendFor returns it.
func SetBackend(backend string) { globalBackend.Store(&backend) }

// BackendFor returns the backend that bills cfg's model: a provider
// profile's backend name (profile.Profile.BackendName, extends applied),
// or the core provider's own name, which is already the core-models
// backend name. An empty model.provider (auto-detection) falls back to
// the backend installed with SetBackend, and a nil cfg likewise.
//
// The backend matters for rows the bare table can't express: OpenAI's
// and xAI's models (priced only as "openai/…" and "xai/…"), Vertex AI
// partner models, and Claude Sonnet 4.5's long-context rate on Vertex.
// For every other model the qualified and bare rows agree.
func BackendFor(cfg *config.Config) string {
	if cfg == nil || cfg.Model.Provider == "" {
		if b := globalBackend.Load(); b != nil {
			return *b
		}
		return ""
	}
	name := cfg.Model.Provider
	if p, ok := cfg.Profile(name); ok {
		if x, err := profile.Expand(p); err == nil {
			p = x
		}
		return p.BackendName()
	}
	return name
}

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
		counts.UserExternal + counts.Builtin + counts.Declared
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
	backend := BackendFor(cfg)
	if c := globalCatalog.Load(); c != nil {
		r, src, found := c.LookupForWithSource(backend, modelID)
		return ratesToPricing(r, found, backend), src
	}
	c, _ := pricing.NewCatalog(pricing.Options{
		CfgOverride: cfgToOverride(cfg),
		Declared:    DeclaredRates(cfg),
		Profiles:    cfgProviders(cfg),
	})
	r, src, found := c.LookupForWithSource(backend, modelID)
	return ratesToPricing(r, found, backend), src
}

// PriceFor returns the Pricing for modelID as billed by cfg's backend
// (BackendFor). Resolution chain (first exact match wins; longest-prefix
// fallback at the end), each layer consulting "<backend>/<model>" before
// the bare id — see pricing.Catalog.LookupFor:
//
//  1. cfg.Model.Pricing[modelID]                — operator override
//  2. .agents/pricing.json models[modelID]      — project file
//  3. ~/.core-agent/pricing.json                — user file (manual + external)
//  4. compiled-in builtin                       — core-models' shared table
//  5. provider profiles' declared rates         — models no catalog covers
//  6. longest-prefix match, layer by layer      — suffix variants
//  7. Pricing{}                                 — rate unknown
//
// cfg is consulted via the catalog (if installed via SetCatalog) or
// via an on-the-fly lookup when no catalog is installed. The
// no-catalog path covers tests + library use that doesn't go
// through cmd/core-agent's startup.
func PriceFor(modelID string, cfg *config.Config) Pricing {
	backend := BackendFor(cfg)
	if c := globalCatalog.Load(); c != nil {
		r, found := c.LookupFor(backend, modelID)
		return ratesToPricing(r, found, backend)
	}
	// Catalog not installed (test / library). Build a one-shot
	// catalog from cfg + builtin so the answer is consistent with
	// what SetCatalog'd consumers would get.
	c, _ := pricing.NewCatalog(pricing.Options{
		CfgOverride: cfgToOverride(cfg),
		Declared:    DeclaredRates(cfg),
		Profiles:    cfgProviders(cfg),
	})
	r, found := c.LookupFor(backend, modelID)
	return ratesToPricing(r, found, backend)
}

// PriceOn returns the Pricing for modelID as billed by backend, for a
// cost site that knows which provider served the call but has no cfg
// naming it: a subagent declared on a provider other than its
// parent's, whose spend PriceFor(model, nil) would otherwise price at
// the process backend (SetBackend). An empty backend is exactly
// PriceFor(modelID, nil).
//
// With no catalog installed (tests, library use) there are no
// declared rates to consult, so it prices from the builtin table.
func PriceOn(backend, modelID string) Pricing {
	if backend == "" {
		return PriceFor(modelID, nil)
	}
	c := globalCatalog.Load()
	if c == nil {
		c, _ = pricing.NewCatalog(pricing.Options{})
	}
	r, found := c.LookupFor(backend, modelID)
	return ratesToPricing(r, found, backend)
}

// PriceForRefreshed re-resolves modelID against the process-wide
// catalog, falling back to the caller's captured rate when no catalog
// is installed.
//
// It exists for billing sites that hold a Pricing *value* resolved
// once and then bill many turns against it (#930). `POST
// /pricing/refresh` and `/pricing/set` rebuild the catalog and install
// it with SetCatalog, which swaps what the value was derived FROM and
// cannot reach the copy — so `GET /pricing` reported the new rate
// while the ledger, WriteSummary, and the --max-session-cost-usd
// ceiling all kept charging the old one. Reporting right and billing
// wrong is the worst of the two: an operator who refreshes because
// they believe the rates are stale gets told they are now correct.
//
// The catalog wins over the fallback whenever one is installed, and
// that is the point rather than a caveat: cmd/core-agent installs one
// at boot, and from then on the catalog IS the definition of the
// rate — including the operator's cfg.Model.Pricing override, which
// SetCatalog folds in as the CfgOverride layer (see PriceFor, which
// likewise ignores its cfg argument once a catalog is present).
//
// The fallback covers library and test use that never calls
// SetCatalog. Those callers keep the value they passed in, so an
// embedder supplying an explicit rate is not quietly overruled by a
// builtin table they never opted into. It is a no-catalog fallback
// and not a lookup-miss one: under an installed catalog an unknown
// model resolves unpriced rather than to the captured value, matching
// what GET /pricing has always reported for it (cmd/core-agent's
// single-session provider re-resolves with no fallback at all) and
// keeping the card and the ledger on one answer.
func PriceForRefreshed(modelID string, fallback Pricing) Pricing {
	if globalCatalog.Load() == nil {
		return fallback
	}
	return PriceFor(modelID, nil)
}

// ratesToPricing projects pkg/pricing.Rates into the public
// Pricing shape. Split out so PriceFor's two code paths stay in
// lockstep as new rate fields land. found is the catalog lookup's
// found flag; !found marks the result Unpriced so downstream cost
// aggregation can tell "rate unknown" apart from "genuinely free".
func ratesToPricing(r pricing.Rates, found bool, backend string) Pricing {
	return Pricing{
		InputPerMTok:                r.InputPerMTok,
		CachedInputPerMTok:          r.CachedInputPerMTok,
		CacheCreationInputPerMTok:   r.CacheCreationInputPerMTok,
		CacheCreation1hInputPerMTok: r.CacheCreation1hInputPerMTok,
		OutputPerMTok:               r.OutputPerMTok,
		UpdatedAt:                   r.UpdatedAt,
		Unpriced:                    !found,
		LongContext:                 r.LongContext,
		Backend:                     backend,
	}
}

// rates is the inverse of ratesToPricing, for the cost arithmetic,
// which pricing.Rates defines once for both products.
func (p Pricing) rates() pricing.Rates {
	return pricing.Rates{
		InputPerMTok:                p.InputPerMTok,
		CachedInputPerMTok:          p.CachedInputPerMTok,
		CacheCreationInputPerMTok:   p.CacheCreationInputPerMTok,
		CacheCreation1hInputPerMTok: p.CacheCreation1hInputPerMTok,
		OutputPerMTok:               p.OutputPerMTok,
		LongContext:                 p.LongContext,
	}
}

// cfgToOverride extracts the cfg.Model.Pricing map into the
// pkg/pricing wire shape. nil-safe.
func cfgToOverride(cfg *config.Config) map[string]pricing.ModelRates {
	if cfg == nil {
		return nil
	}
	return pricingMapToRates(cfg.Model.Pricing)
}

func cfgProviders(cfg *config.Config) []profile.Profile {
	if cfg == nil {
		return nil
	}
	return cfg.Providers
}

// DeclaredRates is the provider profiles' declared rates, for the
// catalog's lowest layer (pricing.Options.Declared).
//
// Each rate is keyed twice. "<backend>/<model>" (pricing.DeclaredFromProfiles)
// is the one LookupFor reaches when the caller knows the backend, so two
// profiles that declare different prices for one model id each get
// their own. The bare id (config.Config.ProfileRates, first profile
// wins) is kept for lookups that can't name a backend — a subagent
// priced with a nil cfg on a daemon whose backend is another profile's —
// which priced from it before backends existed and would otherwise
// turn unpriced, and be refused outright under a cost ceiling.
func DeclaredRates(cfg *config.Config) map[string]pricing.ModelRates {
	if cfg == nil {
		return nil
	}
	out := pricingMapToRates(cfg.ProfileRates())
	expanded := make([]profile.Profile, 0, len(cfg.Providers))
	for _, p := range cfg.Providers {
		if x, err := profile.Expand(p); err == nil {
			p = x
		}
		expanded = append(expanded, p)
	}
	for k, v := range pricing.DeclaredFromProfiles(expanded) {
		if out == nil {
			out = map[string]pricing.ModelRates{}
		}
		out[k] = v
	}
	return out
}

func pricingMapToRates(m config.PricingMap) map[string]pricing.ModelRates {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]pricing.ModelRates, len(m))
	for k, v := range m {
		out[k] = pricing.ModelRates{
			InputPerMTok:                v.InputPerMTok,
			CachedInputPerMTok:          v.CachedInputPerMTok,
			CacheCreationInputPerMTok:   v.CacheCreationInputPerMTok,
			CacheCreation1hInputPerMTok: v.CacheCreation1hInputPerMTok,
			OutputPerMTok:               v.OutputPerMTok,
		}
	}
	return out
}

// CostUSD returns the dollar cost of (input, output) tokens at p.
// Treats every input token as uncached — see CostUSDWithCache for the
// cached-vs-uncached split.
func (p Pricing) CostUSD(inputTokens, outputTokens int) float64 {
	return p.CostUSDWithCacheTTLs(inputTokens, 0, 0, 0, outputTokens)
}

// CostUSDWithCache returns the dollar cost with cache-hit tokens billed
// at CachedInputPerMTok. When CachedInputPerMTok is zero (rate unknown)
// cached tokens fall back to InputPerMTok so the estimate never
// silently drops to zero cost for cached input.
//
// Providers that also report cache-WRITE tokens should call
// CostUSDWithCacheWrites; this signature has no bucket for them, so
// callers fold them into uncached and understate the bill (#263).
func (p Pricing) CostUSDWithCache(uncachedInputTokens, cachedInputTokens, outputTokens int) float64 {
	return p.CostUSDWithCacheWrites(uncachedInputTokens, cachedInputTokens, 0, outputTokens)
}

// CostUSDForTurn prices one turn's full token breakdown, applying
// TurnUsage.Clamped first so the three input buckets are disjoint and
// non-negative. This is the one place turn cost is defined: Tracker's
// AppendUsage and the tracker-less fallbacks in pkg/agent all route
// through it so a cache-warming turn can't be priced two different
// ways depending on which call site saw it.
//
// ThoughtsTokens is billed at the output rate, on top of OutputTokens.
// Gemini reports thoughts as a bucket ADDITIVE to candidates rather
// than a subset of it — live metadata reads promptTokenCount 12455 +
// candidatesTokenCount 85 + thoughtsTokenCount 570 = totalTokenCount
// 13110 — and Google charges for them as output. Tracked since the
// first tap but never priced, which is not a rounding error on a
// thinking model: a measured agentic turn spent 6,449 thought tokens
// against 1,180 candidate tokens, so 85% of the billable output was
// invisible to the ledger, to /usage, and to the --max-session-cost-usd
// ceiling that reads it.
//
// Adding the bucket unconditionally is provider-safe rather than an
// Anthropic double-count: Anthropic bills thinking inside
// Usage.OutputTokens, and pkg/models/anthropic's usageMetadata maps
// that field to CandidatesTokenCount and never populates
// ThoughtsTokenCount — so on that path this term is zero and the turn
// prices exactly as it did before. Only providers that report thoughts
// out of band (Gemini/Vertex, via ThoughtsTokenCount) move.
//
// A negative count cannot subtract from the bill, because Clamped
// floors the three top-line buckets. That belongs there rather than
// here: pricing was never the only reader of these numbers, and a
// thoughts term guarded locally would have kept the negative out of the
// invoice while leaving it in Totals and in the monotonic OTel counter.
func (p Pricing) CostUSDForTurn(u TurnUsage) float64 {
	c := u.Clamped()
	return p.CostUSDWithCacheTTLs(
		c.UncachedInputTokens(), c.CachedInputTokens,
		c.CacheCreationInputTokens, c.CacheCreation1hInputTokens,
		c.OutputTokens+c.ThoughtsTokens)
}

// CostUSDWithCacheWrites is CostUSDWithCache plus the cache-write
// bucket, billed at CacheCreationInputPerMTok.
//
// The three input buckets must be disjoint: pass uncached = total
// prompt - cache reads - cache writes. Both cache rates fall back to
// InputPerMTok when unknown, so a model missing from the catalog
// degrades to the old understated number rather than billing cache
// traffic as free.
//
// Every write token is billed at the 5-minute TTL rate; callers that
// know the TTL split should use CostUSDWithCacheTTLs (#770).
func (p Pricing) CostUSDWithCacheWrites(uncachedInputTokens, cacheReadTokens, cacheWriteTokens, outputTokens int) float64 {
	return p.CostUSDWithCacheTTLs(uncachedInputTokens, cacheReadTokens, cacheWriteTokens, 0, outputTokens)
}

// CostUSDWithCacheTTLs is CostUSDWithCacheWrites with the write bucket
// split by breakpoint TTL. cacheWrite1hTokens is a SUBSET of
// cacheWriteTokens — Anthropic's
// `usage.cache_creation.ephemeral_1h_input_tokens` — billed at
// CacheCreation1hInputPerMTok, with the remainder at the 5-minute rate.
//
// An unknown 1-hour rate falls back to the 5-minute rate rather than to
// base input: the nearer neighbour is the better estimate for a catalog
// row that is missing a field. A prompt (all three input buckets) over
// the model's LongContext threshold bills every bucket and the output at
// the long-context rates. The arithmetic is pricing.Rates.CostUSDWithCacheTTLs,
// shared with mast, so the two products can't price one turn two ways.
func (p Pricing) CostUSDWithCacheTTLs(uncachedInputTokens, cacheReadTokens, cacheWriteTokens, cacheWrite1hTokens, outputTokens int) float64 {
	return p.rates().CostUSDWithCacheTTLs(uncachedInputTokens, cacheReadTokens, cacheWriteTokens, cacheWrite1hTokens, outputTokens)
}
