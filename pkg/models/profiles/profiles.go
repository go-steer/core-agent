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

// Package profiles is the models.Provider for provider profiles: every
// model.provider beyond core-agent's own gemini / vertex / anthropic /
// anthropic-vertex / echo / scripted. The adapters live in
// github.com/go-steer/core-models (docs/model-support-design.md); this
// package opens a profile there, adapts its models to ADK v1 through
// core-models' adkv1 shim, and re-keys its usage record into the shape
// pkg/usage reads.
//
// Import it for its side effect, as with the other backends:
//
//	import _ "github.com/go-steer/core-agent/v2/pkg/models/profiles"
package profiles

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"sync"

	"google.golang.org/adk/model"

	coremodels "github.com/go-steer/core-models"
	"github.com/go-steer/core-models/adkv1"
	"github.com/go-steer/core-models/profile"
	coreusage "github.com/go-steer/core-models/usage"

	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/models"
	"github.com/go-steer/core-agent/v2/pkg/usage"
)

func init() {
	models.RegisterProfiles(New)
}

// Provider is a models.Provider backed by one core-models profile.
type Provider struct {
	p coremodels.Provider
}

var (
	_ models.Provider            = (*Provider)(nil)
	_ models.SmallModelDefaulter = (*Provider)(nil)
)

// opened caches one core-models provider per profile per process, so
// the parent and every subagent on the same profile share its HTTP
// client. Keyed by the declaration itself, not the name, so a config
// reloaded with a changed profile opens the new one instead of reusing
// the old.
var opened sync.Map // cacheKey(profile) → coremodels.Provider

func cacheKey(p profile.Profile) string {
	b, err := json.Marshal(p)
	if err != nil {
		return p.Name
	}
	return string(b)
}

// New builds the Provider cfg.Model.Provider names. Opening resolves
// the profile against the environment — base URL variables, the API
// key — so a missing credential fails here, at startup, naming the
// profile.
func New(cfg *config.Config) (models.Provider, error) {
	name := cfg.Model.Provider
	prof, ok := cfg.Profile(name)
	if !ok {
		return nil, fmt.Errorf("models: %q is not a provider profile (have %v)", name, cfg.ProfileNames())
	}
	// The openai-chat dialect sends function tools only. A config that
	// turned on a server-side search or code tool would otherwise run
	// without it and never say so.
	if on := builtinsOn(cfg.Model.BuiltinTools); len(on) > 0 {
		return nil, fmt.Errorf("models: provider profile %q cannot run the server-side built-in tools %v that model.builtin_tools turns on; turn them off", name, on)
	}
	key := cacheKey(prof)
	if p, ok := opened.Load(key); ok {
		return &Provider{p: p.(coremodels.Provider)}, nil
	}
	p, err := coremodels.Open(context.Background(), prof, coremodels.Options{})
	if err != nil {
		return nil, fmt.Errorf("models: %w", err)
	}
	actual, _ := opened.LoadOrStore(key, p)
	return &Provider{p: actual.(coremodels.Provider)}, nil
}

// Name reports the profile's name.
func (p *Provider) Name() string { return p.p.Name() }

// Backend is the identity the profile's prices are keyed on.
func (p *Provider) Backend() string { return p.p.Backend() }

// Profile is the resolved profile, extends applied.
func (p *Provider) Profile() profile.Profile { return p.p.Profile() }

// DefaultSmallModel is the profile's small tier, or "" when it declares
// none — agentic subtasks then inherit the parent's model.
func (p *Provider) DefaultSmallModel() string { return p.p.DefaultSmallModel() }

// Model returns modelID on the profile's server, refusing one a closed
// profile does not list.
func (p *Provider) Model(ctx context.Context, modelID string) (model.LLM, error) {
	m, err := p.p.Model(ctx, modelID)
	if err != nil {
		return nil, err
	}
	return usageBridge{inner: adkv1.Wrap(m)}, nil
}

// DefaultModel is the model to run on a profile when the operator
// named none: the profile's tier for the task class (mid when no class
// is declared). Profile tiers extend taskclass's Go tables rather than
// outrank them, and a profile is the only source for its own models.
func DefaultModel(cfg *config.Config, name, tier string) (string, error) {
	prof, ok := cfg.Profile(name)
	if !ok {
		return "", fmt.Errorf("%q is not a provider profile", name)
	}
	prof, err := profile.Expand(prof)
	if err != nil {
		return "", err
	}
	if tier == "" {
		tier = string(profile.Mid)
	}
	if id := prof.Tiers[profile.Tier(tier)]; id != "" {
		return id, nil
	}
	return "", fmt.Errorf("provider profile %q declares no %q tier; name the model with --model or model.name, or declare tiers.%s in the profile", name, tier, tier)
}

// builtinsOn lists the server-side built-ins cfg explicitly turns on,
// in the neutral vocabulary of model.builtin_tools. Absent fields keep
// the provider default, and a profile's default is none.
func builtinsOn(bt *config.BuiltinToolsConfig) []string {
	if bt == nil {
		return nil
	}
	var on []string
	for _, f := range []struct {
		name string
		v    *bool
	}{
		{"web_search", bt.WebSearch},
		{"url_context", bt.URLContext},
		{"code_execution", bt.CodeExecution},
	} {
		if f.v != nil && *f.v {
			on = append(on, f.name)
		}
	}
	return on
}

// usageBridge copies the parts of core-models' usage record that
// genai's UsageMetadata cannot hold into the CustomMetadata sidecar
// pkg/usage reads. The openai-chat adapter already fills the genai
// fields (prompt, completion, cache reads, reasoning); what remains is
// the cache-write bucket, which only usage.CacheCreationTokensMetadataKey
// carries — the key pkg/models/anthropic writes and usage.Rebuild
// reads back from an event log. ToolUseTokens is deliberately not
// mapped: no core-models dialect reports it today, and genai's field
// for it is outside the prompt count, so guessing a mapping risks
// counting tokens twice.
type usageBridge struct {
	inner model.LLM
}

func (b usageBridge) Name() string { return b.inner.Name() }

func (b usageBridge) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		for resp, err := range b.inner.GenerateContent(ctx, req, stream) {
			if resp != nil {
				bridgeUsage(resp)
			}
			if !yield(resp, err) {
				return
			}
		}
	}
}

func bridgeUsage(resp *model.LLMResponse) {
	d, ok := coreusage.FromMetadata(resp.CustomMetadata)
	if !ok {
		return
	}
	if d.CacheWriteTokens != nil {
		resp.CustomMetadata[usage.CacheCreationTokensMetadataKey] = *d.CacheWriteTokens
	}
	if d.CacheWrite1hTokens != nil {
		resp.CustomMetadata[usage.CacheCreation1hTokensMetadataKey] = *d.CacheWrite1hTokens
	}
}
