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

// Package modeltier classifies LLM model IDs into capability tiers
// (frontier / mid / small) used to tune behavior whose right setting
// depends on how powerfully the model reasons.
//
// First consumer: the compactor (pkg/agent). A single
// utilization-threshold (0.85) is fine for frontier models — they
// reason coherently with 850k tokens of context. Small models
// degrade much earlier (Flash gets unreliable somewhere in the
// 200-300k range on a 1M window), so the compaction trigger needs
// to fire well before 85% to keep the session functional.
//
// Future consumers: anything that wants a tier label (per-tier
// budget caps, per-tier loop-detection sensitivity, per-tier UI
// hints in `/stats`). Add lookups here so the model→tier table
// stays in one place.
//
// Classification is by substring match against the model ID, same
// approach as pkg/usage/context_window.go's window-size table.
// Unknown models classify as "" — callers should treat the empty
// string as "skip the tier-specific behavior" rather than guess.
//
// Maintenance: when a new model ships, add it to one of the case
// branches in Classify. Substring patterns let the lookup land
// regardless of date suffix (`-20251001`, `@20251101`, etc.).
package modeltier

import "strings"

// Tier labels. Use these constants rather than string literals so
// future tier renames are mechanically findable.
const (
	// TierFrontier covers the most capable models in each provider's
	// lineup — Opus, Pro at the latest generation. They reason
	// coherently with most of their context window full.
	TierFrontier = "frontier"

	// TierMid covers mid-class models — Sonnet, older Pro, GPT-4.1-ish.
	// Better than small at deep reasoning, but degrade past ~60% context
	// utilization.
	TierMid = "mid"

	// TierSmall covers the cheap-tier models — Flash, Haiku, mini.
	// Excellent for digesting tool output and short-question Q&A;
	// degrade fast past ~30% context utilization on long sessions.
	TierSmall = "small"
)

// DefaultCompactionThresholds is the per-tier compaction utilization
// table consumed by pkg/agent's DefaultCompactor. Values are
// fractions of the model's context window — compaction fires when
// `used / window >= threshold`.
//
// The numbers are starting points, not measured optima — they're
// the design doc's best-guess defaults for v2.5. Tune from
// telemetry once we have it. Frontier stays at 0.85 to match the
// historical universal default so existing operators on frontier
// models see no behavior change. Mid and small drop because
// reasoning quality on those tiers falls off well before they hit
// the 0.85 line.
func DefaultCompactionThresholds() map[string]float64 {
	return map[string]float64{
		TierFrontier: 0.85,
		TierMid:      0.65,
		TierSmall:    0.35,
	}
}

// Classify returns the tier label for modelID, or "" when the
// model isn't recognized. Substring match — model IDs come in many
// flavors (date suffixes, "-1m" capacity tags, vertex publication
// names) and we want the lookup to land regardless.
//
// The classifier is intentionally hand-maintained rather than
// derived from price metadata. Provider pricing changes
// independently from the underlying model's reasoning class, and
// the price-to-tier mapping would drift in ways that don't reflect
// what we actually care about here.
func Classify(modelID string) string {
	m := strings.ToLower(modelID)
	switch {
	// Anthropic Claude 4.x.
	case containsAny(m, "claude-opus-4"):
		return TierFrontier
	case containsAny(m, "claude-sonnet-4"):
		return TierMid
	case containsAny(m, "claude-haiku-4"):
		return TierSmall

	// Anthropic Claude 3.x (Sonnet/Haiku still in active use in
	// some setups; Opus 3 is end-of-life).
	case containsAny(m, "claude-3-5-sonnet", "claude-3-7-sonnet"):
		return TierMid
	case containsAny(m, "claude-3-5-haiku", "claude-3-haiku"):
		return TierSmall

	// Google Gemini 3.x.
	case containsAny(m, "gemini-3-pro", "gemini-3.1-pro", "gemini-3.5-pro"):
		return TierFrontier
	case containsAny(m, "gemini-3-flash", "gemini-3.1-flash", "gemini-3.5-flash"):
		return TierSmall

	// Google Gemini 2.x. The 2.5-pro / 2.0-pro line is mid-tier
	// today — capable, but Gemini 3 Pro is the current frontier.
	case containsAny(m, "gemini-2.5-pro", "gemini-2.0-pro"):
		return TierMid
	case containsAny(m, "gemini-2.5-flash", "gemini-2.0-flash"):
		return TierSmall
	}
	return ""
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
