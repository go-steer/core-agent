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

package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v3/pkg/agent"
	"github.com/go-steer/core-agent/v3/pkg/agent/autonomous"
)

func reportSchema() *genai.Schema {
	return &genai.Schema{
		Type: genai.TypeObject,
		Properties: map[string]*genai.Schema{
			"summary":  {Type: genai.TypeString},
			"severity": {Type: genai.TypeString},
			"restarts": {Type: genai.TypeInteger},
		},
		Required: []string{"summary", "severity"},
	}
}

type scriptedTaskModel struct {
	name       string
	args       []map[string]any
	calls      int
	decl       *genai.Schema
	toolErrors []string
}

func (m *scriptedTaskModel) Name() string { return m.name }

func (m *scriptedTaskModel) GenerateContent(_ context.Context, req *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		m.decl = finishTaskParams(req)
		m.toolErrors = collectFinishTaskErrors(req.Contents)

		i := m.calls
		m.calls++
		resp := &adkmodel.LLMResponse{TurnComplete: true, FinishReason: genai.FinishReasonStop}
		if i >= len(m.args) {
			resp.Content = genai.NewContentFromText("out of script", genai.RoleModel)
		} else {
			resp.Content = &genai.Content{
				Role:  genai.RoleModel,
				Parts: []*genai.Part{genai.NewPartFromFunctionCall(agent.FinishTaskToolName, m.args[i])},
			}
		}
		yield(resp, nil)
	}
}

func finishTaskParams(req *adkmodel.LLMRequest) *genai.Schema {
	if req == nil || req.Config == nil {
		return nil
	}
	for _, t := range req.Config.Tools {
		if t == nil {
			continue
		}
		for _, fd := range t.FunctionDeclarations {
			if fd != nil && fd.Name == agent.FinishTaskToolName {
				return fd.Parameters
			}
		}
	}
	return nil
}

func collectFinishTaskErrors(contents []*genai.Content) []string {
	var out []string
	for _, c := range contents {
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			if p == nil || p.FunctionResponse == nil || p.FunctionResponse.Name != agent.FinishTaskToolName {
				continue
			}
			if e, ok := p.FunctionResponse.Response["error"]; ok {
				if s, ok := e.(string); ok {
					out = append(out, s)
				}
			}
		}
	}
	return out
}

type parentDelegatingModel struct {
	subagentName string
	responses    []map[string]any
}

func (m *parentDelegatingModel) Name() string { return "parent" }

func (m *parentDelegatingModel) GenerateContent(_ context.Context, req *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		for _, c := range req.Contents {
			if c == nil {
				continue
			}
			for _, p := range c.Parts {
				if p != nil && p.FunctionResponse != nil && p.FunctionResponse.Name == m.subagentName {
					m.responses = append(m.responses, p.FunctionResponse.Response)
					yield(&adkmodel.LLMResponse{
						TurnComplete: true,
						FinishReason: genai.FinishReasonStop,
						Content:      genai.NewContentFromText("done", genai.RoleModel),
					}, nil)
					return
				}
			}
		}
		yield(&adkmodel.LLMResponse{
			TurnComplete: true,
			FinishReason: genai.FinishReasonStop,
			Content: &genai.Content{
				Role:  genai.RoleModel,
				Parts: []*genai.Part{genai.NewPartFromFunctionCall(m.subagentName, map[string]any{"request": "investigate"})},
			},
		}, nil)
	}
}

func runParentWithSubagent(t *testing.T, sub *agent.Agent) []map[string]any {
	t.Helper()
	pm := &parentDelegatingModel{subagentName: sub.AgentName()}
	parent, err := agent.New(pm,
		agent.WithName("root"),
		agent.WithSubagents([]*agent.Agent{sub}),
	)
	if err != nil {
		t.Fatalf("agent.New(parent): %v", err)
	}
	for _, err := range parent.Run(context.Background(), "hello") {
		if err != nil {
			t.Fatalf("parent.Run: %v", err)
		}
	}
	return pm.responses
}

