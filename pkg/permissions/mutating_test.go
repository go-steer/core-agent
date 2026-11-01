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
	"context"
	"errors"
	"strings"
	"testing"
)

const (
	mutTool = "scale_deployment"
	mutKey  = "scale_deployment(deployment=api, replicas=10)"
)

// alwaysPrompter answers every request with a fixed decision and
// records that it was consulted. CheckMutatingToolCall must never
// consult it: the pause for a mutation is durable and lives outside
// this package.
type alwaysPrompter struct {
	d     Decision
	calls int
}

func (p *alwaysPrompter) AskApproval(context.Context, PromptRequest) (Decision, error) {
	p.calls++
	return p.d, nil
}

// TestCheckMutatingToolCall_NeverAutoApproves is W2.3's core claim: no
// mode, grant, or allowlist entry lets a mutating call through without
// an operator. Each subtest configures the most permissive version of
// one bypass and asserts the answer is still "ask a human".
func TestCheckMutatingToolCall_NeverAutoApproves(t *testing.T) {
	t.Parallel()
	allowEverything, err := NewPolicy([]string{"*"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name  string
		build func() *Gate
	}{
		{"yolo mode", func() *Gate {
			return New(Options{Mode: ModeYolo})
		}},
		{"allow mode", func() *Gate {
			return New(Options{Mode: ModeAllow})
		}},
		{"accept-edits mode", func() *Gate {
			return New(Options{Mode: ModeAcceptEdits})
		}},
		{"policy allowlist matches", func() *Gate {
			return New(Options{Mode: ModeAsk, Policy: allowEverything})
		}},
		{"session grant for this exact request", func() *Gate {
			g := New(Options{Mode: ModeAsk})
			g.rememberSession(mutTool, mutKey)
			return g
		}},
		{"session grant for the whole tool", func() *Gate {
			g := New(Options{Mode: ModeAsk})
			g.rememberSessionTool(mutTool)
			return g
		}},
		{"yolo mode and a wired prompter that says yes", func() *Gate {
			return New(Options{Mode: ModeYolo, Prompter: &alwaysPrompter{d: DecisionAllowAlways}})
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.build().CheckMutatingToolCall(context.Background(), mutTool, mutKey)
			if !errors.Is(err, ErrApprovalRequired) {
				t.Fatalf("CheckMutatingToolCall = %v, want ErrApprovalRequired", err)
			}
		})
	}
}

// TestCheckMutatingToolCall_NeverPrompts pins the division of labour
// this package's half of W2 rests on: pkg/permissions decides policy
// and never asks. A synchronous in-process prompt cannot survive the
// operator taking a coffee break while the process is restarted, which
// is precisely what a mutation approval has to do, so the ask belongs
// to the durable seam in pkg/approval.
func TestCheckMutatingToolCall_NeverPrompts(t *testing.T) {
	t.Parallel()
	p := &alwaysPrompter{d: DecisionAllowOnce}
	g := New(Options{Mode: ModeAsk, Prompter: p})
	if err := g.CheckMutatingToolCall(context.Background(), mutTool, mutKey); !errors.Is(err, ErrApprovalRequired) {
		t.Fatalf("CheckMutatingToolCall = %v, want ErrApprovalRequired", err)
	}
	if p.calls != 0 {
		t.Errorf("prompter consulted %d time(s), want 0: the mutation pause is durable and does not run through Prompter", p.calls)
	}
}

// TestCheckMutatingToolCall_RefusalsStillApply covers the three checks
// that DO survive into the mutation gate. Each must beat the
// ask-a-human answer, so the assertion is that the error is NOT
// ErrApprovalRequired.
func TestCheckMutatingToolCall_RefusalsStillApply(t *testing.T) {
	t.Parallel()
	denyScale, err := NewPolicy(nil, []string{mutTool + ":*"})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		gate *Gate
		want string
	}{
		{
			name: "config deny beats yolo mode",
			gate: New(Options{Mode: ModeYolo, Policy: denyScale}),
			want: "denied by config policy",
		},
		{
			name: "plan mode executes nothing",
			gate: New(Options{Mode: ModePlan}),
			want: "'plan' mode",
		},
		{
			name: "plan-first pre-check runs before anything else",
			gate: New(Options{Mode: ModeYolo, RequirePlanArtifact: true}),
			want: "plan-first mode requires record_plan",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.gate.CheckMutatingToolCall(context.Background(), mutTool, mutKey)
			if err == nil {
				t.Fatal("CheckMutatingToolCall = nil, want a refusal")
			}
			if errors.Is(err, ErrApprovalRequired) {
				t.Fatalf("CheckMutatingToolCall = %v, want a refusal, not an approval request", err)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not mention %q", err, tt.want)
			}
		})
	}
}

