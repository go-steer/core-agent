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

// Package config defines the on-disk schema for `.agents/config.json` and
// the rules for discovering, parsing, and merging it with built-in defaults.
//
// A minimal config.json only needs to set what the consumer wants to override;
// all other fields fall back to DefaultConfig().
package config

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/go-steer/core-agent/v2/pkg/hooks"
)

// SchemaVersion is the current major version of the on-disk config format.
// Bump when making a breaking change; older versions are rejected at load
// time with a clear error suggesting the upgrade path.
const SchemaVersion = 1

// Config is the in-memory representation of `.agents/config.json`.
//
// All sub-sections except Model have sensible zero-valued defaults, so a
// minimal `config.json` only needs to set what the user wants to override.
type Config struct {
	Version     int               `json:"version"`
	Model       ModelConfig       `json:"model"`
	Permissions PermissionsConfig `json:"permissions,omitempty"`
	PathScope   PathScopeConfig   `json:"path_scope,omitempty"`
	Agent       AgentConfig       `json:"agent,omitempty"`
	ToolOutput  ToolOutputConfig  `json:"tool_output,omitempty"`
	Tools       ToolsConfig       `json:"tools,omitempty"`
	Subagents   []SubagentSpec    `json:"subagents,omitempty"`
	Hooks       hooks.Config      `json:"hooks,omitempty"`
	Mock        MockConfig        `json:"mock,omitempty"`
	OTEL        OTELConfig        `json:"otel,omitempty"`
	URLScope    URLScopeConfig    `json:"url_scope,omitempty"`
	Alerts      AlertsConfig      `json:"alerts,omitempty"`
	Attach      AttachConfig      `json:"attach,omitempty"`
	Pricing     PricingFileConfig `json:"pricing,omitempty"`
	UI          UIConfig          `json:"ui,omitempty"`
	Compaction  CompactionConfig  `json:"compaction,omitempty"`
	Session     SessionConfig     `json:"session,omitempty"`
	Safety      SafetyConfig      `json:"safety,omitempty"`

	// ContentRoots are operator-declared external directories trusted as
	// additional instruction/skill scopes, so an unmodified external agent
	// tree (e.g. a kube-agents checkout) can be consumed without vendoring a
	// copy. Paths are resolved relative to the agents dir. Each root is loaded
	// as its own trusted scope: instruction @include stays confined within the
	// root, and skills compose at precedence project > content_roots (listed
	// order) > home-agents > user. Empty = only the project root is trusted
	// (today's behavior exactly). CLI: --agents-content-dir (repeatable),
	// merged with this field. See docs/external-content-root-design.md.
	ContentRoots []string `json:"content_roots,omitempty"`
}

// SafetyConfig carries operator-facing safety guardrails — things
// that are NOT permission gates (those live in PermissionsConfig)
// but rather "the operator probably misconfigured something" checks
// and runaway backstops: the small-tier-parent guard (#121) and the
// behavioral watchdog mode (#660).
type SafetyConfig struct {
	// SmallTierParent controls what happens when an interactive
	// session starts on a small-tier parent model (Flash/Haiku-class).
	// These models work well as agentic_* subtask workers (#118-122)
	// but loop and stall as the parent for long interactive sessions
	// — see #121 for the smoke that motivated this guard.
	//
	// Values: "warn" (default) logs a one-line operator notice but
	// proceeds; "refuse" exits with a config-error code; "allow"
	// suppresses the check entirely. Empty == "warn".
	//
	// The check is skipped regardless when:
	//   - `-p` one-shot mode (operator knows what they're doing;
	//     might be a script invoking Flash on purpose)
	//   - `--yolo` (trust-the-operator mode)
	//   - The parent's tier doesn't classify (unknown model)
	//
	// CLI override: --small-tier-parent=warn|refuse|allow.
	SmallTierParent string `json:"small_tier_parent,omitempty"`

	// Watchdog selects the behavioral watchdog's posture — the
	// runaway-tool-loop backstop from #123/#623.
	//
	// Values: "off" (no observation), "warn" (observe the tool-call
	// stream and log structured alerts, but never halt), "enforce"
	// (same detection plus a turn-error of kind=watchdog; the agent
	// refuses new turns until Agent.ResetWatchdog clears it).
	//
	// Empty == unset, which resolves to a *mode-dependent* default:
	// "enforce" for unattended runs (-p one-shot, --no-repl daemon,
	// or a non-TTY stdin) and "warn" for interactive REPL/TUI runs.
	// An unattended daemon has no operator watching the alert stream,
	// so observe-and-log is not a backstop there (#642).
	//
	// This field exists so a recipe can ship its own backstop instead
	// of relying on every invocation and deploy manifest remembering
	// --watchdog=enforce (#660).
	//
	// CLI override: --watchdog=off|warn|enforce.
	Watchdog string `json:"watchdog,omitempty"`
}

// SessionConfig carries per-session presets — currently just the
// operator-declared task class (#123). CLI flag --task overrides
// this field; both default to unset, which leaves the substrate
// defaults in place.
type SessionConfig struct {
	// TaskClass is the operator-declared task class. Must be one
	// of pkg/taskclass.Classes() ("debug" | "implement" | "chat"
	// | "research" | "review") or empty. When set, the CLI applies
	// the matching Profile to whichever flags the operator left
	// unspecified (--model, --ask, compaction threshold, etc.).
	// Explicit CLI flags always win over the task profile.
	//
	// Useful for project-local defaults — e.g. an infra repo's
	// .agents/config.json sets "debug" because debugging is what
	// happens there; operators get the right defaults without
	// having to remember --task=debug on every invocation.
	TaskClass string `json:"task_class,omitempty"`
}

// CompactionConfig configures the automatic context-window compaction
// trigger. Both fields are optional — leave empty for the substrate
// defaults (per-tier thresholds from pkg/modeltier).
type CompactionConfig struct {
	// Threshold overrides the fallback utilization threshold used
	// when the current model's tier isn't classified or isn't in
	// ThresholdByTier. Pointer so absence is distinguishable from
	// the deliberate value 0 (which would disable compaction).
	// Must be in (0, 1) when set.
	Threshold *float64 `json:"threshold,omitempty"`

	// ThresholdByTier overrides per-tier defaults. Keys are tier
	// labels from pkg/modeltier ("frontier", "mid", "small"). Set
	// only the tiers you want to override; the rest take their
	// package defaults (0.85 / 0.65 / 0.35). Values must be in
	// (0, 1).
	//
	// Example — keep frontier sessions on the historical default
	// while compacting Flash/Haiku much earlier:
	//   "compaction": {
	//     "threshold_by_tier": { "small": 0.30 }
	//   }
	ThresholdByTier map[string]float64 `json:"threshold_by_tier,omitempty"`
}

// UIConfig holds presentation choices for the in-process TUI
// (both internal/tui and the core-tui adapter). Both fields are
// optional with sensible defaults — operators only need to set
// what they want to override.
type UIConfig struct {
	// Theme picks the rendering style for the core-tui surface.
	// Three reserved buckets:
	//   - "auto"  (default) — detect via terminal background query.
	//   - "dark"            — force dark theme; skips the OSC-11 query.
	//   - "light"           — force light theme; skips the OSC-11 query.
	// Any other lowercase identifier (letters, digits, dash,
	// underscore) is treated as a named theme from core-tui's
	// BuiltinThemes registry (e.g. "gopher", "google"). The /theme
	// picker writes back through PersistThemeChoice using these
	// names, so the field round-trips picker choices. Unknown
	// names fall back to the auto path at launch.
	Theme string `json:"theme,omitempty"`

	// Mouse enables terminal mouse capture so the wheel scrolls the
	// chat viewport. When enabled, plain click-drag no longer selects
	// text — terminals route around the capture when Shift is held
	// (Shift-drag to select, copy as usual). Pointer so unset means
	// "use the default" (true). Toggle at runtime with /mouse.
	Mouse *bool `json:"mouse,omitempty"`
}

