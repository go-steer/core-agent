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
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	adkmodel "google.golang.org/adk/model"
	"google.golang.org/adk/tool"
	"google.golang.org/genai"
)

// rosterTemplates is the shape a recipe configures: two declarative
// subagents, each with an operator-written description.
func rosterTemplates(prov *recordingProvider) []SubagentTemplate {
	return []SubagentTemplate{
		{
			Name:         "cluster",
			Description:  "read-only GKE triage for one cluster",
			Instruction:  "triage",
			ModelFactory: tmplFactory(prov, "cluster-model"),
			ModelID:      "cluster-model",
		},
		{
			Name:         "docs",
			Description:  "searches the runbook corpus",
			Instruction:  "search",
			ModelFactory: tmplFactory(prov, "docs-model"),
			ModelID:      "docs-model",
		},
	}
}

// spawnDeclaration builds the spawn_agent tool for mgr and returns the
// declaration the model would actually receive.
func spawnDeclaration(t *testing.T, mgr *Manager) *genai.FunctionDeclaration {
	t.Helper()
	rn, ok := NewSpawnAgentTool(mgr).(runnableTool)
	if !ok {
		t.Fatal("spawn_agent tool is not runnable")
	}
	decl := rn.Declaration()
	if decl == nil {
		t.Fatal("spawn_agent has no declaration")
	}
	return decl
}

func agentProperty(t *testing.T, decl *genai.FunctionDeclaration) *jsonschema.Schema {
	t.Helper()
	schema, ok := decl.ParametersJsonSchema.(*jsonschema.Schema)
	if !ok || schema == nil {
		t.Fatalf("ParametersJsonSchema is %T, want *jsonschema.Schema", decl.ParametersJsonSchema)
	}
	prop := schema.Properties["agent"]
	if prop == nil {
		t.Fatal("spawn_agent schema has no 'agent' property")
	}
	return prop
}

func enumStrings(t *testing.T, s *jsonschema.Schema) []string {
	t.Helper()
	out := make([]string, 0, len(s.Enum))
	for _, v := range s.Enum {
		str, ok := v.(string)
		if !ok {
			t.Fatalf("enum entry %v is %T, want string", v, v)
		}
		out = append(out, str)
	}
	return out
}

// TestSpawnAgentSchema_ListsConfiguredRoster is the #640 acceptance
// test: a parent whose persona never mentions "cluster" can still route
// to it, because the roster is in the tool schema.
//
// Fails on pre-fix code: `agent` was free text with a static
// description and no enum, so the only way to learn a subagent existed
// was for the persona to name it.
func TestSpawnAgentSchema_ListsConfiguredRoster(t *testing.T) {
	t.Parallel()
	prov := &recordingProvider{llm: &completingLLM{detail: "d"}}
	mgr := newTemplateManager(t, prov, rosterTemplates(prov),
		WithPredefinedSpecs([]Spec{{
			Name:         "researcher",
			Description:  "long-horizon background research",
			SystemPrompt: "research",
		}}))
	defer mgr.Close()

	decl := spawnDeclaration(t, mgr)
	for _, want := range []string{
		"cluster — read-only GKE triage for one cluster",
		"docs — searches the runbook corpus",
		"researcher — long-horizon background research",
	} {
		if !strings.Contains(decl.Description, want) {
			t.Errorf("tool description is missing %q:\n%s", want, decl.Description)
		}
	}

	got := enumStrings(t, agentProperty(t, decl))
	want := []string{"cluster", "docs", "researcher"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("agent enum = %v, want exactly the roster %v", got, want)
	}
}

// TestSpawnAgentSchema_SeesTemplatesRegisteredAfterConstruction pins the
// wiring order the fix has to survive: cmd/core-agent builds the spawn
// tools before it resolves declarative subagents (the templates need the
// tool catalog the manager was constructed with), so a roster snapshot
// taken at construction time would be empty for exactly the subagents
// this surfaces.
func TestSpawnAgentSchema_SeesTemplatesRegisteredAfterConstruction(t *testing.T) {
	t.Parallel()
	prov := &recordingProvider{llm: &completingLLM{detail: "d"}}
	mgr, err := NewManager(WithProvider(prov, "parent-model"))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer mgr.Close()

	spawn := NewSpawnAgentTool(mgr)
	rn, ok := spawn.(runnableTool)
	if !ok {
		t.Fatal("spawn_agent tool is not runnable")
	}
	if got := agentProperty(t, rn.Declaration()); len(got.Enum) != 0 {
		t.Fatalf("agent enum = %v before any subagent is registered, want none", got.Enum)
	}

	if err := mgr.SetSubagentTemplates(rosterTemplates(prov)); err != nil {
		t.Fatalf("SetSubagentTemplates: %v", err)
	}
	decl := rn.Declaration()
	if !strings.Contains(decl.Description, "cluster — read-only GKE triage for one cluster") {
		t.Errorf("roster registered after tool construction never reached the schema:\n%s", decl.Description)
	}
	if got := enumStrings(t, agentProperty(t, decl)); strings.Join(got, ",") != "cluster,docs" {
		t.Errorf("agent enum = %v, want [cluster docs]", got)
	}
}

