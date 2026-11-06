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
	"context"
	"iter"
	"strings"
	"sync"
	"testing"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v3/pkg/agent"
)

// goalCapturingLLM records the user text sent on each turn and returns
// a fixed reply so a single-turn or bounded subagent terminates cleanly.
type goalCapturingLLM struct {
	mu    sync.Mutex
	seen  []string
	reply string
}

func (*goalCapturingLLM) Name() string { return "goal-capturing" }

func (l *goalCapturingLLM) GenerateContent(_ context.Context, req *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	l.mu.Lock()
	var parts []string
	for _, c := range req.Contents {
		if c == nil || c.Role != genai.RoleUser {
			continue
		}
		for _, p := range c.Parts {
			if p != nil && p.Text != "" {
				parts = append(parts, p.Text)
			}
		}
	}
	l.seen = append(l.seen, strings.Join(parts, "\n"))
	reply := l.reply
	if reply == "" {
		reply = "done"
	}
	l.mu.Unlock()
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		content := &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: reply}}}
		yield(&adkmodel.LLMResponse{Content: content, FinishReason: genai.FinishReasonStop, TurnComplete: true}, nil)
	}
}

func (l *goalCapturingLLM) firstPrompt() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.seen) == 0 {
		return ""
	}
	return l.seen[0]
}

// spawnCallerLLM calls spawn_agent once with args and then stops.
type spawnCallerLLM struct {
	args  map[string]any
	calls int
	mu    sync.Mutex
}

func (*spawnCallerLLM) Name() string { return "spawn-caller" }

func (l *spawnCallerLLM) GenerateContent(_ context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	l.mu.Lock()
	l.calls++
	first := l.calls == 1
	l.mu.Unlock()
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		content := &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "completed"}}}
		if first {
			fc := &genai.FunctionCall{Name: "spawn_agent", Args: l.args}
			content = &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: fc}}}
		}
		yield(&adkmodel.LLMResponse{Content: content, FinishReason: genai.FinishReasonStop, TurnComplete: true}, nil)
	}
}

func TestSpawnAgentTool_ForwardsParentInputAndPersistsSession(t *testing.T) {
	t.Parallel()
	childLLM := &goalCapturingLLM{reply: "diagnosed"}
	prov := &recordingProvider{llm: childLLM}
	mgr := newTemplateManager(t, prov, []SubagentTemplate{{
		Name:         "cluster",
		Instruction:  "triage cluster",
		ModelFactory: tmplFactory(prov, "cluster-model"),
		ModelID:      "cluster-model",
		SubagentMode: agent.SubagentModeSingleTurn,
	}}, WithDefaultBudgets(Budgets{MaxTurns: 1}))
	defer mgr.Close()

	parentLLM := &spawnCallerLLM{
		args: map[string]any{
			"agent":  "cluster",
			"goal":   "investigate crashloop",
			"attach": []any{"input"},
			"wait":   true,
		},
	}
	parent, err := agent.New(parentLLM,
		agent.WithName("parent"),
		agent.WithSession("u", "parent-bg"),
		agent.WithBackgroundManager(mgr),
		agent.WithTools(NewSpawnTools(mgr)),
	)
	if err != nil {
		t.Fatalf("agent.New(parent): %v", err)
	}

	rawTrigger := "ERROR: kubelet failed to pull image registry.example/app:v9"
	for _, err := range parent.Run(context.Background(), rawTrigger) {
		if err != nil {
			t.Fatalf("parent.Run: %v", err)
		}
	}

	wantGoal := "investigate crashloop\n\n" + rawTrigger
	if got := childLLM.firstPrompt(); !strings.Contains(got, wantGoal) {
		t.Errorf("subagent first prompt = %q, want containing %q", got, wantGoal)
	}

	// Verify the background subagent's session row in the parent's SessionService
	// contains the spliced user message so its transcript is self-contained.
	gotSess, err := parent.SessionService().Get(context.Background(), &session.GetRequest{
		AppName:   "core-agent",
		UserID:    "u",
		SessionID: "parent-bg:sub:bg.cluster-1",
	})
	if err != nil {
		t.Fatalf("SessionService.Get(bg.cluster-1): %v", err)
	}
	var sawSpliced bool
	for ev := range gotSess.Session.Events().All() {
		if ev.Content == nil || ev.Content.Role != genai.RoleUser {
			continue
		}
		for _, p := range ev.Content.Parts {
			if p != nil && strings.Contains(p.Text, wantGoal) {
				sawSpliced = true
			}
		}
	}
	if !sawSpliced {
		t.Errorf("background subagent session parent-bg:sub:bg.cluster-1 missing spliced goal %q", wantGoal)
	}
}

