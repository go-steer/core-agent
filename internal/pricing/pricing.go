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

// Package pricing resolves a model's per-million-token rates across a
// layered set of sources so usage costs stay accurate as new models
// ship and operators add overrides.
//
// Lookup chain (first exact-match wins; longest-prefix only at the
// end):
//
//  1. cfg.Model.Pricing[name] — operator override in .agents/config.json,
//     keyed by model name (case-insensitive). Survives /model switches.
//  2. .agents/pricing.json    — project-local additions (team-internal
//     model variants, project-specific routing).
//  3. ~/.core-agent/pricing.json — user-global file. Two sections:
//     `manual` (operator-curated, hand-edited or set via /pricing set)
//     and `external` (auto-fetched from LiteLLM in PR B; absent in PR A).
//  4. builtin                 — the compiled-in fallback table; the
//     zero-config baseline for common Gemini models. Lives in
//     internal/pricing/builtin.go.
//  5. longest-prefix match across the merge of (1)..(4) — handles
//     `gemini-3.1-pro-preview-customtools`-style suffixes.
//  6. (Rates{}, false)        — rate unknown; callers (e.g. the TUI's
//     cost displays) should render "$—" rather than "$0".
//
// The catalog is built once at startup from these sources (see
// NewCatalog) and consulted on every per-turn cost append; lookups
// are read-only and lock-free.
package pricing

import (
	"strings"
	"time"
)

// Rates is the per-million-token cost for one model. CachedInputPerMTok
// is the rate applied to input tokens served from the provider's prompt
// cache (Gemini's `cachedContentTokenCount`, Anthropic's
// `cache_read_input_tokens`); a zero value means the cache-read rate
// isn't known and callers should bill cached tokens at InputPerMTok.
//
// UpdatedAt records when the rate was last verified against its
// source (LiteLLM refresh time, generator run time for builtin
// entries, operator edit time for manual overrides). Zero when
// unknown. Surfaced through /pricing so operators can spot stale
// entries at a glance — issue #259 called out that hand-authored
// rates drift silently, and staleness visibility is the mitigation
// baked into the "regenerate builtin from LiteLLM" workflow that
// followed.
type Rates struct {
	InputPerMTok       float64
	CachedInputPerMTok float64
	OutputPerMTok      float64
	UpdatedAt          time.Time
}

// IsZero reports whether the rates carry no useful pricing.
// Used by callers to distinguish "free model" from "rate unknown" —
// only the latter should render "$—". CachedInputPerMTok isn't part
// of this check: a row that carries only a cache rate but no base
// input/output rates is still "unpriced" in the useful sense.
func (r Rates) IsZero() bool { return r.InputPerMTok == 0 && r.OutputPerMTok == 0 }

// CostUSD returns the dollar cost of (input, output) tokens at r.
// Treats every input token as uncached — see CostUSDWithCache for the
// cached-vs-uncached split.
func (r Rates) CostUSD(inputTokens, outputTokens int) float64 {
	const million = 1_000_000.0
	return (float64(inputTokens)/million)*r.InputPerMTok +
		(float64(outputTokens)/million)*r.OutputPerMTok
}

// CostUSDWithCache returns the dollar cost with cache-hit tokens billed
// at CachedInputPerMTok. When CachedInputPerMTok is zero (rate unknown)
// cached tokens fall back to InputPerMTok — no silent free-riding.
func (r Rates) CostUSDWithCache(uncachedInputTokens, cachedInputTokens, outputTokens int) float64 {
	const million = 1_000_000.0
	cachedRate := r.CachedInputPerMTok
	if cachedRate == 0 {
		cachedRate = r.InputPerMTok
	}
	return (float64(uncachedInputTokens)/million)*r.InputPerMTok +
		(float64(cachedInputTokens)/million)*cachedRate +
		(float64(outputTokens)/million)*r.OutputPerMTok
}

