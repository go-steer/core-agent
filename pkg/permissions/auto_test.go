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
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/genai"
)

// Auto mode (#1175). Decision numbers refer to the "Settled decisions"
// in docs/auto-mode-design.md. Phase 1 has no call site that passes
// Args yet, so these tests drive gateRequest / prompt directly with
// Args set — the public Check* methods pass none and so never reach
// the approver, which TestAuto_PublicCheckWithoutArgsNeverReachesApprover
// pins.

type stubApprover struct {
	mu      sync.Mutex
	verdict Verdict
	err     error
	calls   []ApproverRequest
}

func (s *stubApprover) Judge(_ context.Context, req ApproverRequest) (Verdict, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, req)
	return s.verdict, s.err
}

func (s *stubApprover) n() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

type stubApproverContext struct {
	mu      sync.Mutex
	task    string
	recent  []ApproverCall
	ceiling bool
	audits  []ApproverAudit
}

func (c *stubApproverContext) Task() string                { return c.task }
func (c *stubApproverContext) RecentCalls() []ApproverCall { return c.recent }
func (c *stubApproverContext) CeilingReached() bool        { return c.ceiling }
func (c *stubApproverContext) Bill(string, *genai.GenerateContentResponseUsageMetadata) {
}

func (c *stubApproverContext) Audit(a ApproverAudit) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.audits = append(c.audits, a)
}

func (c *stubApproverContext) all() []ApproverAudit {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]ApproverAudit(nil), c.audits...)
}

func mustPolicy(t *testing.T, allow, deny []string) *Policy {
	t.Helper()
	p, err := NewPolicy(allow, deny)
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	return p
}

var allowVerdict = Verdict{Outcome: VerdictAllow, Reason: "in task", Model: "approver-1"}

const testCmd = "go test ./..."

var testArgs = json.RawMessage(`{"command":"go test ./..."}`)

// autoGate is an auto-mode gate whose approver may decide `go test`.
func autoGate(t *testing.T, a Approver, p Prompter, mutate func(*Options)) *Gate {
	t.Helper()
	opts := Options{
		Mode:            ModeAuto,
		Prompter:        p,
		Approver:        a,
		AutoEligible:    mustPolicy(t, []string{"bash:go test *"}, nil),
		ApprovalTimeout: time.Minute,
	}
	if mutate != nil {
		mutate(&opts)
	}
	return New(opts)
}

func taskCtx() (context.Context, *stubApproverContext) {
	ac := &stubApproverContext{task: "run the tests and fix what fails"}
	return WithApproverContext(context.Background(), ac), ac
}

func bashCall(ctx context.Context, g *Gate, cmd string, args json.RawMessage) error {
	return g.gateRequest(ctx, PromptKindBash, "bash", cmd, "bash", cmd, "bash", false, args)
}

