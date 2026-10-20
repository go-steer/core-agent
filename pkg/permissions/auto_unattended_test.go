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
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/core-agent/v3/pkg/config"
)

// The auto-mode behaviours for a deployment nobody answers
// (docs/auto-mode-unattended-design.md), learned from the self-dev
// soak's A7 runs.

const compoundTest = "go test ./... 2>&1 | tail -30"

func refusingGate(t *testing.T, a Approver, p Prompter) *Gate {
	t.Helper()
	return autoGate(t, a, p, func(o *Options) { o.AutoRefuseEscalation = true })
}

// A7 run 1: an ineligible call waited 30 minutes for a person nobody
// was. Under escalation "refuse" it is refused at once, says why, and
// a repeat in the same turn is refused without asking anybody.
func TestAutoRefuse_IneligibleCallRefusedAtOnceWithReason(t *testing.T) {
	t.Parallel()
	a := &stubApprover{verdict: allowVerdict}
	p := &fakePrompter{decision: DecisionAllowOnce}
	g := refusingGate(t, a, p)
	ctx, _ := taskCtx()
	args := json.RawMessage(`{"command":"go build ./..."}`)

	err := bashCall(ctx, g, "go build ./...", args)
	if !errors.Is(err, ErrEscalationRefused) {
		t.Fatalf("err = %v, want ErrEscalationRefused", err)
	}
	for _, want := range []string{"eligible list", `escalation is "refuse"`, "done another way"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not say %q", err, want)
		}
	}
	if len(p.calls) != 0 || a.n() != 0 {
		t.Errorf("prompter asked %d times, approver %d; want 0 and 0", len(p.calls), a.n())
	}

	// The repeat must not tell the agent the action is pending and to
	// end the turn: that expiry guidance is what made A7's first run
	// quit, and under "refuse" nothing is pending.
	again := bashCall(ctx, g, "go build ./...", args)
	if again == nil || !strings.Contains(again.Error(), "asks no person") || !strings.Contains(again.Error(), "done another way") {
		t.Errorf("repeat = %v, want the refuse-mode repeat with the refuse guidance", again)
	}
	if strings.Contains(again.Error(), "pending") {
		t.Errorf("repeat %q calls the action pending", again)
	}
	if len(p.calls) != 0 || g.TurnRefusalRepeats(ctx) != 1 {
		t.Errorf("repeat: prompter asked %d times, repeats counted %d; want 0 and 1", len(p.calls), g.TurnRefusalRepeats(ctx))
	}
}

// A7 run 2: an unquoted `-run A|B` made a test command a pipeline. The
// reason names that case, because it is the one the agent can always
// fix by splitting the command.
func TestAutoRefuse_CompoundBashSaysWhy(t *testing.T) {
	t.Parallel()
	g := refusingGate(t, &stubApprover{verdict: allowVerdict}, &fakePrompter{decision: DecisionAllowOnce})
	ctx, _ := taskCtx()
	err := bashCall(ctx, g, compoundTest, json.RawMessage(`{"command":`+mustJSON(compoundTest)+`}`))
	if !errors.Is(err, ErrEscalationRefused) || !strings.Contains(err.Error(), "not one simple literal command") {
		t.Fatalf("err = %v, want a refusal saying it is not one simple command", err)
	}

	// A simple command that is merely unlisted keeps the list reason:
	// telling the agent to split it would send it in the wrong direction.
	err = bashCall(ctx, g, "make deploy", json.RawMessage(`{"command":"make deploy"}`))
	if err == nil || strings.Contains(err.Error(), "simple command") || !strings.Contains(err.Error(), "eligible list") {
		t.Errorf("unlisted simple command: %v, want the eligible-list reason", err)
	}
}

