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
	"fmt"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"google.golang.org/adk/model"
	"google.golang.org/adk/tool"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/pkg/attach"
	coretools "github.com/go-steer/core-agent/v2/pkg/tools"
)

// rosterTool wraps the spawn_agent function tool so the parent model
// sees the configured-subagent roster in the tool's own schema, per
// docs/unified-subagent-invocation-design.md §5 (#640).
//
// Without it, `agent` is free text and the model can only route to a
// subagent its persona happens to name — which is why the
// kube-platform-agent recipe hardcodes "cluster" plus a symptom catalog
// in its AGENTS.md. That hardcoding couples a reusable persona to one
// fleet's roster; the roster belongs in the tool schema, where it is
// always accurate and costs no persona text.
//
// The roster is rendered at Declaration() time, not at construction:
// NewSpawnTools runs before SetSubagentTemplates installs the
// declarative subagents (the templates need the tool catalog the
// manager is built with), so a snapshot taken at construction would be
// empty for exactly the subagents this is meant to surface.
//
// §5's constraint is "no new model tool" — this adds none. It is the
// model-facing counterpart to the operator-facing catalog (#627); both
// read Manager.Catalog.
type rosterTool struct {
	inner tool.Tool
	mgr   *Manager
}

// runnableTool is the LLM-flow surface ADK requires beyond tool.Tool.
// Declared here rather than imported because ADK keeps it internal.
type runnableTool interface {
	Declaration() *genai.FunctionDeclaration
	Run(ctx tool.Context, args any) (map[string]any, error)
}

func (r rosterTool) Name() string        { return r.inner.Name() }
func (r rosterTool) Description() string { return r.inner.Description() }
func (r rosterTool) IsLongRunning() bool { return r.inner.IsLongRunning() }

// Declaration returns the inner tool's declaration with the live roster
// folded in: the names and descriptions appended to the tool
// description, and — when ad-hoc spawning is off — the `agent` property
// constrained to an enum of exactly those names.
//
// The enum is conditional because an ad-hoc spawn deliberately leaves
// `agent` empty; the roster is still listed in the description there,
// so a parent that can author inline personas can still discover and
// prefer a configured subagent.
//
// Everything is copied before mutation. functiontool caches one
// resolved *jsonschema.Schema and hands out the same pointer on every
// Declaration() call, so writing through it would corrupt the schema
// for every subsequent request (and for any other agent sharing the
// tool instance).
func (r rosterTool) Declaration() *genai.FunctionDeclaration {
	rn, ok := r.inner.(runnableTool)
	if !ok {
		return nil
	}
	decl := rn.Declaration()
	if decl == nil {
		return nil
	}
	roster := r.mgr.Catalog()
	if len(roster) == 0 {
		// Nothing configured: leave the shipped declaration untouched
		// rather than telling the model "the roster is empty", which
		// reads as a fault rather than a deployment without subagents.
		return decl
	}
	clone := *decl
	clone.Description = decl.Description + "\n\n" + rosterBlock(roster)
	if schema, ok := decl.ParametersJsonSchema.(*jsonschema.Schema); ok {
		clone.ParametersJsonSchema = withRosterEnum(schema, roster, r.mgr.AllowAdhoc())
	}
	return &clone
}

func (r rosterTool) Run(ctx tool.Context, args any) (map[string]any, error) {
	rn, ok := r.inner.(runnableTool)
	if !ok {
		return nil, fmt.Errorf("background: %q is not runnable", r.inner.Name())
	}
	return rn.Run(ctx, args)
}

// ProcessRequest packs the wrapper — not the inner tool — so the model
// receives the roster-annotated declaration and ADK's dispatch routes
// calls back through this wrapper. Same discipline as the gate,
// serializer, and MCP namespace wrappers.
func (r rosterTool) ProcessRequest(_ tool.Context, req *model.LLMRequest) error {
	return coretools.PackTool(req, r)
}

// rosterBlock renders the roster as a description block. Descriptions
// are what make routing possible ("this is a triage → cluster"), so a
// subagent configured without one is listed with an explicit note
// rather than a blank — a missing description is an operator omission
// the model should see, not a subagent it should assume is unusable.
func rosterBlock(roster []attach.SubagentCatalogInfo) string {
	var b strings.Builder
	b.WriteString("Configured subagents you may reference by name in 'agent' — this is the complete list, there are no others:")
	for _, s := range roster {
		b.WriteString("\n- ")
		b.WriteString(s.Name)
		if d := strings.TrimSpace(s.Description); d != "" {
			b.WriteString(" — ")
			b.WriteString(d)
		} else {
			b.WriteString(" — (no description configured; infer its purpose from its name)")
		}
	}
	return b.String()
}

// withRosterEnum returns a copy of the spawn_agent parameter schema
// whose `agent` property lists the roster. Structural copy only: the
// top-level schema, its Properties map, and the one property being
// rewritten are cloned; the sibling property schemas are shared,
// because nothing here mutates them.
func withRosterEnum(in *jsonschema.Schema, roster []attach.SubagentCatalogInfo, allowAdhoc bool) *jsonschema.Schema {
	if in == nil {
		return nil
	}
	prop, ok := in.Properties["agent"]
	if !ok || prop == nil {
		return in
	}
	schema := *in
	schema.Properties = make(map[string]*jsonschema.Schema, len(in.Properties))
	for k, v := range in.Properties {
		schema.Properties[k] = v
	}
	agentProp := *prop
	names := make([]string, 0, len(roster))
	for _, s := range roster {
		names = append(names, s.Name)
	}
	agentProp.Description = strings.TrimSpace(prop.Description) + " Configured subagents: " + strings.Join(names, ", ") + "."
	if !allowAdhoc {
		// Ad-hoc authoring is off, so a name outside the roster can only
		// be a hallucination — one that costs a wasted turn to discover.
		// Constrain it. With ad-hoc on, `agent` is legitimately omitted
		// and the enum stays off so the inline form remains expressible.
		enum := make([]any, 0, len(names))
		for _, n := range names {
			enum = append(enum, n)
		}
		agentProp.Enum = enum
	}
	schema.Properties["agent"] = &agentProp
	return &schema
}
