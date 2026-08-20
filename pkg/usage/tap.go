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
	"encoding/json"

	"google.golang.org/genai"
)

// CacheCreationTokensMetadataKey is the LLMResponse.CustomMetadata key
// under which a provider reports the turn's cache-WRITE token count —
// tokens billed at a premium for establishing a cache entry rather than
// at the base input rate.
//
// Why a CustomMetadata sidecar and not a UsageMetadata field: genai's
// GenerateContentResponseUsageMetadata models Gemini's two input
// buckets (total prompt + cache reads) and has nowhere to put a third.
// CustomMetadata is the one per-event map that survives ADK's persist
// round-trip, which is what lets usage.Rebuild reconstruct correct cost
// from a reloaded eventlog — pkg/eventlog piggy-backs the same
// mechanism for FinishReason, for the same reason.
//
// Written by pkg/models/anthropic (from cache_creation_input_tokens);
// read by TurnUsageFromMetadata. The value is an int64 when freshly
// stamped and a float64 or json.Number after a JSON round-trip, so
// readers must accept all three — see cacheCreationTokens.
const CacheCreationTokensMetadataKey = "cache_creation_input_tokens"

// CacheCreation1hTokensMetadataKey is the CustomMetadata key under
// which a provider reports how much of CacheCreationTokensMetadataKey
// was written at a 1-hour breakpoint TTL rather than the default
// 5-minute one — a SUBSET of that count, billed at 2x base input
// instead of 1.25x (#770).
//
// Absent for every provider that offers one cache TTL or none, and
// absent from recordings made before #770; both read as zero, which
// prices the whole write bucket at the 5-minute rate exactly as before.
//
// Written by pkg/models/anthropic (from
// usage.cache_creation.ephemeral_1h_input_tokens); read by
// TurnUsageFromMetadata. Same numeric-shape caveat as
// CacheCreationTokensMetadataKey.
const CacheCreation1hTokensMetadataKey = "cache_creation_1h_input_tokens"

// TurnUsageFromGenaiMetadata projects one genai UsageMetadata block
// into the provider-independent TurnUsage shape.
//
// PromptTokenCount is the total effective prompt size and already
// includes cache-hit tokens (see the Turn docstring in tracker.go).
// Returns a zero TurnUsage for a nil input.
//
// This signature has no access to the cache-write sidecar, so turns
// that wrote cache entries come back with CacheCreationInputTokens == 0
// and their cost is understated by the write premium. Callers holding
// an *session.Event or *model.LLMResponse should use
// TurnUsageFromMetadata and pass CustomMetadata through.
func TurnUsageFromGenaiMetadata(u *genai.GenerateContentResponseUsageMetadata) TurnUsage {
	return TurnUsageFromMetadata(u, nil)
}

// TurnUsageFromMetadata projects one turn's genai UsageMetadata plus
// the provider's CustomMetadata sidecar into TurnUsage. All tap sites
// use this so field extraction — including the cache-write bucket
// providers can only report out-of-band — stays identical: call it once
// per event with UsageMetadata != nil, overwriting the "last seen" turn
// snapshot (matching the existing lastIn/lastOut overwrite pattern).
//
// custom may be nil; the cache-write buckets then read zero, which is
// correct for every provider that doesn't bill writes separately.
func TurnUsageFromMetadata(u *genai.GenerateContentResponseUsageMetadata, custom map[string]any) TurnUsage {
	if u == nil {
		return TurnUsage{}
	}
	return TurnUsage{
		InputTokens:                int(u.PromptTokenCount),
		CachedInputTokens:          int(u.CachedContentTokenCount),
		CacheCreationInputTokens:   cacheCreationTokens(custom),
		CacheCreation1hInputTokens: cacheCreation1hTokens(custom),
		OutputTokens:               int(u.CandidatesTokenCount),
		ThoughtsTokens:             int(u.ThoughtsTokenCount),
		ToolUseTokens:              int(u.ToolUsePromptTokenCount),
	}
}

// cacheCreationTokens reads the cache-write count out of a
// CustomMetadata map. Accepts every numeric shape the value can arrive
// in: the int64 the provider stamps in-process, the float64 a Go JSON
// decode produces, and json.Number when the decoder was configured for
// it. Anything else — absent key, wrong type, negative count — reads as
// zero, which degrades to the pre-#263 accounting rather than to a
// bogus charge.
func cacheCreationTokens(custom map[string]any) int {
	return metadataTokens(custom, CacheCreationTokensMetadataKey)
}

// cacheCreation1hTokens reads the 1-hour-TTL share of the cache-write
// count. Not clamped against the total here — TurnUsage.Clamped owns
// that invariant, and applies it to every construction path rather than
// just this one.
func cacheCreation1hTokens(custom map[string]any) int {
	return metadataTokens(custom, CacheCreation1hTokensMetadataKey)
}

func metadataTokens(custom map[string]any, key string) int {
	if len(custom) == 0 {
		return 0
	}
	var n int
	switch v := custom[key].(type) {
	case int:
		n = v
	case int32:
		n = int(v)
	case int64:
		n = int(v)
	case float64:
		n = int(v)
	case json.Number:
		i, err := v.Int64()
		if err != nil {
			return 0
		}
		n = int(i)
	default:
		return 0
	}
	if n < 0 {
		return 0
	}
	return n
}
