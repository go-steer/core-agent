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
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/pricing"
	"github.com/go-steer/core-agent/v2/pkg/usage"
)

// DescribeRefresh renders a one-line summary of a pricing-refresh
// outcome to w. Surfaces the four distinct shapes operators care
// about: fresh write (model count), 304-not-modified, skipped
// (cache still within MinInterval), network failure (cache age +
// error so the operator knows to expect stale rates).
func DescribeRefresh(w io.Writer, out pricing.RefreshOutcome) {
	switch {
	case out.NetworkFailed:
		if out.StaleAge > 0 {
			fmt.Fprintf(w, "core-agent: pricing refresh: using %s-old cache; network: %v\n",
				out.StaleAge.Round(time.Hour), out.NetworkError)
			return
		}
		fmt.Fprintf(w, "core-agent: pricing refresh: %v (no cache; rates will fall back to built-in table)\n", out.NetworkError)
	case out.Skipped:
		// Quiet path — the refresh was a no-op because the cache
		// is still within MinInterval. Don't bother the operator.
		return
	case out.NotModified:
		// Server confirmed cache is current. Also quiet.
		return
	default:
		fmt.Fprintf(w, "core-agent: pricing refresh: updated %d models from upstream\n", out.ModelCount)
	}
}

// CfgToCatalogOverride translates config.PricingMap (the JSON-tagged
// per-model rate map operators put under model.pricing) into the
// pkg/pricing wire shape. nil-safe; an empty map means "no
// cfg override, fall through to the file + builtin layers".
//
// Must stay field-for-field in step with pkg/usage's cfgToOverride,
// which performs the same translation on the path taken when no
// catalog has been installed (tests + library consumers). A field
// carried by one and dropped by the other means the same config
// prices a turn differently under test than it does in the daemon.
// TestCfgOverride_MatchesTheNoCatalogPath enforces the agreement — but
// only over fields its fixture sets to a non-zero value, which is how
// CacheCreation1hInputPerMTok was dropped here and stayed green: both
// translations returned the zero rate for a field neither carried, and
// two zeroes compare equal. New rate fields must be added to that
// fixture in the same change that adds them to config.PricingConfig.
func CfgToCatalogOverride(m config.PricingMap) map[string]pricing.ModelRates {
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

// RebuildPricingCatalog re-reads every pricing source and installs
// the fresh catalog into usage.SetCatalog. Called after /pricing
// refresh + /pricing set so subsequent cost lookups see the new
// rates without a process restart.
func RebuildPricingCatalog(cfg *config.Config, agentsDir, coreHome string) error {
	catalog, err := pricing.NewCatalog(pricing.Options{
		CfgOverride: CfgToCatalogOverride(cfg.Model.Pricing),
		AgentsDir:   agentsDir,
		UserHome:    coreHome,
	})
	if err != nil {
		return err
	}
	usage.SetCatalog(catalog)
	return nil
}

// RefreshPricing is the /pricing refresh slash callback.
// Forces an out-of-cycle fetch (MinInterval: -1s) regardless of how
// recently the daily refresh ran, rebuilds the catalog, and returns
// a summary line for the chat scrollback.
func RefreshPricing(ctx context.Context, cfg *config.Config, agentsDir, coreHome string) (string, error) {
	outcome, err := pricing.Refresh(ctx, coreHome, pricing.RefreshOptions{
		Source:      cfg.Pricing.Source,
		MinInterval: -1 * time.Second,
	})
	if err != nil {
		return "", err
	}
	if rerr := RebuildPricingCatalog(cfg, agentsDir, coreHome); rerr != nil {
		// Rebuild failed; cache was written but catalog still points
		// at the pre-refresh data. Tell the operator both halves.
		return "", fmt.Errorf("refresh wrote cache but catalog rebuild failed: %w", rerr)
	}
	return summarizeRefreshOutcome(outcome), nil
}

// SetPricing is the /pricing set slash callback. Reads the
// user file, writes/updates the manual entry, saves atomically,
// then rebuilds the catalog so the rate takes effect immediately.
func SetPricing(cfg *config.Config, agentsDir, coreHome, model string, inputPerMTok, outputPerMTok float64) (string, error) {
	uf, err := pricing.LoadUserFile(coreHome)
	if err != nil {
		return "", fmt.Errorf("load user pricing file: %w", err)
	}
	if uf.Manual == nil {
		uf.Manual = &pricing.ManualSection{}
	}
	if uf.Manual.Models == nil {
		uf.Manual.Models = make(map[string]pricing.ModelRates)
	}
	key := strings.ToLower(strings.TrimSpace(model))
	// Update in place rather than replacing the entry: /pricing set
	// takes only the base input+output rates, and an entry can also
	// carry cache read/write rates that were hand-edited into the
	// manual section (the documented home for them). Overwriting the
	// whole struct silently reverted an Anthropic model to the
	// pre-#263 undercount the next time an operator bumped its base
	// rate — a cost regression with no message and no diff.
	entry := uf.Manual.Models[key]
	entry.InputPerMTok = inputPerMTok
	entry.OutputPerMTok = outputPerMTok
	uf.Manual.Models[key] = entry
	if err := pricing.SaveUserFile(coreHome, uf); err != nil {
		return "", fmt.Errorf("save user pricing file: %w", err)
	}
	if err := RebuildPricingCatalog(cfg, agentsDir, coreHome); err != nil {
		return "", fmt.Errorf("rebuild catalog: %w", err)
	}
	kept := ""
	if entry.CachedInputPerMTok > 0 || entry.CacheCreationInputPerMTok > 0 {
		kept = fmt.Sprintf(" · kept cache rates ($%g/M read · $%g/M write)",
			entry.CachedInputPerMTok, entry.CacheCreationInputPerMTok)
	}
	return fmt.Sprintf("Set %s = $%g/M in · $%g/M out%s (saved to ~/.core-agent/pricing.json manual section, applied to live catalog)",
		key, inputPerMTok, outputPerMTok, kept), nil
}

// summarizeRefreshOutcome renders the same four-shape outcome as
// startup's DescribeRefresh, but as a string (for the TUI slash
// command's chat response) rather than writing to stderr.
func summarizeRefreshOutcome(out pricing.RefreshOutcome) string {
	switch {
	case out.NetworkFailed:
		if out.StaleAge > 0 {
			return fmt.Sprintf("Refresh failed; using %s-old cache (%s)",
				out.StaleAge.Round(time.Hour), out.NetworkError)
		}
		return fmt.Sprintf("Refresh failed: %v (no cache to fall back to)", out.NetworkError)
	case out.Skipped:
		// Unreachable from RefreshPricing, which forces MinInterval
		// to -1s. Present so the four shapes stay in step with
		// DescribeRefresh: without it a skipped refresh falls into
		// the default arm and reports "updated N models from
		// upstream" for a call that fetched nothing.
		return fmt.Sprintf("Refresh: cache is still current (%d models); no fetch needed", out.ModelCount)
	case out.NotModified:
		return fmt.Sprintf("Refresh: upstream unchanged (cache still authoritative, %d models)", out.ModelCount)
	default:
		return fmt.Sprintf("Refresh: updated %d models from upstream", out.ModelCount)
	}
}
