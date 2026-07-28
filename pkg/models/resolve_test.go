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

package models_test

import (
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/models"

	// Register the real provider constructors so Resolve routes the
	// same way a production binary does. anthropic registers both
	// "anthropic" and "anthropic-vertex"; mock registers "echo" and
	// "scripted". No credentials are needed — the tests below stop at
	// the constructors' own validation errors.
	_ "github.com/go-steer/core-agent/v2/pkg/models/anthropic"
	_ "github.com/go-steer/core-agent/v2/pkg/models/mock"
)

// providerEnvVars is every env var Resolve / autoDetectProvider (and
// the constructors the tests route into) consult. Each test clears
// all of them via t.Setenv before applying its own case so results
// don't depend on the machine running the tests. Never set real creds.
var providerEnvVars = []string{
	"GOOGLE_GENAI_USE_VERTEXAI",
	"GOOGLE_CLOUD_PROJECT",
	"GOOGLE_CLOUD_LOCATION",
	"GOOGLE_API_KEY",
	"GEMINI_API_KEY",
	"ANTHROPIC_API_KEY",
	"ANTHROPIC_VERTEX_PROJECT_ID",
	"CLOUD_ML_REGION",
}

func clearProviderEnv(t *testing.T) {
	t.Helper()
	for _, k := range providerEnvVars {
		t.Setenv(k, "")
	}
}

func TestAutoDetectProvider(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{
			name: "empty env detects nothing",
			env:  nil,
			want: "",
		},
		{
			name: "vertex opt-in with project",
			env: map[string]string{
				"GOOGLE_GENAI_USE_VERTEXAI": "true",
				"GOOGLE_CLOUD_PROJECT":      "proj-1",
			},
			want: config.ProviderVertex,
		},
		{
			name: "vertex opt-in without project is ignored",
			env: map[string]string{
				"GOOGLE_GENAI_USE_VERTEXAI": "true",
			},
			want: "",
		},
		{
			name: "vertex flag must be exactly true",
			env: map[string]string{
				"GOOGLE_GENAI_USE_VERTEXAI": "1",
				"GOOGLE_CLOUD_PROJECT":      "proj-1",
			},
			want: "",
		},
		{
			name: "google api key detects gemini",
			env:  map[string]string{"GOOGLE_API_KEY": "fake-key"},
			want: config.ProviderGemini,
		},
		{
			name: "gemini api key detects gemini",
			env:  map[string]string{"GEMINI_API_KEY": "fake-key"},
			want: config.ProviderGemini,
		},
		{
			name: "anthropic api key detects anthropic",
			env:  map[string]string{"ANTHROPIC_API_KEY": "fake-key"},
			want: config.ProviderAnthropic,
		},
		{
			name: "vertex wins over gemini key",
			env: map[string]string{
				"GOOGLE_GENAI_USE_VERTEXAI": "true",
				"GOOGLE_CLOUD_PROJECT":      "proj-1",
				"GEMINI_API_KEY":            "fake-key",
			},
			want: config.ProviderVertex,
		},
		{
			name: "gemini key wins over anthropic key",
			env: map[string]string{
				"GOOGLE_API_KEY":    "fake-key",
				"ANTHROPIC_API_KEY": "fake-key",
			},
			want: config.ProviderGemini,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearProviderEnv(t)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			if got := models.AutoDetectProvider(); got != tt.want {
				t.Errorf("AutoDetectProvider() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolve(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		cfg  func() *config.Config

		wantName string // Provider.Name() on success ("" means expect error)
		wantErr  string // substring the error must carry
	}{
		{
			name:     "explicit echo provider resolves without env",
			cfg:      cfgWithProvider(config.ProviderEcho),
			wantName: config.ProviderEcho,
		},
		{
			name: "explicit provider wins over env auto-detection",
			env:  map[string]string{"ANTHROPIC_API_KEY": "fake-key"},
			cfg:  cfgWithProvider(config.ProviderEcho),
			// If Resolve consulted the env first this would come back
			// as "anthropic" — pin the precedence.
			wantName: config.ProviderEcho,
		},
		{
			name:    "scripted without a script errors in the constructor",
			cfg:     cfgWithProvider(config.ProviderScripted),
			wantErr: "mock.script is required",
		},
		{
			name:    "anthropic without a key errors in the constructor",
			cfg:     cfgWithProvider(config.ProviderAnthropic),
			wantErr: "api key is required",
		},
		{
			name: "anthropic-vertex routes to the vertex constructor",
			cfg:  cfgWithProvider(config.ProviderAnthropicVertex),
			// With no project anywhere the constructor fails before
			// touching ADC — proves Resolve routed to the right
			// constructor without needing credentials.
			wantErr: "anthropic-vertex: project is required",
		},
		{
			name:    "unknown provider names the registry",
			cfg:     cfgWithProvider("no-such-provider"),
			wantErr: `unknown provider "no-such-provider"`,
		},
		{
			name:    "no provider and no env is a clear error",
			cfg:     cfgWithProvider(""),
			wantErr: "no provider configured and none could be auto-detected",
		},
		{
			name: "empty provider auto-detects anthropic from env",
			env:  map[string]string{"ANTHROPIC_API_KEY": "fake-key"},
			cfg:  cfgWithProvider(""),
			// The constructor accepts the (fake) key without a network
			// call, so resolution succeeds end-to-end.
			wantName: config.ProviderAnthropic,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearProviderEnv(t)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			p, err := models.Resolve(tt.cfg())
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("Resolve() = %v, want error containing %q", p, tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Resolve() error = %q, want it to contain %q", err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Resolve(): %v", err)
			}
			if p.Name() != tt.wantName {
				t.Errorf("Resolve().Name() = %q, want %q", p.Name(), tt.wantName)
			}
		})
	}
}

func cfgWithProvider(name string) func() *config.Config {
	return func() *config.Config {
		cfg := &config.Config{}
		cfg.Model.Provider = name
		return cfg
	}
}