// MouseEnabled reports whether mouse capture should be on at
// startup. Defaults to true when the field is unset.
func (u UIConfig) MouseEnabled() bool {
	if u.Mouse == nil {
		return true
	}
	return *u.Mouse
}

// Theme constants for UIConfig.Theme. Reserved buckets;
// any other lowercase identifier accepted by validateUI is
// passed through to core-tui as a named-theme lookup.
const (
	ThemeAuto  = "auto"
	ThemeDark  = "dark"
	ThemeLight = "light"
)

// PricingFileConfig governs the pricing-catalog refresh behavior —
// distinct from ModelConfig.Pricing (which is the per-model rate
// override map). Defaults: refresh enabled, daily cadence, LiteLLM
// upstream. See pkg/pricing and docs/pricing-design.md.
type PricingFileConfig struct {
	// Refresh enables the daily background fetch from Source into
	// ~/.core-agent/pricing.json's external section. Defaults to
	// true (most operators want fresh rates). Disable for
	// air-gapped pods or CI where outbound network is blocked or
	// undesirable.
	//
	// Pointer so the JSON unmarshaler can distinguish "unset
	// (default true)" from "explicit false". A bare `null` or
	// missing field yields the default.
	Refresh *bool `json:"refresh,omitempty"`

	// Source overrides the upstream URL the refresher fetches from.
	// Empty defaults to pricing.DefaultRefreshSource (LiteLLM's
	// model_prices_and_context_window.json). Override for mirrors
	// or internal pricing services.
	Source string `json:"source,omitempty"`
}

// PathScopeConfig holds extra paths that file tools may read/write
// outside the default project + user-home scope. Patterns may be
// exact paths or directory globs (terminating "/...") and are
// typically appended via the "Always allow this path/tree" prompt
// path.
//
// Two shapes coexist:
//   - Allow: legacy untyped list; each entry implicitly grants
//     both read and write so behavior matches what existed before
//     the access-level work landed.
//   - AllowPaths: typed entries with per-path access spec
//     ("r" / "w" / "rw"). New configurations should prefer this
//     form — it lets the operator say "agent may read this tree
//     but writes still prompt", which the legacy list can't
//     express.
type PathScopeConfig struct {
	Allow      []string              `json:"allow,omitempty"`
	AllowPaths []PathScopeAllowEntry `json:"allow_paths,omitempty"`
}

// PathScopeAllowEntry is one typed allow-list entry. Access is one
// of "r" / "w" / "rw" (long forms "read" / "write" / "readwrite"
// also accepted); empty Access fails validation rather than
// silently broadening to rw. Path uses the same matching rules as
// Allow: exact path, "/.../" subtree, or filepath.Match glob.
type PathScopeAllowEntry struct {
	Path   string `json:"path"`
	Access string `json:"access"`
}

// ModelConfig selects the LLM provider and model.
//
// Provider: one of "gemini", "vertex", "anthropic". When empty, the resolver
// auto-detects from the environment (see models.Resolve).
// Name: a model ID, e.g. "gemini-3.6-flash" or "claude-opus-4-7".
// APIKey: optional inline key for Provider="gemini"; usually unset and
// read from GOOGLE_API_KEY at runtime.
// Vertex: required when Provider="vertex"; project + location.
// Anthropic: optional credentials for Provider="anthropic"; usually unset and
// read from ANTHROPIC_API_KEY at runtime.
type ModelConfig struct {
	Provider  string           `json:"provider,omitempty"`
	Name      string           `json:"name"`
	APIKey    string           `json:"api_key,omitempty"`
	Vertex    *VertexConfig    `json:"vertex,omitempty"`
	Anthropic *AnthropicConfig `json:"anthropic,omitempty"`
	// Pricing is a per-model rate override keyed by model name
	// (case-insensitive). Survives /model switches mid-session —
	// every model the operator routes to can carry its own rates.
	// Layered with .agents/pricing.json + ~/.core-agent/pricing.json
	// + the compiled-in fallback; see pkg/pricing for the
	// lookup chain. Previously a single *PricingConfig that matched
	// only Model.Name; PR core-agent/#NN renamed the JSON key
	// `pricing` from "{input_per_mtok, output_per_mtok}" to a map.
	Pricing PricingMap `json:"pricing,omitempty"`
}

// PricingMap is the model-keyed override map used by ModelConfig.
// Aliased so future expansions (per-context rates, cached vs
// uncached, etc.) localize to one type.
type PricingMap map[string]PricingConfig

// VertexConfig holds GCP-specific settings for the vertex provider.
type VertexConfig struct {
	Project  string `json:"project"`
	Location string `json:"location"`

	// ContextCache toggles Vertex explicit context caching for the
	// stable request prefix (system instruction + tools). When nil
	// or Enabled != false, caching is ON — the daemon creates a
	// CachedContent resource on the first turn and stamps it onto
	// every subsequent turn's GenerateContentConfig.CachedContent.
	// See docs/vertex-context-caching-design.md and
	// internal/vertexcache/manager.go.
	ContextCache *ContextCacheConfig `json:"context_cache,omitempty"`
}

// ContextCacheConfig tunes Vertex explicit context caching. All
// fields have sensible defaults — an empty struct enables caching
// with the design-doc defaults (6h TTL, 30min refresh window).
//
// Enabled is a pointer so the config surface distinguishes "unset"
// (default ON) from an explicit "off" — operators typing
// `"enabled": false` disable caching while leaving TTL/Refresh in
// place for future re-enabling. The --no-context-cache CLI flag
// takes precedence over both.
type ContextCacheConfig struct {
	// Enabled defaults to true (nil = ON). Set to false to disable
	// caching without touching the other fields.
	Enabled *bool `json:"enabled,omitempty"`
	// TTL is how long each Create/Update requests the cache live for.
	// Format: any string time.ParseDuration accepts (e.g. "6h", "30m").
	// Empty → 6h. Vertex caps at 24h.
	TTL string `json:"ttl,omitempty"`
	// Refresh triggers a background Update when time-to-expiry drops
	// below this value. Format matches TTL. Empty → 30min.
	Refresh string `json:"refresh,omitempty"`
}

// IsEnabled reports whether caching should be turned on given this
// config. Nil receiver → true (default ON when the whole block is
// absent from config.json). Set Enabled to false to disable.
func (c *ContextCacheConfig) IsEnabled() bool {
	if c == nil {
		return true
	}
	if c.Enabled == nil {
		return true
	}
	return *c.Enabled
}

// AnthropicConfig holds Claude-specific settings for the anthropic
// provider family. APIKey is used by the first-party "anthropic"
// provider (api.anthropic.com); Vertex is used by "anthropic-vertex"
// (Claude served via Google Vertex AI).
type AnthropicConfig struct {
	APIKey string        `json:"api_key,omitempty"`
	Vertex *VertexConfig `json:"vertex,omitempty"`
}

