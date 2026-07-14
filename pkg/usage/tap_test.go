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

import (
	"testing"

	"google.golang.org/genai"
)

func TestTurnUsageFromGenaiMetadata_FullBreakdown(t *testing.T) {
	t.Parallel()
	got := TurnUsageFromGenaiMetadata(&genai.GenerateContentResponseUsageMetadata{
		PromptTokenCount:        18_250,
		CachedContentTokenCount: 31_892,
		CandidatesTokenCount:    40,
		ThoughtsTokenCount:      120,
		ToolUsePromptTokenCount: 55,
	})
	want := TurnUsage{
		InputTokens:       18_250,
		CachedInputTokens: 31_892,
		OutputTokens:      40,
		ThoughtsTokens:    120,
		ToolUseTokens:     55,
	}
	if got != want {
		t.Errorf("TurnUsageFromGenaiMetadata = %+v, want %+v", got, want)
	}
	// Note: the cached > prompt case here is deliberately preserved by
	// the extractor. Clamping happens in Tracker.AppendUsage so raw
	// per-event snapshots stay observable for debugging.
}
