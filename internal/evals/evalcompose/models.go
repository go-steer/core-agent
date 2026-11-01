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

package evalcompose

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"os"
	"slices"
	"strings"
	"sync"

	"google.golang.org/adk/v2/model"

	coremodels "github.com/go-steer/core-models"
	"github.com/go-steer/core-models/adkv2"
	"github.com/go-steer/core-models/dialect/anthropic"
	"github.com/go-steer/core-models/kvmetrics"
	"github.com/go-steer/core-models/llm"
	"github.com/go-steer/core-models/profile"
	coreusage "github.com/go-steer/core-models/usage"

	"github.com/go-steer/core-agent/v3/internal/modelretry"
	"github.com/go-steer/core-agent/v3/pkg/config"
	"github.com/go-steer/core-agent/v3/pkg/models"
	"github.com/go-steer/core-agent/v3/pkg/taskclass"
)

// Provider aliases and default models for the eval suite.
const (
	ProviderGemini          = "gemini"
	ProviderVertex          = "vertex"
	ProviderVertexExpress   = "vertex-express"
	ProviderAnthropic       = "anthropic"
	ProviderAnthropicVertex = "anthropic-vertex"

	EnvAnthropicAPIKey        = "ANTHROPIC_API_KEY" // #nosec G101 -- env var name
	EnvAnthropicVertexProject = "ANTHROPIC_VERTEX_PROJECT_ID"

	DefaultAnthropicModel      = "claude-opus-5"
	DefaultAnthropicSmallModel = "claude-haiku-4-5"
)

// Prompt-cache breakpoint TTLs.
const (
	PromptCacheTTL5m = "5m"
	PromptCacheTTL1h = "1h"
)

// PromptCache configures Claude prompt caching for the model under test.
type PromptCache struct {
	Enabled *bool  `yaml:"enabled,omitempty"`
	TTL     string `yaml:"ttl,omitempty"`
}

// On resolves Enabled against the default: absent is on.
func (p PromptCache) On() bool { return p.Enabled == nil || *p.Enabled }

// EffectiveTTL returns TTL with the 5m default applied.
func (p PromptCache) EffectiveTTL() string {
	if p.TTL == PromptCacheTTL1h {
		return PromptCacheTTL1h
	}
	return PromptCacheTTL5m
}

// ModelOptions configures how BuildModel constructs a model.
type ModelOptions struct {
	PromptCache PromptCache
}

var profileRegistry struct {
	mu       sync.RWMutex
	declared []profile.Profile
	opened   map[string]coremodels.Provider
	prices   *priceTable
}

// RegisterProfiles sets the declared provider profiles.
func RegisterProfiles(ps []profile.Profile) {
	profileRegistry.mu.Lock()
	defer profileRegistry.mu.Unlock()
	profileRegistry.declared = slices.Clone(ps)
	profileRegistry.opened = nil
	profileRegistry.prices = nil
}

// DiscoverProviders loads provider profiles declared in the nearest
// .agents/config.json if present.
func DiscoverProviders() ([]profile.Profile, error) {
	path, _, err := config.Find("")
	if err != nil || path == "" {
		return nil, nil
	}
	cfg, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	return cfg.Providers, nil
}

func isCoreProvider(provider string) bool {
	switch provider {
	case "", ProviderGemini, ProviderVertex, ProviderAnthropic, ProviderAnthropicVertex, "echo", "scripted":
		return true
	}
	return false
}

// LookupProfile returns the profile a --provider names, extends applied.
func LookupProfile(provider string) (profile.Profile, bool, error) {
	if isCoreProvider(provider) {
		return profile.Profile{}, false, nil
	}
	profileRegistry.mu.RLock()
	declared := profileRegistry.declared
	profileRegistry.mu.RUnlock()
	p, err := profile.Find(provider, declared)
	if err != nil {
		if strings.HasPrefix(err.Error(), "no profile named") {
			return profile.Profile{}, false, nil
		}
		return profile.Profile{}, false, err
	}
	p, err = profile.Expand(p)
	if err != nil {
		return profile.Profile{}, false, err
	}
	return p, true, nil
}

