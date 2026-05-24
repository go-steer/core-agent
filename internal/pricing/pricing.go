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
)

// Rates is the per-million-token cost for one model.
type Rates struct {
	InputPerMTok  float64
	OutputPerMTok float64
}

// IsZero reports whether the rates carry no useful pricing.
// Used by callers to distinguish "free model" from "rate unknown" —
// only the latter should render "$—".
func (r Rates) IsZero() bool { return r.InputPerMTok == 0 && r.OutputPerMTok == 0 }

// CostUSD returns the dollar cost of (input, output) tokens at r.
func (r Rates) CostUSD(inputTokens, outputTokens int) float64 {
	const million = 1_000_000.0
	return (float64(inputTokens)/million)*r.InputPerMTok +
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

// Lookup returns the resolved rates for modelID plus a found flag.
// !found means the caller should treat the cost as unknown ($—)
// rather than zero.
//
// Resolution: exact match scan across layers in precedence order,
// then a longest-prefix scan across the union of all layers.
func (c *Catalog) Lookup(modelID string) (Rates, bool) {
	if c == nil {
		return Rates{}, false
	}
	low := strings.ToLower(strings.TrimSpace(modelID))
	if low == "" {
		return Rates{}, false
	}
	// Exact match by precedence.
	for _, layer := range c.layersInOrder() {
		if r, ok := layer[low]; ok {
			return r, true
		}
	}
	// Longest-prefix fallback across the union.
	var bestKey string
	var bestRates Rates
	for _, layer := range c.layersInOrder() {
		for k, r := range layer {
			if !strings.HasPrefix(low, k) {
				continue
			}
			if len(k) > len(bestKey) {
				bestKey = k
				bestRates = r
			}
		}
	}
	if bestKey != "" {
		return bestRates, true
	}
	return Rates{}, false
}

// layersInOrder returns the per-source maps in precedence order
// (highest first). Used by both the exact-match scan and the
// prefix-match scan so they stay in lockstep.
func (c *Catalog) layersInOrder() []map[string]Rates {
	return []map[string]Rates{
		c.cfgOverride,
		c.projectFile,
		c.userManual,
		c.userExt,
		c.builtin,
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
