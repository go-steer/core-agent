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
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/auth"
	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
	"github.com/go-steer/core-agent/v2/pkg/pricing"
	"github.com/go-steer/core-agent/v2/pkg/usage"
)

// #930, the multi-session half. A per-session /pricing provider closes
// over a rate resolved when the session was CONSTRUCTED, so on this
// path the report goes stale too — the single-session provider in
// cmd/core-agent re-resolves, but this one did not.
//
// That matters beyond the report itself: the block's own comment
// requires /pricing to agree with what the tracker bills (#505). Making
// the wake loop bill the refreshed rate without moving this would have
// kept the two disagreeing, just with the halves swapped.
//
// Installs a process-global catalog, so not parallel.
func TestReproduceAgent_SessionPricingReportsTheRefreshedRate(t *testing.T) {
	usage.SetCatalog(nil)
	t.Cleanup(func() { usage.SetCatalog(nil) })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	project := t.TempDir()
	// The pricing provider is only wired when Cfg is non-nil.
	cfg := &config.Config{}
	deps := SessionFactoryDeps{
		DaemonCtx:   ctx,
		Model:       stubLLM{},
		Template:    permissions.New(permissions.Options{}),
		ProjectRoot: project,
		AgentsDir:   project,
		Cfg:         cfg,
		// What the daemon resolved at boot and handed to the factory.
		PricingRate: usage.Pricing{InputPerMTok: 1, OutputPerMTok: 1},
	}

	ag, cancelAg, err := ReproduceAgent(deps, auth.Anonymous, "sid-pricing", "created")
	if err != nil {
		t.Fatalf("ReproduceAgent: %v", err)
	}
	t.Cleanup(cancelAg)

	// The operator's /pricing refresh lands AFTER the session exists.
	c, err := pricing.NewCatalog(pricing.Options{
		CfgOverride: map[string]pricing.ModelRates{
			stubLLM{}.Name(): {InputPerMTok: 33, OutputPerMTok: 44},
		},
	})
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	usage.SetCatalog(c)

	got := ag.AttachPricing()
	if got.Current == nil {
		t.Fatalf("AttachPricing returned no Current rate")
	}
	if got.Current.InputUSDPerMTok != 33 || got.Current.OutputUSDPerMTok != 44 {
		t.Errorf("reported in %v / out %v, want 33 / 44 from the refreshed catalog — the rate captured at session construction was 1 / 1",
			got.Current.InputUSDPerMTok, got.Current.OutputUSDPerMTok)
	}
}
