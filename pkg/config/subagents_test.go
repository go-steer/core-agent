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

package config

import (
	"encoding/json"
	"reflect"
	"testing"
)

// TestValidate_Subagents pins the declarative subagents[] validation:
// unique tool-name-safe names, non-negative max_depth, a known inline
// provider when model is set, and non-empty inline refs. Existence of a
// referenced MCP server / skill is a wiring-time concern (it needs the
// loaded mcp.json / skills dir) and is deliberately NOT checked here.
func TestValidate_Subagents(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		specs   []SubagentSpec
		wantErr bool
	}{
		{
			name:    "none",
			specs:   nil,
			wantErr: false,
		},
		{
			name:    "minimal valid",
			specs:   []SubagentSpec{{Name: "cluster"}},
			wantErr: false,
		},
		{
			name: "inherit default (no tools/mcp/skills)",
			specs: []SubagentSpec{{
				Name:         "helper",
				Description:  "read-only helper",
				Instructions: "@include upstream/cluster/SOUL.md",
				Model:        &ModelConfig{Provider: ProviderVertex, Name: "gemini-3.5-flash"},
			}},
			wantErr: false,
		},
		{
			name: "full inline scope",
			specs: []SubagentSpec{{
				Name:     "cluster",
				MaxDepth: 2,
				Tools:    []string{"read_file", "grep"},
				MCP:      []string{"gke-readonly"},
				Skills:   []string{"gke-cluster-lifecycle"},
			}},
			wantErr: false,
		},
		{
			name: "explicit empty scope is valid",
			specs: []SubagentSpec{{
				Name: "sandboxed",
				MCP:  []string{},
			}},
			wantErr: false,
		},
		{
			name:    "empty name",
			specs:   []SubagentSpec{{Name: ""}},
			wantErr: true,
		},
		{
			name:    "name with space",
			specs:   []SubagentSpec{{Name: "the cluster"}},
			wantErr: true,
		},
		{
			name:    "name with slash",
			specs:   []SubagentSpec{{Name: "team/cluster"}},
			wantErr: true,
		},
		{
			name:    "duplicate names",
			specs:   []SubagentSpec{{Name: "cluster"}, {Name: "cluster"}},
			wantErr: true,
		},
		{
			name:    "negative max_depth",
			specs:   []SubagentSpec{{Name: "cluster", MaxDepth: -1}},
			wantErr: true,
		},
		{
			name:    "model without name",
			specs:   []SubagentSpec{{Name: "cluster", Model: &ModelConfig{Provider: ProviderVertex}}},
			wantErr: true,
		},
		{
			name:    "unknown model provider",
			specs:   []SubagentSpec{{Name: "cluster", Model: &ModelConfig{Provider: "bedrock", Name: "claude"}}},
			wantErr: true,
		},
		{
			name:    "empty inline mcp entry",
			specs:   []SubagentSpec{{Name: "cluster", MCP: []string{""}}},
			wantErr: true,
		},
		{
			name:    "empty inline skill entry",
			specs:   []SubagentSpec{{Name: "cluster", Skills: []string{"ok", ""}}},
			wantErr: true,
		},
		{
			name:    "empty inline tool entry",
			specs:   []SubagentSpec{{Name: "cluster", Tools: []string{""}}},
			wantErr: true,
		},
		{
			// root existence is a wiring-time check (needs the resolution
			// base), so a plain relative path is structurally valid here.
			name:    "relative root is valid",
			specs:   []SubagentSpec{{Name: "cluster", Root: "../cluster"}},
			wantErr: false,
		},
		{
			name: "budgets on all three dimensions",
			specs: []SubagentSpec{{
				Name:    "cluster",
				Budgets: &SubagentBudgets{MaxTurns: 20, MaxCostUSD: 0.5, MaxWallclockSeconds: 300},
			}},
			wantErr: false,
		},
		{
			// Every dimension is independent; an operator who only cares
			// about the bill sets one.
			name:    "partial budgets",
			specs:   []SubagentSpec{{Name: "cluster", Budgets: &SubagentBudgets{MaxCostUSD: 0.25}}},
			wantErr: false,
		},
		{
			// An empty block is not an error, just no declared cap —
			// same as omitting it.
			name:    "empty budgets block",
			specs:   []SubagentSpec{{Name: "cluster", Budgets: &SubagentBudgets{}}},
			wantErr: false,
		},
		{
			// Rejected rather than clamped: 0 already means "no cap", so
			// clamping a typo'd -1 would leave the subagent uncapped
			// while the config reads as though it were bounded.
			name:    "negative max_turns",
			specs:   []SubagentSpec{{Name: "cluster", Budgets: &SubagentBudgets{MaxTurns: -1}}},
			wantErr: true,
		},
		{
			name:    "negative max_cost_usd",
			specs:   []SubagentSpec{{Name: "cluster", Budgets: &SubagentBudgets{MaxCostUSD: -0.5}}},
			wantErr: true,
		},
		{
			name:    "negative max_wallclock_seconds",
			specs:   []SubagentSpec{{Name: "cluster", Budgets: &SubagentBudgets{MaxWallclockSeconds: -30}}},
			wantErr: true,
		},
		{
			name:    "absolute root is valid",
			specs:   []SubagentSpec{{Name: "cluster", Root: "/recipes/cluster"}},
			wantErr: false,
		},
		{
			// a root combined with inline refs (which then filter WITHIN
			// the root) and an inline persona override is legal.
			name: "root with inline refs and instructions",
			specs: []SubagentSpec{{
				Name:         "cluster",
				Root:         "../cluster",
				Instructions: "You are a read-only cluster specialist.",
				Skills:       []string{"gke-cluster-lifecycle"},
			}},
			wantErr: false,
		},
		{
			name:    "whitespace-only root",
			specs:   []SubagentSpec{{Name: "cluster", Root: "   "}},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := DefaultConfig()
			c.Subagents = tc.specs
			err := c.Validate()
			if tc.wantErr && err == nil {
				t.Errorf("Validate() with %s: got nil, want error", tc.name)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("Validate() with %s: got %v, want nil", tc.name, err)
			}
		})
	}
}

