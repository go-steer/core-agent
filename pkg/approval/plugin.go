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

package approval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/plugin"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/toolconfirmation"

	"github.com/go-steer/core-agent/v3/pkg/permissions"
)

// PluginName is the registered name of the write gate's runner plugin.
const PluginName = "mast-write-gate"

// OnMutation is a workload's policy for what happens when an agent
// calls a mutating tool (hitl_policy.on_mutation).
type OnMutation string

const (
	// OnMutationRequireApproval parks the call and waits for an
	// operator.
	OnMutationRequireApproval OnMutation = "require_approval"

	// OnMutationApply executes mutating calls without asking. Policy
	// still applies — a configured deny still denies — but no human is
	// consulted.
	OnMutationApply OnMutation = "apply"

	// OnMutationDryRun never executes a mutating call and reports the
	// call it would have made.
	OnMutationDryRun OnMutation = "dry_run"
)

// Valid reports whether p is a policy this package implements.
func (p OnMutation) Valid() bool {
	switch p {
	case OnMutationRequireApproval, OnMutationApply, OnMutationDryRun:
		return true
	}
	return false
}

// DecisionSink persists a Decision record durably before BeforeToolCallback
// returns nil to execute the tool (mast #561 fix). Even if the process
// crashes mid-tool before the FunctionResponse event and its StateDelta
// are appended, the approver attribution is durably recorded.
type DecisionSink func(ctx context.Context, d Decision) error

// DecisionLookup reports whether a Decision record for functionCallID in
// sessionID has already been durably persisted in the external DecisionStore
// (e.g. before a mid-tool crash across process restart where session.State
// never received the post-tool StateDelta).
type DecisionLookup func(ctx context.Context, sessionID, functionCallID string) (bool, error)

// Option configures a write-gate plugin at construction time.
type Option func(*Config)

// WithDecisionSink attaches a pre-execution durable sink for Decision
// records (#1400, mast #561).
func WithDecisionSink(sink DecisionSink) Option {
	return func(cfg *Config) {
		cfg.DecisionSink = sink
	}
}

// WithDecisionLookup attaches a durable lookup for already-adjudicated
// FunctionCallIDs across process restarts (#1400, mast #561).
func WithDecisionLookup(lookup DecisionLookup) Option {
	return func(cfg *Config) {
		cfg.DecisionLookup = lookup
	}
}

// Config wires the write gate.
type Config struct {
	// Policy is the workload's hitl_policy.on_mutation. Required.
	Policy OnMutation

	// Mutating classifies a tool by name. Required.
	Mutating func(toolName string) bool

	// Gate adjudicates policy for a parked call and validates the
	// operator's verdict. Required when Policy is
	// OnMutationRequireApproval; unused otherwise.
	Gate *permissions.Gate

	// ChangeSet, when non-nil, enforces the change-set producer
	// contract (W7.0).
	ChangeSet *ChangeSetChecker

	// Grants, when non-nil, lets one operator answer authorize a whole
	// change set (W7).
	Grants *Freshness

	// Captures, when non-nil, records what a mutating call is about to
	// overwrite before it fires, and the call that puts it back (#296).
	Captures *CaptureRules

	// ForgetToolRun, when non-nil, is called with a session ID when the
	// gate turns away a call an operator already refused in this turn.
	ForgetToolRun func(sessionID string)

	// NotifyPark, when non-nil, is told out of band that a call has been
	// parked (#451).
	NotifyPark func(context.Context, ParkNotice)

	// DecisionSink, when non-nil, persists each Decision record before
	// BeforeToolCallback returns to execute the tool (mast #561).
	DecisionSink DecisionSink

	// DecisionLookup, when non-nil, checks whether a FunctionCallID has
	// already been durably adjudicated in the external DecisionStore
	// across process restarts (mast #561).
	DecisionLookup DecisionLookup

	// Workload names the workload whose bundle composed this gate, and
	// is stamped onto every Decision record. Optional.
	Workload string

	// Logger receives the audit trail. Defaults to slog.Default().
	Logger *slog.Logger
}

// NewPlugin is an alias for New.
func NewPlugin(cfg Config, opts ...Option) (*plugin.Plugin, error) {
	return New(cfg, opts...)
}

