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

package gemini

import (
	"strings"
	"testing"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/models"
)

func TestResolve_ExplicitGemini_NoKey(t *testing.T) {
	t.Setenv("GOOGLE_API_KEY", "")
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("GOOGLE_GENAI_USE_VERTEXAI", "")
	t.Setenv("GOOGLE_CLOUD_PROJECT", "")

	cfg := config.DefaultConfig()
	cfg.Model.Provider = config.ProviderGemini
	_, err := models.Resolve(cfg)
	if err == nil || !strings.Contains(err.Error(), "api key is required") {
		t.Fatalf("expected api key error, got %v", err)
	}
}

func TestResolve_ExplicitGemini_GEMINI_API_KEY(t *testing.T) {
	t.Setenv("GOOGLE_API_KEY", "")
	t.Setenv("GEMINI_API_KEY", "test-key")

	cfg := config.DefaultConfig()
	cfg.Model.Provider = config.ProviderGemini
	p, err := models.Resolve(cfg)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if p.Name() != config.ProviderGemini {
		t.Errorf("provider name = %q, want %q", p.Name(), config.ProviderGemini)
	}
}

func TestResolve_ExplicitGemini_WithKey(t *testing.T) {
	t.Setenv("GOOGLE_API_KEY", "test-key")
	cfg := config.DefaultConfig()
	cfg.Model.Provider = config.ProviderGemini
	p, err := models.Resolve(cfg)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if p.Name() != config.ProviderGemini {
		t.Errorf("provider name = %q, want %q", p.Name(), config.ProviderGemini)
	}
}

func TestResolve_ExplicitVertex_MissingProject(t *testing.T) {
	t.Setenv("GOOGLE_CLOUD_PROJECT", "")
	t.Setenv("GOOGLE_CLOUD_LOCATION", "")
	cfg := config.DefaultConfig()
	cfg.Model.Provider = config.ProviderVertex
	_, err := models.Resolve(cfg)
	if err == nil || !strings.Contains(err.Error(), "project and location are required") {
		t.Fatalf("expected vertex creds error, got %v", err)
	}
}

func TestResolve_ExplicitVertex_FromConfig(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Model.Provider = config.ProviderVertex
	cfg.Model.Vertex = &config.VertexConfig{Project: "p", Location: "us-central1"}
	p, err := models.Resolve(cfg)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if p.Name() != config.ProviderVertex {
		t.Errorf("provider name = %q, want %q", p.Name(), config.ProviderVertex)
	}
}

func TestResolve_AutoDetectVertex(t *testing.T) {
	t.Setenv("GOOGLE_GENAI_USE_VERTEXAI", "true")
	t.Setenv("GOOGLE_CLOUD_PROJECT", "p")
	t.Setenv("GOOGLE_CLOUD_LOCATION", "us-central1")
	t.Setenv("GOOGLE_API_KEY", "")
	t.Setenv("GEMINI_API_KEY", "")

	cfg := config.DefaultConfig()
	p, err := models.Resolve(cfg)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if p.Name() != config.ProviderVertex {
		t.Errorf("auto-detect picked %q, want vertex", p.Name())
	}
}

func TestResolve_AutoDetectGemini_GOOGLE_API_KEY(t *testing.T) {
	t.Setenv("GOOGLE_GENAI_USE_VERTEXAI", "")
	t.Setenv("GOOGLE_API_KEY", "k")
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("ANTHROPIC_API_KEY", "")

	cfg := config.DefaultConfig()
	p, err := models.Resolve(cfg)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if p.Name() != config.ProviderGemini {
		t.Errorf("auto-detect picked %q, want gemini", p.Name())
	}
}

func TestResolve_AutoDetectGemini_GEMINI_API_KEY(t *testing.T) {
	t.Setenv("GOOGLE_GENAI_USE_VERTEXAI", "")
	t.Setenv("GOOGLE_API_KEY", "")
	t.Setenv("GEMINI_API_KEY", "k")
	t.Setenv("ANTHROPIC_API_KEY", "")

	cfg := config.DefaultConfig()
	p, err := models.Resolve(cfg)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if p.Name() != config.ProviderGemini {
		t.Errorf("auto-detect picked %q, want gemini", p.Name())
	}
}

// TestProviderImplementsSmallModelDefaulter is a compile-time sanity
// check that *Provider satisfies models.SmallModelDefaulter, so
// ResolveSmallModel routes Gemini subtasks to the cheap-tier default
// without requiring --agentic-small-model on the CLI.
func TestProviderImplementsSmallModelDefaulter(t *testing.T) {
	var _ models.SmallModelDefaulter = (*Provider)(nil)
}

func TestDefaultSmallModel(t *testing.T) {
	// Zero-value Provider — DefaultSmallModel is state-independent.
	p := &Provider{}
	if got, want := p.DefaultSmallModel(), DefaultSmallModelID; got != want {
		t.Errorf("DefaultSmallModel() = %q, want %q", got, want)
	}
	if DefaultSmallModelID != "gemini-2.5-flash" {
		t.Errorf("DefaultSmallModelID = %q; expected gemini-2.5-flash (the cheap-tier alias used across the codebase)", DefaultSmallModelID)
	}
}

// TestNewAPIKey_TracedHTTPClient pins the #325 contract: the direct
// Gemini API backend supplies its own otelhttp-instrumented client
// (genai would otherwise fall back to an untraced bare http.Client).
func TestNewAPIKey_TracedHTTPClient(t *testing.T) {
	p, err := NewAPIKey("test-key")
	if err != nil {
		t.Fatalf("NewAPIKey: %v", err)
	}
	if p.cfg.HTTPClient == nil {
		t.Fatal("NewAPIKey: cfg.HTTPClient is nil; direct Gemini API calls would be untraced")
	}
	if _, ok := p.cfg.HTTPClient.Transport.(*otelhttp.Transport); !ok {
		t.Errorf("NewAPIKey: transport is %T, want *otelhttp.Transport", p.cfg.HTTPClient.Transport)
	}
}

// TestNewVertex_NoHTTPClientOverride pins the other half of the #325
// contract: the Vertex backend must NOT set HTTPClient. genai only
// wires ADC credentials into a client it builds itself, and that
// client is already otelhttp-instrumented via
// cloud.google.com/go/auth/httptransport's default telemetry —
// overriding would break auth and double-instrument.
func TestNewVertex_NoHTTPClientOverride(t *testing.T) {
	p, err := NewVertex("proj", "us-central1")
	if err != nil {
		t.Fatalf("NewVertex: %v", err)
	}
	if p.cfg.HTTPClient != nil {
		t.Error("NewVertex: cfg.HTTPClient is set; this bypasses genai's ADC wiring (auth breaks) and double-instruments the transport")
	}
}
