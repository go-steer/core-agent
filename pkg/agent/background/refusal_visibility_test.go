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

// A refused launch has to be legible as a failure to everything
// downstream of the model, not just to the model (#746).
//
// These tests run the real tools and assert on the map ADK puts in the
// FunctionResponse — the same bytes the event log stores, the REST
// subagent-events endpoint serves, and both TUIs project. Asserting on
// the Go struct instead would pass with a mistyped json tag, which is
// the whole distance between a rendered ✗ and "↳ branch, name, status".

package background

import (
	"context"
	"strings"
	"testing"

	adkagent "google.golang.org/adk/agent"
	"google.golang.org/adk/memory"
	"google.golang.org/adk/session"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/toolconfirmation"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/pkg/agent/internal/subsession"
	"github.com/go-steer/core-agent/v2/pkg/watchdog"
)

// refusalToolCtx is a minimal tool.Context for driving a registered
// tool's Run directly. Full-interface satisfaction is deliberate: an
// ADK bump that adds a method should break the stub rather than
// silently drift.
type refusalToolCtx struct {
	context.Context
}

func (c *refusalToolCtx) UserContent() *genai.Content          { return nil }
func (c *refusalToolCtx) InvocationID() string                 { return "test-invocation" }
func (c *refusalToolCtx) AgentName() string                    { return "test-agent" }
func (c *refusalToolCtx) ReadonlyState() session.ReadonlyState { return nil }
func (c *refusalToolCtx) UserID() string                       { return "test-user" }
func (c *refusalToolCtx) AppName() string                      { return "test-app" }
func (c *refusalToolCtx) SessionID() string                    { return "test-session" }
func (c *refusalToolCtx) Branch() string                       { return "" }
func (c *refusalToolCtx) Artifacts() adkagent.Artifacts        { return nil }
func (c *refusalToolCtx) State() session.State                 { return nil }
func (c *refusalToolCtx) FunctionCallID() string               { return "call-1" }
func (c *refusalToolCtx) Actions() *session.EventActions       { return &session.EventActions{} }
func (c *refusalToolCtx) ToolConfirmation() *toolconfirmation.ToolConfirmation {
	return nil
}
func (c *refusalToolCtx) RequestConfirmation(string, any) error { return nil }
func (c *refusalToolCtx) SearchMemory(context.Context, string) (*memory.SearchResponse, error) {
	return nil, nil
}

// runToolJSON invokes a registered tool the way the ADK flow does and
// returns the response map that becomes FunctionResponse.Response.
func runToolJSON(t *testing.T, tl tool.Tool, ctx context.Context, args map[string]any) map[string]any {
	t.Helper()
	runner, ok := tl.(interface {
		Run(tool.Context, any) (map[string]any, error)
	})
	if !ok {
		t.Fatalf("%s is not runnable", tl.Name())
	}
	res, err := runner.Run(&refusalToolCtx{Context: ctx}, args)
	if err != nil {
		t.Fatalf("%s.Run: %v", tl.Name(), err)
	}
	return res
}

// errorText reads the reserved key the way every consumer does — the
// TUI adapter (internal/coretuievent), the watchdog's failure streak
// (pkg/agent/watchdog.go), and ADK's own tool-result span.
func errorText(t *testing.T, resp map[string]any) string {
	t.Helper()
	v, ok := resp["error"]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		t.Fatalf("error key = %v (%T), want a string", v, v)
	}
	return s
}

