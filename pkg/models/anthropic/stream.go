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
	"encoding/json"

	"github.com/anthropics/anthropic-sdk-go"
	"google.golang.org/genai"
)

// finalResponseFromMessage builds the terminal LLMResponse from a fully-
// accumulated Anthropic Message. Tool-use blocks are surfaced as
// FunctionCall parts so the ADK runner can dispatch them.
//
// Only tests call it today — llm.go assembles the terminal response
// inline because the pause_turn loop concatenates parts across several
// Messages. Any caller that revives it for a real yield must also stamp
// cacheCreationMetadata(msg.Usage) onto CustomMetadata; the three
// buckets returned here can't carry the cache-write count, and dropping
// it silently bills written tokens at the base input rate (#263).
func finalResponseFromMessage(msg *anthropic.Message) (*genai.Content, genai.FinishReason, *genai.GenerateContentResponseUsageMetadata) {
	content := &genai.Content{Role: genai.RoleModel, Parts: contentPartsFromMessage(msg)}
	return content, mapStopReason(msg.StopReason), usageMetadata(msg.Usage)
}

// contentPartsFromMessage converts one accumulated Message's content
// blocks into genai Parts. The pause_turn continuation loop in llm.go
// calls this once per continuation request and concatenates the
// results, so the terminal response carries every surfaced block from
// the whole (possibly multi-request) assistant turn.
func contentPartsFromMessage(msg *anthropic.Message) []*genai.Part {
	var parts []*genai.Part

	for _, block := range msg.Content {
		switch v := block.AsAny().(type) {
		case anthropic.TextBlock:
			if v.Text != "" {
				parts = append(parts, &genai.Part{Text: v.Text})
			}
		case anthropic.ThinkingBlock:
			// Thinking blocks must survive the genai round-trip: on
			// thinking-default models (claude-sonnet-5 / opus-5 /
			// fable-5) the API requires the assistant turn preceding
			// a tool_result to replay its thinking blocks, signature
			// intact — dropping them 400s the second request of every
			// tool loop (#357). genai.Part carries them natively as
			// Thought + ThoughtSignature.
			parts = append(parts, &genai.Part{
				Text:             v.Thinking,
				Thought:          true,
				ThoughtSignature: []byte(v.Signature),
			})
		case anthropic.RedactedThinkingBlock:
			// Redacted thinking is an opaque encrypted payload that
			// must be echoed back verbatim. genai has no dedicated
			// field, so the payload rides in ThoughtSignature behind
			// a marker prefix; partsToBlocks peels it back into a
			// redacted_thinking block on the way out.
			parts = append(parts, &genai.Part{
				Thought:          true,
				ThoughtSignature: []byte(redactedThinkingPrefix + v.Data),
			})
		case anthropic.ToolUseBlock:
			args, _ := decodeArgs(v.Input)
			parts = append(parts, &genai.Part{
				FunctionCall: &genai.FunctionCall{
					ID:   v.ID,
					Name: v.Name,
					Args: args,
				},
			})
		case anthropic.ServerToolUseBlock, anthropic.WebSearchToolResultBlock:
			// Server-side tool blocks (WithWebSearch): deliberately NOT
			// surfaced. genai.Part has no slot for them, and mapping
			// server_tool_use to a FunctionCall part would make the ADK
			// runner try to dispatch a tool that only exists on
			// Anthropic's servers. The web_search_tool_result payload is
			// mostly encrypted_content the client can't read; the
			// model's own text blocks already carry the answer derived
			// from the results. Skipping is also round-trip safe:
			//   - within a pause_turn continuation the paused turn is
			//     replayed verbatim via Message.ToParam() in llm.go, so
			//     these blocks survive where the API requires them;
			//   - for HISTORY replay across turns (partsToBlocks never
			//     sees them, so they're absent from later requests) the
			//     API tolerates completed assistant turns missing their
			//     server tool blocks — only a paused turn must be
			//     replayed exactly.
		}
	}
	return parts
}

