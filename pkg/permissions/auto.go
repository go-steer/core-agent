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

// Auto mode: an approver model decides routine prompts (#1175).
//
// ModeAuto is ModeAsk with one optional step in front of the prompt.
// A call that ask would have put to a person can go first to an
// Approver, which allows it once, denies it, or passes it on to the
// person. docs/auto-mode-design.md is the design of record; the
// decision numbers below refer to its "Settled decisions".
//
// Nothing here relies on the approver's judgement for safety. What
// bounds it is decided in code before it is ever called: deny patterns
// and plan-first have already run (gateRequest), and approverStep
// escalates without a call anything outside the recipe's eligible list
// (decision 3) or on the never-list (decision 4). Within that set, the
// approver only chooses which calls still need a person.

package permissions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"google.golang.org/genai"
)

// ModeAuto routes eligible prompts through the gate's Approver before
// a person sees them. See the package comment above and
// docs/auto-mode-design.md.
const ModeAuto Mode = "auto"

// ErrAutoNeedsApprovalTimeout rejects ModeAuto on a gate with no
// approval timeout. An escalation on a daemon waits on a broker even
// with nobody attached, so auto without a bound is the unanswered
// prompt it exists to remove (decision 12).
var ErrAutoNeedsApprovalTimeout = errors.New(`permission mode "auto" requires permissions.approval_timeout: an escalated call would otherwise wait for an answer forever`)

// ErrUnknownMode rejects a mode the gate does not implement.
var ErrUnknownMode = errors.New("unknown permission mode")

// VerdictOutcome is what the approver decided about one call.
//
// The zero value escalates, so a Verdict nobody filled in can never
// allow anything.
type VerdictOutcome int

const (
	VerdictEscalate VerdictOutcome = iota // pass the call to a person
	VerdictAllow                          // allow this call once
	VerdictDeny                           // refuse it for the rest of the turn
)

// String renders the outcome for audit rows.
func (o VerdictOutcome) String() string {
	switch o {
	case VerdictAllow:
		return "allow"
	case VerdictDeny:
		return "deny"
	default:
		return "escalate"
	}
}

// Verdict is an approver's answer about one call.
type Verdict struct {
	Outcome VerdictOutcome

	// Reason is the approver's explanation. Required for a deny — a
	// deny without one escalates — because it is what the model reads
	// in the tool error and what the operator reads on an escalation.
	// It is model output and is always quoted as such.
	Reason string

	// Model is the approver model's ID, recorded as ApprovalLog.Approver.
	Model string
}

// ApproverCall names one earlier call in the turn: the tool and its
// detail, never its result (decision 6).
type ApproverCall struct {
	ToolName string
	Detail   string
}

// ApproverRequest is everything an Approver sees about one call.
// ToolName, Detail and Args come from the model and are untrusted data;
// Task comes only from sources the operator controls.
type ApproverRequest struct {
	Kind     PromptKind
	ToolName string
	Detail   string
	Args     json.RawMessage

	// Task is the text of the operator's turns (ApproverContext.Task).
	Task string

	// RecentCalls are the turn's earlier calls, without results.
	RecentCalls []ApproverCall
}

// Approver judges eligible calls in ModeAuto. Implementations live
// outside pkg/permissions, which must not import pkg/models.
//
// EXPERIMENTAL: Approver, ApproverRequest, ApproverContext and Verdict
// are outside the v2 compatibility promise until ModeAuto is
// selectable (#1175). Later phases may add methods or change fields.
//
// An error escalates. Judge runs on the tool call's goroutine and may
// run concurrently for calls the model issued together.
type Approver interface {
	Judge(ctx context.Context, req ApproverRequest) (Verdict, error)
}

// ApproverAudit is one approver verdict, for the durable audit row
// (decision 10). Written for every call the approver was asked about,
// whatever it answered.
type ApproverAudit struct {
	ToolName string
	Detail   string
	Verdict  Verdict

	// Err is why the approver's answer was not used, or "" when it was.
	// A non-empty Err always comes with VerdictEscalate.
	Err string
}

// ApproverContext is what the agent stamps on a turn's context for the
// approver, next to WithSessionGate. A call whose context carries none
// escalates: without it there is no task to judge against.
type ApproverContext interface {
	// Task is the text of the turns a verified operator sent, or ""
	// when there is none (decision 6). "" escalates.
	Task() string

	// RecentCalls are the turn's earlier calls, without results.
	RecentCalls() []ApproverCall

	// Bill records the approver's own model usage against the turn
	// that triggered it (decision 8). Called by the Approver.
	Bill(model string, usage *genai.GenerateContentResponseUsageMetadata)

	// CeilingReached reports whether the turn or session cost ceiling
	// is already reached. True escalates without an approver call.
	CeilingReached() bool

	// Audit writes one approver verdict as a durable row.
	Audit(ApproverAudit)
}

