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

package compose

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-steer/core-agent/v3/pkg/config"
	"github.com/go-steer/core-agent/v3/pkg/pricing"
	"github.com/go-steer/core-agent/v3/pkg/usage"
)

// A daily price refresh must not cost a backend its own rates. The
// price A/B of #1328 found that after a refresh, claude-sonnet-4-5 on
// anthropic-vertex billed a 300K-token prompt at the first-party bare
// row's flat $0.96 instead of Vertex's long-context $1.89. The
// refreshed LiteLLM keys were stored verbatim (`vertex_ai/…`, not
// `anthropic-vertex/…`), and the external layer outranks the built-in
// table, so the bare row won. core-models v0.13.2 qualifies refreshed
// keys to its backend names; v0.13.3 also heals a cache an older
// refresh wrote, both on load and on a forced refresh.
//
// These drive the path core-agent ships: the /pricing refresh callback
// (or the catalog rebuild alone) installs the catalog, and the ledger's
// cost function bills the turn. The library has its own tests; these
// fail if core-agent's wiring stops picking the fix up.
//
// Each installs a process-global catalog, so none is parallel.

// liteLLMSonnet is the slice of LiteLLM's catalog the bug lives in: the
// first-party bare row is flat, the Vertex row carries the
// above-200K tier.
const liteLLMSonnet = `{
  "claude-sonnet-4-5": {"input_cost_per_token": 3e-06, "output_cost_per_token": 1.5e-05,
    "litellm_provider": "anthropic", "mode": "chat", "max_input_tokens": 200000, "supports_function_calling": true},
  "vertex_ai/claude-sonnet-4-5": {"input_cost_per_token": 3e-06, "output_cost_per_token": 1.5e-05,
    "input_cost_per_token_above_200k_tokens": 6e-06, "output_cost_per_token_above_200k_tokens": 2.25e-05,
    "litellm_provider": "vertex_ai-anthropic_models", "mode": "chat", "max_input_tokens": 200000, "supports_function_calling": true}
}`

// assertSonnetFromExternal bills a 300K-token turn on both backends and
// requires the refreshed layer to have served it. The source check
// matters: the built-in table alone prices both backends correctly, so
// a price check by itself passes with the refreshed layer missing,
// which is exactly the layer the bug lives in.
func assertSonnetFromExternal(t *testing.T) {
	t.Helper()
	turn := usage.TurnUsage{InputTokens: 300_000, OutputTokens: 4_000}
	for _, c := range []struct {
		backend string
		want    float64
	}{
		// Vertex's rows carry the long-context tier: 300K × $6 + 4K × $22.50.
		{"anthropic-vertex", 1.89},
		// The first-party row is flat: 300K × $3 + 4K × $15.
		{"anthropic", 0.96},
	} {
		bcfg := &config.Config{}
		bcfg.Model.Provider = c.backend
		p, src := usage.PriceForWithSource("claude-sonnet-4-5", bcfg)
		if src != pricing.SourceUserExternal {
			t.Errorf("%s: rate came from %q, want the refreshed layer %q", c.backend, src, pricing.SourceUserExternal)
		}
		if got := p.CostUSDForTurn(turn); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("%s: a 300K-token turn billed $%.4f, want $%.2f", c.backend, got, c.want)
		}
	}
}

// writeOldFormatCache writes the external section a v0.13.1 refresh
// left behind: LiteLLM's keys verbatim, no format, an ETag, and a
// fetched_at too recent for the daily cadence to refetch.
func writeOldFormatCache(t *testing.T, coreHome string) {
	t.Helper()
	uf := &pricing.UserFile{External: &pricing.ExternalSource{
		FetchedAt: time.Now().Add(-time.Hour),
		Source:    "https://example.invalid/litellm.json",
		ETag:      `"old-etag"`,
		Models: map[string]pricing.ModelRates{
			"claude-sonnet-4-5": {InputPerMTok: 3, OutputPerMTok: 15},
			"vertex_ai/claude-sonnet-4-5": {
				InputPerMTok: 3, OutputPerMTok: 15,
				LongContext: &pricing.LongContext{ThresholdTokens: 200_000, InputPerMTok: 6, OutputPerMTok: 22.5},
			},
		},
	}}
	if err := pricing.SaveUserFile(coreHome, uf); err != nil {
		t.Fatalf("SaveUserFile: %v", err)
	}
}

