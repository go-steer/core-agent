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
	"strings"
	"testing"
)

// reasonPrompter answers with a whole Approval, the way the attach
// broker does once /perms/respond carried a reason.
type reasonPrompter struct{ a Approval }

func (p reasonPrompter) AskApproval(ctx context.Context, req PromptRequest) (Decision, error) {
	a, err := p.AskApprovalAttributed(ctx, req)
	return a.Decision, err
}

func (p reasonPrompter) AskApprovalAttributed(context.Context, PromptRequest) (Approval, error) {
	return p.a, nil
}

// TestDenyReasonReachesTheModel is #1165: the error a denied call
// returns is what the model reads, and it used to be the same sentence
// whatever the operator objected to, so the model guessed — usually by
// re-issuing a near-identical call. Both prompt paths are covered: the
// ordinary one and the control-plane write, which has its own deny.
func TestDenyReasonReachesTheModel(t *testing.T) {
	t.Parallel()
	const reason = "use the staging cluster, not prod"
	want := `The operator's reason: "use the staging cluster, not prod".`

	for _, tc := range []struct {
		name     string
		prompter Prompter
	}{
		{"direct", reasonPrompter{Approval{Decision: DecisionDeny, Reason: reason}}},
		// Serialize is on the daemon's default path; a wrapper that
		// dropped the field would make every reason vanish while the
		// direct case passed.
		{"through Serialize", Serialize(reasonPrompter{Approval{Decision: DecisionDeny, Reason: reason}})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := New(Options{Mode: ModeAsk, Prompter: tc.prompter})
			err := g.CheckGeneric(context.Background(), "deploy", "restart deploy/api")
			if err == nil {
				t.Fatal("a deny returned no error")
			}
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error = %q\nwant it to contain %q", err, want)
			}
			if !strings.Contains(err.Error(), denyGuidance) {
				t.Errorf("error = %q\nlost the deny guidance; a reason adds to the refusal, it does not replace it", err)
			}
		})
	}

	t.Run("control-plane write", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		scope, err := NewPathScope(dir, dir, nil)
		if err != nil {
			t.Fatalf("NewPathScope: %v", err)
		}
		g := New(Options{
			Mode:     ModeAsk,
			Scope:    scope,
			Prompter: reasonPrompter{Approval{Decision: DecisionDeny, Reason: reason}},
		})
		err = g.CheckFileWrite(context.Background(), "write_file", dir+"/.agents/config.json")
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v\nwant it to contain %q", err, want)
		}
	})
}

// A deny without a reason must read exactly as it did before #1165;
// every consumer that matches on the refusal text keeps working.
func TestDenyWithoutAReasonIsUnchanged(t *testing.T) {
	t.Parallel()
	g := New(Options{Mode: ModeAsk, Prompter: reasonPrompter{Approval{Decision: DecisionDeny}}})
	err := g.CheckGeneric(context.Background(), "deploy", "restart deploy/api")
	want := "deploy denied by user: restart deploy/api. " + denyGuidance
	if err == nil || err.Error() != want {
		t.Errorf("error = %v\nwant    %q", err, want)
	}
}

// A reason on an approval is ignored, not honoured: the gate never
// reads it, so nothing an operator types can ride along with an allow.
func TestReasonOnAnApprovalIsIgnored(t *testing.T) {
	t.Parallel()
	g := New(Options{Mode: ModeAsk, Prompter: reasonPrompter{Approval{Decision: DecisionAllowOnce, Reason: "also delete the old ones"}}})
	if err := g.CheckGeneric(context.Background(), "deploy", "restart deploy/api"); err != nil {
		t.Fatalf("an approval with a reason was refused: %v", err)
	}
}