// Decision 5: an approver allow is once. Nothing is remembered and no
// policy, grant map or GrantStore is written, so the identical call
// goes back to the approver; the log names the model, not a person.
func TestAuto_ApproverAllowsOnceAndRemembersNothing(t *testing.T) {
	t.Parallel()
	a := &stubApprover{verdict: allowVerdict}
	p := &fakePrompter{decision: DecisionDeny}
	store := &fakeGrantStore{}
	g := autoGate(t, a, p, func(o *Options) { o.GrantStore = store })
	ctx, _ := taskCtx()
	before := g.Snapshot()

	for i := 0; i < 2; i++ {
		if err := bashCall(ctx, g, testCmd, testArgs); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if a.n() != 2 {
		t.Errorf("approver asked %d times, want 2: an allow must not be remembered", a.n())
	}
	if len(p.calls) != 0 {
		t.Errorf("prompter asked %d times, want 0", len(p.calls))
	}
	if len(g.sessionAllow)+len(g.sessionAllowTools)+len(g.sessionAllowVerbs) != 0 {
		t.Errorf("session grants written: %v %v %v", g.sessionAllow, g.sessionAllowTools, g.sessionAllowVerbs)
	}
	if got := store.all(); len(got) != 0 {
		t.Errorf("GrantStore written: %+v", got)
	}
	if after := g.Snapshot(); len(after.Allow) != len(before.Allow) {
		t.Errorf("policy allow list grew from %v to %v", before.Allow, after.Allow)
	}
	log := g.Approvals()
	if len(log) != 2 {
		t.Fatalf("approvals = %+v, want 2", log)
	}
	for _, e := range log {
		if e.Decision != DecisionAllowOnce || e.Approver != "approver-1" || e.By != "" {
			t.Errorf("approval = %+v, want allow-once by approver-1 with no By", e)
		}
	}
}

// Decision 2: the approver runs before the nil-prompter check, so a
// host with no prompter runs eligible calls and denies the rest — with
// advice that fits auto, not "--yolo".
func TestAuto_NoPrompterHost(t *testing.T) {
	t.Parallel()
	a := &stubApprover{verdict: allowVerdict}
	g := autoGate(t, a, nil, nil)
	ctx, _ := taskCtx()
	if err := bashCall(ctx, g, testCmd, testArgs); err != nil {
		t.Fatalf("eligible call with no prompter: %v", err)
	}
	err := bashCall(ctx, g, "make deploy", json.RawMessage(`{"command":"make deploy"}`))
	if !errors.Is(err, ErrNoPrompter) {
		t.Fatalf("ineligible call with no prompter: %v, want ErrNoPrompter", err)
	}
	if strings.Contains(err.Error(), "--yolo") {
		t.Errorf("auto-mode no-prompter error recommends --yolo: %v", err)
	}
}

// Decision 9: an approver deny is final for the turn. The repeat is
// refused without asking the approver or a person, and neither error
// claims a person said no. The next turn asks again.
func TestAuto_ApproverDenyIsFinalForTheTurn(t *testing.T) {
	t.Parallel()
	a := &stubApprover{verdict: Verdict{Outcome: VerdictDeny, Reason: "not part of the task", Model: "approver-1"}}
	p := &fakePrompter{decision: DecisionAllowOnce}
	g := autoGate(t, a, p, nil)
	ctx, _ := taskCtx()

	err := bashCall(ctx, g, testCmd, testArgs)
	if err == nil || !strings.Contains(err.Error(), "approver model approver-1") || !strings.Contains(err.Error(), `"not part of the task"`) {
		t.Fatalf("first deny = %v, want the approver named and its reason quoted", err)
	}
	if strings.Contains(err.Error(), "by user") {
		t.Errorf("approver deny reads as a person's: %v", err)
	}
	err = bashCall(ctx, g, testCmd, testArgs)
	if err == nil || !strings.Contains(err.Error(), "the approver model denied an identical request") {
		t.Fatalf("repeat = %v, want the approver repeat refusal", err)
	}
	if strings.Contains(err.Error(), "human") {
		t.Errorf("repeat refusal claims a human: %v", err)
	}
	if a.n() != 1 || len(p.calls) != 0 {
		t.Errorf("approver asked %d times, prompter %d; want 1 and 0", a.n(), len(p.calls))
	}
	if g.TurnRefusalRepeats(ctx) != 1 {
		t.Errorf("TurnRefusalRepeats = %d, want 1", g.TurnRefusalRepeats(ctx))
	}

	g.ObserveTurnStart(ctx)
	_ = bashCall(ctx, g, testCmd, testArgs)
	if a.n() != 2 {
		t.Errorf("approver asked %d times after a new turn, want 2", a.n())
	}
}

// Decisions 3, 4 and 6: every one of these goes to a person without an
// approver call, whatever the approver would have said. "Zero approver
// calls on must-escalate" is decided in code, so it is a unit test.
func TestAuto_EscalatesWithoutAnApproverCall(t *testing.T) {
	t.Parallel()
	instructions := filepath.Join(t.TempDir(), "approver-policy.md")
	if err := os.WriteFile(instructions, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	type call func(ctx context.Context, g *Gate) error
	bash := func(cmd string) call {
		return func(ctx context.Context, g *Gate) error {
			return bashCall(ctx, g, cmd, json.RawMessage(`{"command":`+mustJSON(cmd)+`}`))
		}
	}
	// eligibleExactly makes cmd itself eligible, so a row escalates for
	// the reason it is about and not because an open-prefix pattern
	// declined a compound command. why pins that reason.
	eligibleExactly := func(cmd string, more func(*Options)) func(*Options) {
		return func(o *Options) {
			o.AutoEligible = mustPolicy(t, []string{"bash:" + cmd}, nil)
			if more != nil {
				more(o)
			}
		}
	}
	withInstructions := func(o *Options) { o.ApproverInstructionsFile = instructions }
	cpSed := "go test ./... && sed -i s/ask/yolo/ .agents/config.json"
	cpCat := "go test ./... ; cat .agents/mcp.json"
	insAbs := "go test ./... > " + instructions
	insBase := "go test ./... ; cp x ../somewhere/approver-policy.md"
	for _, tc := range []struct {
		name string
		why  string
		opts func(*Options)
		ctx  func() context.Context
		call call
	}{
		{name: "not on the eligible list", why: "eligible list", call: bash("go build ./...")},
		{name: "empty eligible list", why: "eligible list", opts: func(o *Options) { o.AutoEligible = nil }, call: bash(testCmd)},
		{name: "no Args", why: "full arguments", call: func(ctx context.Context, g *Gate) error { return bashCall(ctx, g, testCmd, nil) }},
		{name: "background subagent", why: "background subagents", ctx: func() context.Context {
			ctx, _ := taskCtx()
			return WithSubagentSource(ctx, "watcher")
		}, call: bash(testCmd)},
		{name: "no approver context", why: "no approver context", ctx: context.Background, call: bash(testCmd)},
		{name: "no operator task", why: "no operator-sent task", ctx: func() context.Context {
			return WithApproverContext(context.Background(), &stubApproverContext{})
		}, call: bash(testCmd)},
		{name: "cost ceiling reached", why: "ceiling", ctx: func() context.Context {
			return WithApproverContext(context.Background(), &stubApproverContext{task: "t", ceiling: true})
		}, call: bash(testCmd)},
		{name: "no approver", why: "no approver is configured", opts: func(o *Options) { o.Approver = nil }, call: bash(testCmd)},
		{name: "bash names .agents/config.json", why: ".agents/config.json", opts: eligibleExactly(cpSed, nil), call: bash(cpSed)},
		{name: "bash names .agents/mcp.json", why: ".agents/mcp.json", opts: eligibleExactly(cpCat, nil), call: bash(cpCat)},
		{name: "Args name a control-plane file past the Detail", why: ".agents/config.json", opts: func(o *Options) {
			o.AutoEligible = mustPolicy(t, []string{"mcp:*"}, nil)
		}, call: func(ctx context.Context, g *Gate) error {
			return g.gateRequest(ctx, PromptKindGeneric, "mcp", "fs_write path=notes.txt…", "mcp", "fs_write", "mcp/fs_write", false,
				json.RawMessage(`{"path":"notes.txt","also":"`+strings.Repeat("x", 300)+` ../.agents/config.json"}`))
		}},
		{name: "bash names the instructions file", why: instructions, opts: eligibleExactly(insAbs, withInstructions), call: bash(insAbs)},
		{name: "bash names the instructions file by base name", why: "approver-policy.md", opts: eligibleExactly(insBase, withInstructions), call: bash(insBase)},
		{name: "path scope", why: "path scope", opts: func(o *Options) { o.AutoEligible = mustPolicy(t, []string{"read_file:*"}, nil) },
			call: func(ctx context.Context, g *Gate) error {
				return g.prompt(ctx, ModeAuto, PromptRequest{
					Kind: PromptKindPathScope, ToolName: "read_file", Detail: "/home/u/.ssh/id_rsa",
					Args: json.RawMessage(`{"path":"/home/u/.ssh/id_rsa"}`), Access: AccessRead,
				})
			}},
		{name: "control-plane kind", why: "control-plane writes", opts: func(o *Options) { o.AutoEligible = mustPolicy(t, []string{"write_file:*"}, nil) },
			call: func(ctx context.Context, g *Gate) error {
				return g.prompt(ctx, ModeAuto, PromptRequest{
					Kind: PromptKindControlPlaneWrite, ToolName: "write_file", Detail: "x",
					Args: json.RawMessage(`{}`),
				})
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := &stubApprover{verdict: allowVerdict}
			p := &fakePrompter{decision: DecisionAllowOnce}
			g := autoGate(t, a, p, tc.opts)
			ctx, _ := taskCtx()
			if tc.ctx != nil {
				ctx = tc.ctx()
			}
			if err := tc.call(ctx, g); err != nil {
				t.Fatalf("escalated call, prompter allowed: %v", err)
			}
			if a.n() != 0 {
				t.Errorf("approver asked %d times, want 0", a.n())
			}
			if len(p.calls) != 1 {
				t.Fatalf("prompter asked %d times, want 1", len(p.calls))
			}
			if why, _ := g.approverIneligible(ctx, p.calls[0]); !strings.Contains(why, tc.why) {
				t.Errorf("escalated because %q, want a reason naming %q", why, tc.why)
			}
		})
	}
}

// The never-list floor is a substring match, best-effort like the bash
// denylist. A command that reaches a control-plane file without
// spelling its path is NOT caught — this test pins that the gap is
// known, so nobody mistakes the floor for a boundary. The eligible
// list is what keeps such a command away from the approver.
func TestAuto_ControlPlaneFloorIsBestEffort(t *testing.T) {
	t.Parallel()
	a := &stubApprover{verdict: allowVerdict}
	g := autoGate(t, a, &fakePrompter{}, func(o *Options) {
		o.AutoEligible = mustPolicy(t, []string{"bash:cd .agents && sed -i s/ask/yolo/ config.json"}, nil)
	})
	ctx, _ := taskCtx()
	cmd := "cd .agents && sed -i s/ask/yolo/ config.json"
	_ = bashCall(ctx, g, cmd, json.RawMessage(`{}`))
	if a.n() != 1 {
		t.Fatalf("approver asked %d times; the floor's documented gap has changed — update this test and the doc comment on protectedMentions", a.n())
	}
}

// Decision 1: everything that runs before the prompt in ask runs before
// the approver in auto, unchanged.
func TestAuto_PolicyGrantsAndPlanFirstRunFirst(t *testing.T) {
	t.Parallel()
	t.Run("deny pattern", func(t *testing.T) {
		t.Parallel()
		a := &stubApprover{verdict: allowVerdict}
		g := autoGate(t, a, nil, func(o *Options) { o.Policy = mustPolicy(t, nil, []string{"bash:go test *"}) })
		ctx, _ := taskCtx()
		if err := bashCall(ctx, g, testCmd, testArgs); err == nil || !strings.Contains(err.Error(), "denied by config policy") {
			t.Fatalf("err = %v, want the config deny", err)
		}
		if a.n() != 0 {
			t.Errorf("approver asked %d times, want 0", a.n())
		}
	})
	t.Run("allow pattern", func(t *testing.T) {
		t.Parallel()
		a := &stubApprover{verdict: Verdict{Outcome: VerdictDeny, Reason: "no", Model: "m"}}
		g := autoGate(t, a, nil, func(o *Options) { o.Policy = mustPolicy(t, []string{"bash:go test ./..."}, nil) })
		ctx, _ := taskCtx()
		if err := bashCall(ctx, g, testCmd, testArgs); err != nil {
			t.Fatalf("allowlisted call: %v", err)
		}
		if a.n() != 0 {
			t.Errorf("approver asked %d times, want 0", a.n())
		}
	})
	t.Run("session grant", func(t *testing.T) {
		t.Parallel()
		a := &stubApprover{verdict: Verdict{Outcome: VerdictDeny, Reason: "no", Model: "m"}}
		g := autoGate(t, a, nil, nil)
		g.rememberSession("bash", testCmd)
		ctx, _ := taskCtx()
		if err := bashCall(ctx, g, testCmd, testArgs); err != nil {
			t.Fatalf("session-granted call: %v", err)
		}
		if a.n() != 0 {
			t.Errorf("approver asked %d times, want 0", a.n())
		}
	})
	t.Run("plan-first", func(t *testing.T) {
		t.Parallel()
		a := &stubApprover{verdict: allowVerdict}
		g := autoGate(t, a, nil, func(o *Options) { o.RequirePlanArtifact = true })
		ctx, _ := taskCtx()
		if err := bashCall(ctx, g, testCmd, testArgs); err == nil || !strings.Contains(err.Error(), "plan-first") {
			t.Fatalf("err = %v, want the plan-first denial", err)
		}
		if a.n() != 0 {
			t.Errorf("approver asked %d times, want 0", a.n())
		}
	})
}

// Decision 2: control-plane writes never call prompt, so the approver
// is never asked about one, even with write_file eligible.
func TestAuto_ControlPlaneWriteNeverReachesApprover(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), ".agents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	a := &stubApprover{verdict: allowVerdict}
	p := &fakePrompter{decision: DecisionAllowOnce}
	g := autoGate(t, a, p, func(o *Options) { o.AutoEligible = mustPolicy(t, []string{"write_file:*"}, nil) })
	ctx, _ := taskCtx()
	if err := g.CheckFileWrite(ctx, "write_file", filepath.Join(dir, "config.json")); err != nil {
		t.Fatalf("CheckFileWrite: %v", err)
	}
	if a.n() != 0 {
		t.Errorf("approver asked %d times about a control-plane write", a.n())
	}
	if len(p.calls) != 1 || p.calls[0].Kind != PromptKindControlPlaneWrite {
		t.Errorf("prompter calls = %+v, want one control-plane prompt", p.calls)
	}
}

// Phase 1 has no call site passing Args, so a public Check* in auto
// never reaches the approver: a call site nobody updated fails closed.
func TestAuto_PublicCheckWithoutArgsNeverReachesApprover(t *testing.T) {
	t.Parallel()
	a := &stubApprover{verdict: allowVerdict}
	p := &fakePrompter{decision: DecisionAllowOnce}
	g := autoGate(t, a, p, nil)
	ctx, _ := taskCtx()
	if err := g.CheckBash(ctx, testCmd); err != nil {
		t.Fatalf("CheckBash: %v", err)
	}
	if a.n() != 0 || len(p.calls) != 1 {
		t.Errorf("approver %d / prompter %d, want 0 / 1", a.n(), len(p.calls))
	}
}

// An approver answer that can't be used escalates: an error, a deny
// without a reason, an outcome nobody defined, and the zero Verdict.
// The audit row records why; the prompt carries no reason from an
// answer that was not used.
func TestAuto_UnusableAnswersEscalate(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		verdict Verdict
		err     error
	}{
		{"error", Verdict{Outcome: VerdictAllow, Reason: "fine", Model: "m"}, errors.New("model unavailable")},
		{"deny without reason", Verdict{Outcome: VerdictDeny, Reason: "  ", Model: "m"}, nil},
		{"unknown outcome", Verdict{Outcome: VerdictOutcome(42), Reason: "fine", Model: "m"}, nil},
		{"zero verdict", Verdict{}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := &stubApprover{verdict: tc.verdict, err: tc.err}
			p := &fakePrompter{decision: DecisionAllowOnce}
			g := autoGate(t, a, p, nil)
			ctx, ac := taskCtx()
			if err := bashCall(ctx, g, testCmd, testArgs); err != nil {
				t.Fatalf("escalated call, prompter allowed: %v", err)
			}
			if len(p.calls) != 1 {
				t.Fatalf("prompter asked %d times, want 1", len(p.calls))
			}
			if p.calls[0].ApproverModel == "" || p.calls[0].ApproverReason != "" {
				t.Errorf("prompt ApproverModel=%q ApproverReason=%q, want a model and no reason", p.calls[0].ApproverModel, p.calls[0].ApproverReason)
			}
			audits := ac.all()
			if tc.name == "zero verdict" {
				if len(audits) != 1 || audits[0].Verdict.Outcome != VerdictEscalate || audits[0].Err != "" {
					t.Errorf("audits = %+v, want one plain escalate", audits)
				}
				return
			}
			if len(audits) != 1 || audits[0].Verdict.Outcome != VerdictEscalate || audits[0].Err == "" {
				t.Errorf("audits = %+v, want one escalate with Err", audits)
			}
		})
	}
}