// PricingConfig overrides the built-in price table for cost estimation.
// CachedInputPerMTok is the rate for prompt-cache-hit input tokens; when
// zero, cache hits are billed at InputPerMTok (no assumed discount).
type PricingConfig struct {
	InputPerMTok       float64 `json:"input_per_mtok,omitempty"`
	CachedInputPerMTok float64 `json:"cached_input_per_mtok,omitempty"`
	OutputPerMTok      float64 `json:"output_per_mtok,omitempty"`
}

// PermissionsConfig configures the permission gate.
type PermissionsConfig struct {
	Mode  string   `json:"mode,omitempty"`  // "ask" | "allow" | "yolo" | "plan" | "acceptEdits"
	Allow []string `json:"allow,omitempty"` // pattern allowlist
	Deny  []string `json:"deny,omitempty"`  // pattern denylist

	// UseBuiltinAllow toggles core-agent's built-in conservative
	// read-only allowlist bundle. Defaults to true when nil (the
	// pointer carries an explicit "off" signal vs "unset"). false
	// drops the entire built-in bundle including any opt-ins in
	// BuiltinAllowExtras. See permissions/builtin_allow.go for the
	// bundle catalog.
	UseBuiltinAllow *bool `json:"use_builtin_allow,omitempty"`

	// BuiltinAllowExtras names additional built-in bundles to merge
	// on top of read_only when UseBuiltinAllow is on. Unknown names
	// fail at config-validation time rather than silently dropping
	// permissions. Known bundles: see permissions.KnownBundles().
	BuiltinAllowExtras []string `json:"builtin_allow_extras,omitempty"`

	// RequirePlanArtifact enables the plan-first gating pre-check:
	// mutating tool calls (write/edit/delete/bash, spawn family,
	// MCP tools) are denied until the model calls the record_plan
	// tool. Read-only tools and record_plan itself remain allowed
	// so research happens normally. Composes with every Mode —
	// even ModeYolo denies before a plan is recorded; once
	// recorded, the mode's usual semantics resume.
	// See docs/plan-first-design.md.
	RequirePlanArtifact bool `json:"require_plan_artifact,omitempty"`
}

// AgentConfig tunes runtime agent behavior.
type AgentConfig struct {
	MaxSteps int `json:"max_steps,omitempty"`

	// AppendSystemPrompt is layer-5 operator text appended to the
	// assembled system prompt (agent.WithExtraInstruction) — the
	// documented, encouraged customization path: the harness
	// contract and mode overlay stay intact underneath. Mirrored by
	// the --append-system-prompt flag (flag beats config). #459.
	AppendSystemPrompt string `json:"append_system_prompt,omitempty"`

	// SystemPromptFile names a file whose contents REPLACE the
	// assembled system prompt wholesale (agent.WithInstruction).
	// You lose the harness contract — compaction summaries arrive
	// unexplained and tool-use degradation is on you; prefer
	// AppendSystemPrompt. Mirrored by --system-prompt-file (flag
	// beats config). #459.
	SystemPromptFile string `json:"system_prompt_file,omitempty"`

	// MaxTurnCostUSD caps a single conversation turn's cumulative
	// spend. When the post-turn hook detects spend ≥ this value, the
	// agent emits a structured turn-error (kind=cost_ceiling) and
	// refuses new turns until the operator clears the flag via
	// Agent.ResetCostCeiling. Pointer so unset is distinguishable
	// from the deliberate 0 (which would mean "no budget — refuse
	// every turn", which we treat as "disabled"). 0 or negative ==
	// disabled. Defense against the read-file-loop class of bug
	// (#144) within a single turn.
	MaxTurnCostUSD *float64 `json:"max_turn_cost_usd,omitempty"`

	// MaxSessionCostUSD caps the session's cumulative spend across
	// all turns (parent + subtask). Tripped → same behavior as
	// MaxTurnCostUSD. Useful for long-running autonomous deploys
	// where individual turns are reasonable but the session adds up.
	MaxSessionCostUSD *float64 `json:"max_session_cost_usd,omitempty"`

	// AutoContinue controls continuing restart-interrupted turns
	// automatically (#539, #559, docs/auto-continue-design.md). Nil/absent
	// leaves the feature at its precondition-gated default (on for a
	// multi-session or --no-repl daemon with a durable eventlog, off
	// elsewhere — see AutoContinueConfig.Enabled). A session interrupted
	// mid-turn by a daemon restart resumes with intact history; whether it
	// then finishes the turn or waits for the next message is what this
	// controls. Applies to lazily-resumed multi-session agents; autonomous
	// runs have their own checkpoint/resume machinery.
	AutoContinue *AutoContinueConfig `json:"auto_continue,omitempty"`

	// DisplayName overrides the brand line at the top of the TUI. By
	// default the TUI shows the AppName (e.g. "core-agent"); set this
	// to give the agent a human-friendly identity ("Triage Bot",
	// "Code Reviewer", etc.). Empty falls back to AppName.
	DisplayName string `json:"display_name,omitempty"`

	// Description is a one-line summary of what this agent does.
	// Used in two places: (1) ADK's llmagent.Config.Description, which
	// becomes part of the system prompt ("you are an agent named X,
	// description: ..."), and (2) the /.well-known/agent-card.json
	// `description` field if the card endpoint is enabled. Set once,
	// fanned out to both. Empty = no description in the system prompt
	// and the card endpoint stays off unless --agent-card-description
	// overrides.
	Description string `json:"description,omitempty"`
}

// AutoContinueConfig tunes opt-in continuation of restart-interrupted
// turns (#539). See docs/auto-continue-design.md for the full
// semantics, including the crash-loop breaker that ships with the
// boot scan.
type AutoContinueConfig struct {
	// Enabled turns the feature on. A *bool tristate (#559): nil (unset,
	// or a nil parent pointer) means "on by default when the feature can
	// apply" — i.e. a multi-session daemon or a --no-repl single-user
	// daemon with a durable eventlog; interactive REPL/TUI and in-process
	// library use are excluded by that precondition, so they never
	// auto-continue by default. An explicit false is a hard opt-out; an
	// explicit true forces it on (and, in a mode where it cannot apply,
	// warns and is ignored). The precondition gate lives with the CLI
	// wiring (resolveAutoContinue in cmd/core-agent) since config alone
	// cannot see the run mode or eventlog presence.
	Enabled *bool `json:"enabled,omitempty"`

	// Freshness bounds how old an interruption may be and still get
	// auto-continued, as a time.Duration string. Omitted/empty
	// defaults to "1h". Explicit "0s" disables the window (always
	// continue). Staler interruptions wait for the next real
	// message.
	Freshness string `json:"freshness,omitempty"`

	// MaxPerBoot caps how many sessions the boot-time scan will
	// continue in one daemon start, oldest interruption first.
	// Omitted/0 defaults to 10. (The scan itself lands with the
	// design doc's PR 2; the lazy-resume trigger ignores this cap —
	// a touched session is one the operator is already paying
	// attention to.)
	MaxPerBoot int `json:"max_per_boot,omitempty"`

	// Retry controls the in-lifetime retry driver: a background pass
	// that re-attempts a stranded continuation without waiting for a
	// reboot or a human message (#575 defect B), so a transient
	// continuation failure self-heals on a long-lived daemon. It stays
	// bounded by the crash-loop breaker + per-session cumulative cap —
	// a daemon-killing turn kills the driver too, so only survivable
	// failures are ever re-fired.
	//
	// A *bool so the default (nil) can be "on wherever auto-continue is
	// enabled": with the driver on, the promise shifts from "one
	// automatic retry, then wait for a human" to "self-heal up to the
	// cap, minutes apart, unattended". Set an explicit false to keep the
	// one-shot-then-wait contract.
	Retry *bool `json:"retry,omitempty"`

	// RetryInterval is how often the retry driver re-runs a guarded
	// pass, as a time.Duration string. Omitted/empty defaults to "5m".
	// The per-session single-retry guard (breakerWindow, 10m) is the
	// effective per-session cadence, so values below it simply no-op on
	// recently-attempted sessions. Must parse and be > 0.
	RetryInterval string `json:"retry_interval,omitempty"`
}