// TestSubagents_JSONRoundTrip proves the on-disk schema survives a
// marshal→unmarshal cycle unchanged — the same path `core-agent -c` takes
// (json.Unmarshal into DefaultConfig). A field that drops here would make a
// subagent silently lose its model/scope when loaded.
func TestSubagents_JSONRoundTrip(t *testing.T) {
	t.Parallel()
	want := []SubagentSpec{
		{
			Name:         "cluster",
			Description:  "Read-only investigation of a single GKE cluster.",
			Instructions: "@include upstream/cluster/SOUL.md",
			Model:        &ModelConfig{Provider: ProviderVertex, Name: "gemini-3.5-flash"},
			MaxDepth:     2,
			Tools:        []string{"read_file"},
			MCP:          []string{"gke-readonly"},
			Skills:       []string{"gke-cluster-lifecycle"},
			Root:         "../cluster",
		},
	}
	in := DefaultConfig()
	in.Subagents = want

	body, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// The parent's mcp/skills live elsewhere; the subagent references them
	// by name, so the marshalled config must carry those names verbatim.
	got := DefaultConfig()
	if err := json.Unmarshal(body, got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(got.Subagents, want) {
		t.Errorf("round-trip mismatch:\n got %+v\nwant %+v", got.Subagents, want)
	}
	if err := got.Validate(); err != nil {
		t.Errorf("round-tripped config failed Validate: %v", err)
	}
}

// TestSubagents_EmptyVsOmittedRefs pins the load-path distinction the design
// relies on: an operator writing `"mcp": []` means "grant none of this
// dimension", while omitting the key means "inherit the parent's". After
// json.Unmarshal that difference survives as non-nil-empty vs nil — which is
// what wiring-time (cmd/core-agent) branches on. `omitempty` only affects the
// write path, so it cannot erase an operator-authored empty list on read.
//
// What "inherit" then GRANTS is wiring-time policy, not a config-load
// property, and for tools: it has one carve-out — the spawn tools are
// withheld from an inheriting subagent and only an explicit list grants
// them (#748). That is pinned next to the code that does it:
// cmd/core-agent's TestResolveSubagentTools_SpawnCarveOut and
// TestBuildDeclaredSubagents_SpawnCarveOutReachesBothTwins. Here the only
// claim is the one this package owns: nil and empty must stay
// distinguishable, or neither policy can be expressed.
func TestSubagents_EmptyVsOmittedRefs(t *testing.T) {
	t.Parallel()
	const body = `{
		"version": 1,
		"subagents": [
			{"name": "granted-none", "mcp": [], "skills": [], "tools": []},
			{"name": "inherits"}
		]
	}`
	got := DefaultConfig()
	if err := json.Unmarshal([]byte(body), got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Subagents) != 2 {
		t.Fatalf("got %d subagents, want 2", len(got.Subagents))
	}

	grantedNone := got.Subagents[0]
	if grantedNone.MCP == nil || len(grantedNone.MCP) != 0 {
		t.Errorf(`"mcp": [] should unmarshal to non-nil empty slice, got %#v`, grantedNone.MCP)
	}
	if grantedNone.Skills == nil || len(grantedNone.Skills) != 0 {
		t.Errorf(`"skills": [] should unmarshal to non-nil empty slice, got %#v`, grantedNone.Skills)
	}
	if grantedNone.Tools == nil || len(grantedNone.Tools) != 0 {
		t.Errorf(`"tools": [] should unmarshal to non-nil empty slice, got %#v`, grantedNone.Tools)
	}

	inherits := got.Subagents[1]
	if inherits.MCP != nil || inherits.Skills != nil || inherits.Tools != nil {
		t.Errorf("omitted refs should unmarshal to nil (inherit), got mcp=%#v skills=%#v tools=%#v",
			inherits.MCP, inherits.Skills, inherits.Tools)
	}

	if err := got.Validate(); err != nil {
		t.Errorf("empty/omitted refs should validate: %v", err)
	}
}

// TestSubagents_OmittedWhenEmpty guards the `omitempty` tag: a config with
// no subagents must not emit a "subagents" key (keeps minimal configs
// clean and avoids a null-vs-absent surprise on reload).
func TestSubagents_OmittedWhenEmpty(t *testing.T) {
	t.Parallel()
	body, err := json.Marshal(DefaultConfig())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, present := raw["subagents"]; present {
		t.Error("empty Subagents should be omitted from JSON, but \"subagents\" key is present")
	}
}
