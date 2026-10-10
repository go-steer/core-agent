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

// The agent's half of permission mode "auto" (#1175,
// docs/auto-mode-design.md): what a turn tells the approver.
//
// The gate decides in code which calls may reach the approver, and it
// asks the turn's permissions.ApproverContext for four things: the
// operator's task, the turn's earlier calls, a place to bill the
// approver's own model call, and whether the money is already gone. It
// also hands each verdict back for the audit log. All of that is
// stamped on the turn context in Run, next to WithSessionGate, and only
// when the gate has an approver.
//
// The task is the part with a trust decision in it (decision 6). It is
// text an operator sent, and nothing else: the text a host hands Run
// through WithOperatorTask, and inbox messages whose caller
// authenticated directly as an identity in permissions.auto.task_from
// (attach.DirectCaller). Everything else that drives a turn — wake
// payloads relayed by a watcher, auto-continue, scheduler and peer
// traffic, a resume-with-message, subagent reports — is model- or
// machine-authored, and leaving it out costs an escalation, never a
// wrong allow. When a compaction or checkpoint summary replaces the
// history, the task is dropped with it, so the approver never judges
// against an instruction the model can no longer see.

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"

	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/pkg/permissions"
	"github.com/go-steer/core-agent/v2/pkg/usage"
)

const (
	// maxApproverTaskBytes bounds the task text. The oldest operator
	// messages go first: the newest is the instruction being worked.
	maxApproverTaskBytes = 16 << 10
	// maxApproverRecentCalls bounds the turn's earlier calls sent with
	// each judgement.
	maxApproverRecentCalls = 20
	// maxApproverCallDetail bounds one earlier call's rendered arguments.
	maxApproverCallDetail = 200
	// maxApproverAudits bounds the audit rows one turn queues. A turn
	// that judges more calls than this is far past any eligible list an
	// operator would write; the rows past it are dropped, not the calls.
	maxApproverAudits = 1000
	// approverAuditAuthor is the audit row's author, namespaced like
	// refusalStormAuthor.
	approverAuditAuthor = "gate/approver"
	// approverUsageAuthor authors the row that persists one approver
	// call's spend, which usage.RebuildTrackerFromEvents replays.
	approverUsageAuthor = "gate/approver-usage"
)

type operatorTaskKey struct{}

// WithOperatorTask tells the Run that ctx is passed to that text is
// something the operator wrote for this agent: the task the auto-mode
// approver judges calls against (#1175). Pass only words a person
// wrote — a typed message, the -p prompt, an autonomous run's goal
// (on every turn of it: the goal governs the whole run). Never pass
// text the host composed (a continuation, a scheduled reminder,
// relayed event content); a call the approver has no task for
// escalates to a person rather than being judged against the wrong
// thing.
func WithOperatorTask(ctx context.Context, text string) context.Context {
	return context.WithValue(ctx, operatorTaskKey{}, text)
}

func operatorTaskFrom(ctx context.Context) string {
	text, _ := ctx.Value(operatorTaskKey{}).(string)
	return text
}

// captureApproverTask adds this turn's operator-sent text to the task.
// A pre-turn step: it reads the host's WithOperatorTask text and the
// drained inbox with each message's directly verified caller.
func (a *Agent) captureApproverTask(tp *turnPrep) {
	if a.gate == nil || !a.gate.HasApprover() {
		return
	}
	texts := []string{operatorTaskFrom(tp.ctx)}
	for i := range tp.drained.texts {
		if i < len(tp.drained.taskCallers) && i < len(tp.drained.taskTexts) &&
			a.gate.ApproverTaskSource(tp.drained.taskCallers[i]) {
			// The words the operator wrote, not the whole message: a
			// client may have added text of its own (#1230).
			texts = append(texts, tp.drained.taskTexts[i])
		}
	}
	a.recordApproverTask(texts...)
}

// recordApproverTask appends non-blank texts — a text already held
// moves to the end rather than repeating, so an autonomous goal sent
// every turn appears once — then drops the oldest until the whole is
// within maxApproverTaskBytes. A single text over the bound is kept
// whole: cutting it would hand the approver part of an instruction.
func (a *Agent) recordApproverTask(texts ...string) {
	a.approverTaskMu.Lock()
	defer a.approverTaskMu.Unlock()
	for _, t := range texts {
		if strings.TrimSpace(t) == "" {
			continue
		}
		a.approverTask = slices.DeleteFunc(a.approverTask, func(held string) bool { return held == t })
		a.approverTask = append(a.approverTask, t)
	}
	total := 0
	for _, t := range a.approverTask {
		total += len(t)
	}
	for len(a.approverTask) > 1 && total > maxApproverTaskBytes {
		total -= len(a.approverTask[0])
		a.approverTask = a.approverTask[1:]
	}
}

// ApproverTask is the operator-sent text the auto-mode approver judges
// this session's calls against, or "" when there is none (and every
// call it would judge escalates). Read-only: it is for showing an
// operator what the approver sees. Always "" without an approver.
// Nil-safe.
func (a *Agent) ApproverTask() string {
	if a == nil {
		return ""
	}
	return a.approverTaskText()
}

// approverTaskText is the task as the approver reads it, or "".
func (a *Agent) approverTaskText() string {
	a.approverTaskMu.Lock()
	defer a.approverTaskMu.Unlock()
	return strings.Join(a.approverTask, "\n\n")
}

// clearApproverTask drops the task. Called when a summary replaces the
// history the operator's messages were in.
func (a *Agent) clearApproverTask() {
	a.approverTaskMu.Lock()
	a.approverTask = nil
	a.approverTaskMu.Unlock()
}