// RetryEnabled reports whether the in-lifetime retry driver should run.
// Nil (unset) defaults to on — the driver is enabled wherever
// auto-continue itself is enabled; an explicit false opts out. Callers
// must have already checked Enabled.
func (c *AutoContinueConfig) RetryEnabled() bool {
	if c == nil {
		return false
	}
	return c.Retry == nil || *c.Retry
}

// ToolOutputConfig caps tool result size before it enters model context.
type ToolOutputConfig struct {
	MaxBytes int                              `json:"max_bytes,omitempty"`
	MaxLines int                              `json:"max_lines,omitempty"`
	PerTool  map[string]ToolOutputPerToolCaps `json:"per_tool,omitempty"`
}

// ToolOutputPerToolCaps overrides global tool-output limits for one tool.
type ToolOutputPerToolCaps struct {
	MaxBytes int `json:"max_bytes,omitempty"`
	MaxLines int `json:"max_lines,omitempty"`
}

// ToolsConfig configures the bundled CLI's built-in tool suite.
//
// Disable lists tools to turn off. Names must match the canonical
// built-in names (see tools.BuiltinToolNames). Unknown names cause
// a startup error from tools.BuiltinTools.Disable, so typos fail
// loudly rather than silently leaving a tool on.
//
// The CLI's --disable-tools flag composes with this list by union;
// --no-builtin-tools disables the entire suite and makes Disable moot.
type ToolsConfig struct {
	Disable       []string            `json:"disable,omitempty"`
	WaitAndVerify WaitAndVerifyConfig `json:"wait_and_verify,omitempty"`
}

// WaitAndVerifyConfig bounds the wait_and_verify poll loop (#648).
// Zero values mean the built-in defaults; see pkg/tools.
type WaitAndVerifyConfig struct {
	// PollAllow names tools that may be polled despite not being
	// classified read-only by the runtime. This exists for MCP: ADK's
	// MCP adapter does not surface the server's readOnlyHint
	// annotation, so every MCP tool lands on the fail-safe "mutating"
	// side of tools.IsReadOnlyTool and would be refused. Listing a
	// tool here is the operator asserting it only observes state.
	//
	// Names are the ones the model sees, i.e. namespaced for MCP
	// ("gke__get_pod" and not "get_pod").
	PollAllow []string `json:"poll_allow,omitempty"`

	// MaxTimeoutSeconds caps the total wall clock one wait may spend.
	// Default 300. A call asking for more is rejected rather than
	// silently shortened.
	MaxTimeoutSeconds int `json:"max_timeout_seconds,omitempty"`

	// MaxAttempts caps how many times one wait may call its target.
	// Default 60.
	MaxAttempts int `json:"max_attempts,omitempty"`
}

// SubagentSpec declares one in-process subagent the parent agent may call
// by name. Subagents are wired in cmd/core-agent onto the shipped subagent
// substrate (agent.WithSubagents / agent.NewSubagentTool) — see
// docs/declarative-subagents-design.md. This type is the on-disk schema;
// it carries no runtime behavior.
//
// A subagent runs on its own Model (or the parent's, when Model is nil)
// and its own Instructions (which support the @include directive).
//
// There are two ways to give a subagent a tool surface:
//
//   - Inline refs against the SHARED config (Root unset). MCP names servers
//     from .agents/mcp.json, Skills names skills from .agents/skills/, Tools
//     is a built-in allowlist. Each dimension inherits the parent's full
//     surface when unset, selects a subset when a non-empty list, or grants
//     none when an explicit empty list (e.g. "mcp": []). The whole recipe
//     stays in one config.json + one mcp.json + one skills/ tree.
//   - A dedicated content root (Root set). Root names a trusted directory
//     the subagent loads as its OWN scope, independent of the parent: its
//     persona auto-assembles from <root>/AGENTS.md (Instructions overrides),
//     skills load from <root>/skills/, and MCP servers from <root>/mcp.json.
//     The parent loads none of it — this is how a subagent gets a persona,
//     skills, or servers the parent must NOT have (e.g. a read-only
//     single-cluster specialist under a fleet parent). With Root set, MCP
//     and Skills (when non-empty) filter WITHIN the root; Tools remains a
//     built-in allowlist (built-ins live in the binary, not a directory).
//     See docs/declarative-subagents-design.md, "Per-subagent content root".
//
// A declarative subagent is operator-authored and trusted, but still bound
// by the same permission gate (and require_plan_artifact) as the parent, so
// it cannot escalate.
type SubagentSpec struct {
	Name         string       `json:"name"`
	Description  string       `json:"description,omitempty"`
	Instructions string       `json:"instructions,omitempty"`
	Model        *ModelConfig `json:"model,omitempty"`
	MaxDepth     int          `json:"max_depth,omitempty"`
	Tools        []string     `json:"tools,omitempty"`
	MCP          []string     `json:"mcp,omitempty"`
	Skills       []string     `json:"skills,omitempty"`
	Root         string       `json:"root,omitempty"`
}

// MockConfig configures the mock providers (echo, scripted) and the
// orthogonal recording wrapper.
//
// Script is the path to a JSONL transcript consumed by the scripted
// provider; it's required when model.provider is "scripted".
//
// Strict makes the scripted provider assert that each incoming
// request's Contents JSON-equal the recorded request. Off by default
// — the typical use is replaying without caring about prompt drift.
//
// Record is a path to write a JSONL recording of every LLM turn.
// Works with any provider, not just the mocks; lives in MockConfig
// because it shares the file format the scripted provider consumes.
type MockConfig struct {
	Script string `json:"script,omitempty"`
	Strict bool   `json:"strict,omitempty"`
	Record string `json:"record,omitempty"`
}

// OTELConfig configures the OpenTelemetry exporter.
//
// Exporter/Endpoint govern trace export. Metrics is an independent
// nested block for the metric pipeline — separate because the traces
// path delegates to ADK while metrics do not (see docs/metrics-design.md).
type OTELConfig struct {
	Exporter string            `json:"exporter,omitempty"` // "none" | "console" | "otlp"
	Endpoint string            `json:"endpoint,omitempty"`
	Metrics  OTELMetricsConfig `json:"metrics,omitempty"`
}