// A7 run 2's third stall: the approver may pass a call on as well.
// Under "refuse" nobody would answer that either, so it is refused at
// once with the approver's own reason.
func TestAutoRefuse_ApproverPassOnRefusedWithItsReason(t *testing.T) {
	t.Parallel()
	a := &stubApprover{verdict: Verdict{Outcome: VerdictEscalate, Reason: "touches CI config", Model: "approver-1"}}
	p := &fakePrompter{decision: DecisionAllowOnce}
	g := refusingGate(t, a, p)
	ctx, _ := taskCtx()
	err := bashCall(ctx, g, testCmd, testArgs)
	if !errors.Is(err, ErrEscalationRefused) {
		t.Fatalf("err = %v, want ErrEscalationRefused", err)
	}
	if !strings.Contains(err.Error(), `approver-1 passed it to a person, saying: "touches CI config"`) {
		t.Errorf("refusal %q does not carry the approver's reason", err)
	}
	if a.n() != 1 || len(p.calls) != 0 {
		t.Errorf("approver asked %d times, prompter %d; want 1 and 0", a.n(), len(p.calls))
	}
}

// "refuse" changes what happens to calls a person would get. An
// eligible call the approver allows still runs.
func TestAutoRefuse_ApproverAllowStillRuns(t *testing.T) {
	t.Parallel()
	a := &stubApprover{verdict: allowVerdict}
	g := refusingGate(t, a, &fakePrompter{decision: DecisionDeny})
	ctx, _ := taskCtx()
	if err := bashCall(ctx, g, testCmd, testArgs); err != nil || a.n() != 1 {
		t.Errorf("eligible allowed call: err %v, approver asked %d times; want nil and 1", err, a.n())
	}
}

// The default still asks a person, and the prompt's request carries the
// reason, so a prompter can show it and an expiry can repeat it.
func TestAutoPerson_PromptCarriesReasonAndExpiryRepeatsIt(t *testing.T) {
	t.Parallel()
	args := json.RawMessage(`{"command":` + mustJSON(compoundTest) + `}`)

	p := &fakePrompter{decision: DecisionAllowOnce}
	g := autoGate(t, &stubApprover{verdict: allowVerdict}, p, nil)
	ctx, _ := taskCtx()
	if err := bashCall(ctx, g, compoundTest, args); err != nil {
		t.Fatalf("person mode, prompter allowed: %v", err)
	}
	if len(p.calls) != 1 || !strings.Contains(p.calls[0].Ineligible, "not one simple literal command") {
		t.Fatalf("prompts %d (%+v); want one prompt whose Ineligible carries the reason", len(p.calls), p.calls)
	}

	expiring := autoGate(t, &stubApprover{verdict: allowVerdict}, newBlockingPrompter(), func(o *Options) { o.ApprovalTimeout = 20 * time.Millisecond })
	err := bashCall(ctx, expiring, compoundTest, args)
	if !errors.Is(err, ErrPromptExpired) || !strings.Contains(err.Error(), "not one simple literal command") {
		t.Errorf("expiry = %v, want ErrPromptExpired carrying the reason", err)
	}
}

// A sub-gate that dropped the setting would put back the wait for
// exactly the daemon's sessions.
func TestAutoRefuse_DeriveForSessionInherits(t *testing.T) {
	t.Parallel()
	p := &fakePrompter{decision: DecisionAllowOnce}
	sub := refusingGate(t, &stubApprover{verdict: allowVerdict}, p).DeriveForSession("s2", p)
	ctx, _ := taskCtx()
	if err := bashCall(ctx, sub, "go build ./...", json.RawMessage(`{"command":"go build ./..."}`)); !errors.Is(err, ErrEscalationRefused) || len(p.calls) != 0 {
		t.Errorf("derived gate: err %v, prompts %d; want ErrEscalationRefused and no prompt", err, len(p.calls))
	}
}

func TestAutoRefuse_FromConfig(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		escalation string
		want       bool
	}{{"", false}, {config.AutoEscalationPerson, false}, {config.AutoEscalationRefuse, true}} {
		cfg := defaultGateConfig(t)
		cfg.Permissions.Mode = string(ModeAuto)
		cfg.Permissions.ApprovalTimeout = "5m"
		cfg.Permissions.Auto = &config.AutoApproverConfig{Escalation: tc.escalation}
		g, err := FromConfig(cfg, t.TempDir(), t.TempDir(), nil)
		if err != nil {
			t.Fatalf("escalation %q: %v", tc.escalation, err)
		}
		if g.autoRefuseEscalation != tc.want {
			t.Errorf("escalation %q: autoRefuseEscalation = %v, want %v", tc.escalation, g.autoRefuseEscalation, tc.want)
		}
	}
}

