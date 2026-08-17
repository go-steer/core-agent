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

package taskclass_test

import (
	"strconv"
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/pricing"
	"github.com/go-steer/core-agent/v2/pkg/taskclass"
)

// deferredPromotion records a newer same-line model we know about and
// have deliberately NOT promoted to a tier default yet.
//
// The entry is not a suppression, it is a decision with a name on it.
// It excuses exactly ONE successor: if a model newer than Newer shows
// up in the pricing catalog, the test fails again and the deferral has
// to be re-argued rather than silently extended to a model nobody
// looked at.
type deferredPromotion struct {
	Newer string
	Why   string
}

// deferredPromotions is keyed by the model ModelForTier currently
// returns.
// Empty is the healthy state: it means every tier default is the newest
// model in its line. The gemini-3.6-flash → gemini-3.7-flash entry that
// lived here was resolved by promotion, not by expiry.
var deferredPromotions = map[string]deferredPromotion{}

// TestModelForTier_ReturnsLatestInLine enforces the policy documented on
// ModelForTier: a tier default names the LATEST model in its line.
//
// Nothing else can enforce it. LiteLLM's catalog has no recency field,
// and auto-promoting from the weekly regen would push an un-UAT'd model
// to every operator on a Monday. So the rule lives here: the moment
// pricing.Builtin() carries a newer model in the same line as a tier
// default, the build fails and a human either bumps the default or
// records a deferral above. Before this test, "we should always be on
// the latest Opus" was a habit, and the Anthropic frontier default sat
// two generations behind (claude-opus-4-7 with Opus 5 shipped and
// priced).
//
// SHARED LINES: for Gemini the tiers are separated by generation, not by
// line — frontier is gemini-3.7-flash and mid is gemini-3.5-flash, both
// on the "flash" line. Applying the rule to the lower tier would demand
// that mid and frontier be the same model. So a line is checked once,
// at the highest tier that claims it; lower tiers sharing that line are
// generation-differentiated on purpose and skipped.
func TestModelForTier_ReturnsLatestInLine(t *testing.T) {
	// Highest tier first — the walk order is what makes the
	// shared-line skip resolve to the right tier.
	tiers := []string{taskclass.TierFrontier, taskclass.TierMid, taskclass.TierSmall}

	priced := pricing.Builtin()

	for _, provider := range taskclass.Providers() {
		claimed := map[string]string{} // line → tier that owns it
		for _, tier := range tiers {
			got := taskclass.ModelForTier(provider, tier)
			if got == "" {
				continue
			}
			line, ver, ok := lineAndVersion(got)
			if !ok {
				t.Errorf("%s/%s: ModelForTier returned %q, which does not parse "+
					"as <family>-<line>-<version> — either the id is wrong or "+
					"lineAndVersion needs to learn a new naming scheme",
					provider, tier, got)
				continue
			}
			if owner, dup := claimed[line]; dup {
				t.Logf("%s/%s: %q shares the %q line with the %s tier — "+
					"generation-differentiated, not checked", provider, tier, got, line, owner)
				continue
			}
			claimed[line] = tier

			newest, newestVer := got, ver
			for id := range priced {
				l, v, ok := lineAndVersion(id)
				if !ok || l != line {
					continue
				}
				if compareVersion(v, newestVer) > 0 {
					newest, newestVer = id, v
				}
			}
			if newest == got {
				continue
			}
			if d, ok := deferredPromotions[got]; ok && d.Newer == newest {
				t.Logf("%s/%s: %q held at %q — %s", provider, tier, newest, got, d.Why)
				continue
			}
			t.Errorf("%s/%s: ModelForTier returns %q but %q is a newer model on the "+
				"same %q line and is already priced. Bump the default in "+
				"pkg/taskclass, or add a deferredPromotions entry saying why not.",
				provider, tier, got, newest, line)
		}
	}
}

// TestDeferredPromotions_StillApply keeps the deferral list from
// outliving its subjects. A stale entry is worse than no entry: it
// silently excuses a promotion the catalog no longer even offers, and
// the next real successor slides in under it.
func TestDeferredPromotions_StillApply(t *testing.T) {
	priced := pricing.Builtin()
	for current, d := range deferredPromotions {
		if _, ok := priced[current]; !ok {
			t.Errorf("deferredPromotions[%q]: no longer in the pricing catalog — drop the entry", current)
		}
		if _, ok := priced[d.Newer]; !ok {
			t.Errorf("deferredPromotions[%q].Newer = %q: no longer in the pricing catalog — "+
				"the deferral is excusing a model that upstream pulled; drop the entry", current, d.Newer)
		}
	}
}

// lineAndVersion splits a model id into its product line and version.
//
//	gemini-3.6-flash      → ("flash", [3 6])
//	gemini-3.5-flash-lite → ("flash-lite", [3 5])
//	claude-opus-4-7       → ("opus", [4 7])
//	claude-opus-5         → ("opus", [5])
//
// Date-pinned aliases (claude-opus-4-7-20260416) return ok=false: they
// are the same model as the bare id, and their date field would compare
// as a much larger version component and "win" the latest-in-line
// contest against the id an operator would actually type.
func lineAndVersion(id string) (string, []int, bool) {
	fields := strings.Split(id, "-")
	if len(fields) < 3 {
		return "", nil, false
	}
	for _, f := range fields {
		if len(f) == 8 && isAllDigits(f) {
			return "", nil, false
		}
	}
	switch fields[0] {
	case "gemini":
		// gemini-<version>-<line...>
		ver, ok := parseVersion(fields[1])
		if !ok {
			return "", nil, false
		}
		return strings.Join(fields[2:], "-"), ver, true
	case "claude":
		// claude-<line>-<version...>, line is everything before the
		// first numeric field.
		for i := 1; i < len(fields); i++ {
			if !isAllDigits(fields[i]) {
				continue
			}
			var ver []int
			for _, f := range fields[i:] {
				n, ok := parseVersion(f)
				if !ok {
					return "", nil, false
				}
				ver = append(ver, n...)
			}
			return strings.Join(fields[1:i], "-"), ver, true
		}
	}
	return "", nil, false
}

// parseVersion turns "3.6" into [3 6] and "5" into [5].
func parseVersion(s string) ([]int, bool) {
	var out []int
	for _, part := range strings.Split(s, ".") {
		n, err := strconv.Atoi(part)
		if err != nil {
			return nil, false
		}
		out = append(out, n)
	}
	return out, true
}

// compareVersion orders two version vectors, shorter-is-older on a
// common prefix ([5] < [5 1]).
func compareVersion(a, b []int) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		switch {
		case a[i] < b[i]:
			return -1
		case a[i] > b[i]:
			return 1
		}
	}
	return len(a) - len(b)
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