// OTELMetricsConfig configures the OpenTelemetry metrics pipeline.
//
// Exporter values:
//   - "none"       — default; no MeterProvider is installed
//   - "otlp"       — OTLP metrics via HTTP; honors OTEL_EXPORTER_OTLP_METRICS_ENDPOINT
//   - "prometheus" — served at PrometheusAddr for scrape
//   - "both"       — OTLP push + Prometheus pull on the same MeterProvider
//
// The standard OTel env var OTEL_METRICS_EXPORTER overrides Exporter
// when set — matches the OTEL_TRACES_EXPORTER convention (#315) and
// lets K8s Deployments with shared ConfigMaps flip the metrics
// surface per-Pod without duplicating config.json.
//
// PrometheusAddr is the bind address for the /metrics scrape
// endpoint (e.g. ":9464"). Ignored unless Exporter is "prometheus"
// or "both". Empty + Prometheus mode selected implies ":9464"
// (the OTel-conventional Prometheus reader port).
type OTELMetricsConfig struct {
	Exporter       string `json:"exporter,omitempty"`
	PrometheusAddr string `json:"prometheus_addr,omitempty"`

	// SessionLabels controls whether per-session identity attributes
	// (session.id, app.name, user.id) are stamped on the usage
	// metrics. Absent/true — the default — preserves the per-session
	// series shape; false aggregates across sessions before export,
	// for fleet operators where per-session labels would blow up
	// series cardinality (many short-lived attach sessions × models).
	// Per-tool and per-turn histograms never carry session labels
	// regardless of this setting.
	SessionLabels *bool `json:"session_labels,omitempty"`
}

// SessionLabelsEnabled reports whether usage metrics carry per-session
// identity attributes. Nil field → true (default ON; same tri-state
// convention as ContextCacheConfig.IsEnabled).
func (c OTELMetricsConfig) SessionLabelsEnabled() bool {
	if c.SessionLabels == nil {
		return true
	}
	return *c.SessionLabels
}

// URLScopeConfig governs which URLs the fetch_url built-in is allowed
// to reach. Same Allow/Deny grammar + precedence as PathScopeConfig:
// Deny wins on overlap; an empty Allow list with the tool registered
// is treated as default-deny (the tool refuses every fetch and returns
// a clear error pointing at this config field).
//
// Patterns are host-only globs (e.g. "github.com", "*.googleapis.com",
// "*.svc.cluster.local"). HTTPS is assumed unless the pattern is
// prefixed with "http://", in which case plain HTTP is allowed for
// that pattern only (intentionally awkward — operators have to type
// the prefix to opt out of TLS).
//
// MaxBodyBytes caps the response body the tool returns to the model;
// zero means use the built-in default (64 KiB).
// TimeoutSeconds caps the HTTP timeout; zero means 30s.
//
// Headers maps host patterns to header bundles. Header values pass
// through os.ExpandEnv at request time, so values like
// "Bearer ${GITHUB_TOKEN}" pick up rotated env vars without a
// restart. The model never sets headers directly — keeps credential
// exfiltration off the tool argument surface.
//
// AllowMetadataEndpoints opts back into fetching link-local and
// cloud-metadata addresses (169.254.0.0/16 — including the
// 169.254.169.254 metadata service — plus fe80::/10 and the AWS
// IMDS IPv6 address fd00:ec2::254). fetch_url hard-blocks those
// ranges in every permission mode regardless of the allowlist;
// this flag is the only way to reach them. Default false — leave
// it off unless you are deliberately building a metadata-service
// integration and understand the credential-theft blast radius.
type URLScopeConfig struct {
	Allow                  []string                     `json:"allow,omitempty"`
	Deny                   []string                     `json:"deny,omitempty"`
	MaxBodyBytes           int                          `json:"max_body_bytes,omitempty"`
	TimeoutSeconds         int                          `json:"timeout_seconds,omitempty"`
	Headers                map[string]map[string]string `json:"headers,omitempty"`
	AllowMetadataEndpoints bool                         `json:"allow_metadata_endpoints,omitempty"`

	// Proxy controls outbound proxying for fetch_url (#429):
	//
	//   ""       (default) — no proxy. HTTP_PROXY/HTTPS_PROXY env
	//            vars are deliberately IGNORED: with a proxy in the
	//            path, hostname targets are resolved AT the proxy,
	//            outside the SSRF guard's resolve-validate-pin dial,
	//            so proxying must be an explicit operator decision,
	//            not ambient environment.
	//   "env"    — honor the standard proxy environment variables
	//            (HTTP_PROXY / HTTPS_PROXY / NO_PROXY).
	//   <url>    — route through this fixed proxy URL
	//            (http://, https://, or socks5://).
	//
	// In either non-empty mode the operator delegates private/
	// metadata-range SSRF policy for hostname targets to the proxy;
	// literal-IP targets are still screened locally on the initial
	// URL and every redirect hop.
	Proxy string `json:"proxy,omitempty"`
}

// AttachConfig holds defaults for the attach-mode listener and the
// peer-registration client. Every field is also exposed as a CLI flag
// (--attach-*); the CLI flag wins when set, otherwise the config value
// supplies the default. Fields holding URLs / addresses pass through
// os.ExpandEnv so per-pod values like "https://${POD_IP}:7777" can live
// in a shared ConfigMap.
//
// BearerToken is intentionally NOT a field here. The CLI flag form is
// --attach-token=ENVVAR (the name of the env var holding the secret),
// not the secret itself, and that env-var indirection should not be
// duplicated in a config file. Configure the env var via your secret
// manager (K8s Secret, sealed-secret, etc.) and set TokenEnv if you
// want to nail the env-var name down per-deployment.
type AttachConfig struct {
	// Server-side: where the attach listener binds. Set at most one.
	// Non-loopback listen addresses (e.g. "0.0.0.0:7777") refuse to
	// start without authentication (TokenEnv bearer token, mTLS, or
	// enforced multi-session auth) — see pkg/attach.NewServer (#376).
	Listen     string `json:"listen,omitempty"`      // e.g. "127.0.0.1:7777"
	UnixSocket string `json:"unix_socket,omitempty"` // e.g. "/var/run/core-agent.sock"

	// TLS material. TLSCert + TLSKey enable HTTPS; ClientCA additionally
	// enables mTLS (client cert required). Paths only — keys live on disk.
	TLSCert  string `json:"tls_cert,omitempty"`
	TLSKey   string `json:"tls_key,omitempty"`
	ClientCA string `json:"client_ca,omitempty"`

	// TokenEnv is the name of the env var that holds the bearer token
	// clients must present. The secret itself never lives in config.
	TokenEnv string `json:"token_env,omitempty"`

	// ReadOnly disables POST /inject and /wake; read endpoints stay open.
	ReadOnly bool `json:"readonly,omitempty"`

	// PeerHub turns on the peer-registration endpoints on this listener.
	PeerHub bool `json:"peer_hub,omitempty"`

	// Peer-side: this agent registers with a remote hub.
	RegisterTo       string `json:"register_to,omitempty"`       // hub URL
	RegisterEndpoint string `json:"register_endpoint,omitempty"` // expanded via os.ExpandEnv
	RegisterName     string `json:"register_name,omitempty"`     // defaults to hostname when empty

	// MultiSession enables per-caller authentication + per-session
	// ACL on the attach listener. Zero value (disabled) keeps the
	// daemon in single-user mode — the listener treats every request
	// as the same anonymous Caller and TokenEnv / BearerToken still
	// gate at the transport layer as today. See
	// docs/multi-session-design.md.
	MultiSession MultiSessionConfig `json:"multi_session,omitempty"`

	// CostRateLimit tunes the per-caller token bucket that bounds the
	// COST-BEARING attach endpoints (the five slash ops, POST
	// /sessions, pricing/refresh) — see #463. Omitted (nil) keeps the
	// library defaults: burst 5, 10/minute per caller. Reads, /events
	// streams, /inject, and /wake are never limited.
	CostRateLimit *CostRateLimitConfig `json:"cost_rate_limit,omitempty"`

	// ShutdownTimeout caps how long the attach listener's graceful
	// HTTP shutdown waits for in-flight requests once SSE streams are
	// hung up, as a time.Duration string (e.g. "5s"). Omitted/empty
	// keeps the library default (5s). Counts toward the daemon's
	// total teardown budget — keep it comfortably under the
	// supervisor's kill timeout (K8s terminationGracePeriodSeconds,
	// default 30s). See #538.
	ShutdownTimeout string `json:"shutdown_timeout,omitempty"`
}