func TestSpawnAgentTool_SpecInheritAndAttachNoDoubleSplice(t *testing.T) {
	t.Parallel()
	childLLM := &goalCapturingLLM{reply: "ok"}
	prov := &recordingProvider{llm: childLLM}
	mgr := newTemplateManager(t, prov, []SubagentTemplate{{
		Name:         "specialist",
		Instruction:  "inspect",
		ModelFactory: tmplFactory(prov, "m"),
		ModelID:      "m",
		SubagentMode: agent.SubagentModeSingleTurn,
		InheritInput: true,
	}}, WithDefaultBudgets(Budgets{MaxTurns: 1}))
	defer mgr.Close()

	parentLLM := &spawnCallerLLM{
		args: map[string]any{
			"agent":         "specialist",
			"goal":          "check stack",
			"attach":        []any{"input"},
			"include_input": true,
			"wait":          true,
		},
	}
	parent, err := agent.New(parentLLM,
		agent.WithName("parent"),
		agent.WithBackgroundManager(mgr),
		agent.WithTools(NewSpawnTools(mgr)),
	)
	if err != nil {
		t.Fatalf("agent.New(parent): %v", err)
	}

	for _, err := range parent.Run(context.Background(), "panic: index out of range") {
		if err != nil {
			t.Fatalf("parent.Run: %v", err)
		}
	}

	got := childLLM.firstPrompt()
	if count := strings.Count(got, "panic: index out of range"); count != 1 {
		t.Errorf("subagent prompt has %d copies of parent input (want 1):\n%s", count, got)
	}
}

func TestSpawnTemplateAndPredefined_DirectGoCallWithParentInput(t *testing.T) {
	t.Parallel()
	childLLM := &goalCapturingLLM{reply: "ok"}
	prov := &recordingProvider{llm: childLLM}
	mgr := newTemplateManager(t, prov, []SubagentTemplate{{
		Name:         "tmpl",
		Instruction:  "inspect",
		ModelFactory: tmplFactory(prov, "m"),
		ModelID:      "m",
		SubagentMode: agent.SubagentModeSingleTurn,
		InheritInput: true,
	}}, WithPredefinedSpecs([]Spec{{
		Name:         "predef",
		SystemPrompt: "inspect",
		InheritInput: true,
	}}), WithDefaultBudgets(Budgets{MaxTurns: 1}))
	attachEchoParent(t, mgr)
	defer mgr.Close()

	ctx := agent.WithParentInput(context.Background(), "direct trigger")
	h1, err := mgr.SpawnTemplate(ctx, "", "tmpl", RefOverrides{Goal: "goal-1"}, "")
	if err != nil {
		t.Fatalf("SpawnTemplate: %v", err)
	}
	waitDone(t, h1)

	h2, err := mgr.SpawnRef(ctx, "", "predef", "goal-2", RefOverrides{}, "")
	if err != nil {
		t.Fatalf("SpawnRef(predef): %v", err)
	}
	waitDone(t, h2)

	childLLM.mu.Lock()
	seen := append([]string(nil), childLLM.seen...)
	childLLM.mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("expected 2 subagent prompts, got %d (%v)", len(seen), seen)
	}
	if !strings.Contains(seen[0], "goal-1\n\ndirect trigger") {
		t.Errorf("SpawnTemplate prompt = %q, want spliced goal-1 + parent_input", seen[0])
	}
	if !strings.Contains(seen[1], "goal-2\n\ndirect trigger") {
		t.Errorf("SpawnRef(predef) prompt = %q, want spliced goal-2 + parent_input", seen[1])
	}
}

func TestSpawnAgentTool_RejectsUnknownAttachItem(t *testing.T) {
	t.Parallel()
	childLLM := &goalCapturingLLM{}
	prov := &recordingProvider{llm: childLLM}
	mgr := newTemplateManager(t, prov, []SubagentTemplate{{
		Name:         "cluster",
		Instruction:  "inspect",
		ModelFactory: tmplFactory(prov, "m"),
		ModelID:      "m",
	}})
	defer mgr.Close()

	parentLLM := &spawnCallerLLM{
		args: map[string]any{
			"agent":  "cluster",
			"goal":   "check",
			"attach": []any{"everything"},
		},
	}
	parent, err := agent.New(parentLLM,
		agent.WithName("parent"),
		agent.WithBackgroundManager(mgr),
		agent.WithTools(NewSpawnTools(mgr)),
	)
	if err != nil {
		t.Fatalf("agent.New(parent): %v", err)
	}

	for _, err := range parent.Run(context.Background(), "trigger") {
		if err != nil {
			t.Fatalf("parent.Run: %v", err)
		}
	}

	if len(mgr.ListSubagents()) != 0 {
		t.Errorf("invalid attach should refuse before spawning; got %v", mgr.ListSubagents())
	}
}
