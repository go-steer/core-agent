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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/pricing"
	"github.com/go-steer/core-agent/v2/pkg/usage"
)

// The pricing seam is worth asserting on the ASSEMBLED result rather
// than on return values: every function here exists to make the
// global usage catalog reflect a rate the operator just set, and the
// interesting failure is "the call succeeded and the catalog still
// prices the model the old way".
//
// These tests install a process-global catalog (usage.SetCatalog), so
// none of them are parallel, and each restores the no-catalog state
// other tests in this package assume.

// installCatalogGuard restores the "no catalog installed" state on
// cleanup. Without it, a catalog built from one test's temp dir
// leaks into every later test's usage.PriceFor.
func installCatalogGuard(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { usage.SetCatalog(nil) })
}

// liteLLMBody is a minimal upstream payload: two priced chat models
// plus the two rows parseLiteLLMBody is supposed to drop.
const liteLLMBody = `{
  "sample_spec": {"input_cost_per_token": 0.001, "output_cost_per_token": 0.002, "mode": "chat"},
  "test-chat-model": {"input_cost_per_token": 0.000001, "output_cost_per_token": 0.000004, "cache_read_input_token_cost": 0.00000025, "mode": "chat"},
  "test-completion-model": {"input_cost_per_token": 0.000002, "output_cost_per_token": 0.000008, "mode": "completion"},
  "test-embedding-model": {"input_cost_per_token": 0.0000001, "output_cost_per_token": 0.0000001, "mode": "embedding"}
}`

func TestCfgToCatalogOverride(t *testing.T) {
	t.Parallel()

	if got := CfgToCatalogOverride(nil); got != nil {
		t.Errorf("nil map: got %v, want nil (so the catalog falls through to the file + builtin layers)", got)
	}
	if got := CfgToCatalogOverride(config.PricingMap{}); got != nil {
		t.Errorf("empty map: got %v, want nil", got)
	}

	// Every rate field config exposes must survive the translation.
	// A dropped field here is invisible at every call site: the
	// catalog builds, the model resolves, and only the cost is wrong.
	in := config.PricingMap{
		"Model-With-Caps": {InputPerMTok: 3, CachedInputPerMTok: 0.75, OutputPerMTok: 12},
	}
	out := CfgToCatalogOverride(in)
	got, ok := out["Model-With-Caps"]
	if !ok {
		t.Fatalf("key not carried through: %v", out)
	}
	want := pricing.ModelRates{InputPerMTok: 3, CachedInputPerMTok: 0.75, OutputPerMTok: 12}
	if got != want {
		t.Errorf("rates: got %+v, want %+v", got, want)
	}
}

func TestCfgOverride_MatchesTheNoCatalogPath(t *testing.T) {
	// pkg/usage has its own copy of this translation (cfgToOverride,
	// used when no catalog is installed — tests and library
	// consumers). compose's copy is the one cmd/core-agent actually
	// boots with. They must agree, or a config behaves one way under
	// test and a different way in production.
	//
	// This is the regression test for the CachedInputPerMTok drop:
	// pre-fix, the no-catalog path honoured the operator's cache rate
	// and the daemon path silently billed cache hits at the full
	// input rate.
	installCatalogGuard(t)

	const model = "lockstep-probe-model"
	cfg := &config.Config{}
	cfg.Model.Pricing = config.PricingMap{
		model: {InputPerMTok: 3, CachedInputPerMTok: 0.75, OutputPerMTok: 12},
	}

	usage.SetCatalog(nil)
	viaFallback := usage.PriceFor(model, cfg)

	if err := RebuildPricingCatalog(cfg, t.TempDir(), t.TempDir()); err != nil {
		t.Fatalf("RebuildPricingCatalog: %v", err)
	}
	viaCatalog := usage.PriceFor(model, cfg)

	if viaCatalog != viaFallback {
		t.Errorf("cfg override resolves differently once the catalog is installed:\n  no catalog: %+v\n  catalog:    %+v\nthe two translations of config.PricingMap have drifted", viaFallback, viaCatalog)
	}
	if viaCatalog.CachedInputPerMTok != 0.75 {
		t.Errorf("cached_input_per_mtok = %v, want 0.75 — the operator's cache-hit rate was dropped, so cached tokens bill at the full input rate", viaCatalog.CachedInputPerMTok)
	}
}

func TestRebuildPricingCatalog_OverrideBeatsBuiltin(t *testing.T) {
	installCatalogGuard(t)

	// Pick a model the builtin table actually prices, so the test
	// proves precedence rather than just "the only layer won".
	const model = "gemini-2.5-flash"
	base := &config.Config{}
	if err := RebuildPricingCatalog(base, t.TempDir(), t.TempDir()); err != nil {
		t.Fatalf("RebuildPricingCatalog (baseline): %v", err)
	}
	builtin := usage.PriceFor(model, base)
	if builtin.Unpriced {
		t.Skipf("%s is not in the builtin table; precedence check needs a priced model", model)
	}

	cfg := &config.Config{}
	cfg.Model.Pricing = config.PricingMap{model: {InputPerMTok: 999, OutputPerMTok: 1999}}
	if err := RebuildPricingCatalog(cfg, t.TempDir(), t.TempDir()); err != nil {
		t.Fatalf("RebuildPricingCatalog (override): %v", err)
	}
	got := usage.PriceFor(model, cfg)
	if got.InputPerMTok != 999 || got.OutputPerMTok != 1999 {
		t.Errorf("cfg override didn't reach the installed catalog: got %+v, want 999/1999 (builtin was %+v)", got, builtin)
	}
}