// MultiSessionConfig configures the per-caller authentication +
// per-session ACL surface introduced in v2.4. Disabled (zero value)
// preserves single-user behavior. When enabled, the attach server
// authenticates every request against the configured Authenticator
// (typically a bearer table loaded from users.json) and threads the
// resolved Caller through audit logs, per-session permission grants,
// and outbound MCP context.
//
// Field-by-field detail in docs/multi-session-design.md §"Config
// surface".
// CostRateLimitConfig is the config-file form of the attach
// listener's per-caller cost rate limit (attach.CostRateLimit).
// PerMinute/Burst <= 0 fall back to the library defaults (10/min,
// burst 5); Disabled switches enforcement off entirely.
type CostRateLimitConfig struct {
	PerMinute int  `json:"per_minute,omitempty"`
	Burst     int  `json:"burst,omitempty"`
	Disabled  bool `json:"disabled,omitempty"`
}

type MultiSessionConfig struct {
	// Enabled switches the attach listener from single-user mode
	// (daemon-level bearer token, no per-caller threading) to
	// multi-session mode (per-caller authentication, ACL enforcement,
	// audit log identity threading). Default false.
	Enabled bool `json:"enabled,omitempty"`

	// UsersDir is the directory holding per-caller instruction
	// overlays (Phase 3 / PR γ). Each subdirectory named after a
	// Caller's Identity may contain an .agents/ tree merged on top of
	// the daemon-wide instruction stack for sessions belonging to
	// that Caller. Empty disables the overlay path.
	UsersDir string `json:"users_dir,omitempty"`

	// Auth selects the Authenticator implementation and its
	// configuration. Only "bearer_table" is shipped in v2.4; OIDC /
	// JWT / mTLS / K8s ServiceAccount kinds are designed but
	// deferred.
	Auth MultiSessionAuthConfig `json:"auth,omitempty"`

	// AdminIdentities lists the Caller identities that bypass every
	// per-session authorization check (Admin role). Use sparingly —
	// these identities can read every session in the daemon.
	AdminIdentities []string `json:"admin_identities,omitempty"`

	// AllowAnonymous, when true, lets requests without a valid
	// credential resolve to the DefaultIdentity Caller instead of
	// returning 401. Dangerous in shared environments where every
	// unauthenticated request becomes the same Caller. Default false.
	AllowAnonymous bool `json:"allow_anonymous,omitempty"`

	// DefaultIdentity is the Caller.Identity stamped onto the
	// implicit anonymous Caller (when multi-session is disabled or
	// AllowAnonymous=true). Default "anon".
	DefaultIdentity string `json:"default_identity,omitempty"`

	// ProxyIdentities lists Caller identities permitted to assert
	// other Callers via the AssertedCallerHeader. Typical use:
	// chat-bot service-account identities ("sa:slack-bot") that
	// authenticate as themselves but speak on behalf of human users.
	// Empty disables the proxy path.
	ProxyIdentities []string `json:"proxy_identities,omitempty"`

	// AssertedCallerHeader is the header name a proxy Caller uses to
	// assert the effective identity. Default "X-Asserted-Caller".
	AssertedCallerHeader string `json:"asserted_caller_header,omitempty"`

	// SessionIdleTimeout bounds how long an in-memory session may
	// sit untouched before the eviction sweep removes it. Evicted
	// sessions remain resumable from disk (the ACL row stays);
	// the next Lookup re-resumes them lazily via the SessionResumer.
	//
	// Parsed via time.ParseDuration ("24h", "30m", "7d" all work).
	// Omitted or empty → default 24h. Explicit "0s" DISABLES the
	// sweep entirely — sessions stay in memory until the daemon
	// stops. Use "0s" for tiny local-dev daemons where memory
	// isn't a concern; use a shorter value for tight-budget pods.
	//
	// Only meaningful when Enabled=true and the daemon has an
	// aclStore wired (i.e., --session-db is set). See
	// docs/session-resume-design.md §"Lifecycle primitive".
	SessionIdleTimeout string `json:"session_idle_timeout,omitempty"`
}

// MultiSessionAuthConfig selects which Authenticator implementation
// the multi-session attach listener uses. Only "bearer_table" is
// shipped in v2.4; the other kinds are designed but deferred (see
// docs/multi-session-design.md §"Non-goals").
type MultiSessionAuthConfig struct {
	// Kind selects the Authenticator implementation.
	// Recognized values:
	//   - "" or "bearer_table" — static token → identity table loaded
	//     from TableFile (default; v2.4)
	// Future: "oidc" / "mtls" / "k8s_sa" (interfaces designed; not
	// shipped in v2.4).
	Kind string `json:"kind,omitempty"`

	// TableFile is the path to users.json when Kind="bearer_table".
	// File must be mode 0600 or stricter (the loader rejects anything
	// laxer). Required when Kind="bearer_table" and Enabled=true.
	TableFile string `json:"table_file,omitempty"`
}

// Recognized values for MultiSessionAuthConfig.Kind. Only the bearer
// table is implemented in v2.4; other kinds are reserved and return
// a validation error.
const (
	MultiSessionAuthKindBearerTable = "bearer_table"
)

// Permission modes.
const (
	PermissionModeAsk         = "ask"
	PermissionModeAllow       = "allow"
	PermissionModeYolo        = "yolo"
	PermissionModePlan        = "plan"
	PermissionModeAcceptEdits = "acceptEdits"
)

// Provider names recognized by the resolver.
const (
	ProviderGemini          = "gemini"
	ProviderVertex          = "vertex"
	ProviderAnthropic       = "anthropic"
	ProviderAnthropicVertex = "anthropic-vertex"
	ProviderEcho            = "echo"
	ProviderScripted        = "scripted"
)

