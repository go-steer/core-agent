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
	"github.com/go-steer/core-agent/v2/pkg/agent"
	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/modeltier"
)

// BuildCompactor constructs the auto-compaction trigger that the
// post-turn hook consults. Starts from the substrate's per-tier
// defaults (modeltier.DefaultCompactionThresholds) and the historical
// 0.85 fallback, then layers operator config overrides on top.
//
// Resolution precedence for any one threshold lookup:
//  1. cfg.ThresholdByTier[currentModelTier] — the operator, specific
//  2. cfg.Threshold — the operator, general
//  3. The substrate per-tier default for that tier
//  4. agent.DefaultCompactionThreshold
//
// cfg.Threshold used to sit BELOW the substrate per-tier defaults, which
// made it a fallback for unclassified models only — and every supported
// model classifies into a tier, so an operator's compaction.threshold,
// --compaction-threshold (which writes it), and every task class's
// threshold (which writes it when unset) had no effect on any recognised
// model (#1226). Found when a soak ran --compaction-threshold=0.021 on
// gemini-3.7-flash and sessions sailed past 21K tokens uncompacted, the
// substrate's 0.85 of a 1M window still deciding. An operator who sets
// one number means it to apply; the substrate defaults are what you get
// when you have not said.
//
// Operators who want to leave defaults alone provide an empty
// CompactionConfig — same behavior as agent.NewDefaultCompactor()
// returns directly.
func BuildCompactor(cfg config.CompactionConfig) agent.Compactor {
	threshold := agent.DefaultCompactionThreshold
	tierThresholds := modeltier.DefaultCompactionThresholds()
	if cfg.Threshold != nil {
		// The operator's single threshold displaces the substrate tier
		// defaults rather than sitting under them; only the operator's
		// own per-tier entries, layered below, outrank it.
		threshold = *cfg.Threshold
		tierThresholds = map[string]float64{}
	}
	for tier, v := range cfg.ThresholdByTier {
		tierThresholds[tier] = v
	}

	return &agent.DefaultCompactor{
		Threshold:       threshold,
		ThresholdByTier: tierThresholds,
	}
}