// New builds the write gate as an ADK runner plugin.
//
// Register it AFTER the internal/effects outbox plugin at every runner
// construction site.
func New(cfg Config, opts ...Option) (*plugin.Plugin, error) {
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	g, err := newWriteGate(cfg)
	if err != nil {
		return nil, err
	}
	return plugin.New(plugin.Config{
		Name:               PluginName,
		BeforeToolCallback: g.beforeTool,
		AfterRunCallback:   func(ictx agent.InvocationContext) { g.refused.forget(ictx.InvocationID()) },
	})
}

func newWriteGate(cfg Config) (*writeGate, error) {
	if !cfg.Policy.Valid() {
		return nil, fmt.Errorf("approval: Config.Policy %q is not one of %s, %s, %s", cfg.Policy,
			OnMutationRequireApproval, OnMutationApply, OnMutationDryRun)
	}
	if cfg.Mutating == nil {
		return nil, fmt.Errorf("approval: Config.Mutating is required")
	}
	if cfg.Policy == OnMutationRequireApproval && cfg.Gate == nil {
		return nil, fmt.Errorf("approval: Config.Gate is required under %s: without a gate there is nothing to decide policy, and a write gate that cannot refuse is not a gate", OnMutationRequireApproval)
	}
	g := &writeGate{cfg: cfg, refused: newRefusals()}
	if g.cfg.Logger == nil {
		g.cfg.Logger = slog.Default()
	}
	return g, nil
}

type writeGate struct {
	cfg     Config
	refused *refusals
	// dispatched guards against an approved FunctionCallID being
	// dispatched a second time without a fresh confirmation cycle
	// (mast #561).
	dispatched sync.Map
}

func (g *writeGate) beforeTool(ctx agent.Context, t tool.Tool, args map[string]any) (map[string]any, error) {
	if refusal := g.checkChangeSet(ctx, t, args); refusal != nil {
		return refusal, nil
	}
	if !g.cfg.Mutating(t.Name()) {
		return nil, nil
	}
	key := CallKey(t.Name(), args)

	if c := ctx.ToolConfirmation(); c != nil {
		return g.honorVerdict(ctx, t, key, args, c)
	}

	switch g.cfg.Policy {
	case OnMutationApply:
		if refusal := g.permit(ctx, t, key, args); refusal != nil {
			return refusal, nil
		}
		g.audit(ctx, t, key, "apply", "policy is apply; executing without operator approval")
		return nil, nil

	case OnMutationDryRun:
		g.audit(ctx, t, key, "dry_run", "policy is dry_run; reporting the call instead of making it")
		return map[string]any{
			"status": "dry_run",
			"tool":   t.Name(),
			"args":   args,
			"detail": "This workload runs with hitl_policy.on_mutation=dry_run. The call above was NOT made and nothing changed. " +
				"Treat it as a proposal: report what you would have done and continue with read-only work. Do not retry.",
		}, nil
	}

	err := g.cfg.Gate.CheckMutatingToolCall(ctx, t.Name(), key)
	if !errors.Is(err, permissions.ErrApprovalRequired) {
		g.audit(ctx, t, key, "denied_by_policy", err.Error())
		return map[string]any{
			"error": "denied_by_policy",
			"detail": "This call is refused by the operator's configured permission policy, not by a person who might reconsider: " + err.Error() +
				" Do not retry this call and do not attempt the same change by another route. Report the refusal.",
		}, nil
	}

	if suppressed, endTurn, count := g.refused.suppress(ctx.InvocationID(), key); suppressed {
		return g.refuseRepeat(ctx, t, key, count, endTurn)
	}

	grant, stale := g.checkGrant(ctx, t, args)
	if grant != nil {
		return g.spendGrant(ctx, t, key, args, grant)
	}
	if stale != "" {
		g.voidGrant(ctx, t, args, stale)
		g.audit(ctx, t, key, "grant_stale", stale)
	}

	set := g.changeSetContextFor(ctx, t, args)
	hint := parkHint(key, set, stale)
	if err := ctx.RequestConfirmation(hint, Request{
		Tool:      t.Name(),
		Args:      args,
		Key:       key,
		Policy:    string(g.cfg.Policy),
		Agent:     ctx.AgentName(),
		Verdict:   verdictHelp,
		ChangeSet: set,
		Stale:     stale,
	}); err != nil {
		return nil, fmt.Errorf("approval: requesting confirmation for %s: %w", t.Name(), err)
	}
	g.audit(ctx, t, key, "awaiting_approval", "parked for operator approval")

	g.announcePark(ctx, ParkNotice{
		Session:    ctx.SessionID(),
		Invocation: ctx.InvocationID(),
		Workload:   g.cfg.Workload,
		Agent:      ctx.AgentName(),
		Tool:       t.Name(),
		Key:        key,
		Hint:       hint,
		ParkedAt:   time.Now().UTC(),
	})

	if stale != "" {
		return map[string]any{
			"status": "awaiting_operator_approval",
			"detail": "An operator had approved this call as part of a change set, but that approval no longer covers it: " + stale + " " +
				"The call has NOT been made and is parked for a fresh operator decision. " +
				"Do not retry it, do not attempt the same change by another route, and do not treat this as a failure. " +
				"Finish any read-only work, report that the change awaits re-approval, and stop.",
		}, nil
	}
	return map[string]any{
		"status": "awaiting_operator_approval",
		"detail": "This mutating call is parked pending operator approval and has NOT been made. " +
			"It will be made, made with edited arguments, or refused, depending on what the operator decides. " +
			"Do not retry this call, do not attempt the same change by another route, and do not treat this as a failure. " +
			"Finish any read-only work, report that the change awaits approval, and stop.",
	}, nil
}

