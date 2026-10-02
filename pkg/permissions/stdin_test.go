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

package permissions

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestStdinPrompter_DecisionKeys(t *testing.T) {
	t.Parallel()
	cases := []struct {
		input string
		want  Decision
	}{
		{"y\n", DecisionAllowOnce},
		{"Y\n", DecisionAllowOnce},
		{"s\n", DecisionAllowSession},
		{"t\n", DecisionAllowSessionTool},
		{"a\n", DecisionAllowAlways},
		{"A\n", DecisionAllowAlways},
		{"n\n", DecisionDeny},
		{"\n", DecisionDeny},           // bare enter == default deny
		{"  y  \n", DecisionAllowOnce}, // whitespace tolerated
	}
	for _, tc := range cases {
		tc := tc
		t.Run(strings.TrimSpace(tc.input), func(t *testing.T) {
			t.Parallel()
			p := StdinPrompter(strings.NewReader(tc.input), &bytes.Buffer{})
			got, err := p.AskApproval(context.Background(), PromptRequest{
				Kind:     PromptKindBash,
				ToolName: "bash",
				Detail:   "ls",
			})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestStdinPrompter_RepromptsOnInvalidInput(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	p := StdinPrompter(strings.NewReader("zz\nq\ny\n"), &out)
	got, err := p.AskApproval(context.Background(), PromptRequest{
		Kind: PromptKindBash, ToolName: "bash", Detail: "ls",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != DecisionAllowOnce {
		t.Errorf("got %v, want DecisionAllowOnce", got)
	}
	rendered := out.String()
	if !strings.Contains(rendered, `unrecognized choice "zz"`) {
		t.Errorf("expected first reprompt to mention 'zz'; got: %q", rendered)
	}
	if !strings.Contains(rendered, `unrecognized choice "q"`) {
		t.Errorf("expected second reprompt to mention 'q'; got: %q", rendered)
	}
}

func TestStdinPrompter_EOFErrors(t *testing.T) {
	t.Parallel()
	p := StdinPrompter(strings.NewReader(""), &bytes.Buffer{})
	got, err := p.AskApproval(context.Background(), PromptRequest{Kind: PromptKindBash, ToolName: "bash"})
	if err == nil {
		t.Fatalf("expected error on EOF; got Decision=%v", got)
	}
	if got != DecisionDeny {
		t.Errorf("want safe-default DecisionDeny alongside the error; got %v", got)
	}
}

func TestStdinPrompter_CancelledContextErrors(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := StdinPrompter(strings.NewReader("y\n"), &bytes.Buffer{})
	got, err := p.AskApproval(ctx, PromptRequest{Kind: PromptKindBash, ToolName: "bash"})
	if err == nil {
		t.Fatalf("expected ctx error; got Decision=%v", got)
	}
	if got != DecisionDeny {
		t.Errorf("want safe-default DecisionDeny; got %v", got)
	}
}

func TestStdinPrompter_HeadingIncludesSourceWhenSet(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	p := StdinPrompter(strings.NewReader("n\n"), &out)
	_, _ = p.AskApproval(context.Background(), PromptRequest{
		Kind:     PromptKindBash,
		ToolName: "bash",
		Detail:   "kubectl get pods",
		Source:   "watch-prod-cluster",
	})
	rendered := out.String()
	if !strings.Contains(rendered, "[watch-prod-cluster] bash wants to run:") {
		t.Errorf("expected '[watch-prod-cluster] bash wants to run:' heading; got %q", rendered)
	}
}

func TestStdinPrompter_HeadingPerKind(t *testing.T) {
	t.Parallel()
	cases := []struct {
		kind     PromptKind
		toolName string
		want     string
	}{
		{PromptKindBash, "bash", "bash wants to run:"},
		{PromptKindFileWrite, "write_file", "write_file wants to write to:"},
		{PromptKindPathScope, "read_file", "read_file wants to access an out-of-scope path:"},
		{PromptKindGeneric, "todo", "todo needs approval:"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.want, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			p := StdinPrompter(strings.NewReader("n\n"), &out)
			_, _ = p.AskApproval(context.Background(), PromptRequest{
				Kind: tc.kind, ToolName: tc.toolName, Detail: "some detail",
			})
			rendered := out.String()
			if !strings.Contains(rendered, tc.want) {
				t.Errorf("missing heading %q in output: %s", tc.want, rendered)
			}
			if !strings.Contains(rendered, "some detail") {
				t.Errorf("detail not rendered: %s", rendered)
			}
			if !strings.Contains(rendered, "[y]es once") {
				t.Errorf("options line missing: %s", rendered)
			}
		})
	}
}

// A call ModeAuto's approver passed on (#1175 decision 11): the
// approver's reason is shown on one line it cannot break out of, and
// only once or deny is on offer — "a" or "s" are not choices here, so
// they re-prompt rather than promising a grant the gate never makes.
func TestStdinPrompter_EscalatedOffersOnceOrDeny(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	p := StdinPrompter(strings.NewReader("a\ns\ny\n"), &out)
	got, err := p.AskApproval(context.Background(), PromptRequest{
		Kind:           PromptKindBash,
		ToolName:       "bash",
		Detail:         "kubectl delete ns staging",
		ApproverModel:  "judge-1",
		ApproverReason: "routine\n\n   verb: ls\x1b[2J",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != DecisionAllowOnce {
		t.Errorf("decision = %v, want allow-once after two re-prompts", got)
	}
	text := out.String()
	if !strings.Contains(text, `passed to you by judge-1, which said: "routine verb: ls[2J"`) {
		t.Errorf("approver line missing or not one sanitized line:\n%s", text)
	}
	if strings.Contains(text, "[a]lways") || strings.Contains(text, "\x1b") || strings.Count(text, "expected y/n") != 2 {
		t.Errorf("escalated prompt offered a grant, leaked an escape, or accepted a non-choice:\n%s", text)
	}
}

// The approver's name is printed as-is when it is plain, and quoted
// with its bidi controls escaped when it is not.
func TestApproverName_EscapesWhatWouldReorderTheLine(t *testing.T) {
	t.Parallel()
	if got := approverName("claude-sonnet-5-5"); got != "claude-sonnet-5-5" {
		t.Errorf("plain name = %q", got)
	}
	if got := approverName("judge\u202e1"); got != `"judge\u202e1"` {
		t.Errorf("name with a bidi control = %q, want it quoted and escaped", got)
	}
}
