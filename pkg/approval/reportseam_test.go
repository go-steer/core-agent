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

package approval

import (
	"context"
	"iter"
	"strings"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/plugin"
	"google.golang.org/adk/v2/runner"
	adksession "google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"
)

type reportProbe struct {
	toolsSeen []string
	argsSeen  []map[string]any
	responses []map[string]any
	rounds    int
	svc       adksession.Service
}

func (p *reportProbe) argsFor(name string) []map[string]any {
	var out []map[string]any
	for i, seen := range p.toolsSeen {
		if seen == name {
			out = append(out, p.argsSeen[i])
		}
	}
	return out
}

func calledInHistory(req *model.LLMRequest, name string) bool {
	for _, c := range req.Contents {
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			if p != nil && p.FunctionCall != nil && p.FunctionCall.Name == name {
				return true
			}
		}
	}
	return false
}

type callOnce struct{ target string }

func (callOnce) Name() string { return "call-once" }

func (m callOnce) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		resp := &model.LLMResponse{
			Content:      genai.NewContentFromText("done", genai.RoleModel),
			TurnComplete: true,
			FinishReason: genai.FinishReasonStop,
		}
		if _, ok := req.Tools[m.target]; ok && !calledInHistory(req, m.target) {
			resp.Content = &genai.Content{
				Role: genai.RoleModel,
				Parts: []*genai.Part{genai.NewPartFromFunctionCall(m.target,
					map[string]any{"request": "diagnose"})},
			}
		}
		yield(resp, nil)
	}
}

func runReportProbe(t *testing.T, schema *genai.Schema, report map[string]any, gate llmagent.BeforeToolCallback) *reportProbe {
	t.Helper()
	probe := &reportProbe{}

	m := &scriptedModel{name: "report", calls: []*model.LLMResponse{{
		Content: &genai.Content{
			Role:  genai.RoleModel,
			Parts: []*genai.Part{genai.NewPartFromFunctionCall(FinishTaskToolName, report)},
		},
	}}}

	spec, err := llmagent.New(llmagent.Config{
		Name:         "reporter",
		Description:  "returns a structured report",
		Instruction:  "report",
		Model:        m,
		OutputSchema: schema,
		Mode:         llmagent.ModeTask,
	})
	if err != nil {
		t.Fatalf("llmagent.New: %v", err)
	}
	root, err := llmagent.New(llmagent.Config{
		Name:        "coordinator",
		Description: "report seam probe",
		Instruction: "dispatch",
		Model:       callOnce{target: "reporter"},
		SubAgents:   []adkagent.Agent{spec},
	})
	if err != nil {
		t.Fatalf("llmagent.New: %v", err)
	}

	p, err := plugin.New(plugin.Config{
		Name: "report-gate",
		BeforeToolCallback: func(ctx adkagent.Context, tl tool.Tool, args map[string]any) (map[string]any, error) {
			probe.toolsSeen = append(probe.toolsSeen, tl.Name())
			probe.argsSeen = append(probe.argsSeen, args)
			if gate == nil {
				return nil, nil
			}
			return gate(ctx, tl, args)
		},
	})
	if err != nil {
		t.Fatalf("plugin.New: %v", err)
	}

	svc := sqliteService(t)
	probe.svc = svc
	r, err := runner.New(runner.Config{
		AppName:           testApp,
		Agent:             root,
		SessionService:    svc,
		AutoCreateSession: true,
		PluginConfig:      runner.PluginConfig{Plugins: []*plugin.Plugin{p}},
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}
	for _, err := range r.Run(context.Background(), testUser, sid,
		genai.NewContentFromText("diagnose", genai.RoleUser), adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("runner.Run: %v", err)
		}
	}

	m.mu.Lock()
	probe.rounds = m.round
	m.mu.Unlock()
	probe.responses = finishTaskResponses(t, svc)
	return probe
}

func finishTaskResponses(t *testing.T, svc adksession.Service) []map[string]any {
	t.Helper()
	got, err := svc.Get(context.Background(), &adksession.GetRequest{
		AppName: testApp, UserID: testUser, SessionID: sid,
	})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	var out []map[string]any
	for ev := range got.Session.Events().All() {
		if ev == nil || ev.Content == nil {
			continue
		}
		for _, part := range ev.Content.Parts {
			if part == nil || part.FunctionResponse == nil {
				continue
			}
			if part.FunctionResponse.Name != FinishTaskToolName {
				continue
			}
			out = append(out, part.FunctionResponse.Response)
		}
	}
	return out
}

