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

package coretuievent

import (
	"testing"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/adk/session"
	"google.golang.org/genai"
)

// refusedSpawnResponse is the response map spawn_agent now returns for
// a refusal — pinned against the producer by
// pkg/agent/background.TestSpawnTool_RefusalCarriesTheReservedErrorKey,
// which asserts these exact keys come off the real tool's Run.
func refusedSpawnResponse() map[string]any {
	const refusal = `background: a subagent may not spawn itself: "cluster" is already running as an ancestor of this spawn`
	return map[string]any{
		"name":   "cluster",
		"branch": "",
		"status": "error: " + refusal,
		"error":  refusal,
	}
}

// The parent's chat stream. Pre-fix the projection had nothing to lift,
// so the refusal reached the renderer as three ordinary fields and drew
// without the failure affordance the renderer already has (#746).
func TestToolResult_RefusedSpawnProjectsAsAnError(t *testing.T) {
	t.Parallel()
	tr := ToolResult(&genai.Part{FunctionResponse: &genai.FunctionResponse{
		ID: "c1", Name: "spawn_agent", Response: refusedSpawnResponse(),
	}})
	if tr.Error == "" {
		t.Fatalf("Error is empty for a refused spawn: %+v", tr)
	}
	// The prose the model reads stays in the response map untouched —
	// the renderer picks the field, the model keeps the sentence.
	if got, _ := tr.Response["status"].(string); got == "" {
		t.Errorf("status was consumed out of the response: %+v", tr.Response)
	}
}

// The subagent turn-log surfaces (the /subagents overlay and the inline
// tail) — the ones the operator was actually reading during the UAT.
func TestSubagent_RefusedSpawnProjectsAsAnError(t *testing.T) {
	t.Parallel()
	ev := session.NewEvent("e1")
	ev.Author = "cluster-1"
	ev.LLMResponse = adkmodel.LLMResponse{Content: &genai.Content{
		Role: genai.RoleModel,
		Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{
			ID: "c1", Name: "spawn_agent", Response: refusedSpawnResponse(),
		}}},
	}}

	out, ok := Subagent(9, ev)
	if !ok {
		t.Fatalf("turn dropped: %+v", out)
	}
	if len(out.ToolResults) != 1 {
		t.Fatalf("tool results = %+v, want one", out.ToolResults)
	}
	if out.ToolResults[0].Error == "" {
		t.Errorf("Error is empty: %+v — the turn renders as a spawn that happened", out.ToolResults[0])
	}
}

// A launch that happened must stay unmarked, or the glyph stops
// meaning anything.
func TestToolResult_SuccessfulSpawnIsNotAnError(t *testing.T) {
	t.Parallel()
	tr := ToolResult(&genai.Part{FunctionResponse: &genai.FunctionResponse{
		ID: "c2", Name: "spawn_agent", Response: map[string]any{
			"name": "cluster-1", "branch": "bg.cluster-1", "status": "running",
		},
	}})
	if tr.Error != "" {
		t.Errorf("Error = %q for a successful spawn, want empty", tr.Error)
	}
}
