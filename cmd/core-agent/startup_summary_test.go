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

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/adk/agent"
	adktool "google.golang.org/adk/tool"

	"github.com/go-steer/core-agent/pkg/config"
	"github.com/go-steer/core-agent/pkg/mcp"
	"github.com/go-steer/core-agent/pkg/skills"
)

func TestFormatConfigLine(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		cfgPath    string
		agentsDir  string
		wantSubstr []string
	}{
		{
			name:       "explicit -c wins",
			cfgPath:    "/etc/core-agent/.agents/config.json",
			agentsDir:  "/etc/core-agent/.agents",
			wantSubstr: []string{"config:", "/etc/core-agent/.agents/config.json", "(via -c)"},
		},
		{
			name:       "discovery walked up from cwd",
			cfgPath:    "",
			agentsDir:  "/proj/.agents",
			wantSubstr: []string{"config:", "/proj/.agents/config.json", "(via .agents/ discovery)"},
		},
		{
			name:       "neither — pure defaults",
			cfgPath:    "",
			agentsDir:  "",
			wantSubstr: []string{"config: source=<none>", "pure defaults"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := formatConfigLine(tc.cfgPath, tc.agentsDir)
			for _, s := range tc.wantSubstr {
				if !strings.Contains(got, s) {
					t.Errorf("formatConfigLine(%q, %q) missing %q; got: %q", tc.cfgPath, tc.agentsDir, s, got)
				}
			}
		})
	}
}

func TestFormatAgentsDirLine(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		cfgPath    string
		agentsDir  string
		wantSubstr []string
	}{
		{
			name:       "derived from -c",
			cfgPath:    "/etc/core-agent/.agents/config.json",
			agentsDir:  "/etc/core-agent/.agents",
			wantSubstr: []string{"agentsDir:", "/etc/core-agent/.agents", "derived from filepath.Dir(-c)"},
		},
		{
			name:       "found via discovery",
			cfgPath:    "",
			agentsDir:  "/proj/.agents",
			wantSubstr: []string{"agentsDir:", "/proj/.agents", "via .agents/ discovery"},
		},
		{
			name:       "no agents dir at all",
			cfgPath:    "",
			agentsDir:  "",
			wantSubstr: []string{"agentsDir: <none>", "record_plan / MCP / skills"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := formatAgentsDirLine(tc.cfgPath, tc.agentsDir)
			for _, s := range tc.wantSubstr {
				if !strings.Contains(got, s) {
					t.Errorf("formatAgentsDirLine(%q, %q) missing %q; got: %q", tc.cfgPath, tc.agentsDir, s, got)
				}
			}
		})
	}
}

func TestFormatModelLine(t *testing.T) {
	// Sequential — mutates process env for Vertex cases; parallel
	// runs would race on GOOGLE_CLOUD_PROJECT/_LOCATION.
	cases := []struct {
		name         string
		cfg          *config.Config
		providerName string
		env          map[string]string
		wantSubstr   []string
		wantNoSubstr []string
	}{
		{
			name:         "nil cfg",
			cfg:          nil,
			providerName: "",
			wantSubstr:   []string{"model:", "<unknown>", "nil cfg"},
		},
		{
			name: "gemini provider (no vertex extras)",
			cfg: &config.Config{Model: config.ModelConfig{
				Name: "gemini-3.5-flash", Provider: "gemini",
			}},
			providerName: "gemini",
			wantSubstr:   []string{"model: gemini-3.5-flash provider=gemini"},
			// Non-vertex provider must NOT surface project/location.
			wantNoSubstr: []string{"project=", "location="},
		},
		{
			name: "vertex with GCP env set",
			cfg: &config.Config{Model: config.ModelConfig{
				Name: "gemini-3.5-flash", Provider: "vertex",
			}},
			providerName: "vertex",
			env: map[string]string{
				"GOOGLE_CLOUD_PROJECT":  "gke-demos-345619",
				"GOOGLE_CLOUD_LOCATION": "global",
			},
			wantSubstr: []string{
				"model: gemini-3.5-flash provider=vertex",
				"project=gke-demos-345619",
				"location=global",
			},
		},
		{
			name: "vertex with missing GCP env — surfaces <unset>",
			cfg: &config.Config{Model: config.ModelConfig{
				Name: "gemini-3.5-flash", Provider: "vertex",
			}},
			providerName: "vertex",
			env: map[string]string{
				"GOOGLE_CLOUD_PROJECT":  "",
				"GOOGLE_CLOUD_LOCATION": "",
			},
			wantSubstr: []string{"project=<unset>", "location=<unset>"},
		},
		{
			name: "anthropic-vertex also surfaces GCP env",
			cfg: &config.Config{Model: config.ModelConfig{
				Name: "claude-sonnet-4-6", Provider: "anthropic-vertex",
			}},
			providerName: "anthropic-vertex",
			env:          map[string]string{"GOOGLE_CLOUD_PROJECT": "p", "GOOGLE_CLOUD_LOCATION": "us-central1"},
			wantSubstr:   []string{"provider=anthropic-vertex", "project=p", "location=us-central1"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Save + restore env for Vertex-shaped tests (t.Setenv
			// handles restore automatically).
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			got := formatModelLine(tc.cfg, tc.providerName)
			for _, s := range tc.wantSubstr {
				if !strings.Contains(got, s) {
					t.Errorf("formatModelLine: missing %q; got: %q", s, got)
				}
			}
			for _, s := range tc.wantNoSubstr {
				if strings.Contains(got, s) {
					t.Errorf("formatModelLine: unexpected substring %q; got: %q", s, got)
				}
			}
		})
	}
}

func TestFormatMCPLine(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		servers    []*mcp.Server
		wantSubstr []string
	}{
		{
			name:       "empty",
			servers:    nil,
			wantSubstr: []string{"mcp: 0 servers loaded"},
		},
		{
			name: "single ok server",
			servers: []*mcp.Server{
				{Name: "gke", Err: nil},
			},
			wantSubstr: []string{"mcp: 1 server(s) loaded", "gke(ok)"},
		},
		{
			name: "multiple servers, alphabetized",
			servers: []*mcp.Server{
				{Name: "zeta", Err: nil},
				{Name: "alpha", Err: nil},
				{Name: "mid", Err: nil},
			},
			// Order-sensitive: sorted alphabetically for stable log output.
			wantSubstr: []string{"mcp: 3 server(s) loaded", "alpha(ok), mid(ok), zeta(ok)"},
		},
		{
			name: "one failed surfaces failure count",
			servers: []*mcp.Server{
				{Name: "gke", Err: nil},
				{Name: "broken", Err: errors.New("connection refused")},
			},
			wantSubstr: []string{"broken(failed)", "gke(ok)", "[1 failed"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := formatMCPLine(tc.servers)
			for _, s := range tc.wantSubstr {
				if !strings.Contains(got, s) {
					t.Errorf("formatMCPLine: missing %q; got: %q", s, got)
				}
			}
		})
	}
}