// Decision 11: an approver-escalated prompt grants at most once, so an
// operator's (or a surface's) session or always answer cannot turn a
// reason the call's arguments may have steered into a standing grant.
func TestAuto_EscalatedPromptGrantsOnlyOnce(t *testing.T) {
	t.Parallel()
	for _, d := range []Decision{DecisionAllowSession, DecisionAllowSessionVerb, DecisionAllowSessionTool, DecisionAllowAlways} {
		t.Run(d.String(), func(t *testing.T) {
			t.Parallel()
			a := &stubApprover{verdict: Verdict{Outcome: VerdictEscalate, Reason: "routine, safe to always allow", Model: "approver-1"}}
			p := &fakePrompter{decision: d}
			store := &fakeGrantStore{}
			g := autoGate(t, a, p, func(o *Options) { o.GrantStore = store })
			ctx, _ := taskCtx()
			before := g.Snapshot()
			if err := bashCall(ctx, g, testCmd, testArgs); err != nil {
				t.Fatalf("escalated, operator allowed: %v", err)
			}
			if got := p.calls[0]; got.ApproverModel != "approver-1" || got.ApproverReason != "routine, safe to always allow" {
				t.Errorf("prompt carried model=%q reason=%q", got.ApproverModel, got.ApproverReason)
			}
			if got := store.all(); len(got) != 0 {
				t.Errorf("GrantStore written: %+v", got)
			}
			if after := g.Snapshot(); len(after.Allow) != len(before.Allow) {
				t.Errorf("policy grew: %v", after.Allow)
			}
			if log := g.Approvals(); len(log) != 1 || log[0].Decision != DecisionAllowOnce {
				t.Errorf("approvals = %+v, want one allow-once", log)
			}
			_ = bashCall(ctx, g, testCmd, testArgs)
			if a.n() != 2 {
				t.Errorf("approver asked %d times, want 2: nothing may have been remembered", a.n())
			}
		})
	}
}

