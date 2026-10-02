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
	"math"
)

// SideUsageKey marks an event's CustomMetadata as one side call's usage
// (AppendSideUsage), so RebuildTrackerFromEvents can replay it after a
// restart. Without it the cost ceilings would forget that spend on a
// pod roll, which they must not (#643).
//
// The usage rides in CustomMetadata and not in the event's
// UsageMetadata on purpose: every reader of UsageMetadata — the turn
// tap, the attach and core-tui projections — takes it for a
// conversation turn.
const SideUsageKey = "side_usage"

// SideUsageMetadata is the CustomMetadata recording one side call.
func SideUsageMetadata(model string, u TurnUsage) map[string]any {
	u = u.Clamped()
	return map[string]any{
		SideUsageKey: map[string]any{
			"model":                       model,
			"input_tokens":                u.InputTokens,
			"cached_input_tokens":         u.CachedInputTokens,
			"cache_creation_input_tokens": u.CacheCreationInputTokens,
			"output_tokens":               u.OutputTokens,
			"thoughts_tokens":             u.ThoughtsTokens,
			"tool_use_tokens":             u.ToolUseTokens,
		},
	}
}

// sideUsageFrom reads SideUsageMetadata back. The numbers may come back
// from a JSON round trip as float64 or json.Number, so all three are
// accepted.
func sideUsageFrom(meta map[string]any) (model string, u TurnUsage, ok bool) {
	rec, ok := meta[SideUsageKey].(map[string]any)
	if !ok {
		return "", TurnUsage{}, false
	}
	model, _ = rec["model"].(string)
	u = TurnUsage{
		InputTokens:              intField(rec["input_tokens"]),
		CachedInputTokens:        intField(rec["cached_input_tokens"]),
		CacheCreationInputTokens: intField(rec["cache_creation_input_tokens"]),
		OutputTokens:             intField(rec["output_tokens"]),
		ThoughtsTokens:           intField(rec["thoughts_tokens"]),
		ToolUseTokens:            intField(rec["tool_use_tokens"]),
	}
	return model, u, model != ""
}

func intField(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		if math.IsNaN(n) || n < 0 || n > math.MaxInt32 {
			return 0
		}
		return int(n)
	case json.Number:
		i, err := n.Int64()
		if err != nil || i < 0 {
			return 0
		}
		return int(i)
	}
	return 0
}