// DefaultConfig returns a Config with all fields populated by sensible
// defaults. Override-then-merge happens at Load time.
func DefaultConfig() *Config {
	return &Config{
		Version: SchemaVersion,
		Model: ModelConfig{
			// Provider intentionally empty — resolver auto-detects from env.
			// gemini-3.6-flash is the zero-config default (#571): a
			// current-generation, generally-available flash model (the
			// taskclass `frontier` tier) that combines server-side search
			// built-ins with function tools out of the box — a better
			// first impression than the prior 3.1-pro *preview* /
			// `-customtools` build. It satisfies Gemini's "3.0+ required
			// when combining built-ins with function tools" constraint, so
			// zero-config users need not think about it. Override via
			// Model.Name for a pro-class model or the `-customtools`
			// variant (which prefers registered tools over raw bash) when a
			// consumer wants that behavior; revisit the default toward
			// gemini-3.6-pro when it ships.
			Name: "gemini-3.6-flash",
		},
		Permissions: PermissionsConfig{
			Mode: PermissionModeAsk,
		},
		Agent: AgentConfig{
			MaxSteps: 50,
		},
		ToolOutput: ToolOutputConfig{
			MaxBytes: 32 * 1024,
			MaxLines: 500,
			PerTool: map[string]ToolOutputPerToolCaps{
				"bash":            {MaxBytes: 64 * 1024, MaxLines: 2000},
				"read_file":       {MaxBytes: 256 * 1024, MaxLines: 5000},
				"read_many_files": {MaxBytes: 256 * 1024, MaxLines: 5000},
				"glob":            {MaxBytes: 32 * 1024, MaxLines: 500},
				"grep":            {MaxBytes: 256 * 1024, MaxLines: 5000},
			},
		},
		OTEL: OTELConfig{
			Exporter: "none",
			Metrics: OTELMetricsConfig{
				Exporter: "none",
			},
		},
		UI: UIConfig{
			Theme: ThemeAuto,
			// Mouse left nil so MouseEnabled() returns the default
			// (true). Explicit override via config or /mouse.
		},
	}
}

// Validate returns an error if the config is internally inconsistent.
// Validation here is structural; environmental concerns (is GOOGLE_API_KEY
// set? does the GCP project exist?) are checked at provider-construction
// time so test fixtures don't need real creds.
func (c *Config) Validate() error {
	if c.Version != 0 && c.Version != SchemaVersion {
		return fmt.Errorf("config: unsupported schema version %d (expected %d); upgrade your .agents/config.json", c.Version, SchemaVersion)
	}
	if c.Model.Name == "" {
		return fmt.Errorf("config: model.name is required")
	}
	switch c.Model.Provider {
	case "", ProviderGemini, ProviderVertex, ProviderAnthropic, ProviderAnthropicVertex, ProviderEcho, ProviderScripted:
		// ok; "" means auto-detect at resolve time.
	default:
		return fmt.Errorf("config: unknown model.provider %q (want one of %q, %q, %q, %q, %q, %q)", c.Model.Provider, ProviderGemini, ProviderVertex, ProviderAnthropic, ProviderAnthropicVertex, ProviderEcho, ProviderScripted)
	}
	if c.Model.Provider == ProviderScripted && c.Mock.Script == "" {
		return fmt.Errorf("config: mock.script is required when provider is %q (or pass --script PATH)", ProviderScripted)
	}
	if c.Model.Provider == ProviderVertex && c.Model.Vertex != nil {
		if c.Model.Vertex.Project == "" || c.Model.Vertex.Location == "" {
			return fmt.Errorf("config: model.vertex.project and model.vertex.location are required when provider is %q (or set GOOGLE_CLOUD_PROJECT / GOOGLE_CLOUD_LOCATION)", ProviderVertex)
		}
	}
	if c.Model.Provider == ProviderAnthropicVertex && c.Model.Anthropic != nil && c.Model.Anthropic.Vertex != nil {
		if c.Model.Anthropic.Vertex.Project == "" || c.Model.Anthropic.Vertex.Location == "" {
			return fmt.Errorf("config: model.anthropic.vertex.project and model.anthropic.vertex.location are required when provider is %q (or set ANTHROPIC_VERTEX_PROJECT_ID / CLOUD_ML_REGION)", ProviderAnthropicVertex)
		}
	}
	switch c.Permissions.Mode {
	case "", PermissionModeAsk, PermissionModeAllow, PermissionModeYolo, PermissionModePlan, PermissionModeAcceptEdits:
		// ok
	default:
		return fmt.Errorf("config: unknown permissions.mode %q", c.Permissions.Mode)
	}
	for i, e := range c.PathScope.AllowPaths {
		if e.Path == "" {
			return fmt.Errorf("config: path_scope.allow_paths[%d].path is required", i)
		}
		if !validAccessSpec(e.Access) {
			return fmt.Errorf("config: path_scope.allow_paths[%d].access=%q must be r, w, or rw (read / write / readwrite accepted)", i, e.Access)
		}
	}
	if rl := c.Attach.CostRateLimit; rl != nil {
		if rl.PerMinute < 0 || rl.Burst < 0 {
			return fmt.Errorf("config: attach.cost_rate_limit: per_minute (%d) and burst (%d) must be >= 0 (0 = library default; use disabled: true to turn enforcement off)", rl.PerMinute, rl.Burst)
		}
	}
	switch c.URLScope.Proxy {
	case "", "env":
		// ok — no proxy / delegate to the standard env vars.
	default:
		u, err := url.Parse(c.URLScope.Proxy)
		if err != nil {
			return fmt.Errorf("config: url_scope.proxy %q: %v", c.URLScope.Proxy, err)
		}
		switch u.Scheme {
		case "http", "https", "socks5":
			if u.Host == "" {
				return fmt.Errorf("config: url_scope.proxy %q has no host", c.URLScope.Proxy)
			}
		default:
			return fmt.Errorf("config: url_scope.proxy %q must be \"env\" or an http(s)/socks5 URL", c.URLScope.Proxy)
		}
	}
	if c.Compaction.Threshold != nil {
		v := *c.Compaction.Threshold
		if v <= 0 || v >= 1 {
			return fmt.Errorf("config: compaction.threshold=%v must be in (0, 1) exclusive", v)
		}
	}
	for tier, v := range c.Compaction.ThresholdByTier {
		if v <= 0 || v >= 1 {
			return fmt.Errorf("config: compaction.threshold_by_tier[%q]=%v must be in (0, 1) exclusive", tier, v)
		}
	}
	if wv := c.Tools.WaitAndVerify; wv.MaxTimeoutSeconds < 0 || wv.MaxAttempts < 0 {
		return fmt.Errorf("config: tools.wait_and_verify: max_timeout_seconds (%d) and max_attempts (%d) must be >= 0 (0 = built-in default)", wv.MaxTimeoutSeconds, wv.MaxAttempts)
	}
	for i, n := range c.Tools.WaitAndVerify.PollAllow {
		if strings.TrimSpace(n) == "" {
			return fmt.Errorf("config: tools.wait_and_verify.poll_allow[%d] is empty", i)
		}
	}
	if c.Agent.MaxTurnCostUSD != nil {
		if v := *c.Agent.MaxTurnCostUSD; v < 0 {
			return fmt.Errorf("config: agent.max_turn_cost_usd=%v must be >= 0 (0 disables, positive enforces)", v)
		}
	}
	if c.Agent.MaxSessionCostUSD != nil {
		if v := *c.Agent.MaxSessionCostUSD; v < 0 {
			return fmt.Errorf("config: agent.max_session_cost_usd=%v must be >= 0 (0 disables, positive enforces)", v)
		}
	}
	if c.Session.TaskClass != "" {
		// Validation matches pkg/taskclass.Classes() but we don't
		// import taskclass here (would pull a new dep into a
		// foundational config package). Keep the list in sync;
		// pkg/taskclass has tests pinning the canonical names so
		// a drift here would be obvious at build time.
		switch c.Session.TaskClass {
		case "debug", "implement", "chat", "research", "review":
			// ok
		default:
			return fmt.Errorf("config: session.task_class=%q is not a known class (want one of debug, implement, chat, research, review)", c.Session.TaskClass)
		}
	}
	switch c.UI.Theme {
	case "", ThemeAuto, ThemeDark, ThemeLight:
		// ok; "" is equivalent to "auto".
	default:
		if !validNamedTheme(c.UI.Theme) {
			return fmt.Errorf("config: invalid ui.theme %q (want %q/%q/%q or a lowercase named theme [a-z0-9_-]{1,64})", c.UI.Theme, ThemeAuto, ThemeDark, ThemeLight)
		}
	}
	switch c.Safety.SmallTierParent {
	case "", SmallTierParentWarn, SmallTierParentRefuse, SmallTierParentAllow:
		// ok; "" defaults to warn.
	default:
		return fmt.Errorf("config: unknown safety.small_tier_parent %q (want one of %q, %q, %q)", c.Safety.SmallTierParent, SmallTierParentWarn, SmallTierParentRefuse, SmallTierParentAllow)
	}
	switch c.Safety.Watchdog {
	case "", WatchdogOff, WatchdogWarn, WatchdogEnforce:
		// ok; "" resolves to the mode-dependent default (see
		// SafetyConfig.Watchdog).
	default:
		return fmt.Errorf("config: unknown safety.watchdog %q (want one of %q, %q, %q)", c.Safety.Watchdog, WatchdogOff, WatchdogWarn, WatchdogEnforce)
	}
	for i, root := range c.ContentRoots {
		// Environment-free per the Validate contract: no existence/stat
		// check here (the instruction loader errors loudly on a missing
		// root at load time). We only reject entries that could never be
		// a usable path — empty or whitespace-only.
		if strings.TrimSpace(root) == "" {
			return fmt.Errorf("config: content_roots[%d] is empty (each content root must be a non-empty path)", i)
		}
	}
	if err := c.validateSubagents(); err != nil {
		return err
	}
	if err := c.validateAlerts(); err != nil {
		return err
	}
	if err := c.Hooks.Validate(); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if c.Attach.MultiSession.Enabled {
		// Only "bearer_table" (or empty → bearer_table default) is
		// shipped in v2.4. Future kinds (oidc / mtls / k8s_sa) are
		// designed in docs/multi-session-design.md but the
		// implementations are explicitly deferred.
		switch c.Attach.MultiSession.Auth.Kind {
		case "", MultiSessionAuthKindBearerTable:
			if c.Attach.MultiSession.Auth.TableFile == "" {
				return fmt.Errorf("config: attach.multi_session.auth.table_file is required when multi_session is enabled with kind=%q", MultiSessionAuthKindBearerTable)
			}
		default:
			return fmt.Errorf("config: attach.multi_session.auth.kind=%q is not shipped in this version (only %q is supported; oidc/mtls/k8s_sa are designed but deferred)", c.Attach.MultiSession.Auth.Kind, MultiSessionAuthKindBearerTable)
		}
	}
	return nil
}