type approverContextKey struct{}

// WithApproverContext returns ctx carrying ac for the gate's approver.
func WithApproverContext(ctx context.Context, ac ApproverContext) context.Context {
	if ac == nil {
		return ctx
	}
	return context.WithValue(ctx, approverContextKey{}, ac)
}

// ApproverContextFromContext returns the ApproverContext a prior
// WithApproverContext stamped on ctx.
func ApproverContextFromContext(ctx context.Context) (ApproverContext, bool) {
	ac, ok := ctx.Value(approverContextKey{}).(ApproverContext)
	return ac, ok && ac != nil
}

// controlPlaneMentions are the substrings that keep a call away from
// the approver however the recipe's eligible list reads (decision 4).
// The gate's own control-plane check only sees file-tool paths, so
// `sed -i … .agents/config.json` would otherwise reach the approver as
// an ordinary bash prompt.
var controlPlaneMentions = []string{
	controlPlaneDirName + "/config.json",
	controlPlaneDirName + "/mcp.json",
}

// protectedMentions builds the never-list substrings for a gate: the
// control-plane names plus every spelling of the approver's
// instructions file the gate can work out — as configured, absolute,
// symlink-resolved, and its base name. The base name over-matches
// (any file of that name anywhere), which errs toward asking a person.
//
// BEST-EFFORT, like the bash denylist: a command can name a file
// without spelling it (`cd .agents && sed -i … config.json`, a
// variable, a glob). A substring floor narrows what reaches the
// approver; it is not a boundary, and the approver's eligible list is
// what a recipe author should keep narrow.
//
// The match ignores letter case (`.Agents/Config.json` on a
// case-insensitive filesystem), which over-matches toward a person.
// Args are canonical JSON (CallArgs, #1175 phase 2), so a mention is
// matched in Args both as written and in the one spelling the encoder
// gives it (jsonSpelling) — no optional escape can hide it, and one the
// encoder cannot avoid, such as a quote or backslash in the
// instructions file's name, is matched as encoded.
func protectedMentions(instructionsFile string) []string {
	out := append([]string(nil), controlPlaneMentions...)
	if instructionsFile == "" {
		return out
	}
	add := func(s string) {
		if s != "" && s != "." && s != string(filepath.Separator) {
			out = append(out, s)
		}
	}
	add(filepath.Clean(instructionsFile))
	add(filepath.Base(instructionsFile))
	if abs, err := filepath.Abs(instructionsFile); err == nil {
		add(abs)
		if resolved, err := filepath.EvalSymlinks(abs); err == nil {
			add(resolved)
			add(filepath.Base(resolved))
		}
	}
	return out
}

// approverIneligible returns why req never reaches the approver, or ""
// when it may. Every reason is decided in code, before any model call,
// and each one escalates (decisions 3, 4 and 6).
func (g *Gate) approverIneligible(ctx context.Context, req PromptRequest) (string, ApproverContext) {
	if g.approver == nil {
		return "no approver is configured", nil
	}
	switch req.Kind {
	case PromptKindPathScope:
		return "access outside the declared path scope is never the approver's to grant", nil
	case PromptKindControlPlaneWrite:
		return "control-plane writes are never the approver's to grant", nil
	}
	if req.Source != "" {
		return "calls from background subagents are not judged by the approver", nil
	}
	if !hasArgs(req.Args) {
		return "the gate does not hold this call's full arguments", nil
	}
	if g.autoEligible == nil || g.autoEligible.Match(req.ToolName, req.Detail) != OutcomeAllow {
		return "the call is not on the approver's eligible list", nil
	}
	detail, args := strings.ToLower(req.Detail), strings.ToLower(string(req.Args))
	for _, m := range g.autoProtected {
		lm, le := strings.ToLower(m), strings.ToLower(jsonSpelling(m))
		if strings.Contains(detail, lm) || strings.Contains(args, lm) || strings.Contains(args, le) {
			return fmt.Sprintf("the call names %q, which only a person may approve", m), nil
		}
	}
	ac, ok := ApproverContextFromContext(ctx)
	if !ok {
		return "this turn carries no approver context", nil
	}
	if ac.Task() == "" {
		return "no operator-sent task is in context to judge the call against", nil
	}
	if ac.CeilingReached() {
		return "the cost ceiling is already reached", nil
	}
	return "", ac
}

