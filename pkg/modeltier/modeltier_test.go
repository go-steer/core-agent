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

package modeltier_test

import (
	"testing"

	"github.com/go-steer/core-agent/pkg/modeltier"
)

func TestClassify(t *testing.T) {
	tests := []struct {
		model string
		want  string
	}{
		// Anthropic Claude 4.x.
		{"claude-opus-4-7", modeltier.TierFrontier},
		{"claude-opus-4-8", modeltier.TierFrontier},
		{"claude-opus-4-7-1m", modeltier.TierFrontier},
		{"claude-sonnet-4-6", modeltier.TierMid},
		{"claude-sonnet-4-6-1m", modeltier.TierMid},
		{"claude-haiku-4-5", modeltier.TierSmall},
		{"claude-haiku-4-5-20251001", modeltier.TierSmall},

		// Anthropic Claude 3.x.
		{"claude-3-5-sonnet-20241022", modeltier.TierMid},
		{"claude-3-5-haiku-20241022", modeltier.TierSmall},

		// Gemini 3.x.
		{"gemini-3.1-pro-preview-customtools", modeltier.TierFrontier},
		{"gemini-3.5-pro", modeltier.TierFrontier},
		{"gemini-3.5-flash", modeltier.TierSmall},

		// Gemini 2.x.
		{"gemini-2.5-pro", modeltier.TierMid},
		{"gemini-2.5-flash", modeltier.TierSmall},
		{"gemini-2.0-flash", modeltier.TierSmall},

		// Case-insensitive — operators sometimes type capitalized IDs.
		{"CLAUDE-OPUS-4-7", modeltier.TierFrontier},

		// Unknown / future / empty.
		{"", ""},
		{"some-future-model-9000", ""},
		{"gpt-5", ""}, // not classified yet; explicit zero-value contract
	}

	for _, tc := range tests {
		t.Run(tc.model, func(t *testing.T) {
			if got := modeltier.Classify(tc.model); got != tc.want {
				t.Errorf("Classify(%q) = %q, want %q", tc.model, got, tc.want)
			}
		})
	}
}

func TestDefaultCompactionThresholds(t *testing.T) {
	thresholds := modeltier.DefaultCompactionThresholds()

	want := map[string]float64{
		modeltier.TierFrontier: 0.85,
		modeltier.TierMid:      0.65,
		modeltier.TierSmall:    0.35,
	}

	for tier, expected := range want {
		got, ok := thresholds[tier]
		if !ok {
			t.Errorf("DefaultCompactionThresholds() missing tier %q", tier)
			continue
		}
		if got != expected {
			t.Errorf("DefaultCompactionThresholds()[%q] = %v, want %v", tier, got, expected)
		}
	}

	// Ordering invariant — smaller tier should get more aggressive
	// (lower) threshold. Validates against a future regression where
	// someone bumps small above mid or mid above frontier.
	if thresholds[modeltier.TierSmall] >= thresholds[modeltier.TierMid] {
		t.Errorf("small threshold (%v) should be < mid threshold (%v)",
			thresholds[modeltier.TierSmall], thresholds[modeltier.TierMid])
	}
	if thresholds[modeltier.TierMid] >= thresholds[modeltier.TierFrontier] {
		t.Errorf("mid threshold (%v) should be < frontier threshold (%v)",
			thresholds[modeltier.TierMid], thresholds[modeltier.TierFrontier])
	}

	// Caller-mutation safety — DefaultCompactionThresholds returns a
	// fresh map per call so a caller scribbling on it doesn't poison
	// the package default for everyone else.
	thresholds[modeltier.TierSmall] = 0.99
	fresh := modeltier.DefaultCompactionThresholds()
	if fresh[modeltier.TierSmall] != 0.35 {
		t.Errorf("DefaultCompactionThresholds() should return a fresh map; caller mutation leaked through")
	}
}