// Catalog is the merged view of all pricing sources, queried by
// model name. Construct with NewCatalog; consult with Lookup.
//
// Layers are stored separately so PR B's daily refresh can rewrite
// the external slice without touching the others, and so the
// precedence chain stays explicit (no "where did this rate come
// from" mystery).
type Catalog struct {
	// Sources, highest precedence first. Each map is lowercased on
	// insert so lookups are case-insensitive without per-call
	// allocations.
	cfgOverride map[string]Rates // cfg.Model.Pricing
	projectFile map[string]Rates // .agents/pricing.json
	userManual  map[string]Rates // ~/.core-agent/pricing.json "manual"
	userExt     map[string]Rates // ~/.core-agent/pricing.json "external"
	builtin     map[string]Rates // compiled-in fallback
}

// Layer source names surfaced via LookupWithSource + the attach
// /pricing endpoint. Stable strings — operators grep for them, docs
// reference them. Don't rename without a deprecation cycle.
const (
	SourceCfgOverride  = "cfg-override"
	SourceProjectFile  = "project-file"
	SourceUserManual   = "user-manual"
	SourceUserExternal = "user-external"
	SourceBuiltin      = "builtin"
)

// Lookup returns the resolved rates for modelID plus a found flag.
// !found means the caller should treat the cost as unknown ($—)
// rather than zero.
//
// Resolution: exact match scan across layers in precedence order,
// then a longest-prefix scan across the union of all layers.
func (c *Catalog) Lookup(modelID string) (Rates, bool) {
	r, _, ok := c.LookupWithSource(modelID)
	return r, ok
}

// LookupWithSource is Lookup + the name of the catalog layer that
// served the rate (SourceCfgOverride / SourceProjectFile /
// SourceUserManual / SourceUserExternal / SourceBuiltin). Empty
// source string when !ok. Used by /pricing so operators can spot
// stale builtin rates that should have been overridden by a fresh
// LiteLLM refresh but weren't — the visibility that #259 asked for.
//
// Resolution matches Lookup: exact match by precedence first, then
// longest-prefix across the union. The prefix-fallback path returns
// the source of the LAYER that held the winning prefix entry.
func (c *Catalog) LookupWithSource(modelID string) (Rates, string, bool) {
	if c == nil {
		return Rates{}, "", false
	}
	low := strings.ToLower(strings.TrimSpace(modelID))
	if low == "" {
		return Rates{}, "", false
	}
	// Exact match by precedence.
	for _, ls := range c.layersWithSource() {
		if r, ok := ls.layer[low]; ok {
			return r, ls.source, true
		}
	}
	// Longest-prefix fallback across the union.
	var bestKey string
	var bestRates Rates
	var bestSource string
	for _, ls := range c.layersWithSource() {
		for k, r := range ls.layer {
			if !strings.HasPrefix(low, k) {
				continue
			}
			if len(k) > len(bestKey) {
				bestKey = k
				bestRates = r
				bestSource = ls.source
			}
		}
	}
	if bestKey != "" {
		return bestRates, bestSource, true
	}
	return Rates{}, "", false
}

// layerWithSource pairs one layer map with its source-name string.
type layerWithSource struct {
	layer  map[string]Rates
	source string
}

// layersWithSource is the precedence-ordered pairing consulted by
// LookupWithSource — highest precedence first. The layer name is
// carried alongside so callers can attribute the match to the layer
// that served it.
func (c *Catalog) layersWithSource() []layerWithSource {
	return []layerWithSource{
		{c.cfgOverride, SourceCfgOverride},
		{c.projectFile, SourceProjectFile},
		{c.userManual, SourceUserManual},
		{c.userExt, SourceUserExternal},
		{c.builtin, SourceBuiltin},
	}
}

// CountByLayer reports how many model entries each layer holds.
// Surfaced via /pricing list (PR C) and useful for tests that
// want to assert the expected number of rows landed in each layer.
type CountByLayer struct {
	CfgOverride  int
	ProjectFile  int
	UserManual   int
	UserExternal int
	Builtin      int
}

// Counts returns per-layer entry counts.
func (c *Catalog) Counts() CountByLayer {
	if c == nil {
		return CountByLayer{}
	}
	return CountByLayer{
		CfgOverride:  len(c.cfgOverride),
		ProjectFile:  len(c.projectFile),
		UserManual:   len(c.userManual),
		UserExternal: len(c.userExt),
		Builtin:      len(c.builtin),
	}
}