func TestNewSubagentTool_TaskModeEnforcesOutputSchema(t *testing.T) {
	bad := map[string]any{"summary": "api is crashlooping"}
	good := map[string]any{"summary": "api is crashlooping", "severity": "CRITICAL"}

	m := &scriptedTaskModel{name: "analyst", args: []map[string]any{bad, good}}
	sub, err := agent.New(m,
		agent.WithName("diagnoser"),
		agent.WithDescription("diagnose cluster"),
		agent.WithInstruction("diagnose"),
		agent.WithSubagentMode(agent.SubagentModeTask),
		agent.WithOutputSchema(reportSchema()),
	)
	if err != nil {
		t.Fatalf("agent.New(sub): %v", err)
	}

	resps := runParentWithSubagent(t, sub)
	if m.decl == nil {
		t.Fatal("finish_task was declared without parameters: the output schema never reached the model")
	}
	if _, ok := m.decl.Properties["severity"]; !ok {
		t.Errorf("finish_task parameters do not carry the schema's properties: %v", m.decl.Properties)
	}
	if len(m.toolErrors) == 0 {
		t.Fatal("the finish_task call missing a required key was accepted: no error came back to the model")
	}
	if got := m.toolErrors[0]; !strings.Contains(got, "severity") {
		t.Errorf("validation error does not name the missing key: %q", got)
	}

	if len(resps) != 1 {
		t.Fatalf("got %d delegation responses, want 1: %v", len(resps), resps)
	}
	outMap, ok := resps[0]["output"].(map[string]any)
	if !ok {
		t.Fatalf("response[\"output\"] is %T, want map[string]any: %v", resps[0]["output"], resps[0])
	}
	if outMap["severity"] != "CRITICAL" || outMap["summary"] != "api is crashlooping" {
		t.Errorf("output = %v, want conforming object", outMap)
	}
}

type replyModel struct {
	name  string
	reply string
}

func (m *replyModel) Name() string { return m.name }

func (m *replyModel) GenerateContent(_ context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		yield(&adkmodel.LLMResponse{
			TurnComplete: true,
			FinishReason: genai.FinishReasonStop,
			Content:      genai.NewContentFromText(m.reply, genai.RoleModel),
		}, nil)
	}
}