// An ineligible call in auto is an ordinary ask prompt: no approver
// was involved, so the operator's session answer stands.
func TestAuto_IneligiblePromptKeepsSessionAnswers(t *testing.T) {
	t.Parallel()
	a := &stubApprover{verdict: allowVerdict}
	p := &fakePrompter{decision: DecisionAllowSession}
	g := autoGate(t, a, p, nil)
	ctx, _ := taskCtx()
	args := json.RawMessage(`{"command":"make"}`)
	for i := 0; i < 2; i++ {
		if err := bashCall(ctx, g, "make", args); err != nil {
			t.Fatal(err)
		}
	}
	if len(p.calls) != 1 || p.calls[0].ApproverModel != "" {
		t.Errorf("prompter calls = %+v, want one with no approver", p.calls)
	}
}

// Decision 10: every approver verdict is audited, and only those.
func TestAuto_AuditsEveryVerdict(t *testing.T) {
	t.Parallel()
	for _, o := range []VerdictOutcome{VerdictAllow, VerdictDeny, VerdictEscalate} {
		t.Run(o.String(), func(t *testing.T) {
			t.Parallel()
			v := Verdict{Outcome: o, Reason: "because", Model: "approver-1"}
			g := autoGate(t, &stubApprover{verdict: v}, &fakePrompter{decision: DecisionAllowOnce}, nil)
			ctx, ac := taskCtx()
			_ = bashCall(ctx, g, testCmd, testArgs)
			_ = bashCall(ctx, g, "make", json.RawMessage(`{}`)) // ineligible: no row
			audits := ac.all()
			want := ApproverAudit{ToolName: "bash", Detail: testCmd, Verdict: v}
			if len(audits) != 1 || audits[0] != want {
				t.Errorf("audits = %+v, want [%+v]", audits, want)
			}
		})
	}
}

