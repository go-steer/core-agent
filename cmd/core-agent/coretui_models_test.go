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

//go:build !no_tui

package main

import (
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/modeltier"
	"github.com/go-steer/core-agent/v2/pkg/pricing"
	"github.com/go-steer/core-agent/v2/pkg/usage"
)

// TestAvailableModelIDs_AllPriced closes the direction
// pricing.TestBuiltinModelsKnownToCompanionTables explicitly cannot:
// picker => priced.
//
// The picker used to be a hand-maintained literal and had drifted to
// six entries that resolved to no rate at all — gemini-2.5-pro,
// gemini-2.5-flash, gemini-3-flash-preview, gemini-3.1-pro-preview,
// gemini-3.1-pro-preview-customtools and gemini-3.1-flash-image-preview.
// (Its other suffixed entries, claude-opus-4-7-1m and
// gemini-3.1-flash-lite-preview among them, escaped by accident:
// pricing.Lookup's longest-prefix fallback found the bare id. Exact
// membership is the bar here because the fallback is a safety net, not
// a plan.) Picking one of the six showed `$—` in the TUI, which is
// cosmetic, and billed the session at 0, which is not:
// max_turn_cost_usd and max_session_cost_usd compare against the
// running cost, and a cost pinned at 0 never reaches any cap. An
// operator who set a budget got no budget, with no warning.
//
// Today availableModelIDs derives from pricing.Builtin() so this holds
// by construction. The test is the guard against someone reintroducing
// a literal — which is exactly how it broke the first time.
func TestAvailableModelIDs_AllPriced(t *testing.T) {
	priced := pricing.Builtin()
	for _, id := range availableModelIDs() {
		if _, ok := priced[id]; !ok {
			t.Errorf("/model picker offers %q, which has no entry in pricing.Builtin(): "+
				"cost tracking reports 0 for it and max_turn_cost_usd / "+
				"max_session_cost_usd never trip", id)
		}
	}
}

// TestAvailableModelIDs_AllClassifiedAndSized is the same guard for the
// two tables that also fail open — an unclassified model runs the
// universal 0.85 compaction threshold instead of its tier's, and an
// unsized one disables threshold-based compaction outright. Both are
// implied by AllPriced today (pricing's own invariant test covers
// priced => classified+sized), but the picker is the operator-facing
// surface and it is worth failing here with a message that names it.
func TestAvailableModelIDs_AllClassifiedAndSized(t *testing.T) {
	for _, id := range availableModelIDs() {
		if modeltier.Classify(id) == "" {
			t.Errorf("/model picker offers %q, which modeltier.Classify does not recognize", id)
		}
		if usage.ContextWindowSizeFor(id) == 0 {
			t.Errorf("/model picker offers %q, which has no known context window "+
				"(disables threshold-based compaction for the session)", id)
		}
	}
}

// TestAvailableModelIDs_HeadOrder pins the top of the list.
//
// ORDER IS BEHAVIOR here: core-tui's picker opens on index 0 rather
// than on the active model, and `enter` both switches and persists. So
// entry 0 is what a reflexive /model+enter durably lands on, and a
// sorted-only list would hand that slot to whatever sorts first.
func TestAvailableModelIDs_HeadOrder(t *testing.T) {
	ids := availableModelIDs()
	if len(ids) < len(pickerHead) {
		t.Fatalf("picker has %d entries, fewer than the %d pinned head entries", len(ids), len(pickerHead))
	}
	for i, want := range pickerHead {
		if ids[i] != want {
			t.Errorf("picker[%d] = %q, want %q — the pinned head is what /model+enter lands on", i, ids[i], want)
		}
	}
}

// TestAvailableModelIDs_Narrowing pins the exclusions. Each one is a
// row an operator would have to read past in a dialog that is already
// 19 lines long, and none of them offers anything the surviving id
// does not.
func TestAvailableModelIDs_Narrowing(t *testing.T) {
	seen := map[string]bool{}
	for _, id := range availableModelIDs() {
		if seen[id] {
			t.Errorf("%q appears twice in the picker", id)
		}
		seen[id] = true

		switch {
		case strings.Contains(id, "claude-mythos"):
			// LiteLLM publishes the Mythos-class tier three times at
			// identical rates; claude-fable-5 is the id we surface.
			t.Errorf("%q is a duplicate id for the Mythos tier — only claude-fable-5 belongs in the picker", id)
		case strings.HasPrefix(id, "gemini-2."), strings.HasPrefix(id, "gemini-1."):
			// Gemini < 3 cannot mix server-side built-ins with function
			// declarations, so --task=research cannot search on it.
			t.Errorf("%q is below the Gemini 3.x picker cutoff", id)
		case strings.HasPrefix(id, "claude-3"), strings.HasPrefix(id, "claude-2"):
			t.Errorf("%q is below the Claude 4.x picker cutoff", id)
		}

		for _, f := range strings.Split(id, "-") {
			if len(f) == 8 && isAllDigits(f) {
				t.Errorf("%q is a date-pinned alias — same model as the bare id, twice the rows", id)
			}
		}
	}

	// The cutoff has to actually drop something, or a refactor that
	// neutered pickerEligible would pass every assertion above by
	// offering the whole catalog.
	if len(seen) >= len(pricing.Builtin()) {
		t.Errorf("picker offers %d of %d priced models — the narrowing rules are not firing",
			len(seen), len(pricing.Builtin()))
	}
}