// turnApprover is one Run's permissions.ApproverContext. The gate calls
// it from tool goroutines while Run's tap feeds it events, so every
// field behind mu.
type turnApprover struct {
	a *Agent

	mu      sync.Mutex
	pending map[string]permissions.ApproverCall // calls without a result yet, by call ID
	recent  []permissions.ApproverCall          // calls with a result, oldest first
	audits  []permissions.ApproverAudit
	bills   []approverBill
}

// approverBill is one approver call's usage, queued for its row.
type approverBill struct {
	model string
	usage usage.TurnUsage
}

var _ permissions.ApproverContext = (*turnApprover)(nil)

func newTurnApprover(a *Agent) *turnApprover {
	return &turnApprover{a: a, pending: make(map[string]permissions.ApproverCall)}
}

// Task implements permissions.ApproverContext.
func (t *turnApprover) Task() string { return t.a.approverTaskText() }

// RecentCalls implements permissions.ApproverContext: the turn's calls
// that already have a result, without the result. A call still running
// — including the one being judged and its parallel siblings — is not
// an earlier call yet.
func (t *turnApprover) RecentCalls() []permissions.ApproverCall {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]permissions.ApproverCall(nil), t.recent...)
}

// Bill implements permissions.ApproverContext. The approver's call is a
// side call: it counts toward the session's spend and the ceilings, and
// is not a measurement of the conversation.
func (t *turnApprover) Bill(model string, meta *genai.GenerateContentResponseUsageMetadata) {
	if meta == nil {
		return
	}
	u := usage.TurnUsageFromMetadata(meta, nil)
	if t.a.tracker != nil {
		t.a.tracker.AppendSideUsage(model, u, usage.PriceFor(model, nil))
	}
	// And durably, so a restart does not hand the cost ceilings back
	// the money the approver spent (#643).
	t.mu.Lock()
	if len(t.bills) < maxApproverAudits {
		t.bills = append(t.bills, approverBill{model: model, usage: u})
	}
	t.mu.Unlock()
}

// CeilingReached implements permissions.ApproverContext.
func (t *turnApprover) CeilingReached() bool { return t.a.costCeilingReached() }

// Audit implements permissions.ApproverContext. Rows are queued and
// written by drainAudits once the turn's stream has drained: an
// eventlog write while the runner holds its session handle is the race
// #565 closed.
func (t *turnApprover) Audit(au permissions.ApproverAudit) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.audits) < maxApproverAudits {
		t.audits = append(t.audits, au)
	}
}

// observe records the event's tool calls and results. A call moves to
// recent when its result arrives.
func (t *turnApprover) observe(ev *session.Event) {
	if t == nil || ev == nil || ev.Partial || ev.Content == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, p := range ev.Content.Parts {
		switch {
		case p == nil:
		case p.FunctionCall != nil:
			fc := p.FunctionCall
			t.pending[fc.ID] = permissions.ApproverCall{ToolName: fc.Name, Detail: renderCallArgs(fc.Args)}
		case p.FunctionResponse != nil:
			call, ok := t.pending[p.FunctionResponse.ID]
			if !ok {
				continue
			}
			delete(t.pending, p.FunctionResponse.ID)
			t.recent = append(t.recent, call)
			if len(t.recent) > maxApproverRecentCalls {
				t.recent = t.recent[len(t.recent)-maxApproverRecentCalls:]
			}
		}
	}
}

// renderCallArgs is a call's arguments as compact JSON, cut to
// maxApproverCallDetail bytes on a rune boundary.
func renderCallArgs(args map[string]any) string {
	b, err := json.Marshal(args)
	if err != nil {
		return ""
	}
	s := string(b)
	if len(s) <= maxApproverCallDetail {
		return s
	}
	cut := maxApproverCallDetail
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}

// drainAudits writes the turn's queued verdicts as eventlog rows, one
// per verdict, and one row per approver call's usage for the tracker
// to replay after a restart. No row has content: a text part authored here would enter
// the model's history as a message nobody sent. Best-effort, like the
// refusal-storm row — the verdict already took effect, and a lost row
// costs an explanation, not a protection. Nil-safe.
func (t *turnApprover) drainAudits() {
	if t == nil {
		return
	}
	t.mu.Lock()
	audits, bills := t.audits, t.bills
	t.audits, t.bills = nil, nil
	t.mu.Unlock()
	a := t.a
	if (len(audits) == 0 && len(bills) == 0) || a.eventLog == nil {
		return
	}
	getResp, err := a.eventLog.Service.Get(context.Background(), &session.GetRequest{
		AppName:   a.appName,
		UserID:    a.userID,
		SessionID: a.sessionID,
	})
	if err != nil {
		return
	}
	for _, au := range audits {
		ev := session.NewEvent(context.Background(), "gate-approver")
		ev.Author = approverAuditAuthor
		meta := map[string]any{
			"source":  "approver",
			"tool":    au.ToolName,
			"detail":  au.Detail,
			"verdict": au.Verdict.Outcome.String(),
			"reason":  au.Verdict.Reason,
			"model":   au.Verdict.Model,
		}
		if au.Err != "" {
			meta["error"] = au.Err
		}
		ev.CustomMetadata = meta
		_ = a.eventLog.Service.AppendEvent(context.Background(), getResp.Session, ev)
	}
	for _, b := range bills {
		ev := session.NewEvent(context.Background(), "gate-approver-usage")
		ev.Author = approverUsageAuthor
		ev.CustomMetadata = usage.SideUsageMetadata(b.model, b.usage)
		_ = a.eventLog.Service.AppendEvent(context.Background(), getResp.Session, ev)
	}
}
