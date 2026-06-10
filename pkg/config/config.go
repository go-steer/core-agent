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
	"strings"
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
	Mock        MockConfig        `json:"mock,omitempty"`
	OTEL        OTELConfig        `json:"otel,omitempty"`
	URLScope    URLScopeConfig    `json:"url_scope,omitempty"`
	Attach      AttachConfig      `json:"attach,omitempty"`
	Pricing     PricingFileConfig `json:"pricing,omitempty"`
	UI          UIConfig          `json:"ui,omitempty"`
	Compaction  CompactionConfig  `json:"compaction,omitempty"`
	Session     SessionConfig     `json:"session,omitempty"`
	Safety      SafetyConfig      `json:"safety,omitempty"`
}

// SafetyConfig carries operator-facing safety guardrails — things
// that are NOT permission gates (those live in PermissionsConfig)
// but rather "the operator probably misconfigured something" checks.
// Today: just the small-tier-parent guard (#121).
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
// upstream. See internal/pricing and docs/pricing-design.md.
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
// Name: a model ID, e.g. "gemini-3.1-pro-preview-customtools" or "claude-opus-4-7".
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
	// + the compiled-in fallback; see internal/pricing for the
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
type PricingConfig struct {
	InputPerMTok  float64 `json:"input_per_mtok,omitempty"`
	OutputPerMTok float64 `json:"output_per_mtok,omitempty"`
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
	Disable []string `json:"disable,omitempty"`
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
type OTELConfig struct {
	Exporter string `json:"exporter,omitempty"` // "none" | "console" | "otlp"
	Endpoint string `json:"endpoint,omitempty"`
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
type URLScopeConfig struct {
	Allow          []string                     `json:"allow,omitempty"`
	Deny           []string                     `json:"deny,omitempty"`
	MaxBodyBytes   int                          `json:"max_body_bytes,omitempty"`
	TimeoutSeconds int                          `json:"timeout_seconds,omitempty"`
	Headers        map[string]map[string]string `json:"headers,omitempty"`
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
	Listen     string `json:"listen,omitempty"`      // e.g. "0.0.0.0:7777"
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
}

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
			// `-customtools` is a Vertex behavioral variant of
			// gemini-3.1-pro-preview fine-tuned to prefer developer-defined
			// tools over raw shell — without it the model bypasses our
			// structured grep / read_file / edit_file and shells out via
			// bash, breaking the permission gate's coverage and never
			// batching tool calls. Same pricing, same context window, same
			// reasoning behavior. Override via Model.Name when a consumer
			// wants the un-tuned variant for behavior-baseline comparisons.
			Name: "gemini-3.1-pro-preview-customtools",
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
