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

// Package taskclass implements the --task flag's profile lookup —
// the operator-declared task-class story from
// docs/model-selection-design.md (issue #123).
//
// Five canonical classes (debug, implement, chat, research, review)
// each map to a Profile that wraps a model-tier hint, compaction
// threshold, agentic-tools posture, and ask-mode default. The CLI
// applies the profile to whichever flags the operator left
// unspecified — explicit flags always win.
//
// Tier classification (frontier / mid / small) shares vocabulary
// with pkg/modeltier but the resolution is per-provider here because
// we need to pick a SPECIFIC model ID (not just a class label).
// Hard-coded per-provider map for v1 per the design doc's Open
// Question 1 — pricing catalog has no tier field today, and
// inferring tier from price changes the wrong way (a price drop
// shouldn't reclassify a model).
//
// IAP / shape-of-future-work notes:
//
//   - Adding a sixth class (e.g. "monitor" for long-running
//     autonomous): add to Classes + canonical().
//   - Adding a provider (e.g. OpenAI): extend ModelForTier and
//     the per-provider switch in canonical().
//   - Tier-to-model when a new model ships: bump the per-provider
//     table here. The model-tier classifier in pkg/modeltier handles
//     the reverse (model → tier) and gets bumped separately.

package taskclass

// Canonical task-class names. Use these constants rather than string
// literals so future class renames are mechanically findable.
const (
	Debug     = "debug"
	Implement = "implement"
	Chat      = "chat"
	Research  = "research"
	Review    = "review"
)

// Tier names mirror pkg/modeltier's TierFrontier / TierMid / TierSmall —
// duplicated as constants here so taskclass can be referenced without
// pulling in modeltier when only the labels are needed. Resolution
// (which provider's model for which tier) lives in ModelForTier below.
const (
	TierFrontier = "frontier"
	TierMid      = "mid"
	TierSmall    = "small"
)

// Ask-mode aliases for the AskMode field. The CLI's --ask flag
// accepts these strings + "yolo" + "plan" + "acceptEdits"; the ones
// listed here are the only values task-class profiles actually use.
const (
	AskAuto  = "auto"
	AskAsk   = "ask"
	AskAllow = "allow"
)

// Profile is the bundle a task class maps to. Applied to whatever
// flags the operator left unspecified; explicit flags win. All
// fields are optional in the sense that an empty / zero value means
// "don't override the substrate / operator default" — the CLI's
// resolution logic walks each field independently.
type Profile struct {
	// Tier hints which model class to pick. Resolved to a specific
	// model ID per-provider via ModelForTier. Empty = don't change
	// the model.
	Tier string

	// CompactionThreshold goes into the compactor's fallback
	// Threshold field. 0 = leave the substrate default in place.
	// Note: per-tier overrides from config still win for their
	// specific tier (see compactor's resolveThreshold precedence).
	CompactionThreshold float64

	// AgenticToolsEnabled is the desired agentic-tools state. The
	// substrate already defaults to on (PR #118), so today every
	// profile sets this true and the field is mostly informational.
	// Stays as an explicit field so a future "monitor" class that
	// wants agentic-tools off can express that.
	AgenticToolsEnabled bool

	// UseAgenticSmallModel controls whether agentic subtasks route
	// through a cheap-tier model (true) or inherit the parent's
	// model (false). True for tool-heavy task classes; false for
	// chat where subtask overhead doesn't pay off.
	UseAgenticSmallModel bool

	// AskMode is the desired permissions ask-mode default. Empty =
	// don't override the operator / config setting.
	AskMode string
}

// canonical is the source-of-truth profile table. Numbers track the
// design doc (docs/model-selection-design.md §"Piece 1"). Bumping a
// threshold here changes default behavior across every consumer that
// uses --task=<that class>; do it with intent.
func canonical() map[string]Profile {
	return map[string]Profile{
		Debug: {
			Tier:                 TierFrontier,
			CompactionThreshold:  0.65,
			AgenticToolsEnabled:  true,
			UseAgenticSmallModel: true,
			AskMode:              AskAuto,
		},
		Implement: {
			Tier:                 TierFrontier,
			CompactionThreshold:  0.70,
			AgenticToolsEnabled:  true,
			UseAgenticSmallModel: true,
			AskMode:              AskAuto,
		},
		Chat: {
			Tier:                 TierMid,
			CompactionThreshold:  0.85,
			AgenticToolsEnabled:  true,
			UseAgenticSmallModel: false, // chat subtasks are usually one-shot reads; overhead doesn't pay off
			AskMode:              AskAuto,
		},
		Research: {
			Tier:                 TierMid,
			CompactionThreshold:  0.65,
			AgenticToolsEnabled:  true,
			UseAgenticSmallModel: true,
			AskMode:              AskAllow, // research is read-heavy; ask-mode noise is operator-hostile
		},
		Review: {
			Tier:                 TierFrontier,
			CompactionThreshold:  0.75,
			AgenticToolsEnabled:  true,
			UseAgenticSmallModel: true,
			AskMode:              AskAuto,
		},
	}
}

// Resolve returns the Profile for class. Empty class returns
// (Profile{}, false) — caller should not apply anything. Unknown
// class also returns (Profile{}, false); caller is expected to
// surface a useful error listing Classes().
func Resolve(class string) (Profile, bool) {
	if class == "" {
		return Profile{}, false
	}
	p, ok := canonical()[class]
	return p, ok
}

// Classes returns the canonical task-class names in a stable order
// suitable for CLI usage messages and validation errors. Order
// reflects the design doc's table layout (debug, implement, chat,
// research, review) rather than alphabetical so the most common
// operator choices appear first.
func Classes() []string {
	return []string{Debug, Implement, Chat, Research, Review}
}

// ModelForTier returns the default model ID for a (provider, tier)
// pair. Returns "" when no mapping exists — caller should fall
// through to whatever model would've been chosen without --task.
//
// Provider names match pkg/models's registration strings ("gemini",
// "vertex", "anthropic", "anthropic-vertex"). Mock providers
// (echo, scripted) don't appear here — they have no tier concept.
//
// The table embeds knowledge that also lives in pkg/modeltier's
// reverse direction (model → tier). When a new model ships, both
// need bumping. Worth a check at release time; not worth fusing
// into one table (the two directions have different shape needs:
// modeltier wants substring matching, taskclass wants
// canonical-string outputs).
func ModelForTier(provider, tier string) string {
	switch provider {
	case "gemini", "vertex":
		switch tier {
		case TierFrontier:
			return "gemini-3.5-pro"
		case TierMid:
			return "gemini-2.5-pro"
		case TierSmall:
			return "gemini-2.5-flash"
		}
	case "anthropic", "anthropic-vertex":
		switch tier {
		case TierFrontier:
			return "claude-opus-4-7"
		case TierMid:
			return "claude-sonnet-4-6"
		case TierSmall:
			return "claude-haiku-4-5"
		}
	}
	return ""
}