// Small-tier-parent mode constants. See SafetyConfig.SmallTierParent
// for behavior. Exported so consumers (CLI, library) can reference
// the canonical strings.
const (
	SmallTierParentWarn   = "warn"
	SmallTierParentRefuse = "refuse"
	SmallTierParentAllow  = "allow"
)

// Watchdog mode constants. See SafetyConfig.Watchdog for behavior.
// Exported so consumers (CLI, library, recipes) can reference the
// canonical strings rather than re-spelling them.
const (
	WatchdogOff     = "off"
	WatchdogWarn    = "warn"
	WatchdogEnforce = "enforce"
)

// validNamedTheme accepts the shape core-tui's BuiltinThemes
// registry uses: lowercase letters, digits, dash, underscore;
// 1-64 chars. Permissive on the set (core-tui owns the registry)
// but strict on the shape so a typo like "DARK" still fails.
func validNamedTheme(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_':
		default:
			return false
		}
	}
	return true
}

// validAccessSpec mirrors permissions.ParseAccess's accept set
// without importing permissions (which would create a config →
// permissions dependency cycle: permissions already imports
// config). Keep this in sync with ParseAccess.
func validAccessSpec(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "r", "w", "rw", "wr", "read", "write", "readwrite", "read+write":
		return true
	default:
		return false
	}
}

// validateSubagents checks the declarative subagents[] block. Structural
// only (per Validate's contract): names must be unique and tool-name-safe,
// max_depth non-negative, an inline model (if set) must name a known
// provider, and inline tool/mcp/skills refs must be non-empty strings.
// Whether a referenced MCP server or skill actually exists is resolved at
// wiring time in cmd/core-agent (it depends on the loaded mcp.json /
// skills dir), not here. Likewise whether a `root` directory exists is a
// wiring-time check (it depends on the resolution base, which Validate has
// no access to) — here we only reject a whitespace-only path, which would
// otherwise silently resolve to the base itself.
func (c *Config) validateSubagents() error {
	seen := make(map[string]struct{}, len(c.Subagents))
	for i, sa := range c.Subagents {
		if sa.Name == "" {
			return fmt.Errorf("config: subagents[%d].name is required", i)
		}
		if !validSubagentName(sa.Name) {
			return fmt.Errorf("config: subagents[%d].name=%q must be [A-Za-z0-9_-]{1,64} (it becomes the tool name the parent calls)", i, sa.Name)
		}
		if _, dup := seen[sa.Name]; dup {
			return fmt.Errorf("config: subagents[%d]: duplicate name %q", i, sa.Name)
		}
		seen[sa.Name] = struct{}{}
		if sa.MaxDepth < 0 {
			return fmt.Errorf("config: subagents[%d].max_depth=%d must be >= 0 (0 = substrate default)", i, sa.MaxDepth)
		}
		if sa.Model != nil {
			if sa.Model.Name == "" {
				return fmt.Errorf("config: subagents[%d].model.name is required when model is set", i)
			}
			switch sa.Model.Provider {
			case "", ProviderGemini, ProviderVertex, ProviderAnthropic, ProviderAnthropicVertex, ProviderEcho, ProviderScripted:
				// ok; "" means inherit the parent's auto-detected provider.
			default:
				return fmt.Errorf("config: subagents[%d].model.provider %q is unknown (want one of %q, %q, %q, %q, %q, %q)", i, sa.Model.Provider, ProviderGemini, ProviderVertex, ProviderAnthropic, ProviderAnthropicVertex, ProviderEcho, ProviderScripted)
			}
		}
		for j, n := range sa.Tools {
			if n == "" {
				return fmt.Errorf("config: subagents[%d].tools[%d] is empty", i, j)
			}
		}
		for j, n := range sa.MCP {
			if n == "" {
				return fmt.Errorf("config: subagents[%d].mcp[%d] is empty", i, j)
			}
		}
		for j, n := range sa.Skills {
			if n == "" {
				return fmt.Errorf("config: subagents[%d].skills[%d] is empty", i, j)
			}
		}
		if sa.Root != "" && strings.TrimSpace(sa.Root) == "" {
			return fmt.Errorf("config: subagents[%d].root is whitespace-only (omit it, or name a real directory)", i)
		}
	}
	return nil
}

// validSubagentName reports whether s is safe to register as a tool name
// (the parent model calls a subagent by this name). Mirrors the hand-rolled
// charset style of validNamedTheme; regexp is intentionally avoided to keep
// this foundational package dependency-light.
func validSubagentName(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_':
		default:
			return false
		}
	}
	return true
}
