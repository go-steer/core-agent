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

package usage

import "testing"

func TestContextWindowSizeFor(t *testing.T) {
	t.Parallel()
	cases := []struct {
		model string
		want  int
	}{
		{"gemini-3.1-pro-preview-customtools", 1_000_000},
		{"gemini-3.5-pro", 1_000_000},
		{"gemini-3.5-flash", 1_000_000},
		{"gemini-2.5-pro", 2_000_000},
		{"gemini-2.5-flash", 1_000_000},
		{"claude-opus-4-7", 200_000},
		{"claude-opus-4-7-1m", 1_000_000},
		{"claude-sonnet-4-6", 200_000},
		{"claude-sonnet-4-6-1m", 1_000_000},
		{"claude-haiku-4-5-20251001", 200_000},
		{"claude-3-opus", 200_000},
		// Unknown models map to 0 so consumers can treat as "skip".
		{"some-future-llm-7b", 0},
		{"", 0},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			t.Parallel()
			if got := ContextWindowSizeFor(tc.model); got != tc.want {
				t.Errorf("ContextWindowSizeFor(%q) = %d, want %d", tc.model, got, tc.want)
			}
		})
	}
}

func TestTracker_ContextWindow_EmptyIsZero(t *testing.T) {
	t.Parallel()
	tr := NewTracker()
	if got := tr.ContextWindowSize(); got != 0 {
		t.Errorf("ContextWindowSize() with no turns = %d, want 0", got)
	}
	if got := tr.ContextWindowUsed(); got != 0 {
		t.Errorf("ContextWindowUsed() with no turns = %d, want 0", got)
	}
}

func TestTracker_ContextWindow_TracksLastTurn(t *testing.T) {
	t.Parallel()
	tr := NewTracker()
	tr.Append("gemini-3.5-flash", 12345, 678, Pricing{})
	if got := tr.ContextWindowSize(); got != 1_000_000 {
		t.Errorf("ContextWindowSize() = %d, want 1_000_000 (Gemini Flash)", got)
	}
	if got := tr.ContextWindowUsed(); got != 12345 {
		t.Errorf("ContextWindowUsed() = %d, want 12345 (last turn's input)", got)
	}
	// Newer turn updates both — even if the model changed mid-session.
	tr.Append("claude-opus-4-7", 50000, 100, Pricing{})
	if got := tr.ContextWindowSize(); got != 200_000 {
		t.Errorf("ContextWindowSize() after model switch = %d, want 200_000 (Claude Opus)", got)
	}
	if got := tr.ContextWindowUsed(); got != 50000 {
		t.Errorf("ContextWindowUsed() after model switch = %d, want 50000", got)
	}
}
