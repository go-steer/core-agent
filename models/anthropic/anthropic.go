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

	"github.com/go-steer/core-agent/config"
	"github.com/go-steer/core-agent/models"
)

// DefaultModel is used when LLMRequest.Model is empty. We follow the
// claude-api skill's guidance and default to the most capable Opus.
const DefaultModel = "claude-opus-4-7"

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
	name        string
	client      anthropic.Client
	cacheSystem bool
}

// Option configures a Provider at construction.
type Option func(*Provider)

// WithCacheSystem enables prompt caching on the last system block by
// default. Off by default — turn it on once you've confirmed the
// system prompt is stable across turns (otherwise the cache write
// premium is paid for nothing).
func WithCacheSystem(on bool) Option { return func(p *Provider) { p.cacheSystem = on } }

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
		name:   config.ProviderAnthropic,
		client: anthropic.NewClient(option.WithAPIKey(apiKey)),
	}
	for _, opt := range opts {
		opt(p)
	}
	return p, nil
}

// Name reports the provider identity ("anthropic" or "anthropic-vertex").
func (p *Provider) Name() string { return p.name }

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
		client:      p.client,
		modelID:     modelID,
		cacheSystem: p.cacheSystem,
	}, nil
}

func newProvider(cfg *config.Config) (models.Provider, error) {
	key := ""
	if cfg.Model.Anthropic != nil {
		key = cfg.Model.Anthropic.APIKey
	}
	return New(key)
}