// TestSpawnAgentSchema_NoEnumWhenAdhocAllowed keeps the inline-persona
// form expressible where the operator enabled it: an enum on `agent`
// would push the model toward naming a configured subagent even when
// authoring one ad-hoc is the right call. The roster still appears in
// the description, so discovery works either way.
func TestSpawnAgentSchema_NoEnumWhenAdhocAllowed(t *testing.T) {
	t.Parallel()
	prov := &recordingProvider{llm: &completingLLM{detail: "d"}}
	mgr := newTemplateManager(t, prov, rosterTemplates(prov), WithAllowAdhoc(true))
	defer mgr.Close()

	decl := spawnDeclaration(t, mgr)
	if got := agentProperty(t, decl); len(got.Enum) != 0 {
		t.Errorf("agent enum = %v with ad-hoc spawns enabled, want none", got.Enum)
	}
	if !strings.Contains(decl.Description, "cluster — read-only GKE triage for one cluster") {
		t.Errorf("roster missing from the description with ad-hoc enabled:\n%s", decl.Description)
	}
}

// TestSpawnAgentSchema_UnchangedWithoutSubagents leaves a deployment
// that configures no subagents exactly as it was — no empty-roster note
// that reads like a fault, no enum that would reject every value.
func TestSpawnAgentSchema_UnchangedWithoutSubagents(t *testing.T) {
	t.Parallel()
	prov := &recordingProvider{llm: &completingLLM{detail: "d"}}
	mgr, err := NewManager(WithProvider(prov, "parent-model"))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer mgr.Close()

	decl := spawnDeclaration(t, mgr)
	if decl.Description != spawnAgentDescription {
		t.Errorf("description was rewritten with no subagents configured:\n%s", decl.Description)
	}
	if got := agentProperty(t, decl); len(got.Enum) != 0 {
		t.Errorf("agent enum = %v with no subagents configured, want none", got.Enum)
	}
}

// TestSpawnAgentDeclaration_DoesNotMutateSharedSchema is the correctness
// guard on the rewrite. functiontool caches one resolved schema and
// returns the same pointer every call, so a rewrite that wrote through
// it would compound the description and leak the enum into every later
// request — including other agents sharing the tool instance.
func TestSpawnAgentDeclaration_DoesNotMutateSharedSchema(t *testing.T) {
	t.Parallel()
	prov := &recordingProvider{llm: &completingLLM{detail: "d"}}
	mgr := newTemplateManager(t, prov, rosterTemplates(prov))
	defer mgr.Close()

	wrapper, ok := NewSpawnAgentTool(mgr).(rosterTool)
	if !ok {
		t.Fatal("NewSpawnAgentTool did not return the roster wrapper")
	}
	inner, ok := wrapper.inner.(runnableTool)
	if !ok {
		t.Fatal("inner spawn_agent tool is not runnable")
	}

	first := wrapper.Declaration()
	second := wrapper.Declaration()
	if first.Description != second.Description {
		t.Errorf("description differs between calls; the rewrite is accumulating:\n%q\nvs\n%q", first.Description, second.Description)
	}
	if got := enumStrings(t, agentProperty(t, second)); strings.Join(got, ",") != "cluster,docs" {
		t.Errorf("second call's enum = %v, want [cluster docs]", got)
	}

	base := inner.Declaration()
	if base.Description != spawnAgentDescription {
		t.Errorf("the wrapped tool's own description was mutated:\n%s", base.Description)
	}
	baseSchema, ok := base.ParametersJsonSchema.(*jsonschema.Schema)
	if !ok {
		t.Fatalf("inner ParametersJsonSchema is %T", base.ParametersJsonSchema)
	}
	if prop := baseSchema.Properties["agent"]; prop == nil || len(prop.Enum) != 0 {
		t.Errorf("the wrapped tool's shared schema now carries an enum: %+v", prop)
	}
}

// TestSpawnAgentProcessRequest_PacksTheWrapper pins ADK dispatch: the
// request must carry the roster-annotated declaration AND route calls
// back through the wrapper, or the annotation is cosmetic and the tool
// stops running.
func TestSpawnAgentProcessRequest_PacksTheWrapper(t *testing.T) {
	t.Parallel()
	prov := &recordingProvider{llm: &completingLLM{detail: "d"}}
	mgr := newTemplateManager(t, prov, rosterTemplates(prov))
	defer mgr.Close()

	spawn := NewSpawnAgentTool(mgr)
	pr, ok := spawn.(interface {
		ProcessRequest(tool.Context, *adkmodel.LLMRequest) error
	})
	if !ok {
		t.Fatal("spawn_agent does not implement ProcessRequest; ADK preprocess would reject it")
	}
	req := &adkmodel.LLMRequest{}
	if err := pr.ProcessRequest(nil, req); err != nil {
		t.Fatalf("ProcessRequest: %v", err)
	}
	if _, isWrapper := req.Tools["spawn_agent"].(rosterTool); !isWrapper {
		t.Errorf("req.Tools[spawn_agent] = %T, want the roster wrapper (dispatch would bypass it)", req.Tools["spawn_agent"])
	}
	var found bool
	for _, gt := range req.Config.Tools {
		for _, fd := range gt.FunctionDeclarations {
			if fd.Name == "spawn_agent" && strings.Contains(fd.Description, "cluster — read-only GKE triage for one cluster") {
				found = true
			}
		}
	}
	if !found {
		t.Error("the packed declaration does not carry the roster")
	}
}