func TestFormatSkillsLine(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		infos      []skills.Info
		wantSubstr []string
	}{
		{
			name:       "no skills",
			infos:      nil,
			wantSubstr: []string{"skills: 0 loaded"},
		},
		{
			name:       "single skill",
			infos:      []skills.Info{{Name: "k8s-triage"}},
			wantSubstr: []string{"skills: 1 loaded", "k8s-triage"},
		},
		{
			name: "multiple skills alphabetized",
			infos: []skills.Info{
				{Name: "zeta"},
				{Name: "alpha"},
			},
			wantSubstr: []string{"skills: 2 loaded", "alpha, zeta"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// skills.Skills.Empty() returns true only when Instruction
			// is empty (see pkg/instruction/load.go — same shape as
			// Loaded.Empty). For our test we construct Skills{Infos: ...}
			// directly, which means Empty() returns false if Infos has
			// any entries — but our nil case expects "0 loaded". Have
			// to satisfy Empty()'s contract: use an empty Skills{} value
			// for the nil case.
			var loaded skills.Skills
			if len(tc.infos) > 0 {
				// Non-empty case: give it a non-nil Toolset stub so
				// Empty() returns false the way real .LoadAll would.
				loaded = skills.Skills{Infos: tc.infos, Toolset: dummySkillToolset{}}
			}
			got := formatSkillsLine(loaded)
			for _, s := range tc.wantSubstr {
				if !strings.Contains(got, s) {
					t.Errorf("formatSkillsLine: missing %q; got: %q", s, got)
				}
			}
		})
	}
}

func TestFormatAuthLine(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		cfg        *config.Config
		writeUsers func(t *testing.T, dir string) string // returns TableFile path
		wantSubstr []string
	}{
		{
			name:       "nil cfg",
			cfg:        nil,
			wantSubstr: []string{"multi-session auth: <disabled>"},
		},
		{
			name:       "multi-session disabled",
			cfg:        &config.Config{},
			wantSubstr: []string{"multi-session auth: disabled", "single-user mode"},
		},
		{
			name: "enabled with users file present",
			cfg: &config.Config{
				Attach: config.AttachConfig{
					MultiSession: config.MultiSessionConfig{
						Enabled:         true,
						AdminIdentities: []string{"sre@example.com"},
						ProxyIdentities: []string{"sa:bot"},
					},
				},
			},
			writeUsers: func(t *testing.T, dir string) string {
				t.Helper()
				path := filepath.Join(dir, "users.json")
				body := `{"version":1,"users":[
					{"identity":"sre@example.com","token":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
					{"identity":"bob@example.com","token":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
					{"identity":"sa:bot","token":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"}
				]}`
				if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
				return path
			},
			wantSubstr: []string{
				"multi-session auth: bearer_table",
				"3 users",
				"admin=[sre@example.com]",
				"proxy=[sa:bot]",
			},
		},
		{
			name: "enabled but no table file — user count unknown",
			cfg: &config.Config{
				Attach: config.AttachConfig{
					MultiSession: config.MultiSessionConfig{Enabled: true},
				},
			},
			wantSubstr: []string{"multi-session auth: bearer_table", "? users"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if tc.writeUsers != nil {
				dir := t.TempDir()
				tc.cfg.Attach.MultiSession.Auth.TableFile = tc.writeUsers(t, dir)
			}
			got := formatAuthLine(tc.cfg)
			for _, s := range tc.wantSubstr {
				if !strings.Contains(got, s) {
					t.Errorf("formatAuthLine: missing %q; got: %q", s, got)
				}
			}
		})
	}
}

// dummySkillToolset satisfies skills.Skills.Empty() != true check
// (Empty() returns Toolset == nil, so any non-nil Toolset value works).
// Implements the adktool.Toolset interface with no-op methods.
type dummySkillToolset struct{}

func (dummySkillToolset) Name() string { return "test-dummy" }
func (dummySkillToolset) Tools(_ agent.ReadonlyContext) ([]adktool.Tool, error) {
	return nil, nil
}
