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
	"testing"
)

// plainPrompter implements only Prompter — a stdin prompter or a local
// TUI, where the answerer is whoever is at the terminal.
type plainPrompter struct{ decision Decision }

func (p plainPrompter) AskApproval(context.Context, PromptRequest) (Decision, error) {
	return p.decision, nil
}

// attributingPrompter also names the principal, the way a daemon
// serving POST /perms/respond behind authenticated callers can.
type attributingPrompter struct {
	decision Decision
	by       string
}

func (p attributingPrompter) AskApproval(ctx context.Context, req PromptRequest) (Decision, error) {
	a, err := p.AskApprovalAttributed(ctx, req)
	return a.Decision, err
}

func (p attributingPrompter) AskApprovalAttributed(context.Context, PromptRequest) (Approval, error) {
	return Approval{Decision: p.decision, By: p.by}, nil
}

func TestApprovalLog_RecordsAttributedApprover(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		prompter Prompter
		wantBy   string
	}{
		{"attributing prompter", attributingPrompter{DecisionAllowOnce, "oncall@example.com"}, "oncall@example.com"},
		{"plain prompter attributes nothing", plainPrompter{DecisionAllowOnce}, ""},
		// A wrapper must not hide the capability of what it wraps —
		// Serialize is on the daemon's default path, so a wrapper that
		// only implemented Prompter would make every approval anonymous
		// while every direct-prompter test still passed.
		{"through Serialize", Serialize(attributingPrompter{DecisionAllowOnce, "oncall@example.com"}), "oncall@example.com"},
		{"Serialize over a plain prompter", Serialize(plainPrompter{DecisionAllowOnce}), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := New(Options{Mode: ModeAsk, Prompter: tc.prompter})
			if err := g.CheckGeneric(context.Background(), "deploy", "restart deploy/api"); err != nil {
				t.Fatalf("CheckGeneric: %v", err)
			}
			log := g.Approvals()
			if len(log) != 1 {
				t.Fatalf("approvals = %d, want 1", len(log))
			}
			if log[0].By != tc.wantBy {
				t.Errorf("ApprovalLog.By = %q, want %q", log[0].By, tc.wantBy)
			}
		})
	}
}

// TestApprovalLog_ControlPlaneWriteIsAttributed — the elevated gate has
// its own prompt path that bypasses every auto-approval shortcut, so it
// also has its own recordApproval call. It is the single most
// audit-relevant approval the gate issues (a privilege-bearing
// control-plane file), which makes it the worst one to leave anonymous
// because a second call site was missed.
func TestApprovalLog_ControlPlaneWriteIsAttributed(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	scope, err := NewPathScope(dir, dir, nil)
	if err != nil {
		t.Fatalf("NewPathScope: %v", err)
	}
	g := New(Options{
		Mode:     ModeAsk,
		Scope:    scope,
		Prompter: attributingPrompter{DecisionAllowOnce, "admin@example.com"},
	})
	if err := g.CheckFileWrite(context.Background(), "write_file", dir+"/.agents/config.json"); err != nil {
		t.Fatalf("CheckFileWrite: %v", err)
	}
	log := g.Approvals()
	if len(log) != 1 {
		t.Fatalf("approvals = %d, want 1", len(log))
	}
	if log[0].By != "admin@example.com" {
		t.Errorf("ApprovalLog.By = %q, want admin@example.com", log[0].By)
	}
}