// IsProfileProvider reports whether provider names a provider profile.
func IsProfileProvider(provider string) bool {
	_, ok, _ := LookupProfile(provider)
	return ok
}

// KVSampler returns a KV-cache sampler for the self-hosted server a
// provider profile names, or nil when there is nothing to sample.
func KVSampler(ctx context.Context, provider, modelName string) (*kvmetrics.Sampler, error) {
	p, ok, err := LookupProfile(provider)
	if err != nil || !ok {
		return nil, err
	}
	if p.MetricsURL == "" || p.Metrics == "" || p.Metrics == profile.MetricsNone {
		return nil, nil
	}
	r, err := profile.Resolve(ctx, p, models.ResolveProfileOptions(profile.Options{}))
	if err != nil {
		return nil, err
	}
	s, err := kvmetrics.FromProfile(r, kvmetrics.Options{Model: modelName})
	if errors.Is(err, kvmetrics.ErrNoMetrics) {
		return nil, nil
	}
	return s, err
}

var offlineFakes = map[string]bool{
	"echo": true, "mast-echo": true,
	"toolactor": true, "mast-toolactor": true,
	"scripted": true,
}

// IsOfflineFake reports whether name is an offline test double.
func IsOfflineFake(name string) bool { return offlineFakes[name] }

// BuildModel constructs the model.LLM for the given provider alias and model name.
func BuildModel(ctx context.Context, provider, name string, mo ModelOptions) (model.LLM, error) {
	switch {
	case name == "echo":
		return &evalEchoModel{name: "mast-echo"}, nil
	case strings.HasPrefix(name, "gemini-"):
		return buildGemini(ctx, provider, name)
	case strings.HasPrefix(name, "claude-"):
		return buildClaude(ctx, provider, name, mo)
	case IsProfileProvider(provider):
		return buildProfileModel(ctx, provider, name, mo)
	default:
		return nil, fmt.Errorf("unknown model %q (want `echo`, a `gemini-*` or `claude-*` model id, or --provider naming a provider profile)", name)
	}
}

// NewRuntimeModel wraps BuildModel in the shared transient-error retry policy.
func NewRuntimeModel(ctx context.Context, provider, name string, mo ModelOptions) (model.LLM, error) {
	m, err := BuildModel(ctx, provider, name, mo)
	if err != nil {
		return nil, err
	}
	if IsOfflineFake(name) {
		return m, nil
	}
	return modelretry.Shared().Wrap(m), nil
}

// NewModelResolver binds a specialist's model override to a concrete model.LLM.
func NewModelResolver(ctx context.Context, provider, rootName string, root model.LLM, mo ModelOptions) ModelResolver {
	var (
		mu    sync.Mutex
		cache = map[string]model.LLM{}
	)
	return func(name string) (model.LLM, error) {
		if root != nil && (name == rootName || IsOfflineFake(rootName)) {
			return root, nil
		}
		mu.Lock()
		defer mu.Unlock()
		if m, ok := cache[name]; ok {
			return m, nil
		}
		m, err := NewRuntimeModel(ctx, provider, name, mo)
		if err != nil {
			return nil, err
		}
		cache[name] = m
		return m, nil
	}
}

// NewTierResolver binds a specialist's tier declaration to a concrete model.LLM.
func NewTierResolver(provider, rootName string, root model.LLM, resolve ModelResolver) TierResolver {
	return func(tier string) (model.LLM, error) {
		if root != nil && IsOfflineFake(rootName) {
			return root, nil
		}
		name, err := TierModelName(provider, rootName, tier)
		if err != nil {
			return nil, err
		}
		if resolve == nil {
			return nil, fmt.Errorf("tier %q resolves to model %q but no model resolver is configured", tier, name)
		}
		return resolve(name)
	}
}

func providerFamily(provider, rootModelName string) (string, error) {
	if provider != "" {
		return provider, nil
	}
	switch {
	case strings.HasPrefix(rootModelName, "gemini-"):
		return "gemini", nil
	case strings.HasPrefix(rootModelName, "claude-"):
		return "anthropic", nil
	default:
		return "", fmt.Errorf("cannot tell which provider to resolve a tier against from root model %q (pass --provider)", rootModelName)
	}
}

