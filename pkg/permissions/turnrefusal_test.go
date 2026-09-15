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
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// #1074. #1068 told the model the answer would not change; run 10 of
// dev/uat/approval-gate/ proved on a cluster that the model re-issues
// anyway — eight prompts against a floor of three, the watchdog
// escalating with the new guidance quoted back as its "Last error".
// These tests pin the gate's half: the second identical request is
// refused without anybody being asked.
func TestTurnRefusal_ADeniedCallIsNotAskedAgain(t *testing.T) {
	t.Parallel()
	p := &fakePrompter{decision: DecisionDeny}
	g := New(Options{Mode: ModeAsk, Prompter: p})
	ctx := context.Background()

	first := g.CheckBash(ctx, "kubectl delete ns prod")
	if first == nil {
		t.Fatal("expected the denial to fail the first call")
	}
	second := g.CheckBash(ctx, "kubectl delete ns prod")
	if second == nil {
		t.Fatal("expected the repeat to be refused too")
	}
	if len(p.calls) != 1 {
		t.Fatalf("the gate opened %d prompts for one refused request, want 1: %+v", len(p.calls), p.calls)
	}
	// The refusal has to be legible as a repeat, not as a fresh denial:
	// a model that cannot tell "you were told no and asked again" from
	// "you were told no" learns nothing from the second refusal either,
	// which is the whole lesson of #1068.
	got := strings.ToLower(second.Error())
	for _, want := range []string{"denied earlier in this turn", "not put to a human again", "do not re-issue it"} {
		if !strings.Contains(got, want) {
			t.Errorf("repeat refusal does not say %q: %v", want, second)
		}
	}
}

func TestTurnRefusal_AnExpiredPromptIsNotReopened(t *testing.T) {
	t.Parallel()
	p := newBlockingPrompter()
	g := New(Options{Mode: ModeAsk, Prompter: p, ApprovalTimeout: 40 * time.Millisecond})
	ctx := context.Background()

	first := g.CheckBash(ctx, "kubectl apply -f patch.yaml")
	if !errors.Is(first, ErrPromptExpired) {
		t.Fatalf("first call: got %v, want ErrPromptExpired", first)
	}
	// The saving that matters operationally: the retry does not spend
	// another ApprovalTimeout waiting for the operator who was already
	// absent. Generous bound — this is asserting "did not wait", not a
	// latency budget.
	start := time.Now()
	second := g.CheckBash(ctx, "kubectl apply -f patch.yaml")
	if elapsed := time.Since(start); elapsed >= 40*time.Millisecond {
		t.Errorf("the repeat waited %s, i.e. it re-armed the timeout instead of being refused outright", elapsed)
	}
	if second == nil {
		t.Fatal("expected the repeat to be refused")
	}
	got := strings.ToLower(second.Error())
	for _, want := range []string{"expired unanswered earlier in this turn", "not put to anybody either", "do not re-issue this call"} {
		if !strings.Contains(got, want) {
			t.Errorf("repeat refusal does not say %q: %v", want, second)
		}
	}
	// An expiry and a denial are not interchangeable to the model: one
	// is a human decision, the other is the absence of one, and only
	// the second leaves the action genuinely pending.
	if strings.Contains(got, "denied earlier in this turn") {
		t.Errorf("an expiry was reported as a denial: %v", second)
	}
}

// The memory is keyed on the request, not the tool. An agent whose
// `kubectl apply` was refused may legitimately fix its manifest and ask
// about a different one, and that is a different request.
func TestTurnRefusal_ADifferentRequestStillPrompts(t *testing.T) {
	t.Parallel()
	p := &fakePrompter{decision: DecisionDeny}
	g := New(Options{Mode: ModeAsk, Prompter: p})
	ctx := context.Background()

	if err := g.CheckBash(ctx, "kubectl delete ns prod"); err == nil {
		t.Fatal("expected the first call to be denied")
	}
	if err := g.CheckBash(ctx, "kubectl delete ns staging"); err == nil {
		t.Fatal("expected the second call to be denied")
	}
	if len(p.calls) != 2 {
		t.Fatalf("the gate opened %d prompts for two distinct requests, want 2: %+v", len(p.calls), p.calls)
	}
}

// Only a refusal arms the memory. An allow-once followed by an
// identical call is an ordinary second call: the operator said yes to
// one invocation, and saying yes is not evidence that the next one
// should be auto-answered in either direction.
func TestTurnRefusal_AnApprovedCallIsNotSuppressed(t *testing.T) {
	t.Parallel()
	p := &fakePrompter{decision: DecisionAllowOnce}
	g := New(Options{Mode: ModeAsk, Prompter: p})
	ctx := context.Background()

	for i := range 2 {
		if err := g.CheckBash(ctx, "kubectl apply -f patch.yaml"); err != nil {
			t.Fatalf("call %d: approved call refused: %v", i+1, err)
		}
	}
	if len(p.calls) != 2 {
		t.Fatalf("the gate opened %d prompts for two approved calls, want 2: %+v", len(p.calls), p.calls)
	}
}

// A prompter that failed for its own reasons — a closed broker, a
// cancelled turn — has said nothing about the request. Only the two
// refusals arm the memory; anything else must stay askable, or a
// transient broker fault would silently disable a tool for the turn.
func TestTurnRefusal_APrompterErrorDoesNotArmIt(t *testing.T) {
	t.Parallel()
	p := &fakePrompter{decision: DecisionDeny, err: errors.New("prompt broker closed")}
	g := New(Options{Mode: ModeAsk, Prompter: p})
	ctx := context.Background()

	if err := g.CheckBash(ctx, "kubectl get pods"); err == nil {
		t.Fatal("expected the broker failure to fail the call")
	}
	if err := g.CheckBash(ctx, "kubectl get pods"); err == nil {
		t.Fatal("expected the retry to fail too (same broker)")
	}
	if len(p.calls) != 2 {
		t.Fatalf("a broker error suppressed the retry: %d prompts, want 2", len(p.calls))
	}
}

