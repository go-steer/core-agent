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

package background

import (
	"testing"

	"google.golang.org/adk/tool"

	"github.com/go-steer/core-agent/v2/pkg/attach"
)

// catalogToolNames flattens one roster entry's tool grant to names, for
// assertions that care about membership and order.
func catalogToolNames(e attach.SubagentCatalogInfo) []string {
	out := make([]string, 0, len(e.Tools))
	for _, t := range e.Tools {
		out = append(out, t.Name)
	}
	return out
}

func catalogEntry(t *testing.T, cat []attach.SubagentCatalogInfo, name string) attach.SubagentCatalogInfo {
	t.Helper()
	for _, e := range cat {
		if e.Name == name {
			return e
		}
	}
	t.Fatalf("%q absent from the catalog (%d entries)", name, len(cat))
	return attach.SubagentCatalogInfo{}
}

// TestCatalog_TemplateToolsMergeBuiltinsAndToolsets is #768's core
// claim: GET .../subagents can answer "what can this specialist do?".
// Built-ins come from the resolved instances the template holds;
// MCP + skill tools come from the builder's registration snapshot,
// because a tool.Toolset is opaque at projection time. Both halves have
// to land, sorted, in one list.
func TestCatalog_TemplateToolsMergeBuiltinsAndToolsets(t *testing.T) {
	t.Parallel()
	prov := &recordingProvider{llm: &stopRaceLLM{}}
	mgr := newTemplateManager(t, prov, []SubagentTemplate{{
		Name:         "cluster",
		ModelFactory: tmplFactory(prov, "m"),
		Tools: []tool.Tool{
			newNamedStubTool(t, "read_file"),
			newNamedStubTool(t, "grep"),
		},
		ToolsetTools: []attach.ToolInfo{
			{Name: "gke_list_clusters", Description: "list clusters", Source: attach.ToolSourceMCP, Server: "gke"},
			{Name: "list_skills", Description: "enumerate skills", Source: attach.ToolSourceSkill},
		},
	}})

	got := catalogEntry(t, mgr.Catalog(), "cluster")
	want := []string{"gke_list_clusters", "grep", "list_skills", "read_file"}
	if names := catalogToolNames(got); len(names) != len(want) {
		t.Fatalf("tools = %v, want %v", names, want)
	} else {
		for i, n := range names {
			if n != want[i] {
				t.Fatalf("tools = %v, want %v (sorted by name)", names, want)
			}
		}
	}

	byName := map[string]attach.ToolInfo{}
	for _, ti := range got.Tools {
		byName[ti.Name] = ti
	}
	// MCP attribution survives the projection — the whole point of
	// snapshotting at registration rather than enumerating here.
	if mcp := byName["gke_list_clusters"]; mcp.Source != attach.ToolSourceMCP || mcp.Server != "gke" {
		t.Errorf("gke_list_clusters = %+v, want source=mcp server=gke", mcp)
	}
	if sk := byName["list_skills"]; sk.Source != attach.ToolSourceSkill || sk.Server != "" {
		t.Errorf("list_skills = %+v, want source=skill and no server", sk)
	}
	// Built-ins classify by the same roster GET .../tools uses, and keep
	// their descriptions so a detail view has something to render.
	if bi := byName["read_file"]; bi.Source != attach.ToolSourceBuiltin || bi.Description == "" {
		t.Errorf("read_file = %+v, want source=builtin with a description", bi)
	}
}

// TestCatalog_ToolsAbsentWhenNothingIsGranted — an empty grant omits the
// key rather than emitting []. A subagent with no tools and a subagent
// whose tools weren't projected are different facts, and only the first
// one is true here; a client that renders "0 tools" for both would be
// wrong half the time.
func TestCatalog_ToolsAbsentWhenNothingIsGranted(t *testing.T) {
	t.Parallel()
	prov := &recordingProvider{llm: &stopRaceLLM{}}
	mgr := newTemplateManager(t, prov, []SubagentTemplate{
		{Name: "bare", ModelFactory: tmplFactory(prov, "m")},
	})
	if got := catalogEntry(t, mgr.Catalog(), "bare"); got.Tools != nil {
		t.Errorf("tools = %+v, want nil for an ungranted subagent", got.Tools)
	}
}