func TestRefreshPricing_KeepsAnthropicVertexLongContext(t *testing.T) {
	usage.SetCatalog(nil)
	t.Cleanup(func() { usage.SetCatalog(nil) })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(liteLLMSonnet))
	}))
	t.Cleanup(srv.Close)

	cfg := &config.Config{}
	cfg.Pricing.Source = srv.URL
	summary, err := RefreshPricing(context.Background(), cfg, t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatalf("RefreshPricing: %v", err)
	}
	// A refresh that fails or fetches nothing returns a nil error too.
	if !strings.HasPrefix(summary, "Refresh: updated ") {
		t.Fatalf("the refresh did not write rows: %q", summary)
	}
	assertSonnetFromExternal(t)
}

// An operator upgrading keeps the cache the old refresh wrote. Loading
// it must already price Vertex right, with no fetch: a deployment with
// no network, or one that runs --no-pricing-refresh, never replaces it.
func TestRebuildPricingCatalog_HealsAnOldFormatCacheOffline(t *testing.T) {
	usage.SetCatalog(nil)
	t.Cleanup(func() { usage.SetCatalog(nil) })

	coreHome := t.TempDir()
	writeOldFormatCache(t, coreHome)
	if err := RebuildPricingCatalog(&config.Config{}, t.TempDir(), coreHome); err != nil {
		t.Fatalf("RebuildPricingCatalog: %v", err)
	}
	assertSonnetFromExternal(t)
}

// notModifiedOnRevalidation serves liteLLMSonnet, but answers 304 to any
// request that revalidates with an ETag, the way LiteLLM's host does
// while its file is unchanged. So only a full fetch writes rows.
func notModifiedOnRevalidation(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var revalidations atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") != "" {
			revalidations.Add(1)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_, _ = w.Write([]byte(liteLLMSonnet))
	}))
	t.Cleanup(srv.Close)
	return srv, &revalidations
}

// The daemon's startup refresh over an old cache. This is the
// format-staleness half of v0.13.3: the cache is an hour old, inside the
// default 24h interval, and carries an ETag. A current-format cache that
// young would be skipped, or revalidated into a 304. An old-format one
// must be fetched in full anyway, or its rows stay until LiteLLM's file
// next changes upstream. Calls pricing.Refresh the way cmd/core-agent's
// startup does: Source only, default interval.
func TestStartupRefresh_ReplacesAnOldFormatCacheInsideTheInterval(t *testing.T) {
	usage.SetCatalog(nil)
	t.Cleanup(func() { usage.SetCatalog(nil) })

	srv, revalidations := notModifiedOnRevalidation(t)
	coreHome := t.TempDir()
	writeOldFormatCache(t, coreHome)
	out, err := pricing.Refresh(context.Background(), coreHome, pricing.RefreshOptions{Source: srv.URL})
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if out.Skipped || out.NotModified || out.NetworkFailed {
		t.Fatalf("the startup refresh kept the old cache: %+v", out)
	}
	if n := revalidations.Load(); n != 0 {
		t.Errorf("the startup refresh revalidated the old cache %d time(s) instead of fetching it in full", n)
	}
	if err := RebuildPricingCatalog(&config.Config{}, t.TempDir(), coreHome); err != nil {
		t.Fatalf("RebuildPricingCatalog: %v", err)
	}
	assertSonnetFromExternal(t)
}

// A forced /pricing refresh must really fetch. Before v0.13.3 it sent
// the cached ETag, a server whose file hadn't changed answered 304, and
// the old rows stayed. Since v0.13.3 a forced refresh never revalidates,
// whatever the cache's format, so this pins the forced path. The startup
// test above is the one that pins old-format staleness.
func TestRefreshPricing_ForcedRefreshReplacesAnOldFormatCache(t *testing.T) {
	usage.SetCatalog(nil)
	t.Cleanup(func() { usage.SetCatalog(nil) })

	srv, revalidations := notModifiedOnRevalidation(t)
	coreHome := t.TempDir()
	writeOldFormatCache(t, coreHome)
	cfg := &config.Config{}
	cfg.Pricing.Source = srv.URL
	summary, err := RefreshPricing(context.Background(), cfg, t.TempDir(), coreHome)
	if err != nil {
		t.Fatalf("RefreshPricing: %v", err)
	}
	if n := revalidations.Load(); n != 0 {
		t.Errorf("the forced refresh revalidated the old cache %d time(s) instead of fetching it in full", n)
	}
	if !strings.HasPrefix(summary, "Refresh: updated ") {
		t.Fatalf("the forced refresh did not replace the old cache: %q", summary)
	}
	uf, err := pricing.LoadUserFile(coreHome)
	if err != nil {
		t.Fatalf("LoadUserFile: %v", err)
	}
	if uf.External == nil || uf.External.Format < pricing.ExternalFormat {
		t.Errorf("the refreshed cache was not rewritten in the current format: %+v", uf.External)
	}
	assertSonnetFromExternal(t)
}
