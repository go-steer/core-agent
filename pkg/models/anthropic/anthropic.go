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

// Package anthropic implements models.Provider for Anthropic / Claude.
//
// ADK Go ships only the Gemini and Apigee model backends, so this
// package adapts the official Anthropic Go SDK
// (github.com/anthropics/anthropic-sdk-go) to the ADK's model.LLM
// interface. genai-shaped requests are translated to Anthropic's
// Messages API; streaming responses are accumulated back into
// genai-shaped events the ADK runner expects.
//
// Conversation history is preserved automatically by the ADK runner
// (the in-memory session service replays prior events on each turn);
// this provider is stateless aside from the API client.
package anthropic

import (
	"context"
	"fmt"
	"os"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	adkmodel "google.golang.org/adk/model"

	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/models"
)

// DefaultModel is used when LLMRequest.Model is empty. We follow the
// claude-api skill's guidance and default to the most capable Opus —
// the LATEST one, per the policy documented on taskclass.ModelForTier.
// Not the Mythos-class tier above it (claude-fable-5 / claude-mythos-5),
// which costs 2x and isn't a general-purpose default.
//
// Pinned to taskclass.ModelForTier("anthropic", "frontier") by
// TestDefaultModel_MatchesFrontierTier: an operator who sets
// --task=implement and one who sets nothing should land on the same
// model, or the task-class flag reads as a silent downgrade.
const DefaultModel = "claude-opus-5"

// DefaultSmallModelID is the Anthropic cheap-tier model used by default
// for agentic subtasks when the operator hasn't pinned one with
// --agentic-small-model. Same value for the first-party and Vertex
// backends; the Vertex publication name resolves at call time.
const DefaultSmallModelID = "claude-haiku-4-5"

// DefaultMaxTokens caps a single response when the caller hasn't set
// one. 16K is a comfortable middle ground: plenty for most turns,
// well under the streaming SDK's HTTP timeouts.
const DefaultMaxTokens = 16_384

// EnvAPIKey is the environment variable consulted when no key is
// supplied via config.
const EnvAPIKey = "ANTHROPIC_API_KEY" // #nosec G101 -- env var name, not a credential

func init() {
	models.Register(config.ProviderAnthropic, newProvider)
}

// Provider is the Anthropic implementation of models.Provider. The
// same struct serves both the first-party API and Vertex AI backends —
// only the embedded client differs. name carries which one this is so
// telemetry and Resolve() see the right identity.
type Provider struct {
	name     string
	client   anthropic.Client
	cache    CacheOptions
	builtins BuiltinTools
}

// Option configures a Provider at construction.
type Option func(*Provider)

// WithCacheSystem caches the last system block and nothing else.
//
// Deprecated: use WithPromptCache. This option predates the history
// breakpoints and keeps its original all-or-nothing meaning — it
// REPLACES the policy rather than editing one field, so
// WithCacheSystem(false) still means "no caching at all", the way it did
// when system blocks were the only thing that could be marked. A caller
// who wrote it to avoid the write premium keeps that outcome instead of
// silently acquiring rolling history breakpoints.
func WithCacheSystem(on bool) Option {
	return func(p *Provider) { p.cache = CacheOptions{System: on} }
}

// WithPromptCache sets the whole prompt-caching policy, replacing
// DefaultCacheOptions. Pass a zero CacheOptions to turn caching off —
// worth doing for a request shape whose prefix varies every call, where
// the write premium buys reads that never come.
func WithPromptCache(o CacheOptions) Option { return func(p *Provider) { p.cache = o } }

// CacheOptions selects which parts of a request carry Anthropic
// cache_control breakpoints. Both halves are prefix-cached by the same
// mechanism; they're separable because they fail differently — System
// is worthless if the instruction carries a per-turn timestamp, History
// is worthless for one-shot calls that never replay a prefix.
//
// The zero value disables caching entirely.
type CacheOptions struct {
	// System marks the last system block. Since the render order is
	// tools → system → messages, that one marker caches the tool
	// schemas and the system prompt together.
	System bool
	// History places rolling breakpoints over the tail of the
	// conversation so a growing transcript is re-read at the cache
	// rate instead of re-billed in full every turn (#714).
	History bool

	// TTL is the breakpoint lifetime: config.PromptCacheTTL5m (the
	// zero value's meaning) or config.PromptCacheTTL1h. The 1-hour
	// TTL bills writes at 2x base input against the 5-minute TTL's
	// 1.25x, so it only pays when turns are further than five minutes
	// apart — see config.PromptCacheConfig.TTL. Both are priced, and
	// the response reports which one each write used, so the ledger is
	// right either way (#770).
	//
	// Any other value is treated as 5m; see cacheControl.
	TTL string
}

// Enabled reports whether any breakpoint would be placed.
func (o CacheOptions) Enabled() bool { return o.System || o.History }