// Decision 6: the approver sees the call, the operator's task and the
// turn's earlier calls — no results.
func TestAuto_ApproverRequestCarriesTaskAndCall(t *testing.T) {
	t.Parallel()
	a := &stubApprover{verdict: allowVerdict}
	g := autoGate(t, a, nil, nil)
	ac := &stubApproverContext{task: "fix the flaky test", recent: []ApproverCall{{ToolName: "read_file", Detail: "x_test.go"}}}
	ctx := WithApproverContext(context.Background(), ac)
	if err := bashCall(ctx, g, testCmd, testArgs); err != nil {
		t.Fatal(err)
	}
	got := a.calls[0]
	if got.Kind != PromptKindBash || got.ToolName != "bash" || got.Detail != testCmd || string(got.Args) != string(testArgs) ||
		got.Task != "fix the flaky test" || len(got.RecentCalls) != 1 || got.RecentCalls[0].Detail != "x_test.go" {
		t.Errorf("approver request = %+v", got)
	}
}

// Decision 12: auto needs an approval timeout, because an escalation
// on a daemon waits on a broker with nobody attached.
func TestAuto_RequiresApprovalTimeout(t *testing.T) {
	t.Parallel()
	g := New(Options{})
	if err := g.ValidateMode(ModeAuto); !errors.Is(err, ErrAutoNeedsApprovalTimeout) {
		t.Errorf("ValidateMode(auto) without a timeout = %v", err)
	}
	g.SetMode(ModeAuto)
	if prev := g.SwapMode(ModeAuto); prev != ModeAsk || g.Mode() != ModeAsk {
		t.Errorf("auto entered without a timeout: SwapMode returned %q, mode %q", prev, g.Mode())
	}
	if err := g.ValidateMode("bogus"); !errors.Is(err, ErrUnknownMode) {
		t.Errorf("ValidateMode(bogus) = %v, want ErrUnknownMode", err)
	}

	timed := New(Options{ApprovalTimeout: time.Minute})
	if err := timed.ValidateMode(ModeAuto); err != nil {
		t.Errorf("ValidateMode(auto) with a timeout = %v", err)
	}
	if prev := timed.SwapMode(ModeAuto); prev != ModeAsk || timed.Mode() != ModeAuto {
		t.Errorf("SwapMode(auto) with a timeout: prev %q mode %q", prev, timed.Mode())
	}

	cfg := defaultGateConfig(t)
	cfg.Permissions.Mode = string(ModeAuto)
	if _, err := FromConfig(cfg, t.TempDir(), t.TempDir(), nil); !errors.Is(err, ErrAutoNeedsApprovalTimeout) {
		t.Errorf("FromConfig(auto, no timeout) = %v", err)
	}
	cfg.Permissions.ApprovalTimeout = "5m"
	fc, err := FromConfig(cfg, t.TempDir(), t.TempDir(), nil)
	if err != nil || fc.Mode() != ModeAuto {
		t.Errorf("FromConfig(auto, 5m) = %v, mode %v", err, fc)
	}
}

