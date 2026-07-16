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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultRefreshSource is the canonical LiteLLM pricing JSON URL.
// Community-maintained, MIT-licensed, structured, covers hundreds of
// models from every major provider. Overridable via RefreshOptions
// for tests + air-gapped mirrors.
const DefaultRefreshSource = "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"

// DefaultRefreshInterval is the daily cadence Refresh enforces: if
// the cache's FetchedAt is younger than this, Refresh is a no-op.
// 24h is long enough that the network cost is amortized but short
// enough that rate changes propagate within an operator's next
// session.
const DefaultRefreshInterval = 24 * time.Hour

// RefreshOptions controls the refresh fetch. All fields optional;
// defaults match a production launch (LiteLLM upstream, 24h cadence,
// no max-size cap, 30s timeout). Tests + air-gapped mirrors override
// Source; integration tests override Now for deterministic
// stale-vs-fresh decisions.
type RefreshOptions struct {
	// Source is the URL to fetch. Defaults to DefaultRefreshSource.
	Source string

	// Client is the HTTP client used. Defaults to a fresh one with
	// a 30s timeout — long enough for slow links, short enough that
	// startup doesn't hang forever.
	Client *http.Client

	// MinInterval is the minimum age the cache must reach before
	// Refresh actually fetches. Zero defaults to
	// DefaultRefreshInterval (24h). Set to a negative duration to
	// force a fetch regardless of cache age (used by PR C's
	// /pricing refresh slash command).
	MinInterval time.Duration

	// Now is the clock Refresh uses to compute cache age. Tests
	// override to deterministically drive stale-vs-fresh. Defaults
	// to time.Now.
	Now func() time.Time
}

// RefreshOutcome is the result of a Refresh call. Surfaced so the
// caller (cmd/core-agent startup + PR C's /pricing refresh slash)
// can render a meaningful one-liner: "Refreshed 247 models from
// LiteLLM" / "Cache is 4h old; skipped" / "Using 6-day-old cache;
// network unreachable: connection refused".
type RefreshOutcome struct {
	Skipped       bool          // true when the cache was fresh enough to skip the fetch
	NotModified   bool          // true when the server replied 304 (cache still authoritative)
	NetworkFailed bool          // true when the fetch itself failed; cache was preserved
	NetworkError  error         // populated when NetworkFailed
	StaleAge      time.Duration // age of the cache at decision time; zero for fresh writes
	ModelCount    int           // number of models written to the external section (zero on Skipped/NotModified)
	FetchedAt     time.Time     // timestamp stored in the cache file (or pre-existing one for Skipped)
}

