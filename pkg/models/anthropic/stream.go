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
func finalResponseFromMessage(msg *anthropic.Message) (*genai.Content, genai.FinishReason, *genai.GenerateContentResponseUsageMetadata) {
	content := &genai.Content{Role: genai.RoleModel}

	for _, block := range msg.Content {
		switch v := block.AsAny().(type) {
		case anthropic.TextBlock:
			if v.Text != "" {
				content.Parts = append(content.Parts, &genai.Part{Text: v.Text})
			}
		case anthropic.ToolUseBlock:
			args, _ := decodeArgs(v.Input)
			content.Parts = append(content.Parts, &genai.Part{
				FunctionCall: &genai.FunctionCall{
					ID:   v.ID,
					Name: v.Name,
					Args: args,
				},
			})
		}
	}

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
	// KNOWN GAP (Slice B follow-up, tracked separately): cache_creation
	// tokens are billed at 125% of input rate but the tracker's
	// CostUSDWithCache path bills them at 1× (they fold into the
	// uncached-input bucket). Cost is UNDERCOUNTED on cache-warming
	// turns by roughly (cache_creation_tokens × input_rate × 0.25).
	// Fixing this needs a new Rates.CacheCreationInputPerMTok field, a
	// CostUSDWithCache signature bump, and a sidecar for
	// cache_creation token counts (genai UsageMetadata has no place
	// to carry them). Steady-state cache-hit turns (where
	// cache_creation == 0) are unaffected.
	totalInput := msg.Usage.InputTokens + msg.Usage.CacheReadInputTokens + msg.Usage.CacheCreationInputTokens
	return content, mapStopReason(msg.StopReason), &genai.GenerateContentResponseUsageMetadata{
		PromptTokenCount:        int32(totalInput),                          // #nosec G115 -- token counts won't overflow int32
		CachedContentTokenCount: int32(msg.Usage.CacheReadInputTokens),      // #nosec G115 -- token counts won't overflow int32
		CandidatesTokenCount:    int32(msg.Usage.OutputTokens),              // #nosec G115 -- token counts won't overflow int32
		TotalTokenCount:         int32(totalInput + msg.Usage.OutputTokens), // #nosec G115 -- token counts won't overflow int32
	}
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