// A7 run 2: the approver rightly denied a no-op edit_file (identical old
// and new text), and the agent's real edit to the same file was then
// refused as "an identical request" for the rest of the turn. The
// refusal was of that edit, not of the file.
func TestTurnRefusal_FileEditKeyedOnItsArguments(t *testing.T) {
	t.Parallel()
	a := &stubApprover{verdict: Verdict{Outcome: VerdictDeny, Reason: "no-op edit", Model: "approver-1"}}
	g := autoGate(t, a, &fakePrompter{decision: DecisionAllowOnce}, func(o *Options) {
		o.AutoEligible = mustPolicy(t, []string{"edit_file:*"}, nil)
	})
	ctx, _ := taskCtx()
	edit := func(oldText, newText string) error {
		args, _ := json.Marshal(map[string]string{"path": "/w/pkg/x_test.go", "old_string": oldText, "new_string": newText})
		return g.prompt(ctx, ModeAuto, PromptRequest{Kind: PromptKindGeneric, ToolName: "edit_file", Detail: "/w/pkg/x_test.go", Args: args})
	}

	if err := edit("a", "a"); err == nil || !strings.Contains(err.Error(), "denied by the approver") {
		t.Fatalf("no-op edit: %v, want the approver's deny", err)
	}
	if err := edit("a", "b"); err == nil || strings.Contains(err.Error(), "identical request") || a.n() != 2 {
		t.Errorf("a different edit to the same file: %v, approver asked %d times; want it judged again (2 calls), not refused as a repeat", err, a.n())
	}
	if err := edit("a", "a"); err == nil || !strings.Contains(err.Error(), "identical request") || a.n() != 2 {
		t.Errorf("the identical edit again: %v, approver asked %d times; want the repeat refusal and no new call", err, a.n())
	}
}

// Bash keeps tool|detail: its detail is the whole command, and adding
// the arguments would let a changed timeout_seconds launder a refused
// command past the memory (#1074).
func TestTurnRefusal_BashTimeoutDoesNotLaunderARefusal(t *testing.T) {
	t.Parallel()
	a := &stubApprover{verdict: Verdict{Outcome: VerdictDeny, Reason: "no", Model: "approver-1"}}
	g := autoGate(t, a, &fakePrompter{decision: DecisionAllowOnce}, nil)
	ctx, _ := taskCtx()
	if err := bashCall(ctx, g, testCmd, json.RawMessage(`{"command":"go test ./...","timeout_seconds":30}`)); err == nil {
		t.Fatal("first call: want the approver's deny")
	}
	err := bashCall(ctx, g, testCmd, json.RawMessage(`{"command":"go test ./...","timeout_seconds":600}`))
	if err == nil || !strings.Contains(err.Error(), "identical request") || a.n() != 1 {
		t.Errorf("same command, new timeout: %v, approver asked %d times; want the repeat refusal and 1 call", err, a.n())
	}
}

func TestAutoEscalationConfigValidates(t *testing.T) {
	t.Parallel()
	for _, v := range []string{"", config.AutoEscalationPerson, config.AutoEscalationRefuse} {
		cfg := defaultGateConfig(t)
		cfg.Permissions.Auto = &config.AutoApproverConfig{Escalation: v}
		if err := cfg.Validate(); err != nil {
			t.Errorf("escalation %q: %v", v, err)
		}
	}
	cfg := defaultGateConfig(t)
	cfg.Permissions.Auto = &config.AutoApproverConfig{Escalation: "nobody"}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "permissions.auto.escalation") {
		t.Errorf("escalation \"nobody\": %v, want a permissions.auto.escalation error", err)
	}
}

