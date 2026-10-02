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
	"slices"
	"testing"
	"time"

	coretui "github.com/go-steer/core-tui/tui"

	"github.com/go-steer/core-agent/v2/internal/testutil"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

// The local chip's translators round-trip every chip mode, and auto in
// particular never lands on the default chip (#1175 decision 13): shown
// as default, the operator would read "ask" while a model approves
// calls. The attach TUI's twin is TestPermModeChipMapping.
func TestTranslateMode_RoundTripsEveryChipMode(t *testing.T) {
	t.Parallel()
	cases := []struct {
		mode permissions.Mode
		chip coretui.PermissionMode
	}{
		{permissions.ModeAsk, coretui.PermissionModeDefault},
		{permissions.ModeAuto, coretui.PermissionModeAuto},
		{permissions.ModeAcceptEdits, coretui.PermissionModeAcceptEdits},
		{permissions.ModePlan, coretui.PermissionModePlan},
		{permissions.ModeYolo, coretui.PermissionModeBypass},
	}
	for _, c := range cases {
		if got := translateMode(c.mode); got != c.chip {
			t.Errorf("translateMode(%q) = %v, want %v", c.mode, got, c.chip)
		}
		if got := translateModeBack(c.chip); got != c.mode {
			t.Errorf("translateModeBack(%v) = %q, want %q", c.chip, got, c.mode)
		}
	}
	if got := translateMode(permissions.ModeAllow); got != coretui.PermissionModeDefault {
		t.Errorf("translateMode(allow) = %v, want default", got)
	}
}

// capturingTUIPrompter records the request the bridge built.
type capturingTUIPrompter struct{ req coretui.PermissionRequest }

func (p *capturingTUIPrompter) AskApproval(_ context.Context, req coretui.PermissionRequest) (coretui.PermissionDecision, error) {
	p.req = req
	return coretui.DecisionDeny, nil
}

func (p *capturingTUIPrompter) AskApprovalDetailed(_ context.Context, req coretui.PermissionRequest) (coretui.PermissionOutcome, error) {
	p.req = req
	return coretui.PermissionOutcome{Decision: coretui.DecisionDeny}, nil
}

// A prompt ModeAuto's approver passed on reaches the local TUI as an
// escalation (#1175 decision 11); an ordinary prompt does not.
func TestGatePrompterBridge_CarriesTheEscalation(t *testing.T) {
	t.Parallel()
	inner := &capturingTUIPrompter{}
	b := &gatePrompterBridge{inner: inner}
	if _, err := b.AskApprovalAttributed(context.Background(), permissions.PromptRequest{
		Kind: permissions.PromptKindBash, ToolName: "bash", Detail: "go test ./...",
		ApproverModel: "judge-1", ApproverReason: "unsure about the network access",
	}); err != nil {
		t.Fatal(err)
	}
	if e := inner.req.Escalation; e == nil || e.Approver != "judge-1" || e.Reason != "unsure about the network access" {
		t.Errorf("escalation = %+v, want the approver and its reason", e)
	}
	if _, err := b.AskApprovalAttributed(context.Background(), permissions.PromptRequest{Kind: permissions.PromptKindBash, ToolName: "bash", Detail: "ls"}); err != nil {
		t.Fatal(err)
	}
	if inner.req.Escalation != nil {
		t.Errorf("an ordinary prompt became an escalation: %+v", inner.req.Escalation)
	}
}

type allowingApprover struct{}

func (allowingApprover) Judge(context.Context, permissions.ApproverRequest) (permissions.Verdict, error) {
	return permissions.Verdict{Outcome: permissions.VerdictAllow, Reason: "routine", Model: "judge-1"}, nil
}

// /permissions in the local TUI names the approver model for a call it
// allowed without a person (#1175 decision 10).
func TestSessionApprovals_CarriesTheApproverModel(t *testing.T) {
	t.Parallel()
	pol, err := permissions.NewPolicy([]string{"bash:go *"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	g := permissions.New(permissions.Options{
		Mode: permissions.ModeAuto, Approver: allowingApprover{}, AutoEligible: pol, ApprovalTimeout: time.Minute,
	})
	if err := g.CheckBashWithArgs(testutil.ApproverContext(context.Background()), "go test ./...", map[string]string{"command": "go test ./..."}); err != nil {
		t.Fatalf("the approver's allow did not run the call: %v", err)
	}
	rows := (&coreAgentAdapter{deps: tuiDeps{Gate: g}}).SessionApprovals()
	if len(rows) != 1 || rows[0].Approver != "judge-1" {
		t.Errorf("rows = %+v, want one naming judge-1", rows)
	}
}

// The local chip offers auto only to a session that can enter it
// (#1175 phase 4). Offered without an approver or approval_timeout,
// every press would land on auto, be refused, and roll back — and the
// operator could never get past it to ask.
func TestLocalChipCycle_OffersAutoOnlyWhenSelectable(t *testing.T) {
	t.Parallel()
	ready := permissions.New(permissions.Options{Mode: permissions.ModeAsk, Approver: allowingApprover{}, ApprovalTimeout: time.Minute})
	want := []coretui.PermissionMode{coretui.PermissionModeDefault, coretui.PermissionModeAuto,
		coretui.PermissionModeAcceptEdits, coretui.PermissionModePlan, coretui.PermissionModeBypass}
	if got := localChipCycle(ready); !slices.Equal(got, want) {
		t.Errorf("cycle = %v, want %v", got, want)
	}
	for name, g := range map[string]*permissions.Gate{
		"no approver": permissions.New(permissions.Options{Mode: permissions.ModeAsk, ApprovalTimeout: time.Minute}),
		"no timeout":  permissions.New(permissions.Options{Mode: permissions.ModeAsk, Approver: allowingApprover{}}),
	} {
		if got := localChipCycle(g); got != nil {
			t.Errorf("%s: cycle = %v, want nil (core-tui's default four, no auto)", name, got)
		}
	}
}
