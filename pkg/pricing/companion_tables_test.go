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

package pricing_test

import (
	"testing"

	"github.com/go-steer/core-agent/v3/pkg/modeltier"
	"github.com/go-steer/core-agent/v3/pkg/pricing"
	"github.com/go-steer/core-agent/v3/pkg/taskclass"
	"github.com/go-steer/core-agent/v3/pkg/usage"
)

// The table itself, its regenerator and its own invariants live in
// go-steer/core-models (pricing). What stays here is what only
// core-agent can check: that its companion tables keep up with the
// table it bills from.

// TestBuiltinModelsKnownToCompanionTables pins priced => classified +
// sized. modeltier.Classify and usage.ContextWindowSizeFor both fail
// OPEN on an unknown model — no per-tier compaction threshold, and a 0
// window that disables threshold-based compaction for the session — so
// a core-models bump that newly prices a model, with the companions
// left behind, is a live defect this test turns into a build failure.
//
// One direction only: the companions are substring matchers, so there
// is no set to walk the other way. The /model picker, which derives
// from pricing.Builtin, is pinned by TestAvailableModelIDs_AllPriced in
// cmd/core-agent.
func TestBuiltinModelsKnownToCompanionTables(t *testing.T) {
	for id := range pricing.Builtin() {
		t.Run(id, func(t *testing.T) {
			if tier := modeltier.Classify(id); tier == "" {
				t.Errorf("modeltier.Classify(%q) = \"\" (unclassified); "+
					"add a case to pkg/modeltier/modeltier.go", id)
			}
			if size := usage.ContextWindowSizeFor(id); size == 0 {
				t.Errorf("usage.ContextWindowSizeFor(%q) = 0 (unknown; "+
					"disables threshold-based compaction); "+
					"add a case to pkg/usage/context_window.go", id)
			}
		})
	}
}

// TestBuiltin_CoversTaskclassTierDefaults pins every tier default
// taskclass can pick to a positive built-in rate. A default the table
// doesn't price reports "$—" across all cost UI and never trips a cost
// ceiling, and no other test notices.
func TestBuiltin_CoversTaskclassTierDefaults(t *testing.T) {
	t.Parallel()
	builtin := pricing.Builtin()
	tiers := []string{taskclass.TierFrontier, taskclass.TierMid, taskclass.TierSmall}
	for _, provider := range taskclass.Providers() {
		for _, tier := range tiers {
			model := taskclass.ModelForTier(provider, tier)
			if model == "" {
				t.Errorf("ModelForTier(%q, %q) = \"\" — provider missing from the tier table?", provider, tier)
				continue
			}
			r, ok := builtin[model]
			if !ok {
				t.Errorf("the built-in table is missing %q (the %s/%s tier default); its membership rule lives in go-steer/core-models' dev/regen-builtin-pricing", model, provider, tier)
				continue
			}
			if r.InputPerMTok <= 0 || r.OutputPerMTok <= 0 {
				t.Errorf("built-in %q rates = %+v, want positive input/output", model, r)
			}
		}
	}
}
