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

package models

import (
	"fmt"

	"github.com/go-steer/core-agent/v2/pkg/config"
)

// ProviderOptions is the programmatic counterpart of the config
// file's `model` block (#492): before it, the only way through the
// provider registry — Resolve — was to fabricate the on-disk
// *config.Config struct, which coupled every programmatic embedder
// to the file schema. Each per-provider struct below carries exactly
// the fields its backend routes on; New translates it through the
// same registry Resolve uses, so both paths construct identical
// providers. The MODEL ID is deliberately not routing state: pass it
// to Provider.Model(ctx, name) on the returned provider, exactly as
// with Resolve.
//
// Sealed (the toModelConfig method is unexported) so the set of
// option shapes evolves with the registry rather than by third-party
// implementation. Backends with richer knobs (thinking budgets,
// server-side tools, cache flags) keep exposing them on their own
// constructors and functional options — gemini.NewAPIKey /
// gemini.NewVertex / anthropic.New / anthropic.NewVertex remain the
// full-control path; these structs cover the routing layer's job:
// pick a backend and point it at credentials. Pass structs by value
// (a nil *GeminiAPI etc. would panic in the value-receiver call).
//
// Resolve remains the config-file adapter, unchanged.
type ProviderOptions interface {
	// toModelConfig returns the synthesized model block the registry
	// constructor consumes.
	toModelConfig() config.ModelConfig
}

// GeminiAPI selects the Gemini API backend (api-key auth). The
// model ID is NOT part of routing — pass it to Provider.Model(ctx,
// name) afterwards, exactly as with Resolve.
type GeminiAPI struct {
	// APIKey overrides GOOGLE_API_KEY / GEMINI_API_KEY. Empty falls
	// back to the environment, same as the config path.
	APIKey string
}

func (o GeminiAPI) toModelConfig() config.ModelConfig {
	return config.ModelConfig{Provider: config.ProviderGemini, APIKey: o.APIKey}
}

// GeminiVertex selects Gemini via Vertex AI (ADC auth).
type GeminiVertex struct {
	// Project and Location identify the Vertex deployment. Empty
	// values fall back to GOOGLE_CLOUD_PROJECT / GOOGLE_CLOUD_LOCATION,
	// same as the config path.
	Project  string
	Location string
}

func (o GeminiVertex) toModelConfig() config.ModelConfig {
	return config.ModelConfig{
		Provider: config.ProviderVertex,
		Vertex:   &config.VertexConfig{Project: o.Project, Location: o.Location},
	}
}

// AnthropicAPI selects the Anthropic API backend (api-key auth).
type AnthropicAPI struct {
	// APIKey overrides ANTHROPIC_API_KEY. Empty falls back to the
	// environment, same as the config path.
	APIKey string
}

func (o AnthropicAPI) toModelConfig() config.ModelConfig {
	return config.ModelConfig{
		Provider:  config.ProviderAnthropic,
		Anthropic: &config.AnthropicConfig{APIKey: o.APIKey},
	}
}

// AnthropicVertex selects Anthropic models served through Vertex AI
// Model Garden (ADC auth).
type AnthropicVertex struct {
	// Project and Region identify the Model Garden deployment. Empty
	// values fall back to the same env chain as the config path:
	// ANTHROPIC_VERTEX_PROJECT_ID then GOOGLE_CLOUD_PROJECT for the
	// project; CLOUD_ML_REGION then GOOGLE_CLOUD_LOCATION then the
	// "us-east5" default for the region.
	Project string
	Region  string
}

func (o AnthropicVertex) toModelConfig() config.ModelConfig {
	return config.ModelConfig{
		Provider: config.ProviderAnthropicVertex,
		Anthropic: &config.AnthropicConfig{
			Vertex: &config.VertexConfig{Project: o.Project, Location: o.Region},
		},
	}
}

// New constructs a Provider from per-provider options, routing
// through the same registry as Resolve — remember to blank-import
// the backend package (e.g. pkg/models/gemini) exactly as with
// Resolve. For auto-detection from the environment, use Resolve with
// a default config (or AutoDetectProvider for the name alone); New
// is deliberately explicit about the backend.
func New(opts ProviderOptions) (Provider, error) {
	if opts == nil {
		return nil, fmt.Errorf("models: New: options are required (use Resolve for config-file / env-driven selection)")
	}
	cfg := config.DefaultConfig()
	cfg.Model = opts.toModelConfig()
	return Resolve(cfg)
}