// An escalation is bounded by approval_timeout like any ask prompt.
func TestAuto_EscalationExpires(t *testing.T) {
	t.Parallel()
	a := &stubApprover{verdict: Verdict{Outcome: VerdictEscalate, Reason: "unsure", Model: "m"}}
	g := autoGate(t, a, newBlockingPrompter(), func(o *Options) { o.ApprovalTimeout = 20 * time.Millisecond })
	ctx, _ := taskCtx()
	if err := bashCall(ctx, g, testCmd, testArgs); !errors.Is(err, ErrPromptExpired) {
		t.Fatalf("err = %v, want ErrPromptExpired", err)
	}
}

// Decision 14: a derived session gate inherits the approver, its
// eligible list and the never-list floor; the mode stays per session.
func TestAuto_DeriveForSessionInheritsApprover(t *testing.T) {
	t.Parallel()
	instructions := filepath.Join(t.TempDir(), "approver-policy.md")
	a := &stubApprover{verdict: allowVerdict}
	template := autoGate(t, a, nil, func(o *Options) {
		o.AutoEligible = mustPolicy(t, []string{"bash:*"}, nil)
		o.ApproverInstructionsFile = instructions
	})
	sub := template.DeriveForSession("s2", nil)
	ctx, _ := taskCtx()
	if err := bashCall(ctx, sub, testCmd, testArgs); err != nil || a.n() != 1 {
		t.Fatalf("eligible call on the derived gate: err %v, approver asked %d times", err, a.n())
	}
	if err := bashCall(ctx, sub, "cat "+instructions, json.RawMessage(`{}`)); !errors.Is(err, ErrNoPrompter) || a.n() != 1 {
		t.Errorf("instructions-file call on the derived gate: err %v, approver asked %d times; want ErrNoPrompter and no call", err, a.n())
	}
	sub.SetMode(ModeAsk)
	if template.Mode() != ModeAuto {
		t.Errorf("template mode = %q after the session changed its own", template.Mode())
	}
}