// The memory is scoped to the turn. By the next turn the model has a
// tool result in its history saying it was refused, and the operator's
// circumstances may have changed; a session-scoped refusal would also
// mean one misclick silently disables a tool for hours.
func TestTurnRefusal_ClearedAtTheTurnBoundary(t *testing.T) {
	t.Parallel()
	p := &fakePrompter{decision: DecisionDeny}
	g := New(Options{Mode: ModeAsk, Prompter: p})
	ctx := context.Background()

	if err := g.CheckBash(ctx, "kubectl delete ns prod"); err == nil {
		t.Fatal("expected the first call to be denied")
	}
	if err := g.CheckBash(ctx, "kubectl delete ns prod"); err == nil {
		t.Fatal("expected the repeat to be refused")
	}
	if len(p.calls) != 1 {
		t.Fatalf("in-turn prompts = %d, want 1", len(p.calls))
	}

	g.ObserveTurnStart(ctx)

	if err := g.CheckBash(ctx, "kubectl delete ns prod"); err == nil {
		t.Fatal("expected the next turn's call to be denied by the prompter")
	}
	if len(p.calls) != 2 {
		t.Fatalf("the turn boundary did not clear the refusal: prompts = %d, want 2", len(p.calls))
	}
}

// ObserveTurnStart follows the same rule every Check* method does: the
// state that needs clearing belongs to the session's gate, not to the
// template the tool wrappers were built against. Clearing the wrong one
// would leave a real session's refusals in place forever — which is the
// session-scoped behaviour this deliberately is not.
func TestTurnRefusal_TurnBoundaryFollowsTheSessionGate(t *testing.T) {
	t.Parallel()
	template := New(Options{Mode: ModeAsk, Prompter: &fakePrompter{decision: DecisionDeny}})
	p := &fakePrompter{decision: DecisionDeny}
	sub := template.DeriveForSession("sess-1", p)
	ctx := WithSessionGate(context.Background(), sub)

	if err := sub.CheckBash(ctx, "kubectl delete ns prod"); err == nil {
		t.Fatal("expected the first call to be denied")
	}
	// The agent holds the template in this shape; the turn boundary has
	// to reach through ctx to the session that actually did the asking.
	template.ObserveTurnStart(ctx)

	if err := sub.CheckBash(ctx, "kubectl delete ns prod"); err == nil {
		t.Fatal("expected the next turn's call to be denied by the prompter")
	}
	if len(p.calls) != 2 {
		t.Fatalf("the boundary cleared the template instead of the session: prompts = %d, want 2", len(p.calls))
	}
}

// Sub-gates get their own map for the same reason they get their own
// sessionAllow: one session's operator saying no is not an answer on
// behalf of another session's.
func TestTurnRefusal_IsolatedPerSession(t *testing.T) {
	t.Parallel()
	template := New(Options{Mode: ModeAsk})
	pa := &fakePrompter{decision: DecisionDeny}
	pb := &fakePrompter{decision: DecisionDeny}
	subA := template.DeriveForSession("sess-a", pa)
	subB := template.DeriveForSession("sess-b", pb)
	ctx := context.Background()

	if err := subA.CheckBash(ctx, "kubectl delete ns prod"); err == nil {
		t.Fatal("expected session A's call to be denied")
	}
	if err := subB.CheckBash(ctx, "kubectl delete ns prod"); err == nil {
		t.Fatal("expected session B's call to be denied")
	}
	if len(pb.calls) != 1 {
		t.Fatalf("session B was answered by session A's refusal: prompts = %d, want 1", len(pb.calls))
	}
}

// The elevated control-plane path bypasses Gate.prompt, so it carries
// its own check. It is also the path where a re-issue costs the most:
// the operator is being paged, repeatedly, about the file that controls
// the agent's own permissions.
func TestTurnRefusal_ControlPlaneWriteIsNotAskedTwice(t *testing.T) {
	t.Parallel()
	root, configJSON := agentsLayout(t)
	p := &fakePrompter{decision: DecisionDeny}
	g := cpScopeGate(t, root, ModeAsk, p)
	ctx := context.Background()

	if err := g.CheckFileWrite(ctx, "write_file", configJSON); err == nil {
		t.Fatal("expected the control-plane write to be denied")
	}
	second := g.CheckFileWrite(ctx, "write_file", configJSON)
	if second == nil {
		t.Fatal("expected the repeat control-plane write to be refused")
	}
	if len(p.calls) != 1 {
		t.Fatalf("the gate opened %d elevated prompts for one refused write, want 1", len(p.calls))
	}
	if !strings.Contains(strings.ToLower(second.Error()), "denied earlier in this turn") {
		t.Errorf("repeat control-plane refusal does not say it is a repeat: %v", second)
	}

	// A different control-plane file is a different request, and the
	// approval path is untouched: nothing here may install the standing
	// bypass checkControlPlaneWrite exists to prevent.
	other := filepath.Join(root, ".agents", "mcp.json")
	if err := g.CheckFileWrite(ctx, "write_file", other); err == nil {
		t.Fatal("expected the other control-plane write to be denied")
	}
	if len(p.calls) != 2 {
		t.Fatalf("a distinct control-plane path rode the first refusal: prompts = %d, want 2", len(p.calls))
	}
}