// TierModelName maps a `tier:` declaration to a concrete model id for the running provider.
func TierModelName(provider, rootModelName, tier string) (string, error) {
	if p, ok, err := LookupProfile(provider); err != nil {
		return "", fmt.Errorf("tier %q: %w", tier, err)
	} else if ok {
		if id := p.Tiers[profile.Tier(tier)]; id != "" {
			return id, nil
		}
		return "", fmt.Errorf("provider profile %q declares no %q tier (declare tiers.%s in the profile, or name the model)", p.Name, tier, tier)
	}
	family, err := providerFamily(provider, rootModelName)
	if err != nil {
		return "", fmt.Errorf("tier %q: %w", tier, err)
	}
	name := taskclass.ModelForTier(family, tier)
	if name == "" {
		return "", fmt.Errorf("tier %q: no model mapped for provider %q", tier, family)
	}
	return name, nil
}

// SpecModelName is the model id a spec will actually run on, or "" when it inherits the parent's.
func SpecModelName(spec Spec, provider, rootModelName string) string {
	if spec.Model != "" {
		return spec.Model
	}
	if spec.Tier == "" {
		return ""
	}
	name, err := TierModelName(provider, rootModelName, spec.Tier)
	if err != nil {
		return ""
	}
	return name
}

func buildGemini(ctx context.Context, provider, name string) (model.LLM, error) {
	opts := coremodels.Options{Resolve: models.ResolveProfileOptions(profile.Options{})}
	if !geminiOnVertex(provider) {
		return openVendor(ctx, ProviderGemini, name, opts)
	}
	if vertexExpress(provider) {
		p, _ := profile.Builtin(ProviderVertexExpress)
		p.Auth.AltEnv = []string{"GEMINI_API_KEY"}
		return openVendorProfile(ctx, p, name, opts)
	}
	if os.Getenv("GOOGLE_CLOUD_PROJECT") == "" {
		return nil, errors.New("vertex AI for a gemini-* model needs GOOGLE_CLOUD_PROJECT")
	}
	return openVendor(ctx, ProviderVertex, name, opts)
}

func buildClaude(ctx context.Context, provider, name string, mo ModelOptions) (model.LLM, error) {
	backend, err := anthropicBackend(provider)
	if err != nil {
		return nil, err
	}
	return openVendor(ctx, backend, name, coremodels.Options{
		PromptCache: claudeCache(mo.PromptCache),
		Resolve:     models.ResolveProfileOptions(profile.Options{}),
	})
}

func buildProfileModel(ctx context.Context, provider, name string, mo ModelOptions) (model.LLM, error) {
	profileRegistry.mu.RLock()
	declared := profileRegistry.declared
	profileRegistry.mu.RUnlock()
	prof, err := profile.Find(provider, declared)
	if err != nil {
		return nil, err
	}
	p, err := coremodels.Open(ctx, prof, coremodels.Options{
		PromptCache: claudeCache(mo.PromptCache),
		Resolve:     models.ResolveProfileOptions(profile.Options{}),
	})
	if err != nil {
		return nil, err
	}
	m, err := p.Model(ctx, name)
	if err != nil {
		return nil, err
	}
	return wrapUsageBridge(m), nil
}

func openVendor(ctx context.Context, builtin, name string, opts coremodels.Options) (model.LLM, error) {
	p, ok := profile.Builtin(builtin)
	if !ok {
		return nil, fmt.Errorf("core-models has no built-in profile %q", builtin)
	}
	return openVendorProfile(ctx, p, name, opts)
}

func openVendorProfile(ctx context.Context, p profile.Profile, name string, opts coremodels.Options) (model.LLM, error) {
	prov, err := coremodels.Open(ctx, p, opts)
	if err != nil {
		return nil, err
	}
	m, err := prov.Model(ctx, name)
	if err != nil {
		return nil, err
	}
	return wrapUsageBridge(m), nil
}

func claudeCache(pc PromptCache) *anthropic.CacheOptions {
	if !pc.On() {
		return &anthropic.CacheOptions{}
	}
	ttl := anthropic.TTL5m
	if pc.EffectiveTTL() == PromptCacheTTL1h {
		ttl = anthropic.TTL1h
	}
	return &anthropic.CacheOptions{System: true, History: true, TTL: ttl}
}

