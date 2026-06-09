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
	"testing"

	"github.com/go-steer/core-agent/pkg/taskclass"
)

func TestResolve_KnownClasses(t *testing.T) {
	for _, class := range taskclass.Classes() {
		t.Run(class, func(t *testing.T) {
			p, ok := taskclass.Resolve(class)
			if !ok {
				t.Fatalf("Resolve(%q): want ok=true; class is in Classes()", class)
			}
			if p.Tier == "" {
				t.Errorf("Tier should be set for class %q", class)
			}
			if p.CompactionThreshold <= 0 || p.CompactionThreshold >= 1 {
				t.Errorf("CompactionThreshold for class %q = %v; want 0 < x < 1", class, p.CompactionThreshold)
			}
			if p.AskMode == "" {
				t.Errorf("AskMode should be set for class %q", class)
			}
		})
	}
}

func TestResolve_UnknownClass(t *testing.T) {
	if _, ok := taskclass.Resolve("debugg"); ok { // typo
		t.Errorf("Resolve(typo) should return ok=false")
	}
	if _, ok := taskclass.Resolve("MONITOR"); ok { // future class
		t.Errorf("Resolve(unknown) should return ok=false")
	}
}

func TestResolve_EmptyClass(t *testing.T) {
	if _, ok := taskclass.Resolve(""); ok {
		t.Errorf("Resolve(\"\") should return ok=false (no class declared)")
	}
}

func TestClasses_StableOrder(t *testing.T) {
	// Order is design-doc-driven (debug → implement → chat → research
	// → review). Stable so usage messages and CLI shells autocomplete
	// land predictably. Regression guard.
	want := []string{"debug", "implement", "chat", "research", "review"}
	got := taskclass.Classes()
	if len(got) != len(want) {
		t.Fatalf("Classes() len = %d, want %d", len(got), len(want))
	}
	for i, c := range want {
		if got[i] != c {
			t.Errorf("Classes()[%d] = %q, want %q", i, got[i], c)
		}
	}
}

func TestProfileTiers_DesignDocTable(t *testing.T) {
	// Pinned against the design doc's table values. A change here
	// is a behavior change for every --task=<x> user; this test
	// forces it to be intentional + reviewable.
	cases := []struct {
		class string
		tier  string
	}{
		{"debug", "frontier"},
		{"implement", "frontier"},
		{"chat", "mid"},
		{"research", "mid"},
		{"review", "frontier"},
	}
	for _, tc := range cases {
		t.Run(tc.class, func(t *testing.T) {
			p, _ := taskclass.Resolve(tc.class)
			if p.Tier != tc.tier {
				t.Errorf("Resolve(%q).Tier = %q, want %q (design doc table)", tc.class, p.Tier, tc.tier)
			}
		})
	}
}

func TestProfileThresholds_DesignDocTable(t *testing.T) {
	cases := []struct {
		class string
		want  float64
	}{
		{"debug", 0.65},
		{"implement", 0.70},
		{"chat", 0.85},
		{"research", 0.65},
		{"review", 0.75},
	}
	for _, tc := range cases {
		t.Run(tc.class, func(t *testing.T) {
			p, _ := taskclass.Resolve(tc.class)
			if p.CompactionThreshold != tc.want {
				t.Errorf("Resolve(%q).CompactionThreshold = %v, want %v", tc.class, p.CompactionThreshold, tc.want)
			}
		})
	}
}

func TestModelForTier(t *testing.T) {
	cases := []struct {
		provider, tier, want string
	}{
		// Gemini family.
		{"gemini", "frontier", "gemini-3.5-pro"},
		{"gemini", "mid", "gemini-2.5-pro"},
		{"gemini", "small", "gemini-2.5-flash"},
		{"vertex", "frontier", "gemini-3.5-pro"}, // vertex aliases gemini
		{"vertex", "small", "gemini-2.5-flash"},

		// Anthropic family.
		{"anthropic", "frontier", "claude-opus-4-7"},
		{"anthropic", "mid", "claude-sonnet-4-6"},
		{"anthropic", "small", "claude-haiku-4-5"},
		{"anthropic-vertex", "frontier", "claude-opus-4-7"},

		// Negative cases — caller falls through to whatever model
		// would've been chosen without --task.
		{"echo", "frontier", ""},
		{"scripted", "small", ""},
		{"unknown-provider", "frontier", ""},
		{"gemini", "unknown-tier", ""},
		{"gemini", "", ""},
		{"", "frontier", ""},
	}
	for _, tc := range cases {
		t.Run(tc.provider+"/"+tc.tier, func(t *testing.T) {
			got := taskclass.ModelForTier(tc.provider, tc.tier)
			if got != tc.want {
				t.Errorf("ModelForTier(%q, %q) = %q, want %q", tc.provider, tc.tier, got, tc.want)
			}
		})
	}
}

func TestModelForTier_ConsistentWithSmallModelDefaulters(t *testing.T) {
	// The "small" tier for each provider should match the
	// DefaultSmallModelID constants in pkg/models/<provider>/.
	// If these drift, --agentic-small-model defaulting (#122) and
	// --task=*'s small-tier choice would disagree — operator
	// confusion. Worth pinning.
	cases := []struct {
		provider, want string
	}{
		{"gemini", "gemini-2.5-flash"},
		{"vertex", "gemini-2.5-flash"},
		{"anthropic", "claude-haiku-4-5"},
		{"anthropic-vertex", "claude-haiku-4-5"},
	}
	for _, tc := range cases {
		t.Run(tc.provider, func(t *testing.T) {
			if got := taskclass.ModelForTier(tc.provider, "small"); got != tc.want {
				t.Errorf("small-tier mismatch for %q: ModelForTier returned %q, expected %q (matches pkg/models DefaultSmallModelID)", tc.provider, got, tc.want)
			}
		})
	}
}

func TestProfile_ResearchUsesAllowAskMode(t *testing.T) {
	// Research is the one class with ask=allow rather than ask=auto.
	// Pinned so a future refactor doesn't quietly turn research-mode
	// agents into ask-mode-prompting agents (the operator chose
	// research to AVOID per-tool prompts on read-heavy work).
	p, _ := taskclass.Resolve("research")
	if p.AskMode != "allow" {
		t.Errorf("research.AskMode = %q, want %q", p.AskMode, "allow")
	}
}

func TestProfile_ChatSkipsSmallModelSplit(t *testing.T) {
	// Chat is the one class with UseAgenticSmallModel=false. Pinned
	// so the design rationale ("chat subtasks are usually one-shot
	// reads; overhead doesn't pay off") doesn't get accidentally
	// reverted.
	p, _ := taskclass.Resolve("chat")
	if p.UseAgenticSmallModel {
		t.Errorf("chat.UseAgenticSmallModel should be false")
	}
}