// The acceptance test for #746, over the two refusals the 2026-08-14
// GKE UAT could produce. Pre-fix the response map is {branch, name,
// status} with the refusal buried in status, so nothing but the model
// can tell the call failed and the TUI draws it as a launch.
func TestSpawnTool_RefusalCarriesTheReservedErrorKey(t *testing.T) {
	t.Parallel()
	prov := &recordingProvider{llm: &stopRaceLLM{}}
	mgr := newTemplateManager(t, prov, []SubagentTemplate{
		{Name: "cluster", ModelFactory: tmplFactory(prov, "m"), ModelID: "m"},
	})
	attachEchoParent(t, mgr)
	defer mgr.Close()
	spawn := NewSpawnAgentTool(mgr)

	cases := []struct {
		name string
		ctx  context.Context
		args map[string]any
		want string // substring of the refusal
	}{{
		// The UAT verbatim: the cluster subagent delegating to itself.
		name: "self-spawn (#742)",
		ctx:  subsession.WithLineage(context.Background(), "cluster"),
		args: map[string]any{"agent": "cluster", "goal": "Diagnose the image pull failure"},
		want: "may not spawn itself",
	}, {
		name: "unknown reference",
		ctx:  context.Background(),
		args: map[string]any{"agent": "no-such-subagent", "goal": "triage"},
		want: "no-such-subagent",
	}, {
		name: "ad-hoc disabled",
		ctx:  context.Background(),
		args: map[string]any{"name": "improvised", "system_prompt": "you are new", "goal": "triage"},
		want: "ad-hoc",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := runToolJSON(t, spawn, tc.ctx, tc.args)

			errText := errorText(t, resp)
			if errText == "" {
				t.Errorf("no %q key in %v — a refusal renders exactly like a launch", "error", resp)
			}
			if !strings.Contains(errText, tc.want) {
				t.Errorf("error = %q, want it to contain %q: the glyph is only half the fix if the reason isn't next to it", errText, tc.want)
			}
			// The model's half of the contract is unchanged: it still
			// reads the refusal as prose in status and adapts.
			status, _ := resp["status"].(string)
			if !strings.HasPrefix(status, "error: ") || !strings.Contains(status, tc.want) {
				t.Errorf("status = %q, want the unchanged \"error: ...\" prose", status)
			}
		})
	}
}

// The control: a launch that happened must NOT carry the key, or every
// spawn renders as a failure and the affordance means nothing.
func TestSpawnTool_SuccessfulLaunchHasNoErrorKey(t *testing.T) {
	t.Parallel()
	prov := &recordingProvider{llm: &stopRaceLLM{}}
	mgr := newTemplateManager(t, prov, []SubagentTemplate{
		{Name: "cluster", ModelFactory: tmplFactory(prov, "m"), ModelID: "m"},
	})
	attachEchoParent(t, mgr)
	defer mgr.Close()

	resp := runToolJSON(t, NewSpawnAgentTool(mgr), context.Background(),
		map[string]any{"agent": "cluster", "goal": "triage"})
	if got := errorText(t, resp); got != "" {
		t.Errorf("successful spawn carries error = %q, want none", got)
	}
	name, _ := resp["name"].(string)
	if name == "" {
		t.Fatalf("spawn result = %v, want an instance name", resp)
	}
	if h, ok := mgr.Get(name); ok {
		waitDone(t, h)
	}
}

// stop_agent is the same shape one function down: pre-fix, stopping a
// subagent that isn't there reported like stopping one that was.
func TestStopTool_UnknownNameCarriesTheReservedErrorKey(t *testing.T) {
	t.Parallel()
	mgr, _ := newFakeManager(t)
	defer mgr.Close()

	resp := runToolJSON(t, NewStopAgentTool(mgr), context.Background(),
		map[string]any{"name": "ghost"})
	if got := errorText(t, resp); got == "" {
		t.Errorf("no error key in %v — a stop that didn't happen looks like one that did", resp)
	}
	status, _ := resp["status"].(string)
	if !strings.HasPrefix(status, "error: ") {
		t.Errorf("status = %q, want the unchanged \"error: ...\" prose", status)
	}
}

// The reserved key is also what watchdog.ToolResult.Failed() reads
// (pkg/agent.toolResponseError), so a refused spawn now counts toward
// the tool-failure streak (#639) instead of being scored as a success
// — which is what a model retrying a refused spawn in a loop needs.
func TestSpawnRefusal_ScoresAsAFailedToolCall(t *testing.T) {
	t.Parallel()
	prov := &recordingProvider{llm: &stopRaceLLM{}}
	mgr := newTemplateManager(t, prov, []SubagentTemplate{
		{Name: "cluster", ModelFactory: tmplFactory(prov, "m"), ModelID: "m"},
	})
	attachEchoParent(t, mgr)
	defer mgr.Close()

	ctx := subsession.WithLineage(context.Background(), "cluster")
	resp := runToolJSON(t, NewSpawnAgentTool(mgr), ctx,
		map[string]any{"agent": "cluster", "goal": "again"})
	tr := watchdog.ToolResult{Name: SpawnAgentToolName, Error: errorText(t, resp)}
	if !tr.Failed() {
		t.Errorf("watchdog scores %v as a successful call — a refusal loop it can't see", resp)
	}
}
