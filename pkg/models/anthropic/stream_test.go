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

package anthropic

import (
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

func TestFinalResponseFromMessage_UsageIncludesCacheReadAsCachedContent(t *testing.T) {
	t.Parallel()
	// Anthropic reports three input buckets. Confirm the mapping to
	// the genai UsageMetadata shape:
	//   PromptTokenCount        = input + cache_read + cache_creation  (total effective prompt)
	//   CachedContentTokenCount = cache_read                           (subset from cache)
	// This mirrors Gemini's semantics so /usage's input_tokens_cached
	// and cost_usd_uncached_reference render Anthropic sessions the
	// same way Gemini's do.
	msg := &anthropic.Message{
		StopReason: anthropic.StopReasonEndTurn,
		Usage: anthropic.Usage{
			InputTokens:              5000,  // fresh input
			CacheReadInputTokens:     15000, // served from cache (10% rate)
			CacheCreationInputTokens: 2000,  // wrote new cache entries (125% rate)
			OutputTokens:             500,
		},
	}
	_, _, meta := finalResponseFromMessage(msg)
	if meta == nil {
		t.Fatal("expected UsageMetadata")
	}
	// PromptTokenCount is the SUM of all three input buckets — matches
	// Gemini's "total effective prompt size" semantics.
	wantPrompt := int32(5000 + 15000 + 2000)
	if meta.PromptTokenCount != wantPrompt {
		t.Errorf("PromptTokenCount = %d, want %d (input+cache_read+cache_creation)",
			meta.PromptTokenCount, wantPrompt)
	}
	// CachedContentTokenCount is JUST the cache_read subset — what
	// downstream cost math applies the discounted rate to.
	if meta.CachedContentTokenCount != 15000 {
		t.Errorf("CachedContentTokenCount = %d, want 15000 (cache_read only)",
			meta.CachedContentTokenCount)
	}
	if meta.CandidatesTokenCount != 500 {
		t.Errorf("CandidatesTokenCount = %d, want 500", meta.CandidatesTokenCount)
	}
	wantTotal := wantPrompt + 500
	if meta.TotalTokenCount != wantTotal {
		t.Errorf("TotalTokenCount = %d, want %d", meta.TotalTokenCount, wantTotal)
	}
}

func TestFinalResponseFromMessage_NoCacheTokensDegradesCleanly(t *testing.T) {
	t.Parallel()
	// The pre-cache-wiring code path — messages without cache tokens
	// should produce the same PromptTokenCount as before (just
	// InputTokens). Regression guard for the additive change.
	msg := &anthropic.Message{
		StopReason: anthropic.StopReasonEndTurn,
		Usage: anthropic.Usage{
			InputTokens:  1000,
			OutputTokens: 50,
			// cache fields zero
		},
	}
	_, _, meta := finalResponseFromMessage(msg)
	if meta.PromptTokenCount != 1000 {
		t.Errorf("PromptTokenCount = %d, want 1000 (input only when no cache tokens)",
			meta.PromptTokenCount)
	}
	if meta.CachedContentTokenCount != 0 {
		t.Errorf("CachedContentTokenCount = %d, want 0", meta.CachedContentTokenCount)
	}
	if meta.TotalTokenCount != 1050 {
		t.Errorf("TotalTokenCount = %d, want 1050", meta.TotalTokenCount)
	}
}
