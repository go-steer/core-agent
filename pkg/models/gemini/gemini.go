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

// Package gemini implements models.Provider for the Gemini family,
// covering both the public Gemini API (API-key auth) and Vertex AI
// (Application Default Credentials + GCP project).
//
// The two are exposed as distinct provider names ("gemini" and "vertex")
// so users and automation can pin to a backend explicitly. Both delegate
// to google.golang.org/adk/model/gemini under the hood.
package gemini

import (
	"context"
	"fmt"
	"os"

	adkmodel "google.golang.org/adk/model"
	adkgemini "google.golang.org/adk/model/gemini"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/pkg/config"
	"github.com/go-steer/core-agent/pkg/models"
)

func init() {
	models.Register(config.ProviderGemini, newGeminiAPI)
	models.Register(config.ProviderVertex, newVertexAI)
}

// Provider is the Gemini-family implementation of models.Provider.
type Provider struct {
	name     string
	cfg      *genai.ClientConfig
	prefix   string
	builtins BuiltinTools
}

// Name reports the provider identity (e.g. "gemini" or "vertex").
func (p *Provider) Name() string { return p.name }

// DefaultSmallModelID is the Gemini cheap-tier model used by default
// for agentic subtasks when the operator hasn't pinned one with
// --agentic-small-model.
const DefaultSmallModelID = "gemini-2.5-flash"

// DefaultSmallModel satisfies models.SmallModelDefaulter so core-agent
// can route subtask digesting to a cheap-tier Gemini model without
// requiring the operator to set --agentic-small-model.
func (p *Provider) DefaultSmallModel() string { return DefaultSmallModelID }

// Model constructs a model.LLM for the given model ID. When the
// Provider has any built-in tools enabled, the returned LLM is
// wrapped to inject them into Config.Tools on every request.
func (p *Provider) Model(ctx context.Context, modelID string) (adkmodel.LLM, error) {
	if modelID == "" {
		return nil, fmt.Errorf("%s: model id is required", p.prefix)
	}
	llm, err := adkgemini.NewModel(ctx, modelID, p.cfg)
	if err != nil {
		return nil, fmt.Errorf("%s: new model %q: %w", p.prefix, modelID, err)
	}
	isVertex := p.cfg != nil && p.cfg.Backend == genai.BackendVertexAI
	if tools := p.builtins.asTools(); len(tools) > 0 {
		return &builtinsLLM{
			inner:    llm,
			builtins: tools,
			// Direct Gemini API (BackendGeminiAPI) requires the
			// IncludeServerSideToolInvocations flag when combining
			// built-ins with function tools. Vertex AI rejects the
			// flag with "includeServerSideToolInvocations parameter
			// is not supported in Gemini Enterprise Agent Platform
			// (previously known as Vertex AI)" — it permits the
			// combination unconditionally instead.
			isDirectGeminiAPI: p.cfg != nil && p.cfg.Backend == genai.BackendGeminiAPI,
			// Vertex's streaming search-grounding path intermittently
			// emits chunks with empty Candidates[] (heartbeat-like,
			// carrying only UsageMetadata/ResponseID). ADK's stream
			// aggregator surfaces these as "empty response" errors
			// and aborts the stream. Tolerate the heartbeats so the
			// remaining grounded chunks can come through.
			tolerateEmptyChunks: isVertex,
		}, nil
	}
	if isVertex {
		return &builtinsLLM{
			inner:               llm,
			tolerateEmptyChunks: true,
		}, nil
	}
	return llm, nil
}

// NewAPIKey returns a Provider authenticated against the public Gemini API
// using key. Empty key is rejected so the failure mode is clear at startup.
//
// Built-in tools (Google Search + URL Context) are enabled by default;
// pass WithBuiltinTools / WithGoogleSearch / WithURLContext to override.
func NewAPIKey(key string, opts ...Option) (*Provider, error) {
	if key == "" {
		return nil, fmt.Errorf("gemini: api key is required (set GOOGLE_API_KEY or GEMINI_API_KEY, or model.api_key in .agents/config.json)")
	}
	p := &Provider{
		name:   config.ProviderGemini,
		prefix: "gemini",
		cfg: &genai.ClientConfig{
			APIKey:  key,
			Backend: genai.BackendGeminiAPI,
		},
		builtins: DefaultBuiltinTools(),
	}
	for _, opt := range opts {
		opt(p)
	}
	return p, nil
}

// NewVertex returns a Provider authenticated against Vertex AI for the
// given GCP project and location, using Application Default Credentials.
//
// Built-in tools (Google Search + URL Context) are enabled by default;
// pass WithBuiltinTools / WithGoogleSearch / WithURLContext to override.
func NewVertex(project, location string, opts ...Option) (*Provider, error) {
	if project == "" || location == "" {
		return nil, fmt.Errorf("vertex: project and location are required (set model.vertex.{project,location} in .agents/config.json or GOOGLE_CLOUD_PROJECT / GOOGLE_CLOUD_LOCATION env vars)")
	}
	p := &Provider{
		name:   config.ProviderVertex,
		prefix: "vertex",
		cfg: &genai.ClientConfig{
			Backend:  genai.BackendVertexAI,
			Project:  project,
			Location: location,
		},
		builtins: DefaultBuiltinTools(),
	}
	for _, opt := range opts {
		opt(p)
	}
	return p, nil
}

func newGeminiAPI(cfg *config.Config) (models.Provider, error) {
	key := cfg.Model.APIKey
	if key == "" {
		// GOOGLE_API_KEY is the umbrella name; GEMINI_API_KEY is the
		// one Gemini's own docs and tutorials use. Accept either.
		key = firstNonEmpty(os.Getenv("GOOGLE_API_KEY"), os.Getenv("GEMINI_API_KEY"))
	}
	return NewAPIKey(key)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func newVertexAI(cfg *config.Config) (models.Provider, error) {
	project, location := "", ""
	if cfg.Model.Vertex != nil {
		project = cfg.Model.Vertex.Project
		location = cfg.Model.Vertex.Location
	}
	if project == "" {
		project = os.Getenv("GOOGLE_CLOUD_PROJECT")
	}
	if location == "" {
		location = os.Getenv("GOOGLE_CLOUD_LOCATION")
	}
	return NewVertex(project, location)
}