func TestDescribeRefresh(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		outcome pricing.RefreshOutcome
		// want is a substring the line must contain; empty means the
		// path is required to stay silent.
		want string
	}{
		{
			name:    "fresh write reports the model count",
			outcome: pricing.RefreshOutcome{ModelCount: 247},
			want:    "updated 247 models",
		},
		{
			name:    "network failure with a cache names the cache age",
			outcome: pricing.RefreshOutcome{NetworkFailed: true, NetworkError: errRefreshProbe, StaleAge: 150 * time.Hour},
			want:    "using 150h0m0s-old cache",
		},
		{
			name:    "network failure with no cache warns about the builtin fallback",
			outcome: pricing.RefreshOutcome{NetworkFailed: true, NetworkError: errRefreshProbe},
			want:    "fall back to built-in table",
		},
		{
			name:    "skipped is quiet",
			outcome: pricing.RefreshOutcome{Skipped: true, ModelCount: 12},
		},
		{
			name:    "not-modified is quiet",
			outcome: pricing.RefreshOutcome{NotModified: true, ModelCount: 12},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var sb strings.Builder
			DescribeRefresh(&sb, tc.outcome)
			got := sb.String()
			if tc.want == "" {
				if got != "" {
					t.Errorf("want silence on the startup path, got %q", got)
				}
				return
			}
			if !strings.Contains(got, tc.want) {
				t.Errorf("got %q, want a line containing %q", got, tc.want)
			}
		})
	}
}

func TestDescribeRefresh_NetworkFailureAlwaysNamesTheError(t *testing.T) {
	t.Parallel()
	// The operator's next question after "refresh failed" is always
	// "failed how". Both failure branches must carry the cause.
	for _, out := range []pricing.RefreshOutcome{
		{NetworkFailed: true, NetworkError: errRefreshProbe},
		{NetworkFailed: true, NetworkError: errRefreshProbe, StaleAge: time.Hour},
	} {
		var sb strings.Builder
		DescribeRefresh(&sb, out)
		if !strings.Contains(sb.String(), errRefreshProbe.Error()) {
			t.Errorf("StaleAge=%v: %q omits the cause %q", out.StaleAge, sb.String(), errRefreshProbe)
		}
	}
}

func TestRefreshPricing_FetchesAndInstalls(t *testing.T) {
	installCatalogGuard(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(liteLLMBody))
	}))
	defer srv.Close()

	coreHome := t.TempDir()
	cfg := &config.Config{}
	cfg.Pricing.Source = srv.URL

	msg, err := RefreshPricing(context.Background(), cfg, t.TempDir(), coreHome)
	if err != nil {
		t.Fatalf("RefreshPricing: %v", err)
	}
	// Two of the four upstream rows are dropped (sample_spec, the
	// embedding model), so a count of 2 also proves the parse filter
	// ran rather than the body being passed through wholesale.
	if !strings.Contains(msg, "updated 2 models") {
		t.Errorf("summary = %q, want it to report 2 models", msg)
	}

	// The point of the call: the refreshed rate is live, not just on
	// disk. 0.000001/token → $1/Mtok.
	got := usage.PriceFor("test-chat-model", cfg)
	if got.Unpriced {
		t.Fatalf("refreshed model is unpriced — the catalog was not rebuilt from the new cache")
	}
	if got.InputPerMTok != 1 || got.OutputPerMTok != 4 {
		t.Errorf("rates = %+v, want 1/4 per Mtok", got)
	}
	if got.CachedInputPerMTok != 0.25 {
		t.Errorf("cached rate = %v, want 0.25", got.CachedInputPerMTok)
	}
}

func TestRefreshPricing_ForcesTheFetchPastTheDailyCadence(t *testing.T) {
	installCatalogGuard(t)

	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		_, _ = w.Write([]byte(liteLLMBody))
	}))
	defer srv.Close()

	coreHome := t.TempDir()
	cfg := &config.Config{}
	cfg.Pricing.Source = srv.URL

	// /pricing refresh is an operator explicitly asking for fresh
	// rates. It passes MinInterval: -1s precisely so the 24h cadence
	// that governs the startup refresh doesn't turn the second call
	// into a silent no-op.
	for i := range 2 {
		if _, err := RefreshPricing(context.Background(), cfg, t.TempDir(), coreHome); err != nil {
			t.Fatalf("RefreshPricing #%d: %v", i+1, err)
		}
	}
	if hits != 2 {
		t.Errorf("upstream hit %d times across two /pricing refresh calls, want 2 — the cadence gate is swallowing the operator's request", hits)
	}
}