func TestNewSubagentTool_SingleTurnModeEnforcesOutputSchema(t *testing.T) {
	t.Run("a violating reply is refused", func(t *testing.T) {
		sub, err := agent.New(&replyModel{name: "m", reply: `{"summary": "no severity here"}`},
			agent.WithName("classifier_bad"),
			agent.WithDescription("d"),
			agent.WithInstruction("i"),
			agent.WithSubagentMode(agent.SubagentModeSingleTurn),
			agent.WithOutputSchema(reportSchema()),
		)
		if err != nil {
			t.Fatalf("agent.New: %v", err)
		}
		resps := runParentWithSubagent(t, sub)
		if len(resps) != 1 {
			t.Fatalf("got %d delegation responses, want 1: %v", len(resps), resps)
		}
		msg, ok := resps[0]["error"].(string)
		if !ok {
			t.Fatalf("a reply missing a required key was accepted; response = %v", resps[0])
		}
		if !strings.Contains(msg, "severity") {
			t.Errorf("refusal does not name the missing key: %q", msg)
		}
		if _, ok := resps[0]["output"]; ok {
			t.Errorf("a refused reply still produced an output: %v", resps[0])
		}
	})

	t.Run("a conforming reply is parsed into an object", func(t *testing.T) {
		sub, err := agent.New(&replyModel{name: "m", reply: `{"summary": "ok", "severity": "INFO"}`},
			agent.WithName("classifier_good"),
			agent.WithDescription("d"),
			agent.WithInstruction("i"),
			agent.WithSubagentMode(agent.SubagentModeSingleTurn),
			agent.WithOutputSchema(reportSchema()),
		)
		if err != nil {
			t.Fatalf("agent.New: %v", err)
		}
		resps := runParentWithSubagent(t, sub)
		if len(resps) != 1 {
			t.Fatalf("got %d delegation responses, want 1: %v", len(resps), resps)
		}
		if e, ok := resps[0]["error"]; ok {
			t.Fatalf("a conforming reply was refused: %v", e)
		}
		got, ok := resps[0]["output"].(map[string]any)
		if !ok {
			t.Fatalf("output is %T, want parsed object: %v", resps[0]["output"], resps[0])
		}
		if got["severity"] != "INFO" || got["summary"] != "ok" {
			t.Errorf("output = %v, want parsed reply", got)
		}
	})

	t.Run("without a schema the same reply is accepted as text", func(t *testing.T) {
		sub, err := agent.New(&replyModel{name: "m", reply: `{"summary": "no severity here"}`},
			agent.WithName("classifier_loose"),
			agent.WithDescription("d"),
			agent.WithInstruction("i"),
			agent.WithSubagentMode(agent.SubagentModeSingleTurn),
		)
		if err != nil {
			t.Fatalf("agent.New: %v", err)
		}
		resps := runParentWithSubagent(t, sub)
		if len(resps) != 1 {
			t.Fatalf("got %d delegation responses, want 1: %v", len(resps), resps)
		}
		if e, ok := resps[0]["error"]; ok {
			t.Fatalf("an unconstrained specialist refused its own reply: %v", e)
		}
		if got, ok := resps[0]["result"].(string); !ok {
			t.Errorf("result is %T, want raw text", resps[0]["result"])
		} else if !strings.Contains(got, "no severity here") {
			t.Errorf("result = %q, want reply verbatim", got)
		}
	})
}

type declCaptureModel struct {
	name  string
	decls []*genai.FunctionDeclaration
}

func (m *declCaptureModel) Name() string { return m.name }

func (m *declCaptureModel) GenerateContent(_ context.Context, req *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		m.decls = nil
		if req != nil && req.Config != nil {
			for _, gt := range req.Config.Tools {
				if gt != nil {
					m.decls = append(m.decls, gt.FunctionDeclarations...)
				}
			}
		}
		yield(&adkmodel.LLMResponse{
			TurnComplete: true,
			FinishReason: genai.FinishReasonStop,
			Content:      genai.NewContentFromText("ok", genai.RoleModel),
		}, nil)
	}
}

func TestNewSubagentTool_ModeUnsetIsChat(t *testing.T) {
	subUnset, err := agent.New(&replyModel{name: "sub", reply: "hi"},
		agent.WithName("researcher"),
		agent.WithDescription("Research specialist"),
	)
	if err != nil {
		t.Fatalf("agent.New(subUnset): %v", err)
	}
	subChat, err := agent.New(&replyModel{name: "sub", reply: "hi"},
		agent.WithName("researcher"),
		agent.WithDescription("Research specialist"),
		agent.WithSubagentMode(agent.SubagentModeChat),
	)
	if err != nil {
		t.Fatalf("agent.New(subChat): %v", err)
	}

	pmUnset := &declCaptureModel{name: "parent"}
	parentUnset, err := agent.New(pmUnset, agent.WithSubagents([]*agent.Agent{subUnset}))
	if err != nil {
		t.Fatalf("agent.New(parentUnset): %v", err)
	}
	for _, err := range parentUnset.Run(context.Background(), "hi") {
		if err != nil {
			t.Fatalf("parentUnset.Run: %v", err)
		}
	}

	pmChat := &declCaptureModel{name: "parent"}
	parentChat, err := agent.New(pmChat, agent.WithSubagents([]*agent.Agent{subChat}))
	if err != nil {
		t.Fatalf("agent.New(parentChat): %v", err)
	}
	for _, err := range parentChat.Run(context.Background(), "hi") {
		if err != nil {
			t.Fatalf("parentChat.Run: %v", err)
		}
	}

	rawUnset, err := json.Marshal(pmUnset.decls)
	if err != nil {
		t.Fatalf("json.Marshal(pmUnset.decls): %v", err)
	}
	rawChat, err := json.Marshal(pmChat.decls)
	if err != nil {
		t.Fatalf("json.Marshal(pmChat.decls): %v", err)
	}
	if string(rawUnset) != string(rawChat) {
		t.Errorf("mode unset declaration differs from chat:\nunset: %s\nchat:  %s", rawUnset, rawChat)
	}
	if strings.Contains(string(rawUnset), `"output"`) {
		t.Errorf("chat-mode subagent declaration unexpectedly contains task output field: %s", rawUnset)
	}
}

