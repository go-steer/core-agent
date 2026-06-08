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

package main

import (
	"github.com/go-steer/core-agent/pkg/agent"
	"github.com/go-steer/core-agent/pkg/config"
	"github.com/go-steer/core-agent/pkg/modeltier"
)

// buildCompactor constructs the auto-compaction trigger that the
// post-turn hook consults. Starts from the substrate's per-tier
// defaults (modeltier.DefaultCompactionThresholds) and the historical
// 0.85 fallback, then layers operator config overrides on top.
//
// Resolution precedence for any one threshold lookup:
//  1. cfg.ThresholdByTier[currentModelTier], when present
//  2. The substrate per-tier default for that tier
//  3. cfg.Threshold (single fallback), when set
//  4. agent.DefaultCompactionThreshold
//
// Operators who want to leave defaults alone provide an empty
// CompactionConfig — same behavior as agent.NewDefaultCompactor()
// returns directly.
func buildCompactor(cfg config.CompactionConfig) agent.Compactor {
	tierThresholds := modeltier.DefaultCompactionThresholds()
	for tier, v := range cfg.ThresholdByTier {
		tierThresholds[tier] = v
	}

	threshold := agent.DefaultCompactionThreshold
	if cfg.Threshold != nil {
		threshold = *cfg.Threshold
	}

	return &agent.DefaultCompactor{
		Threshold:       threshold,
		ThresholdByTier: tierThresholds,
	}
}
