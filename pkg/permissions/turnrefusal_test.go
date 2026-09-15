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

// --- #1081: the count the agent reads to end a turn ------------------

// The count is of SUPPRESSED calls, not of refusals. A first denial is
// the operator answering; it is the call after it — made with the
// answer already in the model's history — that is evidence of a model
// not taking no for an answer, and only those may arm anything.
func TestTurnRefusalRepeats_CountsOnlySuppressedCalls(t *testing.T) {
	t.Parallel()
	p := &fakePrompter{decision: DecisionDeny}
	g := New(Options{Mode: ModeAsk, Prompter: p})
	ctx := context.Background()

	if got := g.TurnRefusalRepeats(ctx); got != 0 {
		t.Fatalf("repeats before any call = %d, want 0", got)
	}
	if err := g.CheckBash(ctx, "kubectl delete ns prod"); err == nil {
		t.Fatal("expected the first call to be denied")
	}
	if got := g.TurnRefusalRepeats(ctx); got != 0 {
		t.Errorf("repeats after the operator's own denial = %d, want 0 — "+
			"the human answering is not the agent ignoring them", got)
	}
	for i := range 2 {
		if err := g.CheckBash(ctx, "kubectl delete ns prod"); err == nil {
			t.Fatalf("repeat %d: expected the call to be refused", i+1)
		}
	}
	if got := g.TurnRefusalRepeats(ctx); got != 2 {
		t.Errorf("repeats after two suppressed calls = %d, want 2", got)
	}
}

// Nothing that reaches a prompter counts, whatever the prompter says.
// An approved repeat and a denied NEW request are both the gate doing
// its ordinary job, and folding either into the count would arm the
// agent's cut on an operator who is present and answering.
func TestTurnRefusalRepeats_OnlySuppressionCounts(t *testing.T) {
	t.Parallel()

	t.Run("an approved repeat", func(t *testing.T) {
		t.Parallel()
		p := &fakePrompter{decision: DecisionAllowOnce}
		g := New(Options{Mode: ModeAsk, Prompter: p})
		ctx := context.Background()
		for i := range 4 {
			if err := g.CheckBash(ctx, "kubectl apply -f patch.yaml"); err != nil {
				t.Fatalf("call %d: approved call refused: %v", i+1, err)
			}
		}
		if got := g.TurnRefusalRepeats(ctx); got != 0 {
			t.Errorf("repeats after four approved calls = %d, want 0", got)
		}
	})

	t.Run("four distinct denied requests", func(t *testing.T) {
		t.Parallel()
		p := &fakePrompter{decision: DecisionDeny}
		g := New(Options{Mode: ModeAsk, Prompter: p})
		ctx := context.Background()
		for _, ns := range []string{"prod", "staging", "dev", "test"} {
			if err := g.CheckBash(ctx, "kubectl delete ns "+ns); err == nil {
				t.Fatalf("expected %q to be denied", ns)
			}
		}
		if got := g.TurnRefusalRepeats(ctx); got != 0 {
			t.Errorf("repeats after four distinct denials = %d, want 0 — an agent "+
				"asking about four different things is being told no four times, "+
				"not re-issuing one refused call", got)
		}
		if len(p.calls) != 4 {
			t.Errorf("prompts = %d, want 4", len(p.calls))
		}
	})
}

// Counted across keys. A model alternating between two requests the
// operator has already refused is in the same state as one repeating a
// single request, and per-key counting would let it stay just under any
// threshold by alternating.
func TestTurnRefusalRepeats_CountsAcrossRequests(t *testing.T) {
	t.Parallel()
	p := &fakePrompter{decision: DecisionDeny}
	g := New(Options{Mode: ModeAsk, Prompter: p})
	ctx := context.Background()

	for _, ns := range []string{"prod", "staging"} {
		if err := g.CheckBash(ctx, "kubectl delete ns "+ns); err == nil {
			t.Fatalf("expected %q to be denied", ns)
		}
	}
	for _, ns := range []string{"prod", "staging", "prod"} {
		if err := g.CheckBash(ctx, "kubectl delete ns "+ns); err == nil {
			t.Fatalf("expected the repeat of %q to be refused", ns)
		}
	}
	if got := g.TurnRefusalRepeats(ctx); got != 3 {
		t.Errorf("repeats across two alternating requests = %d, want 3", got)
	}
}

// The count is turn-scoped for the same reason the map it counts is.
// It has to clear at the SAME boundary, or a turn opens already part of
// the way to a cut it did nothing to earn.
func TestTurnRefusalRepeats_ClearedAtTheTurnBoundary(t *testing.T) {
	t.Parallel()
	p := &fakePrompter{decision: DecisionDeny}
	g := New(Options{Mode: ModeAsk, Prompter: p})
	ctx := context.Background()

	if err := g.CheckBash(ctx, "kubectl delete ns prod"); err == nil {
		t.Fatal("expected the first call to be denied")
	}
	for range 3 {
		if err := g.CheckBash(ctx, "kubectl delete ns prod"); err == nil {
			t.Fatal("expected the repeat to be refused")
		}
	}
	if got := g.TurnRefusalRepeats(ctx); got != 3 {
		t.Fatalf("in-turn repeats = %d, want 3", got)
	}

	g.ObserveTurnStart(ctx)

	if got := g.TurnRefusalRepeats(ctx); got != 0 {
		t.Errorf("repeats after the turn boundary = %d, want 0", got)
	}
}

// Both the reader and the counter have to land on the same gate. The
// suppressions happen on the session's sub-gate, so a reader that
// answered from the template would report 0 forever and the agent's arm
// would never fire in the deployment it exists for — a multi-session
// daemon, which is every gated deployment that is not a terminal.
func TestTurnRefusalRepeats_FollowsTheSessionGate(t *testing.T) {
	t.Parallel()
	template := New(Options{Mode: ModeAsk, Prompter: &fakePrompter{decision: DecisionDeny}})
	p := &fakePrompter{decision: DecisionDeny}
	sub := template.DeriveForSession("sess-1", p)
	ctx := WithSessionGate(context.Background(), sub)

	if err := sub.CheckBash(ctx, "kubectl delete ns prod"); err == nil {
		t.Fatal("expected the first call to be denied")
	}
	for range 2 {
		if err := sub.CheckBash(ctx, "kubectl delete ns prod"); err == nil {
			t.Fatal("expected the repeat to be refused")
		}
	}
	// The agent holds the template — WithGate is wired once at startup —
	// and has to reach through ctx to the session that did the refusing.
	if got := template.TurnRefusalRepeats(ctx); got != 2 {
		t.Errorf("template.TurnRefusalRepeats through the session ctx = %d, want 2", got)
	}
	// And the count belongs to that session alone.
	other := template.DeriveForSession("sess-2", &fakePrompter{decision: DecisionDeny})
	if got := other.TurnRefusalRepeats(context.Background()); got != 0 {
		t.Errorf("a second session sees %d repeats, want 0", got)
	}
}

// Nil-safe for the same reason ObserveTurnStart is: a host may hold a
// gate it never wired an agent to, and the agent's arm reads this on
// every tool result.
func TestTurnRefusalRepeats_NilSafe(t *testing.T) {
	t.Parallel()
	var g *Gate
	if got := g.TurnRefusalRepeats(context.Background()); got != 0 {
		t.Errorf("nil gate reported %d repeats, want 0", got)
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
