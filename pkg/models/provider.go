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

// Package models is the adapter layer between core-agent's
// configuration and concrete LLM backends. The Provider interface
// keeps the rest of the codebase free of provider-specific imports
// so additional backends plug in behind the same contract.
//
// Built-in providers:
//   - "gemini" / "vertex" — google.golang.org/adk/model/gemini
//   - "anthropic"         — Claude via github.com/anthropics/anthropic-sdk-go
//
// Each backend's package init() calls Register so importing the
// subpackage is enough to make the provider available.
package models

import (
	"context"
	"fmt"
	"os"

	"google.golang.org/adk/model"

	"github.com/go-steer/core-agent/pkg/config"
)

// Provider constructs concrete model.LLM instances on demand. A Provider
// is bound to one credential source (API key, Vertex project, etc.) at
// construction time; callers ask for specific models by ID through Model.
type Provider interface {
	// Name reports the provider identity ("gemini", "vertex", "anthropic").
	// Used for telemetry and diagnostic messages.
	Name() string

	// Model returns a usable model.LLM for the given model ID. The same
	// Provider may be asked for several models over its lifetime.
	Model(ctx context.Context, modelID string) (model.LLM, error)
}

// SmallModelDefaulter is an optional Provider extension. A Provider that
// implements this declares its preferred cheap-tier model — used by
// core-agent as the default for --agentic-small-model when the operator
// hasn't pinned one explicitly. Providers without a cheap-tier concept
// (echo, scripted) simply don't implement this; ResolveSmallModel
// returns "" for them and callers fall back to inheriting the parent's
// model.
type SmallModelDefaulter interface {
	DefaultSmallModel() string
}

// ResolveSmallModel picks the model ID that agentic subtasks should
// run on. Operator override (a non-empty explicit --agentic-small-model
// value) always wins. Otherwise: if p implements SmallModelDefaulter,
// return whatever it reports; if not, return "" — agentic wrappers
// treat empty as "inherit the parent's model."
func ResolveSmallModel(p Provider, override string) string {
	if override != "" {
		return override
	}
	if d, ok := p.(SmallModelDefaulter); ok {
		return d.DefaultSmallModel()
	}
	return ""
}

// Constructor builds a Provider from validated config. Tests register
// alternates via Register so resolution stays decoupled from the
// imports of any single backend.
type Constructor func(*config.Config) (Provider, error)

var registry = map[string]Constructor{}

// Register installs a Constructor under its provider name. Idiomatically
// called from package init() in each backend implementation.
func Register(name string, c Constructor) {
	registry[name] = c
}

// Resolve picks the right Provider for cfg, honoring (in order):
// explicit cfg.Model.Provider, then env-based auto-detection. Returns
// a clear error when no path is viable so the user knows which env var
// or config field to set.
func Resolve(cfg *config.Config) (Provider, error) {
	name := cfg.Model.Provider
	if name == "" {
		name = autoDetectProvider()
	}
	if name == "" {
		return nil, fmt.Errorf("models: no provider configured and none could be auto-detected; set model.provider in .agents/config.json or one of GOOGLE_API_KEY, GEMINI_API_KEY, ANTHROPIC_API_KEY, GOOGLE_GENAI_USE_VERTEXAI=true (with GOOGLE_CLOUD_PROJECT)")
	}
	c, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("models: unknown provider %q (registered: %v); did you forget to import the provider's package?", name, registeredNames())
	}
	return c(cfg)
}

// AutoDetectProvider walks the env to pick a default backend.
// Exported alias of autoDetectProvider for callers that need the
// provider name BEFORE constructing a Provider (e.g. cmd/core-agent's
// --task flag resolution needs the provider name to pick a model for
// a given tier without paying for full provider construction).
//
// Returns "" when no env-based default is detectable. Returns the
// same canonical name strings as Resolve would route to.
func AutoDetectProvider() string { return autoDetectProvider() }

// autoDetectProvider walks the env to pick a default backend. Order:
// Vertex (explicit opt-in) → Gemini API key → Anthropic API key.
//
// Gemini accepts either GOOGLE_API_KEY (the umbrella name) or
// GEMINI_API_KEY (the one Gemini's own docs and tutorials use).
func autoDetectProvider() string {
	if os.Getenv("GOOGLE_GENAI_USE_VERTEXAI") == "true" && os.Getenv("GOOGLE_CLOUD_PROJECT") != "" {
		return config.ProviderVertex
	}
	if os.Getenv("GOOGLE_API_KEY") != "" || os.Getenv("GEMINI_API_KEY") != "" {
		return config.ProviderGemini
	}
	if os.Getenv("ANTHROPIC_API_KEY") != "" {
		return config.ProviderAnthropic
	}
	return ""
}

func registeredNames() []string {
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	return out
}