// Refresh fetches the LiteLLM pricing table and writes it into the
// `external` section of <userHome>/pricing.json, preserving the
// `manual` section operators curate by hand. Daily cadence is
// enforced via MinInterval — if the cache is younger, the fetch
// is skipped.
//
// Network failures are non-fatal: existing cached data stays in
// place, the caller gets a populated RefreshOutcome.NetworkError,
// and the surfaced one-liner ("using N-day-old cache") lets the
// operator know the rates may be stale without breaking the
// session. Same fall-through for HTTP 5xx / malformed bodies.
//
// userHome must resolve to a writable directory (typically
// ~/.core-agent). Empty userHome is an error.
func Refresh(ctx context.Context, userHome string, opts RefreshOptions) (RefreshOutcome, error) {
	if userHome == "" {
		return RefreshOutcome{}, errors.New("pricing refresh: userHome is required")
	}
	if opts.Source == "" {
		opts.Source = DefaultRefreshSource
	}
	if opts.MinInterval == 0 {
		opts.MinInterval = DefaultRefreshInterval
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Client == nil {
		opts.Client = &http.Client{Timeout: 30 * time.Second}
	}

	// Load existing cache to drive both cadence + If-None-Match.
	existing, err := LoadUserFile(userHome)
	if err != nil {
		// Corrupt cache shouldn't block refresh — log via the
		// returned outcome by treating it like a missing cache.
		existing = &UserFile{Version: SchemaVersion}
	}
	now := opts.Now()
	var existingETag string
	var existingFetchedAt time.Time
	if existing.External != nil {
		existingETag = existing.External.ETag
		existingFetchedAt = existing.External.FetchedAt
		if opts.MinInterval > 0 && !existingFetchedAt.IsZero() {
			age := now.Sub(existingFetchedAt)
			if age < opts.MinInterval {
				return RefreshOutcome{
					Skipped:    true,
					FetchedAt:  existingFetchedAt,
					ModelCount: len(existing.External.Models),
				}, nil
			}
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, opts.Source, nil)
	if err != nil {
		return RefreshOutcome{NetworkFailed: true, NetworkError: err, FetchedAt: existingFetchedAt}, err
	}
	if existingETag != "" {
		req.Header.Set("If-None-Match", existingETag)
	}
	resp, err := opts.Client.Do(req)
	if err != nil {
		return RefreshOutcome{
			NetworkFailed: true,
			NetworkError:  err,
			FetchedAt:     existingFetchedAt,
			StaleAge:      ageOrZero(now, existingFetchedAt),
			ModelCount:    externalCount(existing),
		}, nil // intentionally nil — caller treats this as "use cache" rather than fatal
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotModified {
		// Cache is still authoritative. Refresh the FetchedAt
		// stamp so we don't re-check on every startup within the
		// MinInterval window.
		if existing.External != nil {
			existing.External.FetchedAt = now
			if err := SaveUserFile(userHome, existing); err != nil {
				return RefreshOutcome{}, fmt.Errorf("pricing refresh: save cache: %w", err)
			}
		}
		return RefreshOutcome{
			NotModified: true,
			FetchedAt:   now,
			ModelCount:  externalCount(existing),
		}, nil
	}
	if resp.StatusCode != http.StatusOK {
		err := fmt.Errorf("pricing refresh: %s returned HTTP %d", opts.Source, resp.StatusCode)
		return RefreshOutcome{
			NetworkFailed: true,
			NetworkError:  err,
			FetchedAt:     existingFetchedAt,
			StaleAge:      ageOrZero(now, existingFetchedAt),
			ModelCount:    externalCount(existing),
		}, nil
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return RefreshOutcome{
			NetworkFailed: true,
			NetworkError:  fmt.Errorf("pricing refresh: read body: %w", err),
			FetchedAt:     existingFetchedAt,
			StaleAge:      ageOrZero(now, existingFetchedAt),
			ModelCount:    externalCount(existing),
		}, nil
	}

	parsed, err := parseLiteLLMBody(body)
	if err != nil {
		return RefreshOutcome{
			NetworkFailed: true,
			NetworkError:  err,
			FetchedAt:     existingFetchedAt,
			StaleAge:      ageOrZero(now, existingFetchedAt),
			ModelCount:    externalCount(existing),
		}, nil
	}

	// Write the new external section atomically; the manual section
	// (if any) round-trips intact.
	existing.External = &ExternalSource{
		FetchedAt: now,
		Source:    opts.Source,
		ETag:      resp.Header.Get("ETag"),
		Models:    parsed,
	}
	if err := SaveUserFile(userHome, existing); err != nil {
		return RefreshOutcome{}, fmt.Errorf("pricing refresh: save cache: %w", err)
	}
	return RefreshOutcome{
		FetchedAt:  now,
		ModelCount: len(parsed),
	}, nil
}

// liteLLMEntry mirrors the subset of LiteLLM's JSON schema we
// care about. The full file carries fields for context window,
// supported features, etc. — irrelevant for pricing.
type liteLLMEntry struct {
	InputCostPerToken  *float64 `json:"input_cost_per_token,omitempty"`
	OutputCostPerToken *float64 `json:"output_cost_per_token,omitempty"`
	// CacheReadInputTokenCost is populated by LiteLLM for models that
	// support prompt caching with a distinct read rate — Gemini
	// (implicit + explicit), Anthropic (`cache_read_input_tokens`),
	// Bedrock, etc. Absent for models that don't offer cache reads at
	// a different rate. When present, it feeds Rates.CachedInputPerMTok
	// so CostUSDWithCache applies the discount rather than silently
	// billing cached tokens at InputPerMTok.
	CacheReadInputTokenCost *float64 `json:"cache_read_input_token_cost,omitempty"`
	// CacheCreationInputTokenCost is Anthropic-specific: the rate for
	// tokens that CREATE cache entries (billed at ~125% of input per
	// Anthropic's docs). Captured here so LiteLLM data isn't lost, but
	// NOT plumbed anywhere yet — Slice B follow-up work needs to
	// extend Rates + Pricing.CostUSDWithCache to attribute these
	// tokens at the correct rate. Today they're folded into the
	// uncached-input bucket for Anthropic, undercounting cost on
	// cache-warming turns.
	CacheCreationInputTokenCost *float64 `json:"cache_creation_input_token_cost,omitempty"`
	Mode                        string   `json:"mode,omitempty"`
}

// parseLiteLLMBody decodes the LiteLLM JSON and maps it into our
// per-model ModelRates shape. Filters:
//   - Skip "sample_spec" (LiteLLM's documentation row)
//   - Skip entries without both cost fields populated (image gen,
//     embeddings, etc. either have no costs or mode-specific costs
//     we don't model yet — PR C extension if it becomes a gap).
//   - Convert per-token rates → per-million-token rates to match
//     our internal Rates struct.
func parseLiteLLMBody(body []byte) (map[string]ModelRates, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("pricing refresh: parse litellm json: %w", err)
	}
	const million = 1_000_000.0
	out := make(map[string]ModelRates, len(raw))
	for name, payload := range raw {
		if name == "sample_spec" {
			continue
		}
		var e liteLLMEntry
		if err := json.Unmarshal(payload, &e); err != nil {
			// Skip a single malformed entry rather than failing
			// the whole refresh — LiteLLM's schema occasionally
			// adds string-valued cost fields for special pricing
			// (e.g. tiered per-token-range pricing) we can't model.
			continue
		}
		if e.InputCostPerToken == nil || e.OutputCostPerToken == nil {
			continue
		}
		if *e.InputCostPerToken == 0 && *e.OutputCostPerToken == 0 {
			continue
		}
		// Mode filter: keep chat/completion/responses entries. Image
		// generation, embedding, audio, etc. don't fit the per-turn
		// token-cost shape the tracker uses.
		if e.Mode != "" && !isTextMode(e.Mode) {
			continue
		}
		rates := ModelRates{
			InputPerMTok:  *e.InputCostPerToken * million,
			OutputPerMTok: *e.OutputCostPerToken * million,
		}
		// Populate cache-read rate when LiteLLM provides it. Ignore
		// zero values — some entries carry `cache_read_input_token_cost: 0`
		// as a placeholder for "not supported" rather than free.
		if e.CacheReadInputTokenCost != nil && *e.CacheReadInputTokenCost > 0 {
			rates.CachedInputPerMTok = *e.CacheReadInputTokenCost * million
		}
		out[strings.ToLower(strings.TrimSpace(name))] = rates
	}
	if len(out) == 0 {
		return nil, errors.New("pricing refresh: litellm body contained zero usable entries")
	}
	return out, nil
}

// isTextMode reports whether the LiteLLM mode tag indicates a
// text-completion-shaped model whose input/output rates correspond
// to our token-counting model.
func isTextMode(mode string) bool {
	switch strings.ToLower(mode) {
	case "chat", "completion", "responses":
		return true
	}
	return false
}

func externalCount(uf *UserFile) int {
	if uf == nil || uf.External == nil {
		return 0
	}
	return len(uf.External.Models)
}

func ageOrZero(now, t time.Time) time.Duration {
	if t.IsZero() {
		return 0
	}
	return now.Sub(t)
}