// TestCatalog_PredefinedSpecToolsResolveDescriptions — a predefined spec
// carries tool NAMES, resolved against the manager's catalog at spawn
// time. The roster resolves them the same way, so the operator reads the
// same descriptions GET .../tools shows.
func TestCatalog_PredefinedSpecToolsResolveDescriptions(t *testing.T) {
	t.Parallel()
	prov := &recordingProvider{llm: &stopRaceLLM{}}
	mgr, err := NewManager(
		WithProvider(prov, "parent-model"),
		WithCatalog([]tool.Tool{newNamedStubTool(t, "read_file"), newNamedStubTool(t, "grep")}),
		WithPredefinedSpecs([]Spec{{
			Name:         "researcher",
			SystemPrompt: "p",
			Tools:        []string{"read_file"},
			Extras:       []string{"grep"},
		}}),
	)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	got := catalogEntry(t, mgr.Catalog(), "researcher")
	// Extras are part of the grant: resolveTools concatenates the two,
	// so a roster that showed only Tools would understate what spawns.
	if names := catalogToolNames(got); len(names) != 2 || names[0] != "grep" || names[1] != "read_file" {
		t.Fatalf("tools = %v, want [grep read_file] (Tools + Extras, sorted)", names)
	}
	for _, ti := range got.Tools {
		if ti.Description == "" {
			t.Errorf("%s has no description; the catalog lookup didn't resolve", ti.Name)
		}
	}
}

// TestCatalog_PredefinedSpecKeepsUnresolvableAndDropsAutoWired draws the
// line between the two reasons a configured name might not resolve.
//
// An auto-wired name (return_result, report_alert, schedule_next_turn)
// is dropped: resolveTools drops it too, because the runtime registers
// the real implementation itself, so listing it would attribute a
// runtime tool to this subagent's configuration.
//
// A name the catalog has never heard of is KEPT, described as nothing.
// It is a misconfiguration that makes Spawn fail with ErrUnknownTool,
// and hiding it would leave an operator reading a healthy-looking roster
// next to a subagent that cannot start.
func TestCatalog_PredefinedSpecKeepsUnresolvableAndDropsAutoWired(t *testing.T) {
	t.Parallel()
	prov := &recordingProvider{llm: &stopRaceLLM{}}
	mgr, err := NewManager(
		WithProvider(prov, "parent-model"),
		WithCatalog([]tool.Tool{newNamedStubTool(t, "read_file")}),
		WithPredefinedSpecs([]Spec{{
			Name:         "researcher",
			SystemPrompt: "p",
			Tools:        []string{"read_file", "report_alert", "kubectl_get"},
		}}),
	)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	got := catalogEntry(t, mgr.Catalog(), "researcher")
	names := catalogToolNames(got)
	if len(names) != 2 || names[0] != "kubectl_get" || names[1] != "read_file" {
		t.Fatalf("tools = %v, want [kubectl_get read_file] — auto-wired dropped, unknown kept", names)
	}
	for _, ti := range got.Tools {
		if ti.Name == "kubectl_get" && ti.Description != "" {
			t.Errorf("kubectl_get described as %q; an unresolvable name has nothing to describe", ti.Description)
		}
	}
}

// TestCatalog_ToolsDedupeByName — a name arriving twice (a spec listing
// it in both Tools and Extras, or a toolset snapshot colliding with a
// built-in) must produce one row. Two rows under one name make the
// list's own key ambiguous for any client that indexes by it, which is
// the same rule AttachTools follows (#767).
func TestCatalog_ToolsDedupeByName(t *testing.T) {
	t.Parallel()
	prov := &recordingProvider{llm: &stopRaceLLM{}}
	mgr := newTemplateManager(t, prov, []SubagentTemplate{{
		Name:         "cluster",
		ModelFactory: tmplFactory(prov, "m"),
		Tools:        []tool.Tool{newNamedStubTool(t, "read_file")},
		ToolsetTools: []attach.ToolInfo{
			{Name: "read_file", Source: attach.ToolSourceMCP, Server: "impostor"},
		},
	}})

	got := catalogEntry(t, mgr.Catalog(), "cluster")
	if names := catalogToolNames(got); len(names) != 1 {
		t.Fatalf("tools = %v, want one row for read_file", names)
	}
	if got.Tools[0].Source != attach.ToolSourceBuiltin {
		t.Errorf("read_file source = %q, want builtin — the resolved instance wins", got.Tools[0].Source)
	}
}
