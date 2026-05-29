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

import "strings"

// ContextWindowSize returns the model's max input window from a
// hardcoded table, keyed on the most recent turn's model name. Returns
// 0 for unknown models (or when no turn has landed yet) — consumers
// should treat 0 as "unknown; suppress any per-context UI segment and
// skip threshold-based behaviors like compaction." See
// contextWindowSizeFor for the lookup table.
//
// Lifted from cmd/core-agent/coretui_enabled.go where it was first
// implemented as part of the core-tui adapter tier-3+ work
// (commit be8dae5). Agent-level code (compaction trigger,
// micro-subagents) needs the same accessor, so it lives on the
// substrate type rather than the adapter bridge.
func (t *Tracker) ContextWindowSize() int {
	last, ok := t.Last()
	if !ok {
		return 0
	}
	return contextWindowSizeFor(last.Model)
}

// ContextWindowUsed approximates the current context fill as the
// most recent turn's input-token count. Each turn re-sends the full
// conversation, so the input count is the rolling context size.
// Returns 0 before any turn has landed (matches "unknown" semantics
// — consumers should suppress the segment).
func (t *Tracker) ContextWindowUsed() int {
	last, ok := t.Last()
	if !ok {
		return 0
	}
	return last.InputTokens
}

// contextWindowSizeFor returns the configured max input window for
// model. Hardcoded table; bump when new models land. Unknown models
// return 0. Substring match — model IDs come in many flavors
// ("gemini-3.1-pro-preview-customtools", "claude-sonnet-4-6-1m", …)
// and we want the limit to land regardless of suffix.
//
// Exported as a package-level function so callers that have a model
// name in hand (without going through the Tracker) can resolve it
// directly. The Tracker methods above are the common path.
func ContextWindowSizeFor(model string) int { return contextWindowSizeFor(model) }

func contextWindowSizeFor(model string) int {
	switch {
	case containsAny(model, "gemini-3.1-pro", "gemini-3.5-pro", "gemini-3-pro"):
		return 1_000_000
	case containsAny(model, "gemini-3.5-flash", "gemini-3-flash", "gemini-3.1-flash"):
		return 1_000_000
	case containsAny(model, "gemini-2.5-pro"):
		return 2_000_000
	case containsAny(model, "gemini-2.5-flash", "gemini-2.0-flash"):
		return 1_000_000
	case containsAny(model, "claude-opus-4", "claude-sonnet-4", "claude-haiku-4"):
		// Claude 4.x family: 200K base, 1M tier when the "-1m" suffix
		// is set. Honor the suffix when present.
		if containsAny(model, "-1m") {
			return 1_000_000
		}
		return 200_000
	case containsAny(model, "claude"):
		return 200_000
	}
	return 0
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