func (g *writeGate) refuseRepeat(ctx agent.Context, t tool.Tool, key string, count int, endTurn bool) (map[string]any, error) {
	if g.cfg.ForgetToolRun != nil {
		g.cfg.ForgetToolRun(ctx.SessionID())
	}

	out := map[string]any{
		"error": "already_refused",
		"detail": "An operator has already refused this exact call in this turn. It has NOT been made, it was not put to them a second time, and proposing it again will not change the answer. " +
			"Do not retry it and do not attempt the same change by another route. Finish any read-only work, report that the change was refused, and stop.",
	}
	if !endTurn {
		g.audit(ctx, t, key, "already_refused", fmt.Sprintf("suppressed re-proposal %d of %d", count, suppressedCallsEndTurn))
		return out, nil
	}

	g.audit(ctx, t, key, "refusal_loop", fmt.Sprintf("ending the turn after %d suppressed re-proposals", count))
	g.cfg.Logger.Warn("ending the turn: the model will not accept a refusal",
		"tool", t.Name(), "call", key, "suppressed", count,
		"session", ctx.SessionID(), "invocation", ctx.InvocationID())

	if stop := TurnStopFrom(ctx); stop != nil {
		stop.StopTurn(&RefusalLoopError{Tool: t.Name(), Key: key, Count: count})
	}
	return out, nil
}

func parkHint(key string, set *ChangeSetContext, stale string) string {
	hint := fmt.Sprintf("Approve mutating call %s?", key)
	if stale != "" {
		return hint + " (you approved this as part of a change set, and mast is asking again: " + stale + ")"
	}
	if set == nil {
		return hint
	}
	if !set.Grantable {
		return fmt.Sprintf("%s It is 1 of %d calls %s proposed, but they must be approved one at a time: %s",
			hint, len(set.Changes), set.Specialist, set.Ungrantable)
	}
	return fmt.Sprintf("%s It is one of %d calls in the change set %s proposed; answer with scope=change_set to authorize all %d.",
		hint, len(set.Changes), set.Specialist, len(set.Changes))
}

