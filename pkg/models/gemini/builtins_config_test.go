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
	"slices"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/config"
)

func ptr(b bool) *bool { return &b }

// #876: before this, the registry constructors read nothing from cfg
// for built-ins, so google_search and url_context were on for every
// deployment with no way to say otherwise. These tests pin the config
// surface that makes them expressible.
func TestBuiltinToolsFromConfig(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		bt   *config.BuiltinToolsConfig
		want BuiltinTools
	}{
		{
			// The overwhelmingly common case: no block at all leaves
			// the shipped defaults exactly as they were.
			name: "absent block keeps defaults",
			bt:   nil,
			want: DefaultBuiltinTools(),
		},
		{
			// Tri-state: an empty block is not "turn everything off".
			name: "empty block keeps defaults",
			bt:   &config.BuiltinToolsConfig{},
			want: DefaultBuiltinTools(),
		},
		{
			// The #876 motivating case, straight from the live 54-search
			// spiral: kill web search, keep everything else alone.
			name: "web_search false leaves url_context alone",
			bt:   &config.BuiltinToolsConfig{WebSearch: ptr(false)},
			want: BuiltinTools{URLContext: true},
		},
		{
			name: "all three off",
			bt: &config.BuiltinToolsConfig{
				WebSearch:     ptr(false),
				URLContext:    ptr(false),
				CodeExecution: ptr(false),
			},
			want: BuiltinTools{},
		},
		{
			// Enabling direction works too — code_execution is the one
			// that ships off, so this is the only way to get it.
			name: "code_execution true",
			bt:   &config.BuiltinToolsConfig{CodeExecution: ptr(true)},
			want: BuiltinTools{GoogleSearch: true, URLContext: true, CodeExecution: true},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := &config.Config{Model: config.ModelConfig{BuiltinTools: tc.bt}}
			p, err := NewAPIKey("k", builtinToolsFromConfig(cfg)...)
			if err != nil {
				t.Fatalf("NewAPIKey: %v", err)
			}
			if p.builtins != tc.want {
				t.Errorf("builtins = %+v, want %+v", p.builtins, tc.want)
			}
		})
	}
}

// The constructors the registry actually dispatches to are the ones
// that were ignoring cfg, so exercise those rather than only the helper.
func TestRegistryConstructors_HonorBuiltinToolsConfig(t *testing.T) {
	cfg := &config.Config{Model: config.ModelConfig{
		BuiltinTools: &config.BuiltinToolsConfig{WebSearch: ptr(false)},
		APIKey:       "k",
		Vertex:       &config.VertexConfig{Project: "p", Location: "global"},
	}}

	api, err := newGeminiAPI(cfg)
	if err != nil {
		t.Fatalf("newGeminiAPI: %v", err)
	}
	if got := api.(*Provider).builtins.GoogleSearch; got {
		t.Errorf("gemini: GoogleSearch still on despite web_search=false")
	}

	vx, err := newVertexAI(cfg)
	if err != nil {
		t.Fatalf("newVertexAI: %v", err)
	}
	if got := vx.(*Provider).builtins.GoogleSearch; got {
		t.Errorf("vertex: GoogleSearch still on despite web_search=false")
	}
}

// The names are the operator-facing contract: they must be the neutral
// config keys, not the Gemini-native ones, or the startup line can't be
// matched against what was typed.
func TestBuiltinToolNames(t *testing.T) {
	t.Parallel()
	p := &Provider{builtins: DefaultBuiltinTools()}
	if got, want := p.BuiltinToolNames(), []string{"web_search", "url_context"}; !slices.Equal(got, want) {
		t.Errorf("names = %v, want %v", got, want)
	}
	off := &Provider{}
	if got := off.BuiltinToolNames(); len(got) != 0 {
		t.Errorf("all-off names = %v, want empty", got)
	}
}
