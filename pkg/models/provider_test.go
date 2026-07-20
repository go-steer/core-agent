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
	"context"
	"testing"

	adkmodel "google.golang.org/adk/model"

	"github.com/go-steer/core-agent/v2/pkg/models"
)

// stubProvider implements models.Provider but NOT SmallModelDefaulter.
// Used to verify ResolveSmallModel falls through to "" for providers
// without a cheap-tier concept (mirrors the mock/echo/scripted case).
type stubProvider struct{ name string }

func (s stubProvider) Name() string                                            { return s.name }
func (s stubProvider) Model(_ context.Context, _ string) (adkmodel.LLM, error) { return nil, nil }

// stubProviderWithSmall implements both Provider and SmallModelDefaulter.
// Used to verify ResolveSmallModel calls DefaultSmallModel when the
// override is empty (mirrors the gemini/anthropic case).
type stubProviderWithSmall struct {
	stubProvider
	small string
}

func (s stubProviderWithSmall) DefaultSmallModel() string { return s.small }

func TestResolveSmallModel(t *testing.T) {
	tests := []struct {
		name     string
		provider models.Provider
		override string
		want     string
	}{
		{
			name:     "operator override wins over default",
			provider: stubProviderWithSmall{stubProvider{"gemini"}, "gemini-2.5-flash"},
			override: "claude-haiku-4-5",
			want:     "claude-haiku-4-5",
		},
		{
			name:     "operator override wins when provider has no default",
			provider: stubProvider{"echo"},
			override: "gemini-2.5-flash",
			want:     "gemini-2.5-flash",
		},
		{
			name:     "provider default used when override empty",
			provider: stubProviderWithSmall{stubProvider{"gemini"}, "gemini-2.5-flash"},
			override: "",
			want:     "gemini-2.5-flash",
		},
		{
			name:     "no override + no default → empty (inherit parent)",
			provider: stubProvider{"echo"},
			override: "",
			want:     "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := models.ResolveSmallModel(tt.provider, tt.override)
			if got != tt.want {
				t.Errorf("ResolveSmallModel(%q, %q) = %q, want %q", tt.provider.Name(), tt.override, got, tt.want)
			}
		})
	}
}

// TestResolveMCPSmallModel pins the three-layer precedence chain for
// the MCP-wrap subagent (#223): mcp-specific override → general
// agentic override → provider cheap-tier default → "" (inherit).
//
// Regression signal: if this test fails, operators lose the ability
// to tune MCP-wrap and built-in-wrap subagents independently — the
// exact use case the extra layer was introduced for.
func TestResolveMCPSmallModel(t *testing.T) {
	geminiCheap := stubProviderWithSmall{stubProvider{"gemini"}, "gemini-2.5-flash"}
	tests := []struct {
		name        string
		provider    models.Provider
		mcpOverride string
		agentic     string
		want        string
	}{
		{
			name:        "mcp override wins over agentic override",
			provider:    geminiCheap,
			mcpOverride: "gemini-2.5-pro",
			agentic:     "claude-haiku-4-5",
			want:        "gemini-2.5-pro",
		},
		{
			name:        "mcp override wins over provider default",
			provider:    geminiCheap,
			mcpOverride: "claude-haiku-4-5",
			agentic:     "",
			want:        "claude-haiku-4-5",
		},
		{
			name:        "agentic override wins when mcp empty",
			provider:    geminiCheap,
			mcpOverride: "",
			agentic:     "claude-haiku-4-5",
			want:        "claude-haiku-4-5",
		},
		{
			name:        "provider default when both empty",
			provider:    geminiCheap,
			mcpOverride: "",
			agentic:     "",
			want:        "gemini-2.5-flash",
		},
		{
			name:        "empty when provider has no default and both empty",
			provider:    stubProvider{"echo"},
			mcpOverride: "",
			agentic:     "",
			want:        "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := models.ResolveMCPSmallModel(tt.provider, tt.mcpOverride, tt.agentic)
			if got != tt.want {
				t.Errorf("ResolveMCPSmallModel(%q, %q, %q) = %q, want %q",
					tt.provider.Name(), tt.mcpOverride, tt.agentic, got, tt.want)
			}
		})
	}
}