func reportSchema() *genai.Schema {
	return &genai.Schema{
		Type: genai.TypeObject,
		Properties: map[string]*genai.Schema{
			"detail": {Type: genai.TypeString},
			"change": {
				Type: genai.TypeObject,
				Properties: map[string]*genai.Schema{
					"tool": {Type: genai.TypeString},
				},
			},
		},
		Required: []string{"detail"},
	}
}

func responseText(resp map[string]any) string {
	var b strings.Builder
	for k, v := range resp {
		b.WriteString(k)
		b.WriteString("=")
		if s, ok := v.(string); ok {
			b.WriteString(s)
		} else {
			b.WriteString("?")
		}
		b.WriteString(" ")
	}
	return b.String()
}

func TestFinishTaskIsTheReportSeam(t *testing.T) {
	schema := &genai.Schema{
		Type: genai.TypeObject,
		Properties: map[string]*genai.Schema{
			"detail":         {Type: genai.TypeString},
			"recommendation": {Type: genai.TypeString},
			ChangeSetField: {Type: genai.TypeArray, Items: &genai.Schema{
				Type: genai.TypeObject,
				Properties: map[string]*genai.Schema{
					"tool":      {Type: genai.TypeString},
					"arguments": {Type: genai.TypeString},
				},
				Required: []string{"tool", "arguments"},
			}},
		},
		Required: []string{"detail"},
	}
	report := map[string]any{
		"detail":         "api is OOMKilled",
		ChangeSetField:   []any{map[string]any{"tool": "patch_k8s_resource", "arguments": "{}"}},
		"recommendation": "raise the limit",
	}

	t.Run("the callback sees the report as arguments", func(t *testing.T) {
		p := runReportProbe(t, schema, report, nil)

		args := p.argsFor(FinishTaskToolName)
		if len(args) != 1 {
			t.Fatalf("before-tool callback saw %v, want one %s among them",
				p.toolsSeen, FinishTaskToolName)
		}
		got, err := ParseChangeSet(args[0])
		if err != nil {
			t.Fatalf("ParseChangeSet over the arguments the callback saw: %v", err)
		}
		if len(got) != 1 || got[0].Tool != "patch_k8s_resource" {
			t.Fatalf("change set read back as %+v", got)
		}
	})

	t.Run("an error response is refused back to the model", func(t *testing.T) {
		refuse := func(_ adkagent.Context, _ tool.Tool, _ map[string]any) (map[string]any, error) {
			return map[string]any{"error": "invalid_proposed_change", "detail": "fix it"}, nil
		}
		p := runReportProbe(t, schema, report, refuse)

		if p.rounds < 2 {
			t.Fatalf("model was asked %d time(s), want at least 2", p.rounds)
		}
		if len(p.responses) != 1 {
			t.Fatalf("finish_task responses = %d, want 1: %v", len(p.responses), p.responses)
		}
		if _, bad := p.responses[0]["error"]; !bad {
			t.Fatalf("the model was told %v, want the refusal", p.responses[0])
		}
	})

	t.Run("no response accepts the report", func(t *testing.T) {
		p := runReportProbe(t, schema, report, nil)

		if p.rounds != 1 {
			t.Fatalf("model was asked %d time(s), want exactly 1", p.rounds)
		}
		if len(p.responses) != 1 {
			t.Fatalf("finish_task responses = %d, want 1: %v", len(p.responses), p.responses)
		}
		if _, bad := p.responses[0]["error"]; bad {
			t.Fatalf("an unrefused report came back as %v, want success", p.responses[0])
		}
	})
}

func TestADKRefusesUndeclaredKeysInNestedObjects(t *testing.T) {
	ok := runReportProbe(t, reportSchema(), map[string]any{
		"detail": "api is OOMKilled",
		"change": map[string]any{"tool": "patch_k8s_resource"},
	}, nil)
	if len(ok.responses) != 1 {
		t.Fatalf("control: finish_task responses = %v, want 1", ok.responses)
	}
	if _, bad := ok.responses[0]["error"]; bad {
		t.Fatalf("control: a conforming report was refused (%v)",
			responseText(ok.responses[0]))
	}

	bad := runReportProbe(t, reportSchema(), map[string]any{
		"detail": "api is OOMKilled",
		"change": map[string]any{"tool": "patch_k8s_resource", "namespace": "prod"},
	}, nil)
	if len(bad.responses) == 0 {
		t.Fatalf("no finish_task response at all")
	}
	if _, refused := bad.responses[0]["error"]; !refused {
		t.Fatalf("an undeclared nested key was ACCEPTED (%v)",
			responseText(bad.responses[0]))
	}
	if !strings.Contains(strings.ToLower(responseText(bad.responses[0])), "namespace") {
		t.Errorf("refusal does not name the offending key: %v", bad.responses[0])
	}
}
