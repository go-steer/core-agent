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
	"strings"
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
func TestRegistryConstructor_HonorsBuiltinToolsConfig(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "k")

	p, err := newProvider(builtinsCfg(&config.BuiltinToolsConfig{WebSearch: ptr(true)}))
	if err != nil {
		t.Fatalf("newProvider: %v", err)
	}
	checkWebSearchAndCache(t, "anthropic", p.(*Provider))
}

// Split from the first-party case rather than folded into it: NewVertex
// loads ADC and most CI machines have none, so a combined test would
// report the first-party half as skipped whenever the Vertex half can't
// run. Same skip rationale as TestResolve_AnthropicVertex_FromConfig —
// what's under test is option plumbing, not the GCP creds load.
func TestVertexRegistryConstructor_HonorsBuiltinToolsConfig(t *testing.T) {
	t.Setenv("ANTHROPIC_VERTEX_PROJECT_ID", "p")
	t.Setenv("CLOUD_ML_REGION", "us-east5")

	p, err := newVertexProvider(builtinsCfg(&config.BuiltinToolsConfig{WebSearch: ptr(true)}))
	if err != nil {
		if strings.Contains(err.Error(), "load default credentials") {
			t.Skipf("no ADC on this machine: %v", err)
		}
		t.Fatalf("newVertexProvider: %v", err)
	}
	checkWebSearchAndCache(t, "anthropic-vertex", p.(*Provider))
}

func checkWebSearchAndCache(t *testing.T, label string, p *Provider) {
	t.Helper()
	if !p.builtins.WebSearch {
		t.Errorf("%s: WebSearch off despite web_search=true", label)
	}
	if !p.cache.Enabled() {
		t.Errorf("%s: prompt cache lost when built-in options were appended", label)
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
