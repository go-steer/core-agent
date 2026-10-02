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

	"github.com/go-steer/core-agent/v2/pkg/agent"
	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/models/mock"
	"github.com/go-steer/core-agent/v2/pkg/modeltier"
	"github.com/go-steer/core-agent/v2/pkg/usage"
)

// #1226. An operator's compaction.threshold sat BELOW the substrate
// per-tier defaults, so it governed only unclassified models — none of the
// supported ones. --compaction-threshold writes that field, and so does a
// task class when it is unset, so all three were silently ignored for
// every recognised model. Found when a soak ran
// --compaction-threshold=0.021 on gemini-3.7-flash and every session
// sailed past 21K tokens without compacting.
//
// These drive the real decision — DefaultCompactor.ShouldCompact against a
// tracker — rather than reading the tier map back, because the map was
// never the problem: which entry won was.

// shouldCompactAt reports whether a compactor built from c would compact
// a session whose last turn on model sent `fill` input tokens.
func shouldCompactAt(t *testing.T, c config.CompactionConfig, model string, fill int) bool {
	t.Helper()
	tr := usage.NewTracker()
	tr.AppendUsage(model, usage.TurnUsage{InputTokens: fill, OutputTokens: 10}, usage.Pricing{})
	llm, err := mock.NewEcho().Model(context.Background(), "echo")
	if err != nil {
		t.Fatal(err)
	}
	a, err := agent.New(llm, agent.WithUsageTracker(tr))
	if err != nil {
		t.Fatal(err)
	}
	return BuildCompactor(c).ShouldCompact(context.Background(), a)
}

// One model per substrate tier, each with a ~1M window, so 30,000 tokens
// is ~3%: above a 2.1% operator threshold, far below every substrate
// default (0.85 / 0.65 / 0.35). TestOneModelPerTierIsWhatItClaims holds
// the fixture to modeltier.Classify — the first draft used gemini-3.1-pro
// as "mid", which classifies frontier, and so tested one tier twice.
var oneModelPerTier = map[string]string{
	"frontier": "gemini-3.7-flash",
	"mid":      "gemini-3.5-flash",
	"small":    "gemini-3.5-flash-lite",
}

func TestOneModelPerTierIsWhatItClaims(t *testing.T) {
	t.Parallel()
	for tier, model := range oneModelPerTier {
		if got := modeltier.Classify(model); got != tier {
			t.Errorf("fixture: %s classifies as %q, not %q", model, got, tier)
		}
	}
}

const fill3pct = 30_000

func ptr(v float64) *float64 { return &v }

func TestAnOperatorThresholdAppliesToEveryTier(t *testing.T) {
	t.Parallel()
	c := config.CompactionConfig{Threshold: ptr(0.021)}
	for tier, model := range oneModelPerTier {
		if !shouldCompactAt(t, c, model, fill3pct) {
			t.Errorf("%s (%s): compaction.threshold=0.021 did not compact a session at 3%% of its window; the substrate tier default still decides", tier, model)
		}
		// Control: with nothing set the same session is nowhere near
		// compaction, so the assertion above is the threshold's doing.
		if shouldCompactAt(t, config.CompactionConfig{}, model, fill3pct) {
			t.Errorf("%s (%s) control: an unconfigured session compacted at 3%% of its window", tier, model)
		}
	}
}

// The operator's per-tier entry is more specific than their single
// threshold, so it still wins for its own tier — and only there.
func TestAnOperatorTierEntryStillBeatsTheirSingleThreshold(t *testing.T) {
	t.Parallel()
	c := config.CompactionConfig{
		Threshold:       ptr(0.021),
		ThresholdByTier: map[string]float64{"frontier": 0.9},
	}
	if shouldCompactAt(t, c, oneModelPerTier["frontier"], fill3pct) {
		t.Error("the operator's threshold_by_tier.frontier=0.9 lost to their single threshold for a frontier model")
	}
	if !shouldCompactAt(t, c, oneModelPerTier["mid"], fill3pct) {
		t.Error("a frontier-only tier entry stopped the single threshold applying to a mid model")
	}
}