func TestRefreshPricing_NetworkFailureIsNotFatal(t *testing.T) {
	installCatalogGuard(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "upstream sad", http.StatusInternalServerError)
	}))
	defer srv.Close()

	cfg := &config.Config{}
	cfg.Pricing.Source = srv.URL

	msg, err := RefreshPricing(context.Background(), cfg, t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatalf("a failed upstream must not fail the slash command: %v", err)
	}
	if !strings.Contains(msg, "Refresh failed") {
		t.Errorf("summary = %q, want it to say the refresh failed", msg)
	}
	if !strings.Contains(msg, "no cache to fall back to") {
		t.Errorf("summary = %q, want it to say there was no cache", msg)
	}
}

func TestSummarizeRefreshOutcome(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		outcome pricing.RefreshOutcome
		want    string
	}{
		{"fresh", pricing.RefreshOutcome{ModelCount: 5}, "updated 5 models"},
		{"not modified", pricing.RefreshOutcome{NotModified: true, ModelCount: 5}, "upstream unchanged"},
		{"failed with cache", pricing.RefreshOutcome{NetworkFailed: true, NetworkError: errRefreshProbe, StaleAge: 2 * time.Hour}, "using 2h0m0s-old cache"},
		{"failed without cache", pricing.RefreshOutcome{NetworkFailed: true, NetworkError: errRefreshProbe}, "no cache to fall back to"},
		// Skipped is unreachable from RefreshPricing — the only
		// caller — because MinInterval: -1s disables the cadence gate
		// at pkg/pricing/refresh.go:133. Covered anyway: without an
		// arm of its own it falls into default and reports "updated 5
		// models from upstream" for a call that made no request, and
		// a second caller passing a positive interval would inherit
		// that. Asserting on the distinct wording is what keeps the
		// arm from being deleted as dead code.
		{"skipped says no fetch happened", pricing.RefreshOutcome{Skipped: true, ModelCount: 5}, "cache is still current (5 models)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := summarizeRefreshOutcome(tc.outcome); !strings.Contains(got, tc.want) {
				t.Errorf("got %q, want a summary containing %q", got, tc.want)
			}
		})
	}
}

func TestSetPricing_AppliesImmediatelyAndPersists(t *testing.T) {
	installCatalogGuard(t)

	coreHome := t.TempDir()
	cfg := &config.Config{}

	msg, err := SetPricing(cfg, t.TempDir(), coreHome, "  My-Custom-Model  ", 2.5, 10)
	if err != nil {
		t.Fatalf("SetPricing: %v", err)
	}
	// The key is lowercased + trimmed; the operator should see the
	// key that was actually stored, not the one they typed.
	if !strings.Contains(msg, "my-custom-model") {
		t.Errorf("summary = %q, want the normalized key", msg)
	}

	// Applied to the live catalog, under the normalized key.
	got := usage.PriceFor("my-custom-model", cfg)
	if got.InputPerMTok != 2.5 || got.OutputPerMTok != 10 {
		t.Errorf("live catalog rates = %+v, want 2.5/10", got)
	}

	// Persisted into the manual section, which is the half that
	// survives an upstream refresh.
	uf, err := pricing.LoadUserFile(coreHome)
	if err != nil {
		t.Fatalf("LoadUserFile: %v", err)
	}
	if uf.Manual == nil || uf.Manual.Models["my-custom-model"].InputPerMTok != 2.5 {
		t.Fatalf("manual section = %+v, want the rate under my-custom-model", uf.Manual)
	}
}

func TestSetPricing_ManualRateSurvivesARefresh(t *testing.T) {
	installCatalogGuard(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(liteLLMBody))
	}))
	defer srv.Close()

	coreHome := t.TempDir()
	agentsDir := t.TempDir()
	cfg := &config.Config{}
	cfg.Pricing.Source = srv.URL

	// An operator hand-curating a rate for a model upstream doesn't
	// know about, then picking up an upstream refresh, must not lose
	// the hand-curated rate — Refresh rewrites only the external
	// section.
	if _, err := SetPricing(cfg, agentsDir, coreHome, "hand-curated-model", 7, 21); err != nil {
		t.Fatalf("SetPricing: %v", err)
	}
	if _, err := RefreshPricing(context.Background(), cfg, agentsDir, coreHome); err != nil {
		t.Fatalf("RefreshPricing: %v", err)
	}

	if got := usage.PriceFor("hand-curated-model", cfg); got.InputPerMTok != 7 {
		t.Errorf("manual rate after refresh = %+v, want 7 per Mtok", got)
	}
	if got := usage.PriceFor("test-chat-model", cfg); got.InputPerMTok != 1 {
		t.Errorf("refreshed rate = %+v, want 1 per Mtok — the refresh didn't land alongside the manual section", got)
	}
}

// errRefreshProbe stands in for a transport error in the outcome
// tables above.
var errRefreshProbe = refreshProbeError("dial tcp: connection refused")

type refreshProbeError string

func (e refreshProbeError) Error() string { return string(e) }