// cacheControl is the marker every breakpoint in this request carries.
// One TTL per request: Anthropic permits mixing, but a mixed request
// would make "which breakpoint expired" depend on marker position, and
// there is no use case in the loop for the two policies at once.
func (o CacheOptions) cacheControl() anthropic.CacheControlEphemeralParam {
	cc := anthropic.NewCacheControlEphemeralParam()
	if o.TTL == config.PromptCacheTTL1h {
		cc.TTL = anthropic.CacheControlEphemeralTTLTTL1h
	}
	return cc
}

// DefaultCacheOptions is what every constructor starts from: cache the
// stable prefix and the conversation tail. On by default because the
// break-even is two requests against a 5-minute TTL and core-agent's
// agentic loop issues its second request seconds after the first — the
// shape that loses (a single request whose prefix is never seen again)
// is the rare one, and it is the one an operator can turn off.
func DefaultCacheOptions() CacheOptions {
	return CacheOptions{System: true, History: true, TTL: config.PromptCacheTTL5m}
}

// cacheOptionsFromConfig maps the config block onto the policy. Absent
// block → the defaults; explicit enabled=false → off.
func cacheOptionsFromConfig(cfg *config.Config) CacheOptions {
	if cfg == nil || cfg.Model.Anthropic == nil {
		return DefaultCacheOptions()
	}
	pc := cfg.Model.Anthropic.PromptCache
	if !pc.IsEnabled() {
		return CacheOptions{}
	}
	o := DefaultCacheOptions()
	o.TTL = pc.CacheTTL()
	return o
}

// New constructs a Provider with the given API key (first-party
// api.anthropic.com). Pass options to tune behavior. Empty key falls
// back to the ANTHROPIC_API_KEY env var.
func New(apiKey string, opts ...Option) (*Provider, error) {
	if apiKey == "" {
		apiKey = os.Getenv(EnvAPIKey)
	}
	if apiKey == "" {
		return nil, fmt.Errorf("anthropic: api key is required (set ANTHROPIC_API_KEY or model.anthropic.api_key in .agents/config.json)")
	}
	p := &Provider{
		name:     config.ProviderAnthropic,
		client:   anthropic.NewClient(option.WithAPIKey(apiKey)),
		cache:    DefaultCacheOptions(),
		builtins: DefaultBuiltinTools(),
	}
	for _, opt := range opts {
		opt(p)
	}
	return p, nil
}

// Name reports the provider identity ("anthropic" or "anthropic-vertex").
func (p *Provider) Name() string { return p.name }

// DefaultSmallModel satisfies models.SmallModelDefaulter so core-agent
// can route subtask digesting to a cheap-tier Claude model without
// requiring the operator to set --agentic-small-model.
func (p *Provider) DefaultSmallModel() string { return DefaultSmallModelID }

// Model returns a model.LLM for the given model ID. modelID may be
// empty, in which case DefaultModel is used.
//
// Note: Vertex AI sometimes serves Claude under date-suffixed model IDs
// (e.g. "claude-opus-4-5@20251101"). When using "anthropic-vertex",
// pass the exact ID Vertex expects via cfg.Model.Name; the SDK plugs
// it into the Vertex URL path verbatim.
func (p *Provider) Model(_ context.Context, modelID string) (adkmodel.LLM, error) {
	if modelID == "" {
		modelID = DefaultModel
	}
	return &llm{
		client:   p.client,
		modelID:  modelID,
		cache:    p.cache,
		builtins: p.builtins,
	}, nil
}

// SetPromptCache installs the caching policy after construction. Exists
// for the daemon's wiring order: the provider comes out of the registry
// (models.Resolve, which sees only config) before the CLI kill switch
// can be applied, and Model() copies the policy into each LLM it
// builds. Call it before the first Model() call — like the Gemini
// provider's cache hooks, it is startup wiring, not a live control.
func (p *Provider) SetPromptCache(o CacheOptions) { p.cache = o }

// PromptCache reports the currently installed policy. Lets a host log
// what it wired without keeping its own copy.
func (p *Provider) PromptCache() CacheOptions { return p.cache }

func newProvider(cfg *config.Config) (models.Provider, error) {
	key := ""
	if cfg.Model.Anthropic != nil {
		key = cfg.Model.Anthropic.APIKey
	}
	return New(key, append(builtinToolsFromConfig(cfg), WithPromptCache(cacheOptionsFromConfig(cfg)))...)
}

// builtinToolsFromConfig maps the provider-neutral model.builtin_tools
// block onto this provider's toggles. Tri-state: an absent field yields
// no option, so DefaultBuiltinTools survives untouched.
//
// Only web_search has an Anthropic equivalent surfaced today; the
// block's url_context and code_execution are Gemini-side and land
// nowhere here. That is fail-safe by construction — a tool this
// provider cannot send is a tool it cannot leave on — but it does mean
// a `code_execution: true` written against an Anthropic model buys
// nothing. The startup summary prints the effective set so the gap is
// visible rather than assumed.
func builtinToolsFromConfig(cfg *config.Config) []Option {
	if cfg == nil || cfg.Model.BuiltinTools == nil {
		return nil
	}
	if ws := cfg.Model.BuiltinTools.WebSearch; ws != nil {
		return []Option{WithWebSearch(*ws)}
	}
	return nil
}
