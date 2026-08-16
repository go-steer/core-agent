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

package anthropic_test

import (
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/models/anthropic"
	"github.com/go-steer/core-agent/v2/pkg/pricing"
	"github.com/go-steer/core-agent/v2/pkg/taskclass"
)

// TestDefaultModel_MatchesFrontierTier pins DefaultModel to the
// task-class frontier default for the same provider.
//
// They are two answers to one question — "which Opus does an operator
// get when they don't name one" — reached by different code paths: an
// empty cfg.Model.Name falls through to DefaultModel, while --task=debug
// resolves through ModelForTier. When they disagree, --task=debug reads
// as a silent model DOWNGRADE on a flag whose whole point is to ask for
// more capability, and nothing warns.
//
// This is the pin that keeps the latest-in-line policy from applying to
// only half the entry points: TestModelForTier_ReturnsLatestInLine
// walks the task-class table and would never have looked at this
// constant, which is how it sat on claude-opus-4-7 with Opus 5 shipped.
func TestDefaultModel_MatchesFrontierTier(t *testing.T) {
	for _, provider := range []string{"anthropic", "anthropic-vertex"} {
		want := taskclass.ModelForTier(provider, taskclass.TierFrontier)
		if want == "" {
			t.Fatalf("taskclass.ModelForTier(%q, frontier) = \"\" — provider table drifted", provider)
		}
		if anthropic.DefaultModel != want {
			t.Errorf("anthropic.DefaultModel = %q, but taskclass frontier for %q is %q — "+
				"bump both together", anthropic.DefaultModel, provider, want)
		}
	}
}

// TestDefaultModel_IsPriced keeps the zero-config model inside the
// pricing catalog. An unpriced model bills at 0, which does not merely
// under-report: max_turn_cost_usd and max_session_cost_usd compare
// against that 0 and never trip, so the budget guardrails an operator
// configured are silently inert on the default model.
func TestDefaultModel_IsPriced(t *testing.T) {
	for _, id := range []string{anthropic.DefaultModel, anthropic.DefaultSmallModelID} {
		if _, ok := pricing.Builtin()[id]; !ok {
			t.Errorf("%q is not in pricing.Builtin() — cost tracking reports 0 for it "+
				"and budget caps never fire", id)
		}
	}
}
