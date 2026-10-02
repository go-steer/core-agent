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

package agent

import (
	"context"
	"iter"
	"strings"
	"sync"
	"testing"
	"time"

	adkagent "google.golang.org/adk/agent"
	adkmodel "google.golang.org/adk/model"
	"google.golang.org/adk/session"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/pkg/permissions"
	"github.com/go-steer/core-agent/v2/pkg/usage"
)

// #1175 phase 3b: what a turn tells the auto-mode approver. These drive
// the real Run loop with a real ModeAuto gate and a scripted approver,
// so a call reaches the approver through the same path a live one does.

const (
	approverProbeTool  = "run_cmd"
	approverJudgeModel = "judge-model"
	approverTaskUser   = "alice@example.com"
)

// scriptedApprover allows every call, records what it was asked, and
// bills a fixed usage through the context, as pkg/approver does.
type scriptedApprover struct {
	mu   sync.Mutex
	reqs []permissions.ApproverRequest
}

func (s *scriptedApprover) Judge(ctx context.Context, req permissions.ApproverRequest) (permissions.Verdict, error) {
	s.mu.Lock()
	s.reqs = append(s.reqs, req)
	s.mu.Unlock()
	if ac, ok := permissions.ApproverContextFromContext(ctx); ok {
		ac.Bill(approverJudgeModel, &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 900, CandidatesTokenCount: 30})
	}
	return permissions.Verdict{Outcome: permissions.VerdictAllow, Reason: "routine", Model: approverJudgeModel}, nil
}

func (s *scriptedApprover) requests() []permissions.ApproverRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]permissions.ApproverRequest(nil), s.reqs...)
}

// commandScriptLLM issues one run_cmd call per command, in order, then
// answers with text. reset re-arms it for another turn.
type commandScriptLLM struct {
	mu       sync.Mutex
	commands []string
	n        int
}

func (*commandScriptLLM) Name() string { return "main-model" }

func (l *commandScriptLLM) reset() {
	l.mu.Lock()
	l.n = 0
	l.mu.Unlock()
}

func (l *commandScriptLLM) GenerateContent(context.Context, *adkmodel.LLMRequest, bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		l.mu.Lock()
		i := l.n
		l.n++
		l.mu.Unlock()
		if i >= len(l.commands) {
			yield(&adkmodel.LLMResponse{
				Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "done"}}},
				TurnComplete: true,
				UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
					PromptTokenCount: 5000, CandidatesTokenCount: 10,
				},
			}, nil)
			return
		}
		yield(&adkmodel.LLMResponse{
			Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{
				FunctionCall: &genai.FunctionCall{
					ID:   "call_" + string(rune('a'+i)),
					Name: approverProbeTool,
					Args: map[string]any{"command": l.commands[i]},
				},
			}}},
			TurnComplete: true,
		}, nil)
	}
}

// approverRig is an agent whose only tool asks a ModeAuto gate with the
// call's arguments, as a real built-in does. toolSaw records, per call,
// whether the tool's context still carried the operator task mark.
type approverRig struct {
	a        *Agent
	llm      *commandScriptLLM
	approver *scriptedApprover
	tracker  *usage.Tracker

	mu      sync.Mutex
	toolSaw []string
}

