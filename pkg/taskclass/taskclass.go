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
// threshold, agentic-tools posture, ask-mode default, a set of
// built-in tools to drop, and a plan-first posture. The CLI applies
// the profile to whichever flags the operator left unspecified —
// explicit flags always win.
//
// The tool and plan-first fields (#160) are the class's opinion about
// what the model should be *able* to do, not just how it should be
// billed: the investigation-shaped classes (debug, research, review)
// drop `bash` and require a recorded plan, because that is the shape
// where the measured failures were — bash-as-grep on the first call,
// zero plan sentences before the first mutation.
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

	// DisableTools names built-in tools this class drops from the
	// default registration. Nil = register the usual suite.
	//
	// This is the profile's opinion, not the operator's: an operator
	// who explicitly disables a tool (tools.disable / --disable-tools)
	// is never overruled, and one who wants a profile-dropped tool
	// back passes --enable-tools=<name>. See resolveProfileDisables
	// in cmd/core-agent.
	//
	// Investigation-shaped classes drop `bash` because that is where
	// the measured failure lives: in the 2026-06-10 debug session the
	// model reached for `bash $ grep -rn` on call #1 with the native
	// grep tool right there in the schema. #158's search gate refuses
	// the search-shaped subset; this is the blunter version for the
	// classes where the shell earns its keep least.
	DisableTools []string

	// RequirePlanArtifact turns plan-first gating on for this class —
	// mutating tools stay denied until the model calls record_plan.
	// It maps to permissions.plan_mode: "required".
	// False = leave permissions.plan_mode as configured;
	// the profile only ever turns the gate ON, never off, since an
	// operator who put `true` in config meant it.
	//
	// Only honored when a .agents/ directory exists to persist plans
	// into: record_plan doesn't register without one (see
	// tools.Build), and a gate with no way to record a plan denies
	// every mutating call for the life of the session.
	RequirePlanArtifact bool
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
			DisableTools:         []string{"bash"},
			RequirePlanArtifact:  true,
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
			DisableTools:         []string{"bash"},
			RequirePlanArtifact:  true,
		},
		Review: {
			Tier:                 TierFrontier,
			CompactionThreshold:  0.75,
			AgenticToolsEnabled:  true,
			UseAgenticSmallModel: true,
			AskMode:              AskAuto,
			DisableTools:         []string{"bash"},
			RequirePlanArtifact:  true,
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

// Providers returns the provider names ModelForTier has tier
// mappings for. Extend together with ModelForTier's switch when a
// provider is added — consumers iterate this list to verify
// cross-table invariants for every default (pricing's
// TestBuiltin_CoversTaskclassTierDefaults walks Providers() × tiers,
// so a provider missing here silently loses that coverage).
func Providers() []string {
	return []string{"gemini", "vertex", "anthropic", "anthropic-vertex"}
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
// need bumping. Not worth fusing into one table (the two directions
// have different shape needs: modeltier wants substring matching,
// taskclass wants canonical-string outputs).
//
// POLICY: each entry names the LATEST model in its line. Picking Opus
// means picking the newest Opus, not whichever Opus was current when
// the line was last edited. That is not enforceable from LiteLLM —
// the catalog has no recency field, and auto-promoting would ship an
// un-UAT'd model to every operator on a Monday regen — so it is
// enforced instead by TestModelForTier_ReturnsLatestInLine, which
// fails the build when pricing.Builtin() contains a newer model in the
// same line than the one returned here.
func ModelForTier(provider, tier string) string {
	switch provider {
	case "gemini", "vertex":
		switch tier {
		case TierFrontier:
			// gemini-3.6-flash: the current top of the flash-first
			// agentic line. The table used to say gemini-3.5-pro — a
			// model id that never shipped (3.5 went flash-first);
			// caught by mast's first live-credential run (#530).
			return "gemini-3.6-flash"
		case TierMid:
			// gemini-3.5-flash, not the older 2.5-pro: mid-tier
			// classes (research, chat) need built-in grounding to
			// coexist with function tools, which Gemini supports only
			// from 3.0 on — on 2.5-pro the research class literally
			// could not search (#531). Also cheaper per the pricing
			// catalog, and modeltier already classifies the
			// 3.5-flash line as mid.
			return "gemini-3.5-flash"
		case TierSmall:
			// gemini-3.5-flash-lite: current-gen budget tier at the
			// same price point as the 2.5-flash it replaced
			// ($0.30/$2.50 per MTok), with far stronger agentic
			// scores (OSWorld 74.0, Terminal-bench 54.0) and a
			// March 2026 knowledge cutoff. Moves in lockstep with
			// pkg/models/gemini's DefaultSmallModelID — pinned by
			// TestModelForTier_ConsistentWithSmallModelDefaulters.
			return "gemini-3.5-flash-lite"
		}
	case "anthropic", "anthropic-vertex":
		switch tier {
		case TierFrontier:
			// Latest in the Opus line. Deliberately Opus and not the
			// Mythos-class tier (claude-fable-5 / claude-mythos-5),
			// which sits above Opus at 2x the rate — "frontier" is the
			// top of the general-purpose line, not the most expensive
			// model on offer.
			return "claude-opus-5"
		case TierMid:
			return "claude-sonnet-5"
		case TierSmall:
			// claude-haiku-4-5 is still the latest Haiku — no
			// 5-generation Haiku has shipped. Moves in lockstep with
			// pkg/models/anthropic's DefaultSmallModelID; pinned by
			// TestModelForTier_ConsistentWithSmallModelDefaulters.
			return "claude-haiku-4-5"
		}
	}
	return ""
}