func (g *writeGate) alreadyDispatched(ctx agent.Context, t tool.Tool, key string) map[string]any {
	if ctx == nil || ctx.FunctionCallID() == "" {
		return nil
	}
	callID := ctx.FunctionCallID()
	if _, exists := stateValue(ctx, DecisionStateKey(callID)); exists {
		return g.refuseDuplicateDispatch(ctx, t, key, callID)
	}
	if _, loaded := g.dispatched.Load(callID); loaded {
		return g.refuseDuplicateDispatch(ctx, t, key, callID)
	}
	if g.cfg.DecisionLookup != nil {
		found, err := g.cfg.DecisionLookup(ctx, ctx.SessionID(), callID)
		if err != nil {
			g.audit(ctx, t, key, "decision_lookup_failed", err.Error())
			return map[string]any{
				"error":  "decision_lookup_failed",
				"detail": "mast could not check the durable approval ledger before dispatching this call (" + err.Error() + "). The call was NOT made. Report this and stop.",
			}
		}
		if found {
			return g.refuseDuplicateDispatch(ctx, t, key, callID)
		}
	}
	return nil
}

func (g *writeGate) honorVerdict(ctx agent.Context, t tool.Tool, key string, args map[string]any, c *toolconfirmation.ToolConfirmation) (out map[string]any, err error) {
	if refusal := g.alreadyDispatched(ctx, t, key); refusal != nil {
		return refusal, nil
	}

	rec := g.newDecision(ctx, t, key, args)
	defer func() {
		if out != nil || err != nil {
			g.recordDecision(ctx, t, &rec, out, err)
		}
	}()

	v, err := DecodeVerdict(c)
	if err != nil {
		g.audit(ctx, t, key, "malformed_verdict", err.Error())
		return map[string]any{
			"error":  "malformed_verdict",
			"detail": err.Error() + " The call was not made. Report this to the operator; do not retry.",
		}, nil
	}
	rec.Outcome, rec.Scope = v.Verdict, v.Scope
	rec.Approver, rec.Note = v.Approver, v.Note

	effKey, edited := key, map[string]any(nil)
	if v.Verdict == OutcomeEdit {
		refusal, norm := g.prepareEdit(ctx, t, key, v)
		if refusal != nil {
			return refusal, nil
		}
		edited = norm
		effKey = CallKey(t.Name(), norm)
		rec.ExecutedKey, rec.ExecutedArgs = effKey, copyArgs(norm)
	}

	d, err := v.Decision()
	if err != nil {
		g.audit(ctx, t, effKey, "malformed_verdict", err.Error())
		return map[string]any{
			"error":  "malformed_verdict",
			"detail": err.Error() + " The call was not made.",
		}, nil
	}

	var pending []Grant
	if v.Verdict != OutcomeReject && v.Scope == ScopeChangeSet {
		p, refusal := g.planGrants(ctx, t, effKey, args, v)
		if refusal != nil {
			return refusal, nil
		}
		pending = p
	}

	if err := g.cfg.Gate.RecordMutationVerdict(ctx, t.Name(), effKey, d); err != nil {
		return g.verdictRefusal(ctx, t, effKey, v, err), nil
	}

	if v.Scope == ScopeChangeSet {
		g.commitGrants(ctx, t, effKey, v, pending)
	}

	running := args
	if edited != nil {
		running = edited
	}
	if refusal := g.permit(ctx, t, effKey, running); refusal != nil {
		return refusal, nil
	}

	if refusal := g.authorizeAndPersistDecision(ctx, t, effKey, &rec); refusal != nil {
		return refusal, nil
	}

	if edited != nil {
		record := AppliedEdit{
			Tool:         t.Name(),
			Approver:     v.Approver,
			ProposedKey:  key,
			ExecutedKey:  effKey,
			ProposedArgs: copyArgs(args),
			ExecutedArgs: copyArgs(edited),
			Note:         v.Note,
		}
		applyArgs(args, edited)
		g.recordEdit(ctx, t, record)
	}

	g.cfg.Logger.Info("mutating tool call approved by operator",
		"tool", t.Name(), "call", effKey, "approver", v.Approver, "note", v.Note,
		"edited", edited != nil, "proposed_call", key,
		"session", ctx.SessionID(), "invocation", ctx.InvocationID(),
		"function_call_id", ctx.FunctionCallID())
	return nil, nil
}

