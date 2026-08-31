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

package anthropic

import (
	"slices"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/config"
)

func ptr(b bool) *bool { return &b }

func builtinsCfg(bt *config.BuiltinToolsConfig) *config.Config {
	c := &config.Config{}
	c.Model.BuiltinTools = bt
	return c
}

// #876 is as much about symmetry as about the missing surface: Gemini
// ships web search ON and Anthropic ships it OFF, so a deployment that
// switches providers switches the agent's internet reachability with
// it. The same key has to move both, in both directions.
func TestBuiltinToolsFromConfig(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		bt   *config.BuiltinToolsConfig
		want BuiltinTools
	}{
		{"absent block keeps defaults", nil, DefaultBuiltinTools()},
		{"empty block keeps defaults", &config.BuiltinToolsConfig{}, DefaultBuiltinTools()},
		{"web_search true", &config.BuiltinToolsConfig{WebSearch: ptr(true)}, BuiltinTools{WebSearch: true}},
		{"web_search false", &config.BuiltinToolsConfig{WebSearch: ptr(false)}, BuiltinTools{}},
		{
			// url_context and code_execution are Gemini-side; they land
			// nowhere here. Fail-safe by construction — a tool this
			// provider cannot send is one it cannot leave on — but the
			// web_search field beside them must still take.
			name: "gemini-only fields are ignored, web_search still applies",
			bt: &config.BuiltinToolsConfig{
				WebSearch:     ptr(true),
				URLContext:    ptr(true),
				CodeExecution: ptr(true),
			},
			want: BuiltinTools{WebSearch: true},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p, err := New("k", builtinToolsFromConfig(builtinsCfg(tc.bt))...)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if p.builtins != tc.want {
				t.Errorf("builtins = %+v, want %+v", p.builtins, tc.want)
			}
		})
	}
}

// The registry constructors are what the daemon calls, and they also
// carry the prompt-cache option — appending the built-in options must
// not displace it.
func TestRegistryConstructors_HonorBuiltinToolsConfig(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "k")
	t.Setenv("ANTHROPIC_VERTEX_PROJECT_ID", "p")
	t.Setenv("CLOUD_ML_REGION", "us-east5")

	cfg := builtinsCfg(&config.BuiltinToolsConfig{WebSearch: ptr(true)})

	first, err := newProvider(cfg)
	if err != nil {
		t.Fatalf("newProvider: %v", err)
	}
	fp := first.(*Provider)
	if !fp.builtins.WebSearch {
		t.Errorf("anthropic: WebSearch off despite web_search=true")
	}
	if !fp.cache.Enabled() {
		t.Errorf("anthropic: prompt cache lost when built-in options were appended")
	}

	vx, err := newVertexProvider(cfg)
	if err != nil {
		t.Fatalf("newVertexProvider: %v", err)
	}
	vp := vx.(*Provider)
	if !vp.builtins.WebSearch {
		t.Errorf("anthropic-vertex: WebSearch off despite web_search=true")
	}
	if !vp.cache.Enabled() {
		t.Errorf("anthropic-vertex: prompt cache lost when built-in options were appended")
	}
}

// Neutral names, not the SDK's — the startup line has to read the same
// whichever provider resolved, so an operator can match it against the
// key they typed.
func TestBuiltinToolNames(t *testing.T) {
	t.Parallel()
	on := &Provider{builtins: BuiltinTools{WebSearch: true}}
	if got, want := on.BuiltinToolNames(), []string{"web_search"}; !slices.Equal(got, want) {
		t.Errorf("names = %v, want %v", got, want)
	}
	if got := (&Provider{}).BuiltinToolNames(); len(got) != 0 {
		t.Errorf("all-off names = %v, want empty", got)
	}
}