func TestAuto_ToolGateStateIsPrompted(t *testing.T) {
	t.Parallel()
	g := autoGate(t, &stubApprover{}, nil, nil)
	if got := g.ToolGateState("bash"); got != ToolGatePrompted {
		t.Errorf("ToolGateState(bash) in auto = %q, want %q", got, ToolGatePrompted)
	}
}

// A model's allow is not an operator's answer, so it never seeds an
// allowlist recommendation.
func TestRecommend_SkipsApproverAllows(t *testing.T) {
	t.Parallel()
	got := Recommend([]ApprovalLog{
		{Tool: "bash", Key: "go test ./...", Decision: DecisionAllowOnce, Approver: "approver-1"},
		{Tool: "bash", Key: "git status", Decision: DecisionAllowOnce},
	})
	if len(got) != 1 || got[0].Pattern != "bash:git status" {
		t.Errorf("Recommend = %+v, want only the operator's answer", got)
	}
}

func mustJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// Review finding: with no prompter, an escalated call is refused, and
// the identical retry must not go back to the approver — in ask mode a
// retry is free, in auto it is a paid model call. The repeat is
// counted for the #1081 cut and the next turn asks again.
func TestAuto_NoPersonRepeatIsNotReJudged(t *testing.T) {
	t.Parallel()
	a := &stubApprover{verdict: Verdict{Outcome: VerdictEscalate, Reason: "unsure", Model: "approver-1"}}
	g := autoGate(t, a, nil, nil)
	ctx, _ := taskCtx()

	if err := bashCall(ctx, g, testCmd, testArgs); !errors.Is(err, ErrNoPrompter) {
		t.Fatalf("first call = %v, want ErrNoPrompter", err)
	}
	err := bashCall(ctx, g, testCmd, testArgs)
	if err == nil || !strings.Contains(err.Error(), "needed a person's approval") {
		t.Fatalf("repeat = %v, want the no-person repeat refusal", err)
	}
	if a.n() != 1 {
		t.Errorf("approver asked %d times, want 1: the repeat must not be re-judged", a.n())
	}
	if got := g.TurnRefusalRepeats(ctx); got != 1 {
		t.Errorf("TurnRefusalRepeats = %d, want 1", got)
	}
	g.ObserveTurnStart(ctx)
	_ = bashCall(ctx, g, testCmd, testArgs)
	if a.n() != 2 {
		t.Errorf("approver asked %d times after a new turn, want 2", a.n())
	}
}