func (g *writeGate) authorizeAndPersistDecision(ctx agent.Context, t tool.Tool, effKey string, rec *Decision) map[string]any {
	if ctx != nil && ctx.FunctionCallID() != "" {
		callID := ctx.FunctionCallID()
		if _, already := g.dispatched.LoadOrStore(callID, true); already {
			return g.refuseDuplicateDispatch(ctx, t, effKey, callID)
		}
	}
	rec.Disposition = DispositionAuthorized
	if err := g.writeDecision(ctx, t, *rec); err != nil {
		if ctx != nil && ctx.FunctionCallID() != "" {
			g.dispatched.Delete(ctx.FunctionCallID())
		}
		g.audit(ctx, t, effKey, "decision_persist_failed", err.Error())
		return map[string]any{
			"error":  "decision_persist_failed",
			"detail": "This call was authorized, but mast could not durably record the approval decision before execution (" + err.Error() + "). The call was NOT made. Report this and stop.",
		}
	}
	return nil
}

func (g *writeGate) refuseDuplicateDispatch(ctx agent.Context, t tool.Tool, key, callID string) map[string]any {
	g.audit(ctx, t, key, "ambiguous_prior_effect", "refusing duplicate dispatch of already-dispatched function_call_id")
	return map[string]any{
		"error":            "ambiguous_prior_effect",
		"status":           "ambiguous_prior_effect",
		"tool":             t.Name(),
		"function_call_id": callID,
		"detail":           "This approved mutating tool call was already dispatched once for function_call_id " + callID + " and cannot be executed a second time without operator acknowledgement. Do not retry.",
	}
}

func (g *writeGate) verdictRefusal(ctx agent.Context, t tool.Tool, effKey string, v Verdict, err error) map[string]any {
	code := "denied_by_operator"
	detail := "The operator refused this call. Do not retry it and do not attempt the same change by another route. " +
		"Report the refusal, including the operator's reason if one was given, and stop."
	if errors.Is(err, permissions.ErrGrantScopeRefused) {
		code = "approval_scope_refused"
		detail = "The approval asked to authorize more than this one call, which mast does not allow for a mutating tool. " +
			"The call was NOT made. Report that the approval must be re-issued for this single call."
	} else {
		g.refused.remember(ctx.InvocationID(), effKey)
	}
	g.audit(ctx, t, effKey, code, err.Error())
	out := map[string]any{"error": code, "detail": detail}
	if v.Note != "" {
		out["operator_note"] = v.Note
	}
	if v.Approver != "" {
		out["approver"] = v.Approver
	}
	return out
}

func (g *writeGate) permit(ctx agent.Context, t tool.Tool, key string, args map[string]any) map[string]any {
	decl, err := g.cfg.Captures.declared(t.Name())
	if err != nil {
		g.audit(ctx, t, key, "capture_undeclarable", err.Error())
		return map[string]any{
			"error": "capture_failed",
			"detail": "This workload's declaration of what to record before changing " + t.Name() + " could not be read (" + err.Error() + "), " +
				"so mast did not make the change. Nothing happened. Report this and stop.",
		}
	}
	if decl == nil {
		return nil
	}
	rec, err := g.cfg.Captures.take(ctx, t.Name(), key, args, decl)
	if err != nil {
		g.audit(ctx, t, key, "capture_failed", err.Error())
		g.cfg.Logger.Warn("write gate: refusing an authorized mutating call because its prior state could not be captured",
			"tool", t.Name(), "call", key, "error", err.Error(),
			"session", ctx.SessionID(), "invocation", ctx.InvocationID(),
			"function_call_id", ctx.FunctionCallID())
		return map[string]any{
			"error": "capture_failed",
			"detail": "This call was authorized, but mast could not first record what it would overwrite: " + err.Error() + ". " +
				"The call was NOT made and nothing changed. This workload declares that " + t.Name() + " must not run without a prior-state record, " +
				"so this is a refusal and not a retryable error. Report it and stop.",
		}
	}
	g.recordCapture(ctx, t, rec)
	return nil
}

