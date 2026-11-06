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

package agent

import (
	"context"
	"iter"
	"strings"
	"sync"
	"testing"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

func TestSpliceParentInput(t *testing.T) {
	t.Parallel()
	if got := SpliceParentInput("triage", ""); got != "triage" {
		t.Errorf("SpliceParentInput with empty parentInput = %q, want %q", got, "triage")
	}
	if got := SpliceParentInput("", "stack trace"); got != "stack trace" {
		t.Errorf("SpliceParentInput with empty prompt = %q, want %q", got, "stack trace")
	}
	want := "triage pod\n\nline 1\nline 2"
	if got := SpliceParentInput("triage pod", "line 1\nline 2"); got != want {
		t.Errorf("SpliceParentInput = %q, want %q", got, want)
	}
}

func TestValidateSubagentAttach(t *testing.T) {
	t.Parallel()
	for _, valid := range [][]string{nil, {}, {"input"}, {" input "}} {
		if _, err := ValidateSubagentAttach(valid); err != nil {
			t.Errorf("ValidateSubagentAttach(%v) unexpected error: %v", valid, err)
		}
	}
	if _, err := ValidateSubagentAttach([]string{"history"}); err == nil || !strings.Contains(err.Error(), "unknown attach item") {
		t.Errorf("ValidateSubagentAttach([history]) = %v, want unknown attach item error", err)
	}
}

// promptCapturingLLM records the user text sent to GenerateContent and
// responds with a fixed string.
type promptCapturingLLM struct {
	mu      sync.Mutex
	prompts []string
	reply   string
}

func (*promptCapturingLLM) Name() string { return "prompt-capturing" }

func (l *promptCapturingLLM) GenerateContent(_ context.Context, req *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
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
	l.prompts = append(l.prompts, strings.Join(parts, "\n"))
	reply := l.reply
	if reply == "" {
		reply = "handled"
	}
	l.mu.Unlock()
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		content := &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: reply}}}
		yield(&adkmodel.LLMResponse{Content: content, FinishReason: genai.FinishReasonStop, TurnComplete: true}, nil)
	}
}

func (l *promptCapturingLLM) lastPrompt() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.prompts) == 0 {
		return ""
	}
	return l.prompts[len(l.prompts)-1]
}

// callWithArgsLLM invokes a named tool with caller-supplied args on its
// first turn and finishes on the second turn.
type callWithArgsLLM struct {
	tool     string
	args     map[string]any
	calls    int
	requests []*adkmodel.LLMRequest
	mu       sync.Mutex
}

func (*callWithArgsLLM) Name() string { return "call-with-args" }

func (l *callWithArgsLLM) GenerateContent(_ context.Context, req *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	l.mu.Lock()
	l.calls++
	l.requests = append(l.requests, req)
	first := l.calls == 1
	l.mu.Unlock()
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		content := &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "done"}}}
		if first {
			fc := &genai.FunctionCall{Name: l.tool, Args: l.args}
			content = &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: fc}}}
		}
		yield(&adkmodel.LLMResponse{Content: content, FinishReason: genai.FinishReasonStop, TurnComplete: true}, nil)
	}
}

