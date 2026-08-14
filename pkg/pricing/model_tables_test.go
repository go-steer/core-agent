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

	"github.com/go-steer/core-agent/v2/pkg/modeltier"
	"github.com/go-steer/core-agent/v2/pkg/pricing"
	"github.com/go-steer/core-agent/v2/pkg/usage"
)

// TestBuiltinModelsKnownToCompanionTables pins the invariant that the
// four hand-maintained model tables move together.
//
// pricing.Builtin is the curated list of models this project actually
// expects to run (dev/regen-builtin-pricing's allowlist), so it is the
// natural source of truth. The other two tables answer the same
// operator-typed id — and both fail OPEN rather than loudly:
//
//   - modeltier.Classify returns "" for an unknown model, which drops
//     the per-tier compaction threshold and leaves the small-tier
//     parent guard unable to reason about the model.
//   - usage.ContextWindowSizeFor returns a 0 sentinel, and
//     DefaultCompactor.ShouldCompact treats 0 as "don't compact" — so
//     threshold-based compaction is silently disabled for the whole
//     session.
//
// Neither surfaces at startup; you find out when a long session dies on
// a provider context-length error. Adding a model to the pricing
// allowlist and forgetting the companions is therefore a live defect
// this test converts into a build failure.
//
// SCOPE: this checks one direction only — priced => classified+sized.
// The reverse (a model the tier/window tables match but pricing does
// not, so cost tracking reports 0 and max_turn_cost_usd never trips)
// cannot be enumerated here: both companions are substring matchers,
// not lists. Known unpriced ids reachable from the /model picker today
// are the four `-preview` entries in availableModelIDs.
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