// Review finding: New must not build what SetMode refuses (decision 12).
func TestAuto_NewWithoutTimeoutBuildsAsk(t *testing.T) {
	t.Parallel()
	if got := New(Options{Mode: ModeAuto, Approver: &stubApprover{}}).Mode(); got != ModeAsk {
		t.Errorf("New(auto, no timeout).Mode() = %q, want ask", got)
	}
	if got := New(Options{Mode: ModeAuto, ApprovalTimeout: time.Minute}).Mode(); got != ModeAuto {
		t.Errorf("New(auto, timeout).Mode() = %q, want auto", got)
	}
}

type panicApprover struct{}

func (panicApprover) Judge(context.Context, ApproverRequest) (Verdict, error) {
	panic("boom")
}

// Review finding: a panicking approver — or a typed-nil one — escalates
// and is audited, rather than crashing the tool call.
func TestAuto_PanickingApproverEscalates(t *testing.T) {
	t.Parallel()
	var typedNil *stubApprover
	for name, a := range map[string]Approver{"panics": panicApprover{}, "typed nil": typedNil} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := &fakePrompter{decision: DecisionAllowOnce}
			g := autoGate(t, a, p, nil)
			ctx, ac := taskCtx()
			if err := bashCall(ctx, g, testCmd, testArgs); err != nil {
				t.Fatalf("call: %v", err)
			}
			if len(p.calls) != 1 || p.calls[0].ApproverReason != "" {
				t.Errorf("prompter calls = %+v, want one escalation with no reason", p.calls)
			}
			if au := ac.all(); len(au) != 1 || !strings.Contains(au[0].Err, "panicked") {
				t.Errorf("audits = %+v, want one row recording the panic", au)
			}
		})
	}
}

// Review finding: an empty or JSON-null Args is a call site holding no
// arguments, and escalates without an approver call.
func TestAuto_EmptyArgsNeverReachApprover(t *testing.T) {
	t.Parallel()
	for _, args := range []json.RawMessage{{}, json.RawMessage(" "), json.RawMessage("null"), json.RawMessage(" null\n")} {
		a := &stubApprover{verdict: allowVerdict}
		p := &fakePrompter{decision: DecisionAllowOnce}
		g := autoGate(t, a, p, nil)
		ctx, _ := taskCtx()
		if err := bashCall(ctx, g, testCmd, args); err != nil {
			t.Fatalf("args %q: %v", args, err)
		}
		if a.n() != 0 || len(p.calls) != 1 {
			t.Errorf("args %q: approver %d calls, prompter %d, want 0 and 1", args, a.n(), len(p.calls))
		}
	}
}

// denyingSibling stands in for an identical call issued alongside this
// one: while this call is being judged, the sibling's deny lands.
type denyingSibling struct{ g *Gate }

func (d *denyingSibling) Judge(_ context.Context, req ApproverRequest) (Verdict, error) {
	d.g.rememberTurnRefusal(req.ToolName, req.Detail, refusedByApprover)
	return allowVerdict, nil
}

// Review finding: decision 9 holds across calls issued together — an
// allow that returns after an identical call's deny is refused.
func TestAuto_ConcurrentDenyWinsOverAllow(t *testing.T) {
	t.Parallel()
	d := &denyingSibling{}
	g := autoGate(t, d, nil, nil)
	d.g = g
	ctx, _ := taskCtx()
	err := bashCall(ctx, g, testCmd, testArgs)
	if err == nil || !strings.Contains(err.Error(), "the approver model denied an identical request") {
		t.Fatalf("err = %v, want the approver repeat refusal", err)
	}
	if log := g.Approvals(); len(log) != 0 {
		t.Errorf("approvals = %+v, want none", log)
	}
}