// TestRecordMutationVerdict_ScopesRefused pins that only allow-once passes.
func TestRecordMutationVerdict_ScopesRefused(t *testing.T) {
	t.Parallel()
	broad := []Decision{
		DecisionAllowSession,
		DecisionAllowSessionVerb,
		DecisionAllowSessionTool,
		DecisionAllowAlways,
	}
	for _, d := range broad {
		t.Run(d.String(), func(t *testing.T) {
			t.Parallel()
			g := New(Options{Mode: ModeYolo})
			err := g.RecordMutationVerdict(context.Background(), mutTool, mutKey, d)
			if !errors.Is(err, ErrGrantScopeRefused) {
				t.Fatalf("RecordMutationVerdict(%s) = %v, want ErrGrantScopeRefused", d, err)
			}
			if got := g.Approvals(); len(got) != 0 {
				t.Errorf("approval log has %d entr(ies) after a refused scope, want 0: %+v", len(got), got)
			}
			if g.sessionToolAllowed(mutTool) || g.sessionAllowed(mutTool, mutKey) {
				t.Error("a refused scope installed a session grant")
			}
		})
	}
}

func TestRecordMutationVerdict_AllowOnce(t *testing.T) {
	t.Parallel()
	g := New(Options{Mode: ModeAsk})
	if err := g.RecordMutationVerdict(context.Background(), mutTool, mutKey, DecisionAllowOnce); err != nil {
		t.Fatalf("RecordMutationVerdict(allow-once) = %v, want nil", err)
	}
	log := g.Approvals()
	if len(log) != 1 || log[0].Tool != mutTool || log[0].Key != mutKey || log[0].Decision != DecisionAllowOnce {
		t.Fatalf("approval log = %+v, want one allow-once for %s/%s", log, mutTool, mutKey)
	}
	if err := g.CheckMutatingToolCall(context.Background(), mutTool, mutKey); !errors.Is(err, ErrApprovalRequired) {
		t.Errorf("second CheckMutatingToolCall = %v, want ErrApprovalRequired — an allow-once must not install a standing grant", err)
	}
}

func TestRecordMutationVerdict_Deny(t *testing.T) {
	t.Parallel()
	g := New(Options{Mode: ModeYolo})
	err := g.RecordMutationVerdict(context.Background(), mutTool, mutKey, DecisionDeny)
	if err == nil {
		t.Fatal("RecordMutationVerdict(deny) = nil, want a denial")
	}
	if errors.Is(err, ErrGrantScopeRefused) {
		t.Fatalf("deny reported as a scope refusal: %v", err)
	}
	if !strings.Contains(err.Error(), "denied by operator") {
		t.Errorf("error %q does not attribute the denial to the operator", err)
	}
}