// Review finding: control-plane writes take their own elevated path and
// never reached the refuse branch, so under "refuse" they still waited
// for a person, with no approval_timeout. Ask mode is untouched.
func TestAutoRefuse_ControlPlaneWriteRefusedAtOnce(t *testing.T) {
	t.Parallel()
	cp := filepath.Join(t.TempDir(), "config.json")
	ctx, _ := taskCtx()

	p := &fakePrompter{decision: DecisionAllowOnce}
	g := refusingGate(t, &stubApprover{verdict: allowVerdict}, p)
	err := g.checkControlPlaneWrite(ctx, "write_file", cp)
	if !errors.Is(err, ErrEscalationRefused) || !strings.Contains(err.Error(), "control-plane writes are never the approver's") || len(p.calls) != 0 {
		t.Fatalf("auto+refuse control-plane write: err %v, prompts %d; want an immediate refusal naming why", err, len(p.calls))
	}
	if again := g.checkControlPlaneWrite(ctx, "write_file", cp); again == nil || !strings.Contains(again.Error(), "asks no person") {
		t.Errorf("repeat: %v, want the refuse-mode repeat", again)
	}

	asked := &fakePrompter{decision: DecisionAllowOnce}
	ask := New(Options{Prompter: asked, AutoRefuseEscalation: true})
	if err := ask.checkControlPlaneWrite(ctx, "write_file", cp); err != nil || len(asked.calls) != 1 {
		t.Errorf("ask mode: err %v, prompts %d; want the ordinary prompt", err, len(asked.calls))
	}
}

// Review finding: an approver that errors is not an approver that passed
// the call on. The refusal says so, and is not armed for the turn, so a
// retry goes back to the approver.
func TestAutoRefuse_ApproverFailureIsNotAPassOn(t *testing.T) {
	t.Parallel()
	a := &stubApprover{err: errors.New("429 resource exhausted")}
	g := refusingGate(t, a, &fakePrompter{decision: DecisionAllowOnce})
	ctx, _ := taskCtx()
	err := bashCall(ctx, g, testCmd, testArgs)
	if !errors.Is(err, ErrEscalationRefused) || !strings.Contains(err.Error(), "could not decide it") || strings.Contains(err.Error(), "passed it to a person") {
		t.Fatalf("err = %v, want a refusal saying the approver could not decide", err)
	}
	if err := bashCall(ctx, g, testCmd, testArgs); err == nil || strings.Contains(err.Error(), "identical request") || a.n() != 2 {
		t.Errorf("retry: %v, approver asked %d times; want it judged again (2), not a repeat refusal", err, a.n())
	}
}

// The refuse branch sits in front of every person-bound outcome of
// Gate.prompt: no prompter, a path-scope prompt, a background
// subagent's call. In ModeAsk it does nothing.
func TestAutoRefuse_CoversEveryPersonBoundPath(t *testing.T) {
	t.Parallel()
	ctx, _ := taskCtx()
	notEligible := json.RawMessage(`{"command":"go build ./..."}`)

	noPrompter := refusingGate(t, &stubApprover{verdict: allowVerdict}, nil)
	if err := bashCall(ctx, noPrompter, "go build ./...", notEligible); !errors.Is(err, ErrEscalationRefused) {
		t.Errorf("no prompter: %v, want ErrEscalationRefused", err)
	}

	p := &fakePrompter{decision: DecisionAllowOnce}
	g := refusingGate(t, &stubApprover{verdict: allowVerdict}, p)
	err := g.prompt(ctx, ModeAuto, PromptRequest{
		Kind: PromptKindPathScope, ToolName: "read_file", Detail: "/home/u/.ssh/id_rsa",
		Args: json.RawMessage(`{"path":"/home/u/.ssh/id_rsa"}`), Access: AccessRead,
	})
	if !errors.Is(err, ErrEscalationRefused) || !strings.Contains(err.Error(), "path scope") {
		t.Errorf("path scope: %v, want a refusal naming the path scope", err)
	}
	sub := WithSubagentSource(ctx, "reviewer")
	if err := bashCall(sub, g, testCmd, testArgs); !errors.Is(err, ErrEscalationRefused) || !strings.Contains(err.Error(), "background subagents") {
		t.Errorf("background subagent: %v, want a refusal naming why", err)
	}
	if len(p.calls) != 0 {
		t.Errorf("prompter asked %d times under refuse, want 0", len(p.calls))
	}

	asked := &fakePrompter{decision: DecisionAllowOnce}
	ask := autoGate(t, &stubApprover{verdict: allowVerdict}, asked, func(o *Options) {
		o.Mode = ModeAsk
		o.AutoRefuseEscalation = true
	})
	if err := bashCall(ctx, ask, "go build ./...", notEligible); err != nil || len(asked.calls) != 1 {
		t.Errorf("ask mode with refuse set: err %v, prompts %d; want the ordinary prompt", err, len(asked.calls))
	}
}

