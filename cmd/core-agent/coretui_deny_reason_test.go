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

//go:build !no_tui

package main

import (
	"context"
	"strings"
	"testing"

	coretui "github.com/go-steer/core-tui/tui"

	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

// fakeTUIPrompter stands in for *coretui.Prompter: it answers every
// request with a fixed outcome and records which entry point was used,
// because which one the bridge calls is what decides whether the
// operator is offered the "r" deny-with-reason step at all.
type fakeTUIPrompter struct {
	out         coretui.PermissionOutcome
	plainCalls  int
	detailCalls int
}

func (f *fakeTUIPrompter) AskApproval(context.Context, coretui.PermissionRequest) (coretui.PermissionDecision, error) {
	f.plainCalls++
	return f.out.Decision, nil
}

func (f *fakeTUIPrompter) AskApprovalDetailed(context.Context, coretui.PermissionRequest) (coretui.PermissionOutcome, error) {
	f.detailCalls++
	return f.out, nil
}

// plainTUIPrompter has only the PermissionPrompter method, like a
// prompter from before core-tui v0.28.0.
type plainTUIPrompter struct{ d coretui.PermissionDecision }

func (p plainTUIPrompter) AskApproval(context.Context, coretui.PermissionRequest) (coretui.PermissionDecision, error) {
	return p.d, nil
}

// TestGatePrompterBridge_DenyReasonReachesTheModel is the local --tui
// half of #1165: the reason the operator types after "r" must end up
// in the error the gated call returns, which is the text the model
// reads. Driven through a real gate so a bridge that stopped
// implementing AttributingPrompter — the only door the gate has for a
// reason — fails here rather than silently dropping every reason.
func TestGatePrompterBridge_DenyReasonReachesTheModel(t *testing.T) {
	t.Parallel()
	inner := &fakeTUIPrompter{out: coretui.PermissionOutcome{Decision: coretui.DecisionDeny, Reason: "use the staging cluster"}}
	g := permissions.New(permissions.Options{Mode: permissions.ModeAsk, Prompter: &gatePrompterBridge{inner: inner}})

	err := g.CheckGeneric(context.Background(), "deploy", "restart deploy/api")
	if err == nil {
		t.Fatal("a deny returned no error")
	}
	want := `The operator's reason: "use the staging cluster".`
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q\nwant it to contain %q", err, want)
	}
	if inner.detailCalls != 1 || inner.plainCalls != 0 {
		t.Errorf("detailed calls = %d, plain calls = %d; want the bridge to ask with AskApprovalDetailed so the prompt offers \"r\"", inner.detailCalls, inner.plainCalls)
	}
}

func TestGatePrompterBridge_ReasonOnlyOnDeny(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		out        coretui.PermissionOutcome
		want       permissions.Decision
		wantReason string
	}{
		{"deny with reason", coretui.PermissionOutcome{Decision: coretui.DecisionDeny, Reason: "not prod"}, permissions.DecisionDeny, "not prod"},
		{"plain deny", coretui.PermissionOutcome{Decision: coretui.DecisionDeny}, permissions.DecisionDeny, ""},
		// core-tui never sets a reason on an allow; if one ever arrived
		// it must not ride along, because the model would read text on
		// an approval as conditions on the call it just authorized.
		{"allow once", coretui.PermissionOutcome{Decision: coretui.DecisionAllowOnce, Reason: "stray"}, permissions.DecisionAllowOnce, ""},
		{"allow session", coretui.PermissionOutcome{Decision: coretui.DecisionAllowSession}, permissions.DecisionAllowSession, ""},
		{"allow session verb", coretui.PermissionOutcome{Decision: coretui.DecisionAllowSessionVerb}, permissions.DecisionAllowSessionVerb, ""},
		{"allow session tool", coretui.PermissionOutcome{Decision: coretui.DecisionAllowSessionTool}, permissions.DecisionAllowSessionTool, ""},
		{"allow always", coretui.PermissionOutcome{Decision: coretui.DecisionAllowAlways}, permissions.DecisionAllowAlways, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := &gatePrompterBridge{inner: &fakeTUIPrompter{out: tc.out}}
			a, err := b.AskApprovalAttributed(context.Background(), permissions.PromptRequest{ToolName: "bash"})
			if err != nil {
				t.Fatalf("AskApprovalAttributed: %v", err)
			}
			if a.Decision != tc.want || a.Reason != tc.wantReason || a.By != "" {
				t.Errorf("approval = %+v, want decision %v reason %q and no By", a, tc.want, tc.wantReason)
			}
			d, err := b.AskApproval(context.Background(), permissions.PromptRequest{ToolName: "bash"})
			if err != nil || d != tc.want {
				t.Errorf("AskApproval = %v, %v; want %v", d, err, tc.want)
			}
		})
	}
}

// A plain deny through a real gate reads exactly as it always did.
func TestGatePrompterBridge_PlainDenyHasNoReason(t *testing.T) {
	t.Parallel()
	inner := &fakeTUIPrompter{out: coretui.PermissionOutcome{Decision: coretui.DecisionDeny}}
	g := permissions.New(permissions.Options{Mode: permissions.ModeAsk, Prompter: &gatePrompterBridge{inner: inner}})
	err := g.CheckGeneric(context.Background(), "deploy", "restart deploy/api")
	if err == nil {
		t.Fatal("a deny returned no error")
	}
	if strings.Contains(err.Error(), "operator's reason") {
		t.Errorf("error = %q; a deny without a reason must not claim one", err)
	}
}

// An inner prompter without AskApprovalDetailed still works: the bridge
// falls back to AskApproval and maps the decision as before.
func TestGatePrompterBridge_PlainPrompterFallsBack(t *testing.T) {
	t.Parallel()
	for _, d := range []coretui.PermissionDecision{coretui.DecisionDeny, coretui.DecisionAllowOnce} {
		b := &gatePrompterBridge{inner: plainTUIPrompter{d: d}}
		a, err := b.AskApprovalAttributed(context.Background(), permissions.PromptRequest{ToolName: "bash"})
		if err != nil {
			t.Fatalf("AskApprovalAttributed: %v", err)
		}
		if a.Decision != translateDecision(d) || a.Reason != "" {
			t.Errorf("approval = %+v for %v", a, d)
		}
	}
}