func (g *writeGate) recordCapture(ctx agent.Context, t tool.Tool, rec *CaptureRecord) {
	if rec == nil || ctx == nil {
		return
	}
	rec.Workload = g.cfg.Workload
	raw, err := EncodeCapture(*rec)
	if err != nil {
		g.cfg.Logger.Error("write gate: captured prior state but could not encode its record",
			"tool", t.Name(), "call", rec.Key, "error", err.Error())
		return
	}
	a := ctx.Actions()
	if a == nil {
		g.cfg.Logger.Error("write gate: no event actions to record captured prior state on",
			"tool", t.Name(), "call", rec.Key)
		return
	}
	if a.StateDelta == nil {
		a.StateDelta = map[string]any{}
	}
	a.StateDelta[CaptureStateKey(ctx.FunctionCallID())] = raw
	g.cfg.Logger.Info("write gate: prior state captured before a mutating call",
		"tool", t.Name(), "call", rec.Key, "read", CallKey(rec.Read, rec.ReadArgs),
		"digest", rec.Digest, "undoable", rec.Undoable(), "revert", revertKey(rec),
		"session", ctx.SessionID(), "invocation", ctx.InvocationID(),
		"function_call_id", ctx.FunctionCallID())
}

func revertKey(rec *CaptureRecord) string {
	if rec == nil || rec.Revert == nil {
		return ""
	}
	return CallKey(rec.Revert.Tool, rec.Revert.Arguments)
}

func (g *writeGate) prepareEdit(ctx agent.Context, t tool.Tool, key string, v Verdict) (refusal, edited map[string]any) {
	if v.Approver == "" {
		const detail = "an edited verdict arrived with no approver; mast will not run operator-authored arguments it cannot attribute"
		g.audit(ctx, t, key, "edit_unattributed", detail)
		return map[string]any{
			"error": "edit_unattributed",
			"detail": "The operator's edited arguments arrived without an authenticated approver, so mast refused them. " +
				"The call was NOT made, with either the original or the edited arguments. Report this and stop.",
		}, nil
	}
	norm, err := normalizeEdit(t, v.Args)
	if err != nil {
		g.audit(ctx, t, key, "edit_refused", err.Error())
		return map[string]any{
			"error": "edit_refused",
			"detail": "The operator's edited arguments were refused: " + err.Error() +
				". The call was NOT made, with either the original or the edited arguments. Report this and stop.",
		}, nil
	}
	editedKey := CallKey(t.Name(), norm)
	err = g.cfg.Gate.CheckMutatingToolCall(ctx, t.Name(), editedKey)
	if !errors.Is(err, permissions.ErrApprovalRequired) {
		g.audit(ctx, t, editedKey, "denied_by_policy", err.Error())
		return map[string]any{
			"error": "denied_by_policy",
			"detail": "The operator's edited call is refused by the configured permission policy, not by a person who might reconsider: " + err.Error() +
				" The call was NOT made. Do not retry it and do not attempt the same change by another route.",
		}, nil
	}
	return nil, norm
}

func (g *writeGate) recordEdit(ctx agent.Context, t tool.Tool, e AppliedEdit) {
	raw, err := json.Marshal(e)
	if err != nil {
		g.cfg.Logger.Error("write gate: applied an edit but could not encode its audit record",
			"tool", t.Name(), "call", e.ExecutedKey, "error", err.Error())
		return
	}
	if a := ctx.Actions(); a != nil {
		if a.StateDelta == nil {
			a.StateDelta = map[string]any{}
		}
		a.StateDelta[EditStateKey(ctx.FunctionCallID())] = string(raw)
	}
	g.cfg.Logger.Info("write gate: operator edit applied",
		"outcome", "edit_applied", "tool", t.Name(),
		"call", e.ExecutedKey, "proposed_call", e.ProposedKey,
		"approver", e.Approver, "agent", ctx.AgentName(),
		"session", ctx.SessionID(), "invocation", ctx.InvocationID(),
		"function_call_id", ctx.FunctionCallID())
}

func (g *writeGate) newDecision(ctx agent.Context, t tool.Tool, key string, args map[string]any) Decision {
	d := Decision{
		DecidedAt:    time.Now().UTC(),
		Workload:     g.cfg.Workload,
		Tool:         t.Name(),
		Authority:    AuthorityVerdict,
		ProposedKey:  key,
		ProposedArgs: copyArgs(args),
	}
	if ctx != nil {
		d.Session = ctx.SessionID()
		d.Specialist = ctx.AgentName()
		d.Invocation = ctx.InvocationID()
		d.FunctionCallID = ctx.FunctionCallID()
	}
	return d
}