func newApproverRig(t *testing.T, withApprover bool, commands ...string) *approverRig {
	t.Helper()
	r := &approverRig{
		llm:      &commandScriptLLM{commands: commands},
		approver: &scriptedApprover{},
		tracker:  usage.NewTracker(),
	}
	pol, err := permissions.NewPolicy([]string{"bash:go *"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	opts := permissions.Options{
		Mode:             permissions.ModeAuto,
		AutoEligible:     pol,
		ApprovalTimeout:  time.Minute,
		ApproverTaskFrom: []string{approverTaskUser},
	}
	if withApprover {
		opts.Approver = r.approver
	}
	g := permissions.New(opts)
	type args struct {
		Command string `json:"command"`
	}
	type empty struct{}
	tl, err := functiontool.New(
		functiontool.Config{Name: approverProbeTool, Description: "run a command"},
		func(ctx adkagent.ToolContext, in args) (empty, error) {
			r.mu.Lock()
			r.toolSaw = append(r.toolSaw, operatorTaskFrom(ctx))
			r.mu.Unlock()
			return empty{}, g.CheckBashWithArgs(ctx, in.Command, in)
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	h, cleanup := openTestEventLog(t)
	t.Cleanup(cleanup)
	a, err := New(r.llm,
		WithSession("u-1175", "s-"+strings.ReplaceAll(t.Name(), "/", "-")),
		WithGate(g),
		WithTools([]tool.Tool{tl}),
		WithUsageTracker(r.tracker),
		WithEventLog(h),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Service.Create(context.Background(), &session.CreateRequest{
		AppName: a.appName, UserID: a.userID, SessionID: a.sessionID,
	}); err != nil {
		t.Fatal(err)
	}
	r.a = a
	return r
}

func (r *approverRig) run(t *testing.T, ctx context.Context, prompt string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	for _, err := range r.a.Run(ctx, prompt) {
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	}
}

func (r *approverRig) auditRows(t *testing.T) []*session.Event {
	t.Helper()
	resp, err := r.a.eventLog.Service.Get(context.Background(), &session.GetRequest{
		AppName: r.a.appName, UserID: r.a.userID, SessionID: r.a.sessionID,
	})
	if err != nil {
		t.Fatal(err)
	}
	var rows []*session.Event
	for ev := range resp.Session.Events().All() {
		if ev.Author == approverAuditAuthor {
			rows = append(rows, ev)
		}
	}
	return rows
}

// The whole path: the operator's words are the task, each judgement
// carries the calls that already finished, the approver's spend lands
// in the session's totals without becoming the conversation's last
// turn, and every verdict is a durable row.
func TestRun_ApproverGetsTaskEarlierCallsBillAndAudit(t *testing.T) {
	t.Parallel()
	r := newApproverRig(t, true, "go build ./...", "go test ./...")
	const task = "make the build green"
	r.run(t, WithOperatorTask(context.Background(), task), task)

	reqs := r.approver.requests()
	if len(reqs) != 2 {
		t.Fatalf("approver asked %d times, want 2: %+v", len(reqs), reqs)
	}
	if reqs[0].Task != task || reqs[1].Task != task {
		t.Errorf("tasks = %q, %q; want the operator's %q both times", reqs[0].Task, reqs[1].Task, task)
	}
	if len(reqs[0].RecentCalls) != 0 {
		t.Errorf("first judgement's earlier calls = %+v, want none: the call being judged is not an earlier call", reqs[0].RecentCalls)
	}
	want := permissions.ApproverCall{ToolName: approverProbeTool, Detail: `{"command":"go build ./..."}`}
	if len(reqs[1].RecentCalls) != 1 || reqs[1].RecentCalls[0] != want {
		t.Errorf("second judgement's earlier calls = %+v, want [%+v]", reqs[1].RecentCalls, want)
	}

	if by := r.tracker.TotalsByModel()[approverJudgeModel]; by.Turns != 2 || by.InputTokens != 1800 {
		t.Errorf("approver spend in the tracker = %+v, want two 900-token calls", by)
	}
	if last, _ := r.tracker.Last(); last.Model == approverJudgeModel {
		t.Errorf("the tracker's last turn is the approver's (%+v); it must stay the conversation's", last)
	}

	rows := r.auditRows(t)
	if len(rows) != 2 {
		t.Fatalf("audit rows = %d, want one per verdict (2)", len(rows))
	}
	for _, row := range rows {
		m := row.CustomMetadata
		if m["verdict"] != "allow" || m["model"] != approverJudgeModel || m["tool"] != "bash" || m["reason"] != "routine" {
			t.Errorf("audit row metadata = %v", m)
		}
		if row.Content != nil {
			t.Errorf("audit row carries content %+v; it would enter the model's history", row.Content)
		}
	}

	// A restart rebuilds the tracker from the eventlog, and the
	// approver's spend must come back with it: a pod roll must not hand
	// the cost ceilings a fresh budget (#643).
	resp, err := r.a.eventLog.Service.Get(context.Background(), &session.GetRequest{
		AppName: r.a.appName, UserID: r.a.userID, SessionID: r.a.sessionID,
	})
	if err != nil {
		t.Fatal(err)
	}
	rebuilt := usage.NewTracker()
	events := func(yield func(*session.Event, error) bool) {
		for ev := range resp.Session.Events().All() {
			if !yield(ev, nil) {
				return
			}
		}
	}
	if err := usage.RebuildTrackerFromEvents(context.Background(), rebuilt, events, "main-model", nil); err != nil {
		t.Fatal(err)
	}
	if by := rebuilt.TotalsByModel()[approverJudgeModel]; by.Turns != 2 || by.InputTokens != 1800 {
		t.Errorf("approver spend after a rebuild = %+v, want both calls back", by)
	}

	for i, saw := range r.toolSaw {
		if saw != "" {
			t.Errorf("tool call %d inherited the operator task mark %q; nested work must not", i, saw)
		}
	}
}

// A prompt the host did not mark is not the task, so nothing reaches
// the approver: with no task it escalates, and this gate has no one to
// escalate to.
func TestRun_UnmarkedPromptIsNotTheTask(t *testing.T) {
	t.Parallel()
	r := newApproverRig(t, true, "go build ./...")
	r.run(t, context.Background(), "make the build green")
	if n := len(r.approver.requests()); n != 0 {
		t.Errorf("approver asked %d times about a turn with no operator task, want 0", n)
	}
}

// An inbox message is the task only when its caller authenticated
// directly as an identity on task_from. A relayed or anonymous message,
// or one from an unlisted identity, adds nothing — and a later
// operator message adds to what is there.
func TestRun_InboxTaskNeedsADirectListedCaller(t *testing.T) {
	t.Parallel()
	r := newApproverRig(t, true, "go build ./...")
	for _, m := range []inboxMessage{
		{text: "from alice", taskCaller: approverTaskUser},
		{text: "from bob", taskCaller: "bob@example.com"},
		{text: "relayed or anonymous"},
	} {
		if _, err := r.a.inbox.enqueue(m); err != nil {
			t.Fatal(err)
		}
	}
	r.run(t, context.Background(), "")
	reqs := r.approver.requests()
	if len(reqs) != 1 || reqs[0].Task != "from alice" {
		t.Fatalf("approver requests = %+v, want one with alice's message alone as the task", reqs)
	}

	r.llm.reset()
	r.run(t, WithOperatorTask(context.Background(), "and then run the tests"), "and then run the tests")
	reqs = r.approver.requests()
	if got := reqs[len(reqs)-1].Task; got != "from alice\n\nand then run the tests" {
		t.Errorf("second turn's task = %q, want both operator messages, oldest first", got)
	}
}

// Without an approver the turn carries no approver context and keeps no
// task: nothing would read either.
func TestRun_NoApproverStampsNothing(t *testing.T) {
	t.Parallel()
	r := newApproverRig(t, false, "go build ./...")
	r.run(t, WithOperatorTask(context.Background(), "make the build green"), "make the build green")
	if got := r.a.approverTaskText(); got != "" {
		t.Errorf("task kept with no approver wired: %q", got)
	}
	if rows := r.auditRows(t); len(rows) != 0 {
		t.Errorf("audit rows with no approver: %d", len(rows))
	}
}

// A compaction summary becomes where the model's history starts, so the
// operator messages behind it stop being the task.
func TestCompact_ClearsTheApproverTask(t *testing.T) {
	t.Parallel()
	llm := &captureLLM{response: "SUMMARY"}
	a, err := New(llm, WithCompactor(NewDefaultCompactor()))
	if err != nil {
		t.Fatal(err)
	}
	a.recordApproverTask("delete the old namespaces")
	plantEvent(t, a, genai.RoleUser, "delete the old namespaces")
	plantEvent(t, a, genai.RoleModel, "on it")
	if _, err := a.Compact(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if got := a.approverTaskText(); got != "" {
		t.Errorf("task after compaction = %q, want none", got)
	}
}

// The mechanical fallback (#974) writes a boundary without a
// summarizer call, and drops the history just the same.
func TestMechanicalCompact_ClearsTheApproverTask(t *testing.T) {
	t.Parallel()
	a, err := New(&captureLLM{response: "unused"}, WithCompactor(NewDefaultCompactor()))
	if err != nil {
		t.Fatal(err)
	}
	a.recordApproverTask("delete the old namespaces")
	plantEvent(t, a, genai.RoleUser, "delete the old namespaces")
	plantEvent(t, a, genai.RoleModel, "on it")
	if _, err := a.mechanicalCompact(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := a.approverTaskText(); got != "" {
		t.Errorf("task after mechanical compaction = %q, want none", got)
	}
}

func TestRecordApproverTask_DedupesAndBounds(t *testing.T) {
	t.Parallel()
	a := &Agent{}
	a.recordApproverTask("goal", "  ", "fix it", "goal")
	if got := a.approverTaskText(); got != "fix it\n\ngoal" {
		t.Errorf("task = %q, want a repeated text moved to the end and blanks dropped", got)
	}
	big := strings.Repeat("x", maxApproverTaskBytes)
	a.recordApproverTask(big)
	if got := a.approverTaskText(); got != big {
		t.Errorf("task is %d bytes, want only the newest text once the bound is passed", len(got))
	}
	huge := strings.Repeat("y", maxApproverTaskBytes+1)
	a.recordApproverTask(huge)
	if got := a.approverTaskText(); got != huge {
		t.Errorf("a single over-bound text was cut or dropped (%d bytes)", len(got))
	}
}

func TestRenderCallArgs_CutsOnARuneBoundary(t *testing.T) {
	t.Parallel()
	if got := renderCallArgs(map[string]any{"path": "a.go"}); got != `{"path":"a.go"}` {
		t.Errorf("short args = %q", got)
	}
	got := renderCallArgs(map[string]any{"text": strings.Repeat("é", maxApproverCallDetail)})
	if !strings.HasSuffix(got, "...") || len(got) > maxApproverCallDetail+3 || !strings.HasPrefix(got, `{"text":"é`) {
		t.Errorf("long args = %q", got)
	}
	if body := strings.TrimSuffix(got, "..."); !utf8Valid(body) {
		t.Errorf("cut mid-rune: %q", body)
	}
}

func utf8Valid(s string) bool { return strings.ToValidUTF8(s, "�") == s }

func TestCostCeilingReached(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		ceiling   CostCeiling
		spend     float64
		turnStart float64
		exceeded  bool
		tripped   bool
		want      bool
	}{
		{"no ceiling", CostCeiling{}, 100, 0, false, false, false},
		{"under both", CostCeiling{MaxTurnUSD: 5, MaxSessionUSD: 50}, 10, 8, false, false, false},
		{"session met", CostCeiling{MaxSessionUSD: 10}, 10, 0, false, false, true},
		{"turn met", CostCeiling{MaxTurnUSD: 2}, 10, 8, false, false, true},
		{"session halted", CostCeiling{MaxSessionUSD: 50}, 1, 0, true, false, true},
		{"turn already tripped", CostCeiling{MaxTurnUSD: 50}, 1, 0, false, true, true},
	}
	for _, tc := range cases {
		tr := usage.NewTracker()
		tr.Append("m", 1_000_000, 0, usage.Pricing{InputPerMTok: tc.spend})
		a := &Agent{tracker: tr, costCeiling: tc.ceiling, costCeilingExceeded: tc.exceeded, turnCeilingTripped: tc.tripped,
			turnStartCost: tc.turnStart, turnStartCostSet: true}
		if got := a.costCeilingReached(); got != tc.want {
			t.Errorf("%s: costCeilingReached() = %v, want %v", tc.name, got, tc.want)
		}
	}
	if (&Agent{}).costCeilingReached() {
		t.Error("an agent with no tracker reports a reached ceiling")
	}
}