func TestSubagentTool_ForwardsParentInputAndPersistsInSession(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestEventLog(t)
	defer cleanup()

	childLLM := &promptCapturingLLM{reply: "root cause found"}
	child, err := New(childLLM, WithName("triage"), WithEventLog(h), WithSession("u", "c"))
	if err != nil {
		t.Fatalf("New child: %v", err)
	}
	parentLLM := &callWithArgsLLM{
		tool: "triage",
		args: map[string]any{
			"request": "diagnose crash",
			"attach":  []any{"input"},
		},
	}
	parent, err := New(parentLLM,
		WithName("parent"),
		WithEventLog(h),
		WithSession("u", "parent-sess"),
		WithSubagents([]*Agent{child}),
	)
	if err != nil {
		t.Fatalf("New parent: %v", err)
	}

	rawInput := "Pod web-0 CrashLoopBackOff\npanic: nil pointer dereference at server.go:88"
	for _, err := range parent.Run(context.Background(), rawInput) {
		if err != nil {
			t.Fatalf("parent.Run: %v", err)
		}
	}

	wantSpliced := "diagnose crash\n\n" + rawInput
	if got := childLLM.lastPrompt(); got != wantSpliced {
		t.Errorf("child LLM prompt = %q, want %q", got, wantSpliced)
	}

	// Verify the subagent's own session row in the eventlog is self-contained.
	listed, err := h.Service.List(context.Background(), &session.ListRequest{
		AppName: "core-agent",
		UserID:  "u",
	})
	if err != nil {
		t.Fatalf("session.List: %v", err)
	}
	var foundSubSession bool
	for _, s := range listed.Sessions {
		if !strings.HasPrefix(s.ID(), "parent-sess:sub:triage") {
			continue
		}
		foundSubSession = true
		gotSess, err := h.Service.Get(context.Background(), &session.GetRequest{
			AppName:   "core-agent",
			UserID:    "u",
			SessionID: s.ID(),
		})
		if err != nil {
			t.Fatalf("session.Get(%s): %v", s.ID(), err)
		}
		var sawSplicedUserEvent bool
		for ev := range gotSess.Session.Events().All() {
			if ev.Content == nil || ev.Content.Role != genai.RoleUser {
				continue
			}
			for _, p := range ev.Content.Parts {
				if p != nil && p.Text == wantSpliced {
					sawSplicedUserEvent = true
				}
			}
		}
		if !sawSplicedUserEvent {
			t.Errorf("subagent session %s did not record spliced user event %q", s.ID(), wantSpliced)
		}
	}
	if !foundSubSession {
		t.Errorf("no subagent session with prefix parent-sess:sub:triage found")
	}
}

func TestSubagentTool_IncludeInputAndSpecInheritNoDoubleSplice(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestEventLog(t)
	defer cleanup()

	childLLM := &promptCapturingLLM{}
	child, err := New(childLLM,
		WithName("specialist"),
		WithSubagentInheritInput(true),
		WithEventLog(h),
		WithSession("u", "c"),
	)
	if err != nil {
		t.Fatalf("New child: %v", err)
	}
	parentLLM := &callWithArgsLLM{
		tool: "specialist",
		args: map[string]any{
			"request":       "check logs",
			"attach":        []any{"input"},
			"include_input": true,
		},
	}
	parent, err := New(parentLLM,
		WithName("parent"),
		WithEventLog(h),
		WithSession("u", "p"),
		WithSubagents([]*Agent{child}),
	)
	if err != nil {
		t.Fatalf("New parent: %v", err)
	}

	for _, err := range parent.Run(context.Background(), "raw payload") {
		if err != nil {
			t.Fatalf("parent.Run: %v", err)
		}
	}

	got := childLLM.lastPrompt()
	if count := strings.Count(got, "raw payload"); count != 1 {
		t.Errorf("child prompt has %d copies of raw payload (want 1):\n%s", count, got)
	}
}

func TestSubagentTool_InboxTriggeredTurnForwardsInboxPayload(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestEventLog(t)
	defer cleanup()

	childLLM := &promptCapturingLLM{}
	child, err := New(childLLM,
		WithName("responder"),
		WithSubagentInheritInput(true),
		WithEventLog(h),
		WithSession("u", "c"),
	)
	if err != nil {
		t.Fatalf("New child: %v", err)
	}
	parentLLM := &callWithArgsLLM{
		tool: "responder",
		args: map[string]any{"request": "handle alert"},
	}
	parent, err := New(parentLLM,
		WithName("parent"),
		WithEventLog(h),
		WithSession("u", "p"),
		WithSubagents([]*Agent{child}),
	)
	if err != nil {
		t.Fatalf("New parent: %v", err)
	}

	if err := parent.Inject("webhook: deployment/frontend rollout stalled"); err != nil {
		t.Fatalf("parent.Inject: %v", err)
	}
	for _, err := range parent.Run(context.Background(), "") {
		if err != nil {
			t.Fatalf("parent.Run: %v", err)
		}
	}

	want := "handle alert\n\nwebhook: deployment/frontend rollout stalled"
	if got := childLLM.lastPrompt(); got != want {
		t.Errorf("child prompt on inbox-triggered turn = %q, want %q", got, want)
	}
}