func TestSubagentModeValidation(t *testing.T) {
	t.Run("OutputSchema rejected on chat mode", func(t *testing.T) {
		_, err := agent.New(&replyModel{name: "m"},
			agent.WithName("bad"),
			agent.WithOutputSchema(reportSchema()),
		)
		if err == nil {
			t.Fatal("expected error when OutputSchema is set without SubagentModeTask or SubagentModeSingleTurn")
		}
	})

	t.Run("Tools rejected on single_turn mode", func(t *testing.T) {
		dummyTool, err := functiontool.New(functiontool.Config{Name: "t", Description: "t"},
			func(_ adkagent.Context, _ struct{}) (string, error) { return "", nil })
		if err != nil {
			t.Fatalf("functiontool.New: %v", err)
		}
		_, err = agent.New(&replyModel{name: "m"},
			agent.WithName("bad"),
			agent.WithSubagentMode(agent.SubagentModeSingleTurn),
			agent.WithTools([]tool.Tool{dummyTool}),
		)
		if err == nil {
			t.Fatal("expected error when tools are passed to SubagentModeSingleTurn")
		}
	})
}

func TestFinishTaskIsStillCalledThat(t *testing.T) {
	m := &declCaptureModel{name: "task"}
	a, err := agent.NewTaskAgent(agent.TaskAgentConfig{
		Name:        "worker",
		Description: "worker",
		Model:       m,
	})
	if err != nil {
		t.Fatalf("NewTaskAgent: %v", err)
	}
	coord, err := agent.NewCoordinator(agent.CoordinatorConfig{
		Name:        "coord",
		Description: "coord",
		Model:       &parentDelegatingModel{subagentName: "worker"},
		SubAgents:   []adkagent.Agent{a},
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	r, err := runner.New(runner.Config{
		AppName:           "test",
		Agent:             coord,
		SessionService:    session.InMemoryService(),
		AutoCreateSession: true,
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}
	for _, err := range r.Run(context.Background(), "u", "s", genai.NewContentFromText("go", genai.RoleUser), adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("r.Run: %v", err)
		}
	}
	found := false
	for _, fd := range m.decls {
		if fd != nil && fd.Name == agent.FinishTaskToolName {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("Task agent was not offered %q; declared: %v", agent.FinishTaskToolName, m.decls)
	}
}

func TestFinishOnStallClosesASilentTurn(t *testing.T) {
	cb := agent.FinishOnStall("pod_inspector", nil)
	resp := &adkmodel.LLMResponse{
		Content: &genai.Content{
			Role: genai.RoleModel,
			Parts: []*genai.Part{
				{Text: "reasoning", Thought: true},
				{Text: "Could you run kubectl get all and paste the output?"},
			},
		},
		TurnComplete: true,
		FinishReason: genai.FinishReasonStop,
	}
	out, err := cb(nil, resp, nil)
	if err != nil {
		t.Fatalf("FinishOnStall: %v", err)
	}
	if out == nil || len(out.Content.Parts) != 3 {
		t.Fatalf("expected original 2 parts + 1 injected function call, got %v", out)
	}
	fc := out.Content.Parts[2].FunctionCall
	if fc == nil || fc.Name != agent.FinishTaskToolName {
		t.Fatalf("expected injected %q call, got %v", agent.FinishTaskToolName, fc)
	}
	result, _ := fc.Args["result"].(string)
	if !agent.Stalled(result) {
		t.Errorf("result not marked as stalled: %q", result)
	}
	if !strings.Contains(result, "kubectl get all") || strings.Contains(result, "reasoning") {
		t.Errorf("unexpected stall text (should include non-thought text and exclude thought): %q", result)
	}
}

type stubCallGate struct {
	mu      sync.Mutex
	refuse  map[string]string
	granted map[string]bool
}

func (g *stubCallGate) Allow(agentName string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if reason, ok := g.refuse[agentName]; ok {
		return errors.New(reason)
	}
	return nil
}

func (g *stubCallGate) AllowFinalReport(agentName string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.granted[agentName] {
		delete(g.granted, agentName)
		return true
	}
	return false
}

func TestCallGateAndFinalReport(t *testing.T) {
	t.Run("refuses before reaching provider", func(t *testing.T) {
		m := &scriptedTaskModel{name: "analyst", args: []map[string]any{{"result": "ok"}}}
		sub, err := agent.New(m,
			agent.WithName("diagnoser"),
			agent.WithDescription("d"),
			agent.WithSubagentMode(agent.SubagentModeTask),
		)
		if err != nil {
			t.Fatalf("agent.New: %v", err)
		}
		pm := &parentDelegatingModel{subagentName: "diagnoser"}
		parent, err := agent.New(pm, agent.WithSubagents([]*agent.Agent{sub}))
		if err != nil {
			t.Fatalf("agent.New(parent): %v", err)
		}
		gate := &stubCallGate{refuse: map[string]string{"diagnoser": "budget exhausted"}}
		ctx := agent.WithCallGate(context.Background(), gate)
		for _, err := range parent.Run(ctx, "go") {
			if err != nil {
				t.Fatalf("parent.Run: %v", err)
			}
		}
		if m.calls != 0 {
			t.Errorf("refused subagent model was called %d times, want 0", m.calls)
		}
		if len(pm.responses) != 1 {
			t.Fatalf("got %d responses, want 1", len(pm.responses))
		}
		res, _ := pm.responses[0]["result"].(string)
		if !agent.Refused(res) {
			t.Errorf("result not marked as refused: %q", res)
		}
	})

	t.Run("final report grant strips investigation tools and allows one report call", func(t *testing.T) {
		m := &scriptedTaskModel{
			name: "analyst",
			args: []map[string]any{{"summary": "partial finding", "severity": "WARN"}},
		}
		dummyTool, err := functiontool.New(functiontool.Config{Name: "get_logs", Description: "get logs"},
			func(_ adkagent.Context, _ struct{}) (string, error) { return "logs", nil })
		if err != nil {
			t.Fatalf("functiontool.New: %v", err)
		}
		sub, err := agent.New(m,
			agent.WithName("diagnoser"),
			agent.WithDescription("d"),
			agent.WithSubagentMode(agent.SubagentModeTask),
			agent.WithOutputSchema(reportSchema()),
			agent.WithTools([]tool.Tool{dummyTool}),
		)
		if err != nil {
			t.Fatalf("agent.New: %v", err)
		}
		pm := &parentDelegatingModel{subagentName: "diagnoser"}
		parent, err := agent.New(pm, agent.WithSubagents([]*agent.Agent{sub}))
		if err != nil {
			t.Fatalf("agent.New(parent): %v", err)
		}
		gate := &stubCallGate{
			refuse:  map[string]string{"diagnoser": "turn cap 5"},
			granted: map[string]bool{"diagnoser": true},
		}
		ctx := agent.WithCallGate(context.Background(), gate)
		for _, err := range parent.Run(ctx, "go") {
			if err != nil {
				t.Fatalf("parent.Run: %v", err)
			}
		}
		if m.calls != 1 {
			t.Fatalf("granted subagent made %d calls, want 1", m.calls)
		}
		if len(pm.responses) != 1 {
			t.Fatalf("got %d responses, want 1", len(pm.responses))
		}
		outMap, ok := pm.responses[0]["output"].(map[string]any)
		if !ok || outMap["summary"] != "partial finding" {
			t.Errorf("output = %v, want partial finding", pm.responses[0])
		}
	})

	t.Run("task subagent with output_schema that stalls without calling finish_task is marked stalled and does not bypass schema", func(t *testing.T) {
		sub, err := agent.New(&replyModel{name: "m", reply: "I stopped talking without calling finish_task"},
			agent.WithName("stalled_diagnoser"),
			agent.WithDescription("d"),
			agent.WithSubagentMode(agent.SubagentModeTask),
			agent.WithOutputSchema(reportSchema()),
		)
		if err != nil {
			t.Fatalf("agent.New: %v", err)
		}
		resps := runParentWithSubagent(t, sub)
		if len(resps) != 1 {
			t.Fatalf("got %d delegation responses, want 1: %v", len(resps), resps)
		}
		res, _ := resps[0]["result"].(string)
		if !agent.Stalled(res) {
			t.Errorf("result not marked stalled: %q", res)
		}
		if _, ok := resps[0]["output"]; ok {
			t.Errorf("stalled task subagent should not populate structured output, got %v", resps[0]["output"])
		}
	})
}

func TestModesHardening_1450(t *testing.T) {
	t.Parallel()

	enumAndAnyOfSchema := &genai.Schema{
		Type:     genai.TypeObject,
		Required: []string{"severity", "detail"},
		Properties: map[string]*genai.Schema{
			"severity": {
				Type: genai.TypeString,
				Enum: []string{"INFO", "WARN", "CRITICAL"},
			},
			"detail": {
				Type: genai.TypeString,
				AnyOf: []*genai.Schema{
					{Type: genai.TypeString, Enum: []string{"ok", "first try"}},
					{Type: genai.TypeString, Enum: []string{"resolved"}},
				},
			},
		},
	}

	t.Run("single_turn with output_schema rejects empty reply in NewSubagentTool and autonomous.Run", func(t *testing.T) {
		sub, err := agent.New(&replyModel{name: "m", reply: ""},
			agent.WithName("empty_classifier"),
			agent.WithDescription("d"),
			agent.WithSubagentMode(agent.SubagentModeSingleTurn),
			agent.WithOutputSchema(reportSchema()),
		)
		if err != nil {
			t.Fatalf("agent.New: %v", err)
		}
		resps := runParentWithSubagent(t, sub)
		if len(resps) != 1 {
			t.Fatalf("got %d responses, want 1", len(resps))
		}
		if errMsg, _ := resps[0]["error"].(string); !strings.Contains(errMsg, "empty response did not satisfy output_schema") {
			t.Errorf("NewSubagentTool empty reply error = %q, want 'empty response did not satisfy output_schema'", errMsg)
		}

		subAuto, err := agent.New(&replyModel{name: "m", reply: ""},
			agent.WithName("empty_auto"),
			agent.WithDescription("d"),
			agent.WithMode(agent.ModeAutonomous),
			agent.WithSubagentMode(agent.SubagentModeSingleTurn),
			agent.WithOutputSchema(reportSchema()),
		)
		if err != nil {
			t.Fatalf("agent.New(subAuto): %v", err)
		}
		_, runErr := autonomous.Run(
			context.Background(),
			func([]tool.Tool) (*agent.Agent, error) { return subAuto, nil },
			"classify",
			autonomous.WithMaxTurns(1),
		)
		if runErr == nil || !strings.Contains(runErr.Error(), "empty response did not satisfy output_schema") {
			t.Errorf("autonomous.Run empty reply err = %v, want 'empty response did not satisfy output_schema'", runErr)
		}
	})

	t.Run("enum and anyOf enforced at runtime across task and single_turn modes", func(t *testing.T) {
		subBadEnum, err := agent.New(&replyModel{name: "m", reply: `{"severity":"BOGUS","detail":"ok"}`},
			agent.WithName("bad_enum"),
			agent.WithDescription("d"),
			agent.WithSubagentMode(agent.SubagentModeSingleTurn),
			agent.WithOutputSchema(enumAndAnyOfSchema),
		)
		if err != nil {
			t.Fatalf("agent.New: %v", err)
		}
		resps := runParentWithSubagent(t, subBadEnum)
		if errMsg, _ := resps[0]["error"].(string); !strings.Contains(errMsg, "not one of enum") {
			t.Errorf("single_turn bad enum error = %q, want 'not one of enum'", errMsg)
		}

		subBadAnyOf, err := agent.New(&replyModel{name: "m", reply: `{"severity":"WARN","detail":"unlisted"}`},
			agent.WithName("bad_anyof"),
			agent.WithDescription("d"),
			agent.WithSubagentMode(agent.SubagentModeSingleTurn),
			agent.WithOutputSchema(enumAndAnyOfSchema),
		)
		if err != nil {
			t.Fatalf("agent.New: %v", err)
		}
		respsAnyOf := runParentWithSubagent(t, subBadAnyOf)
		if errMsg, _ := respsAnyOf[0]["error"].(string); !strings.Contains(errMsg, "anyOf") {
			t.Errorf("single_turn bad anyOf error = %q, want 'anyOf'", errMsg)
		}

		// Task mode: first call fails enum check via finish_task callback, second call succeeds.
		taskModel := &scriptedTaskModel{
			name: "task_enum",
			args: []map[string]any{
				{"severity": "INVALID", "detail": "first try"},
				{"severity": "CRITICAL", "detail": "resolved"},
			},
		}
		subTask, err := agent.New(taskModel,
			agent.WithName("task_enum_agent"),
			agent.WithDescription("d"),
			agent.WithMode(agent.ModeAutonomous),
			agent.WithSubagentMode(agent.SubagentModeTask),
			agent.WithOutputSchema(enumAndAnyOfSchema),
		)
		if err != nil {
			t.Fatalf("agent.New(subTask): %v", err)
		}
		runRes, runErr := autonomous.Run(
			context.Background(),
			func([]tool.Tool) (*agent.Agent, error) { return subTask, nil },
			"go",
			autonomous.WithMaxTurns(5),
		)
		if runErr != nil {
			t.Fatalf("autonomous.Run: %v", runErr)
		}
		if !runRes.Returned {
			t.Errorf("runRes.Returned = false, want true after retry")
		}
		for _, c := range runRes.Calls {
			if c.Tool == agent.FinishTaskToolName {
				t.Errorf("driver tool %q leaked into RunResult.Calls: %+v", agent.FinishTaskToolName, runRes.Calls)
			}
		}
	})

	t.Run("ParseOutputSchema supports yaml and enforces baseDirs containment", func(t *testing.T) {
		tmp := t.TempDir()
		baseDir := filepath.Join(tmp, "bundle")
		if err := os.MkdirAll(baseDir, 0o755); err != nil {
			t.Fatal(err)
		}
		yamlFile := filepath.Join(baseDir, "out.yaml")
		if err := os.WriteFile(yamlFile, []byte("type: object\nproperties:\n  ok:\n    type: boolean\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if s, err := agent.ParseOutputSchema("out.yaml", baseDir); err != nil || s == nil {
			t.Fatalf("ParseOutputSchema(out.yaml) = (%v, %v), want valid schema", s, err)
		}
		outsideFile := filepath.Join(tmp, "outside.json")
		if err := os.WriteFile(outsideFile, []byte(`{"type":"object","properties":{"ok":{"type":"boolean"}}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := agent.ParseOutputSchema("../outside.json", baseDir); err == nil || !strings.Contains(err.Error(), "escapes scope root") {
			t.Fatalf("ParseOutputSchema(../outside.json) = %v, want containment error", err)
		}
	})
}
