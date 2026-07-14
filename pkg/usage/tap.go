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
	"google.golang.org/genai"
)

// TurnUsageFromGenaiMetadata projects one genai UsageMetadata block
// into the provider-independent TurnUsage shape. All Gemini/Vertex tap
// sites use this to get identical field extraction — call it once per
// event with UsageMetadata != nil, overwriting the "last seen" turn
// snapshot (matching the existing lastIn/lastOut overwrite pattern).
//
// PromptTokenCount is the total effective prompt size and already
// includes cache-hit tokens (see the Turn docstring in tracker.go).
// Returns a zero TurnUsage for a nil input.
func TurnUsageFromGenaiMetadata(u *genai.GenerateContentResponseUsageMetadata) TurnUsage {
	if u == nil {
		return TurnUsage{}
	}
	return TurnUsage{
		InputTokens:       int(u.PromptTokenCount),
		CachedInputTokens: int(u.CachedContentTokenCount),
		OutputTokens:      int(u.CandidatesTokenCount),
		ThoughtsTokens:    int(u.ThoughtsTokenCount),
		ToolUseTokens:     int(u.ToolUsePromptTokenCount),
	}
}
