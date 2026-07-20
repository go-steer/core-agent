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
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/agent"
	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/modeltier"
)

func TestBuildCompactor_DefaultsWhenConfigEmpty(t *testing.T) {
	c := buildCompactor(config.CompactionConfig{})
	dc, ok := c.(*agent.DefaultCompactor)
	if !ok {
		t.Fatalf("buildCompactor returned %T, want *agent.DefaultCompactor", c)
	}
	if dc.Threshold != agent.DefaultCompactionThreshold {
		t.Errorf("Threshold = %v, want substrate default %v", dc.Threshold, agent.DefaultCompactionThreshold)
	}
	defaults := modeltier.DefaultCompactionThresholds()
	for tier, want := range defaults {
		if dc.ThresholdByTier[tier] != want {
			t.Errorf("ThresholdByTier[%q] = %v, want substrate default %v", tier, dc.ThresholdByTier[tier], want)
		}
	}
}

func TestBuildCompactor_OperatorOverrides(t *testing.T) {
	custom := 0.5
	c := buildCompactor(config.CompactionConfig{
		Threshold: &custom,
		ThresholdByTier: map[string]float64{
			// Operator override: keep frontier on the substrate default
			// implicitly, but compact small tier much earlier than the
			// 0.35 substrate default.
			modeltier.TierSmall: 0.20,
		},
	})
	dc, ok := c.(*agent.DefaultCompactor)
	if !ok {
		t.Fatalf("buildCompactor returned %T, want *agent.DefaultCompactor", c)
	}
	if dc.Threshold != 0.5 {
		t.Errorf("Threshold = %v, want operator override 0.5", dc.Threshold)
	}
	if got := dc.ThresholdByTier[modeltier.TierSmall]; got != 0.20 {
		t.Errorf("ThresholdByTier[small] = %v, want operator override 0.20", got)
	}
	// Substrate defaults still present for tiers the operator didn't
	// override.
	if got := dc.ThresholdByTier[modeltier.TierFrontier]; got != 0.85 {
		t.Errorf("ThresholdByTier[frontier] = %v, want substrate default 0.85 (operator didn't override)", got)
	}
	if got := dc.ThresholdByTier[modeltier.TierMid]; got != 0.65 {
		t.Errorf("ThresholdByTier[mid] = %v, want substrate default 0.65 (operator didn't override)", got)
	}
}