// The specific bash reasons: a quoting error is not a pipe, and a flag a
// verb profile rejects is not a missing pattern.
func TestAutoIneligible_BashReasons(t *testing.T) {
	t.Parallel()
	g := autoGate(t, &stubApprover{verdict: allowVerdict}, &fakePrompter{decision: DecisionAllowOnce}, func(o *Options) {
		o.AutoRefuseEscalation = true
		o.AutoEligible = mustPolicy(t, []string{"bash:go test *", "bash:find *"}, nil)
	})
	ctx, _ := taskCtx()
	for _, tc := range []struct{ cmd, want string }{
		{"go test -run 'Foo ./...", "unbalanced quotes"},
		{"(cd x && go test ./...)", "not one simple literal command"},
		{"find . -name x -delete", "find's -exec or -delete"},
	} {
		err := bashCall(ctx, g, tc.cmd, json.RawMessage(`{"command":`+mustJSON(tc.cmd)+`}`))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: %v, want a reason naming %q", tc.cmd, err, tc.want)
		}
	}
}

// Review finding: the denial key reaches ask mode on purpose (#1074's
// "keyed on the request" rule): a person who denied one edit is asked
// about a different edit to the same file, and not about the same one.
func TestTurnRefusal_PersonDenyKeyedOnArguments(t *testing.T) {
	t.Parallel()
	p := &fakePrompter{decision: DecisionDeny}
	g := New(Options{Prompter: p})
	edit := func(newText string) error {
		args, _ := json.Marshal(map[string]string{"path": "/w/x.go", "old_string": "a", "new_string": newText})
		return g.prompt(context.Background(), ModeAsk, PromptRequest{Kind: PromptKindGeneric, ToolName: "edit_file", Detail: "/w/x.go", Args: args})
	}
	_ = edit("b")
	_ = edit("c")
	if len(p.calls) != 2 {
		t.Errorf("a different edit after a person's deny: %d prompts, want 2", len(p.calls))
	}
	if err := edit("b"); err == nil || !strings.Contains(err.Error(), "identical request") || len(p.calls) != 2 {
		t.Errorf("the denied edit again: %v, prompts %d; want the repeat refusal and no prompt", err, len(p.calls))
	}
}

// Review finding: an expiry is not about the content. A reworded call
// after an expiry is refused at once, not given another full wait.
func TestTurnRefusal_ExpiryKeepsTheBareKey(t *testing.T) {
	t.Parallel()
	g := New(Options{Prompter: newBlockingPrompter(), ApprovalTimeout: 20 * time.Millisecond})
	write := func(content string) error {
		args, _ := json.Marshal(map[string]string{"path": "/w/x.go", "content": content})
		return g.prompt(context.Background(), ModeAsk, PromptRequest{Kind: PromptKindGeneric, ToolName: "write_file", Detail: "/w/x.go", Args: args})
	}
	if err := write("one"); !errors.Is(err, ErrPromptExpired) {
		t.Fatalf("first write: %v, want ErrPromptExpired", err)
	}
	start := time.Now()
	err := write("two")
	if err == nil || !strings.Contains(err.Error(), "expired unanswered") || time.Since(start) > 10*time.Millisecond {
		t.Errorf("reworded write after an expiry: %v after %s; want the instant repeat refusal", err, time.Since(start))
	}
}