func (g *writeGate) recordDecision(ctx agent.Context, t tool.Tool, d *Decision, out map[string]any, err error) {
	switch {
	case err != nil:
		d.Disposition, d.Refusal = DispositionRefusedByMast, "gate_error"
	case out == nil:
		d.Disposition = DispositionAuthorized
	case d.Outcome == OutcomeReject:
		d.Disposition, d.Refusal = DispositionRefusedByOperator, refusalCode(out)
	default:
		d.Disposition, d.Refusal = DispositionRefusedByMast, refusalCode(out)
	}
	_ = g.writeDecision(ctx, t, *d)
}

func refusalCode(out map[string]any) string {
	code, _ := out["error"].(string)
	return code
}

func (g *writeGate) writeDecision(ctx agent.Context, t tool.Tool, d Decision) error {
	if g.cfg.DecisionSink != nil {
		sinkCtx := context.Background()
		if ctx != nil {
			sinkCtx = ctx
		}
		if err := g.cfg.DecisionSink(sinkCtx, d); err != nil {
			g.cfg.Logger.Error("write gate: adjudicated a call but could not persist its decision record to DecisionSink",
				"tool", t.Name(), "call", d.ProposedKey, "error", err.Error())
			return err
		}
	}
	if ctx == nil {
		return nil
	}
	raw, err := EncodeDecision(d)
	if err != nil {
		g.cfg.Logger.Error("write gate: adjudicated a call but could not encode its decision record",
			"tool", t.Name(), "call", d.ProposedKey, "error", err.Error())
		return err
	}
	a := ctx.Actions()
	if a == nil {
		return nil
	}
	if a.StateDelta == nil {
		a.StateDelta = map[string]any{}
	}
	a.StateDelta[DecisionStateKey(ctx.FunctionCallID())] = raw
	return nil
}

func copyArgs(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func (g *writeGate) audit(ctx agent.Context, t tool.Tool, key, outcome, detail string) {
	attrs := []any{"outcome", outcome, "tool", t.Name(), "call", key, "detail", detail}
	if ctx != nil {
		attrs = append(attrs,
			"agent", ctx.AgentName(), "session", ctx.SessionID(),
			"invocation", ctx.InvocationID(), "function_call_id", ctx.FunctionCallID())
	}
	g.cfg.Logger.Info("write gate", attrs...)
}

// Request is the payload of the parked confirmation.
type Request struct {
	Tool      string            `json:"tool"`
	Args      map[string]any    `json:"args"`
	Key       string            `json:"key"`
	Policy    string            `json:"policy"`
	Agent     string            `json:"agent"`
	Verdict   map[string]any    `json:"verdict_format"`
	ChangeSet *ChangeSetContext `json:"change_set,omitempty"`
	Stale     string            `json:"stale,omitempty"`
}

// DecodeRequest reads the parked confirmation's payload back into the
// typed Request.
func DecodeRequest(v any) (Request, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return Request{}, fmt.Errorf("approval: re-marshalling parked request: %w", err)
	}
	var r Request
	if err := json.Unmarshal(raw, &r); err != nil {
		return Request{}, fmt.Errorf("approval: parked payload is not an approval request: %w", err)
	}
	return r, nil
}

var verdictHelp = map[string]any{
	"verdict": "approve | reject | edit",
	"scope":   "once (default) | change_set (only when this question carries a change_set: authorizes every call listed there, each bound to its exact arguments)",
	"args":    "edit only: the arguments to run instead of the agent's",
	"note":    "optional; shown to the agent",
}

// CallKey renders a tool call as the one-line description an operator
// approves and the deny policy matches against.
func CallKey(name string, args map[string]any) string {
	if len(args) == 0 {
		return name + "()"
	}
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+renderValue(args[k]))
	}
	return name + "(" + strings.Join(parts, ", ") + ")"
}

const maxValueLen = 120

func renderValue(v any) string {
	if s, ok := v.(string); ok {
		return elide(s)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "?"
	}
	return elide(string(b))
}

func elide(s string) string {
	if len(s) <= maxValueLen {
		return s
	}
	return s[:maxValueLen] + "…"
}
