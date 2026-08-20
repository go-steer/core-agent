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

package attachadapter

import (
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/agent"
	"github.com/go-steer/core-agent/v2/pkg/attach"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

// byName indexes an AttachTools result so assertions can name the row
// they care about instead of scanning.
func byName(infos []attach.ToolInfo) map[string]attach.ToolInfo {
	out := make(map[string]attach.ToolInfo, len(infos))
	for _, i := range infos {
		out[i.Name] = i
	}
	return out
}

func mcpSnapshot() attach.MCPInfo {
	return attach.MCPInfo{Servers: []attach.MCPServerInfo{
		{Name: "gke", Status: "running", Tools: []attach.MCPToolInfo{
			{Name: "gke_list_clusters", Description: "list clusters"},
			{Name: "gke_get_pod", Description: "get a pod"},
		}},
		{Name: "filesystem", Status: "running", Tools: []attach.MCPToolInfo{
			{Name: "filesystem_read_file", Description: "read a file"},
		}},
		// A server that failed to start contributes no tools, and must
		// not contribute an empty row either.
		{Name: "broken", Status: "failed"},
	}}
}

func skillToolsSnapshot() []attach.ToolInfo {
	return []attach.ToolInfo{
		{Name: "list_skills", Description: "enumerate skills"},
		{Name: "load_skill", Description: "load one skill"},
	}
}

// TestAttachTools_MCPToolsCarrySourceAndServer — the #767 defect.
// MCP tools reach the agent through agent.WithToolsets, which never
// lands in agent.Tools(), so /tools omitted them entirely and clients
// (mast-web#44, core-tui) fell back to splitting tool names on "_" and
// calling the prefix a server. Every MCP tool must now arrive with
// source="mcp" and its real server name.
func TestAttachTools_MCPToolsCarrySourceAndServer(t *testing.T) {
	t.Parallel()
	ad := New(newEchoAgent(t), WithMCPProvider(mcpSnapshot))
	got := byName(ad.AttachTools())

	for name, wantServer := range map[string]string{
		"gke_list_clusters":    "gke",
		"gke_get_pod":          "gke",
		"filesystem_read_file": "filesystem",
	} {
		info, ok := got[name]
		if !ok {
			t.Errorf("%s absent from /tools; MCP tools are still invisible there", name)
			continue
		}
		if info.Source != attach.ToolSourceMCP {
			t.Errorf("%s source = %q, want %q", name, info.Source, attach.ToolSourceMCP)
		}
		if info.Server != wantServer {
			t.Errorf("%s server = %q, want %q", name, info.Server, wantServer)
		}
	}
	if info, ok := got["broken"]; ok {
		t.Errorf("failed server contributed a tool row: %+v", info)
	}
}

// TestAttachTools_SkillToolsCarrySkillSource — the other half of the
// toolset gap. The skill bundle exposes a small fixed set of tools
// (list_skills / load_skill / ...), not one tool per skill, and they
// were missing from /tools for the same reason MCP tools were.
func TestAttachTools_SkillToolsCarrySkillSource(t *testing.T) {
	t.Parallel()
	ad := New(newEchoAgent(t), WithSkillToolsProvider(skillToolsSnapshot))
	got := byName(ad.AttachTools())

	for _, name := range []string{"list_skills", "load_skill"} {
		info, ok := got[name]
		if !ok {
			t.Errorf("%s absent from /tools", name)
			continue
		}
		if info.Source != attach.ToolSourceSkill {
			t.Errorf("%s source = %q, want %q", name, info.Source, attach.ToolSourceSkill)
		}
		if info.Server != "" {
			t.Errorf("%s server = %q, want empty (skills have no server)", name, info.Server)
		}
	}
}

// TestAttachTools_UnwiredProvidersOmitTheirSection — an embedder that
// wires neither provider gets no mcp/skill rows and no panic. Pins the
// "absence is empty, not an error" contract the rest of the Attach*
// surface follows, and guards against a future default that invents
// attribution the host never declared.
func TestAttachTools_UnwiredProvidersOmitTheirSection(t *testing.T) {
	t.Parallel()
	for _, i := range New(newEchoAgent(t)).AttachTools() {
		if i.Source == attach.ToolSourceMCP || i.Source == attach.ToolSourceSkill {
			t.Errorf("unwired providers produced a %s row: %+v", i.Source, i)
		}
	}
	// Contrast: the same agent WITH providers does report them, so the
	// assertion above is measuring wiring rather than an empty agent.
	if n := len(New(newEchoAgent(t), WithMCPProvider(mcpSnapshot)).AttachTools()); n != 3 {
		t.Errorf("wired MCP provider produced %d rows, want 3", n)
	}
}

// TestAttachTools_GateStateUsesTheNamespaceForToolsetTools is the
// subtle half. tools.GateToolset routes every MCP call through
// CheckToolCall with the NAMESPACE ("mcp") as the tool name, so a deny
// rule is written `mcp` / `mcp:*` and never names the underlying tool.
// Projecting ToolGateState off `gke_list_clusters` would report
// "prompted" for a tool the gate will refuse outright — a /tools column
// that contradicts what happens on the next call.
func TestAttachTools_GateStateUsesTheNamespaceForToolsetTools(t *testing.T) {
	t.Parallel()
	policy, err := permissions.NewPolicy([]string{"skill:*"}, []string{"mcp:*"})
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	gate := permissions.New(permissions.Options{Mode: permissions.ModeAsk, Policy: policy})
	ad := New(
		newEchoAgent(t, agent.WithGate(gate)),
		WithMCPProvider(mcpSnapshot),
		WithSkillToolsProvider(skillToolsSnapshot),
	)
	got := byName(ad.AttachTools())

	if s := got["gke_list_clusters"].GateState; s != permissions.ToolGateDenied {
		t.Errorf("gke_list_clusters gate_state = %q, want %q — the deny rule is on the mcp namespace",
			s, permissions.ToolGateDenied)
	}
	if s := got["list_skills"].GateState; s != permissions.ToolGateAllowed {
		t.Errorf("list_skills gate_state = %q, want %q — the allow rule is on the skill namespace",
			s, permissions.ToolGateAllowed)
	}
}

// TestAttachTools_AgentToolsWinANameCollision — the dedupe rule. A
// provider snapshot claiming a name agent.Tools() already used must
// not produce a second row: two rows with one name make the endpoint's
// own key ambiguous for every client that indexes by it.
func TestAttachTools_AgentToolsWinANameCollision(t *testing.T) {
	t.Parallel()
	child := newEchoAgent(t, agent.WithName("cluster"))
	parent := newEchoAgent(t, agent.WithName("parent"), agent.WithSubagents([]*agent.Agent{child}))
	ad := New(parent, WithSkillToolsProvider(func() []attach.ToolInfo {
		return []attach.ToolInfo{{Name: "cluster", Description: "an impostor"}}
	}))

	n := 0
	var source string
	for _, i := range ad.AttachTools() {
		if i.Name == "cluster" {
			n++
			source = i.Source
		}
	}
	if n != 1 {
		t.Fatalf("cluster listed %d times, want 1", n)
	}
	if source != attach.ToolSourceSubagent {
		t.Errorf("cluster source = %q, want %q — agent.Tools() is authoritative", source, attach.ToolSourceSubagent)
	}
}