// usageMetadata maps an Anthropic Usage (possibly summed across the
// requests of a pause_turn continuation loop — see addUsage) onto
// genai's UsageMetadata shape.
func usageMetadata(u anthropic.Usage) *genai.GenerateContentResponseUsageMetadata {
	// Token counts come from the SDK as int64; genai's metadata type
	// uses int32. Realistic token counts (under ~2B) fit comfortably,
	// so the narrowing is safe.
	//
	// Anthropic reports three mutually-exclusive input buckets:
	//   - Usage.InputTokens               — fresh input, billed at 1× input rate
	//   - Usage.CacheReadInputTokens      — served from cache, ~10% of input rate
	//   - Usage.CacheCreationInputTokens  — created cache entries, ~125% of input rate
	//
	// We fold ALL three into PromptTokenCount so it matches Gemini's
	// "total effective prompt size" semantics (the genai SDK docstring
	// says this field is the whole prompt including cached content).
	// CachedContentTokenCount carries just the cache_read subset,
	// letting /usage's input_tokens_cached / cost_usd_uncached_reference
	// render Anthropic cache savings the same way Gemini's do.
	//
	// genai's shape has no third bucket, so the cache_creation count
	// rides out-of-band on the response's CustomMetadata — see
	// cacheCreationMetadata. Without it the tracker folds written
	// tokens into uncached input and undercounts every cache-warming
	// turn by the write premium (#263).
	totalInput := u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
	return &genai.GenerateContentResponseUsageMetadata{
		PromptTokenCount:        int32(totalInput),                  // #nosec G115 -- token counts won't overflow int32
		CachedContentTokenCount: int32(u.CacheReadInputTokens),      // #nosec G115 -- token counts won't overflow int32
		CandidatesTokenCount:    int32(u.OutputTokens),              // #nosec G115 -- token counts won't overflow int32
		TotalTokenCount:         int32(totalInput + u.OutputTokens), // #nosec G115 -- token counts won't overflow int32
	}
}

// cacheCreationMetadata returns the CustomMetadata sidecar carrying the
// turn's cache_creation_input_tokens, or nil when the turn wrote no
// cache entries (the overwhelmingly common case — don't stamp an
// all-zero map onto every event).
//
// usage.TurnUsageFromMetadata reads this back to bill the write bucket
// at its own premium rate. CustomMetadata is used because it is the one
// per-event field that survives ADK's persist round-trip, so a session
// rebuilt from the eventlog reconstructs the same cost the live run
// reported.
//
// The key is spelled literally rather than imported so a model provider
// doesn't take a build dependency on the accounting layer for one
// string; TestCacheCreationMetadata_Shape asserts it against
// usage.CacheCreationTokensMetadataKey, so a rename on either side is a
// test failure, not a silent revert to the undercount.
func cacheCreationMetadata(u anthropic.Usage) map[string]any {
	if u.CacheCreationInputTokens <= 0 {
		return nil
	}
	return map[string]any{
		cacheCreationTokensMetadataKey: u.CacheCreationInputTokens,
	}
}

// cacheCreationTokensMetadataKey mirrors usage.CacheCreationTokensMetadataKey.
const cacheCreationTokensMetadataKey = "cache_creation_input_tokens"

// addUsage folds one request's usage buckets into a running total so
// the terminal UsageMetadata of a pause_turn continuation loop reflects
// the spend of every request in the turn. Only the four token buckets
// usageMetadata reads are summed — the fold stays consistent with the
// three-input-bucket mapping documented above.
func addUsage(dst *anthropic.Usage, src anthropic.Usage) {
	dst.InputTokens += src.InputTokens
	dst.OutputTokens += src.OutputTokens
	dst.CacheReadInputTokens += src.CacheReadInputTokens
	dst.CacheCreationInputTokens += src.CacheCreationInputTokens
}

// decodeArgs unmarshals Anthropic's tool-input JSON into the
// map[string]any genai expects on FunctionCall.Args.
func decodeArgs(raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return map[string]any{}, err
	}
	if out == nil {
		out = map[string]any{}
	}
	return out, nil
}

// mapStopReason translates Anthropic's StopReason to genai's
// FinishReason. The mappings follow the table in core-agent's design
// notes; unknown values fall through to FinishReasonOther.
func mapStopReason(r anthropic.StopReason) genai.FinishReason {
	switch r {
	case anthropic.StopReasonEndTurn, anthropic.StopReasonStopSequence, anthropic.StopReasonToolUse:
		return genai.FinishReasonStop
	case anthropic.StopReasonMaxTokens:
		return genai.FinishReasonMaxTokens
	case anthropic.StopReasonRefusal:
		return genai.FinishReasonSafety
	case anthropic.StopReasonPauseTurn:
		return genai.FinishReasonOther
	}
	if r == "" {
		return genai.FinishReasonUnspecified
	}
	return genai.FinishReasonOther
}