func geminiOnVertex(provider string) bool {
	if provider == ProviderVertex {
		return true
	}
	if v, ok := os.LookupEnv("GOOGLE_GENAI_USE_ENTERPRISE"); ok {
		return strings.EqualFold(v, "1") || strings.EqualFold(v, "true")
	}
	v, ok := os.LookupEnv("GOOGLE_GENAI_USE_VERTEXAI")
	return ok && (strings.EqualFold(v, "1") || strings.EqualFold(v, "true"))
}

func vertexExpress(provider string) bool {
	if provider == ProviderVertex || !geminiOnVertex(provider) {
		return false
	}
	if os.Getenv("GOOGLE_CLOUD_PROJECT") != "" || os.Getenv("GOOGLE_CLOUD_LOCATION") != "" || os.Getenv("GOOGLE_CLOUD_REGION") != "" {
		return false
	}
	return os.Getenv("GOOGLE_API_KEY") != "" || os.Getenv("GEMINI_API_KEY") != ""
}

func anthropicBackend(provider string) (string, error) {
	switch provider {
	case ProviderAnthropic, ProviderAnthropicVertex:
		return provider, nil
	case ProviderGemini, ProviderVertex, "":
		if os.Getenv(EnvAnthropicAPIKey) != "" {
			return ProviderAnthropic, nil
		}
		if os.Getenv(EnvAnthropicVertexProject) != "" || os.Getenv("GOOGLE_CLOUD_PROJECT") != "" {
			return ProviderAnthropicVertex, nil
		}
		return "", fmt.Errorf("claude-* models need ANTHROPIC_API_KEY or a Vertex project (ANTHROPIC_VERTEX_PROJECT_ID / GOOGLE_CLOUD_PROJECT)")
	default:
		if IsProfileProvider(provider) {
			return anthropicBackend("")
		}
		return "", fmt.Errorf("provider %q cannot serve claude-* models", provider)
	}
}

// Backend resolves the --provider alias and environment to the pricing backend.
func Backend(provider, modelName string) string {
	switch {
	case IsOfflineFake(modelName):
		return ""
	case strings.HasPrefix(modelName, "claude-"):
		b, err := anthropicBackend(provider)
		if err != nil {
			return ""
		}
		return b
	case strings.HasPrefix(modelName, "gemini-"):
		if geminiOnVertex(provider) {
			return ProviderVertex
		}
		return ProviderGemini
	default:
		if p, ok, _ := LookupProfile(provider); ok {
			return p.BackendName()
		}
		return ""
	}
}

type usageDetail struct {
	cacheRead    *int64
	cacheWrite   *int64
	cacheWrite1h *int64
	reasoning    *int64
	servedModel  string
}

func (d *usageDetail) UsageDetail() (*int64, *int64, *int64, *int64, string) {
	return d.cacheRead, d.cacheWrite, d.cacheWrite1h, d.reasoning, d.servedModel
}

type evalUsageBridge struct {
	inner model.LLM
}

func wrapUsageBridge(m llm.LLM) model.LLM {
	return evalUsageBridge{inner: adkv2.Wrap(m)}
}

func (b evalUsageBridge) Name() string { return b.inner.Name() }

func (b evalUsageBridge) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		for resp, err := range b.inner.GenerateContent(ctx, req, stream) {
			if resp != nil {
				models.BridgeUsage(resp)
				if d, ok := coreusage.FromMetadata(resp.CustomMetadata); ok {
					if resp.CustomMetadata == nil {
						resp.CustomMetadata = map[string]any{}
					}
					resp.CustomMetadata[DetailKey] = &usageDetail{
						cacheRead:    d.CacheReadTokens,
						cacheWrite:   d.CacheWriteTokens,
						cacheWrite1h: d.CacheWrite1hTokens,
						reasoning:    d.ReasoningTokens,
						servedModel:  d.ServedModel,
					}
				}
			}
			if !yield(resp, err) {
				return
			}
		}
	}
}