// approverStep runs the approver for one prompt in ModeAuto. handled is
// true when the approver's verdict ended the request (err is then the
// call's result). Otherwise the request escalates to the existing
// prompter path, carrying the approver's reason when there was one.
func (g *Gate) approverStep(ctx context.Context, req *PromptRequest) (handled bool, err error) {
	why, ac := g.approverIneligible(ctx, *req)
	if why != "" {
		return false, nil
	}
	v, jerr := judge(ctx, g.approver, ac, req)
	audit := ApproverAudit{ToolName: req.ToolName, Detail: req.Detail, Verdict: v}
	switch {
	case jerr != nil:
		audit.Err = jerr.Error()
	case v.Outcome == VerdictDeny && strings.TrimSpace(v.Reason) == "":
		audit.Err = "deny without a reason"
	case v.Outcome != VerdictAllow && v.Outcome != VerdictDeny && v.Outcome != VerdictEscalate:
		audit.Err = fmt.Sprintf("unknown verdict %d", v.Outcome)
	}
	if audit.Err != "" {
		audit.Verdict.Outcome = VerdictEscalate
	}
	ac.Audit(audit)

	switch audit.Verdict.Outcome {
	case VerdictAllow:
		// An identical call issued alongside this one may have been
		// denied while the approver was deciding this one. Decision 9
		// makes that deny final for the turn, so it wins.
		if kind, refused := g.turnRefusal(req.ToolName, req.Detail); refused {
			return true, repeatRefusalError(kind, req.ToolName, req.Detail)
		}
		// Decision 5: once, and nothing remembered — no session, verb,
		// tool or always grant, no policy or GrantStore write.
		g.recordApproverAllow(req.ToolName, req.Detail, modelLabel(v.Model))
		return true, nil
	case VerdictDeny:
		// Decision 9: final for the turn. Neither the approver nor a
		// person is asked about this exact request again.
		g.rememberTurnRefusal(req.ToolName, req.Detail, refusedByApprover)
		return true, fmt.Errorf("%s denied by the approver model %s: %s. The approver's reason: %q. %s",
			req.ToolName, modelLabel(v.Model), req.Detail, v.Reason, denyGuidance)
	}
	// Decision 11: the prompt that follows says the approver passed it
	// on, and quotes its reason as model output. A reason from an
	// answer that was not used is not shown at all.
	req.ApproverModel = modelLabel(v.Model)
	if audit.Err == "" {
		req.ApproverReason = v.Reason
	}
	return false, nil
}

// hasArgs reports whether a call site actually supplied arguments. A
// nil, empty or JSON-null value is a call site that holds none.
func hasArgs(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) > 0 && !bytes.Equal(t, []byte("null"))
}

// judge calls the approver, turning a panic — including the nil
// dereference of a typed-nil Approver or ApproverContext — into an
// error, which escalates like any other.
func judge(ctx context.Context, a Approver, ac ApproverContext, req *PromptRequest) (v Verdict, err error) {
	defer func() {
		if r := recover(); r != nil {
			v, err = Verdict{}, fmt.Errorf("approver panicked: %v", r)
		}
	}()
	return a.Judge(ctx, ApproverRequest{
		Kind:        req.Kind,
		ToolName:    req.ToolName,
		Detail:      req.Detail,
		Args:        req.Args,
		Task:        ac.Task(),
		RecentCalls: ac.RecentCalls(),
	})
}

func modelLabel(model string) string {
	if model == "" {
		return "(unnamed)"
	}
	return model
}

// ValidateMode reports whether the gate can run in m: ErrUnknownMode
// for a mode it does not implement, ErrAutoNeedsApprovalTimeout for
// ModeAuto without an approval timeout. SetMode and SwapMode refuse
// what this refuses, so a host can call it first to report why.
func (g *Gate) ValidateMode(m Mode) error {
	switch m {
	case ModeAsk, ModeAllow, ModeYolo, ModePlan, ModeAcceptEdits:
		return nil
	case ModeAuto:
		if g.approvalTimeout <= 0 {
			return ErrAutoNeedsApprovalTimeout
		}
		return nil
	}
	return fmt.Errorf("%w %q", ErrUnknownMode, m)
}

// HasApprover reports whether an Approver is wired.
func (g *Gate) HasApprover() bool { return g.approver != nil }