func TestMutationGate_RoutesThroughTheSessionGate(t *testing.T) {
	t.Parallel()
	denyScale, err := NewPolicy(nil, []string{mutTool + ":*"})
	if err != nil {
		t.Fatal(err)
	}
	template := New(Options{Mode: ModeAsk})
	session := New(Options{Mode: ModeAsk, Policy: denyScale})
	ctx := WithSessionGate(context.Background(), session)

	if err := template.CheckMutatingToolCall(ctx, mutTool, mutKey); !strings.Contains(errString(err), "denied by config policy") {
		t.Errorf("CheckMutatingToolCall on the template gate = %v, want the session gate's deny", err)
	}
	if err := template.RecordMutationVerdict(ctx, mutTool, mutKey, DecisionAllowOnce); err != nil {
		t.Fatalf("RecordMutationVerdict = %v", err)
	}
	if len(session.Approvals()) != 1 {
		t.Error("approval recorded on the template gate, not the session gate")
	}
	if len(template.Approvals()) != 0 {
		t.Error("approval recorded on the template gate")
	}
}

func TestToolClassification(t *testing.T) {
	t.Parallel()
	if DefaultClass != ClassMutating {
		t.Fatalf("DefaultClass = %v, want ClassMutating", DefaultClass)
	}
	for _, ctrl := range []string{"adk_request_input", "adk_request_confirmation", "finish_task", "mark_task_done", "transfer_to_agent", "pause_session", "request_operator_input", "exit_loop"} {
		if !IsEngineControlCall(ctrl) {
			t.Errorf("IsEngineControlCall(%q) = false, want true", ctrl)
		}
		if got := DefaultToolClass(ctrl); got != ClassReadOnly {
			t.Errorf("DefaultToolClass(%q) = %v, want ClassReadOnly", ctrl, got)
		}
	}
	if got := DefaultToolClass("read_file"); got != ClassReadOnly {
		t.Errorf("DefaultToolClass(read_file) = %v, want ClassReadOnly", got)
	}
	if got := DefaultToolClass("task"); got != ClassSpawning {
		t.Errorf("DefaultToolClass(task) = %v, want ClassSpawning", got)
	}
	if got := DefaultToolClass("write_file"); got != ClassMutating {
		t.Errorf("DefaultToolClass(write_file) = %v, want ClassMutating", got)
	}
	if got := DefaultToolClass("unknown_mcp_tool"); got != ClassMutating {
		t.Errorf("DefaultToolClass(unknown_mcp_tool) = %v, want ClassMutating", got)
	}

	// MCP readOnlyHint is opt-in per server (#573).
	if got := MCPToolClass(true, false); got != ClassMutating {
		t.Errorf("MCPToolClass(readOnlyHint=true, trustServer=false) = %v, want ClassMutating", got)
	}
	if got := MCPToolClass(true, true); got != ClassReadOnly {
		t.Errorf("MCPToolClass(readOnlyHint=true, trustServer=true) = %v, want ClassReadOnly", got)
	}
	if got := MCPToolClass(false, true); got != ClassMutating {
		t.Errorf("MCPToolClass(readOnlyHint=false, trustServer=true) = %v, want ClassMutating", got)
	}

	g := New(Options{Mode: ModeAsk})
	g.SetToolClasses(map[string]ToolClass{
		"custom_read":       ClassReadOnly,
		"read_file":         ClassMutating,
		"adk_request_input": ClassMutating, // must be ignored
	})
	if got := g.ToolClass("custom_read"); got != ClassReadOnly {
		t.Errorf("g.ToolClass(custom_read) = %v, want ClassReadOnly", got)
	}
	if got := g.ToolClass("read_file"); got != ClassMutating {
		t.Errorf("g.ToolClass(read_file) = %v, want ClassMutating", got)
	}
	if got := g.ToolClass("adk_request_input"); got != ClassReadOnly {
		t.Errorf("g.ToolClass(adk_request_input) = %v, want ClassReadOnly (control call cannot be overridden)", got)
	}

	sub := g.DeriveForSession("s1", nil)
	if got := sub.ToolClass("custom_read"); got != ClassReadOnly {
		t.Errorf("sub.ToolClass(custom_read) = %v, want ClassReadOnly", got)
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
