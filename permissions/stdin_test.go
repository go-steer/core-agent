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
