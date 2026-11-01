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
	"fmt"
	"sort"
	"strings"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"

	"github.com/go-steer/core-agent/v3/pkg/permissions"
)

func (g *writeGate) checkGrant(ctx agent.Context, t tool.Tool, args map[string]any) (*Grant, string) {
	if g.cfg.Grants == nil || ctx == nil {
		return nil, ""
	}
	sig, err := Signature(t.Name(), args)
	if err != nil {
		return nil, ""
	}
	raw, ok := stateValue(ctx, GrantStateKey(sig))
	if !ok {
		return nil, ""
	}
	grant, err := DecodeGrant(raw)
	if err != nil {
		g.cfg.Logger.Error("write gate: a change-set grant could not be read back; the call will be parked",
			"tool", t.Name(), "session", ctx.SessionID(), "error", err.Error())
		return nil, "an approval was recorded for this call but mast could not read it back"
	}
	if grant.Signature != sig {
		return nil, ""
	}
	if grant.VoidedBy != "" {
		return nil, "an earlier approval of this call is no longer valid: " + grant.VoidedBy
	}
	if grant.ConsumedBy != "" && grant.ConsumedBy != ctx.FunctionCallID() {
		return nil, "this call was already approved once and has already been made; a second identical call needs its own approval"
	}
	now := g.cfg.Grants.now()
	if !grant.ExpiresAt.IsZero() && now.After(grant.ExpiresAt) {
		return nil, fmt.Sprintf("the change set was approved at %s and that approval expired at %s",
			grant.MintedAt.UTC().Format("15:04:05Z"), grant.ExpiresAt.UTC().Format("15:04:05Z"))
	}
	if reason := g.cfg.Grants.verify(ctx, grant.Precondition, t.Name()); reason != "" {
		return nil, reason
	}
	return &grant, ""
}

func (g *writeGate) spendGrant(ctx agent.Context, t tool.Tool, key string, args map[string]any, grant *Grant) (out map[string]any, err error) {
	if refusal := g.alreadyDispatched(ctx, t, key); refusal != nil {
		return refusal, nil
	}
	rec := g.newDecision(ctx, t, key, args)
	rec.Authority, rec.ChangeSet = AuthorityChangeSetGrant, grant.Origin
	rec.Outcome, rec.Scope = OutcomeApprove, ScopeChangeSet
	rec.Approver, rec.Note = grant.Approver, grant.Note
	defer func() {
		if out != nil || err != nil {
			g.recordDecision(ctx, t, &rec, out, err)
		}
	}()

	if err := g.cfg.Gate.RecordMutationVerdict(ctx, t.Name(), key, permissions.DecisionAllowOnce); err != nil {
		g.audit(ctx, t, key, "grant_not_honored", err.Error())
		return map[string]any{
			"error":  "denied_by_policy",
			"detail": "This call carried an operator approval that mast could not honor: " + err.Error() + " The call was NOT made. Report this and stop.",
		}, nil
	}
	if refusal := g.permit(ctx, t, key, args); refusal != nil {
		return refusal, nil
	}
	if refusal := g.authorizeAndPersistDecision(ctx, t, key, &rec); refusal != nil {
		return refusal, nil
	}

	spent := *grant
	spent.ConsumedBy = ctx.FunctionCallID()
	g.writeGrant(ctx, spent)
	g.audit(ctx, t, key, "approved_by_change_set", fmt.Sprintf(
		"authorized by the change set %s approved at %s", grant.Origin, grant.MintedAt.UTC().Format("15:04:05Z")))
	g.cfg.Logger.Info("mutating tool call authorized by an approved change set",
		"tool", t.Name(), "call", key, "approver", grant.Approver, "origin", grant.Origin,
		"minted_at", grant.MintedAt.UTC(), "expires_at", grant.ExpiresAt.UTC(),
		"session", ctx.SessionID(), "invocation", ctx.InvocationID(),
		"function_call_id", ctx.FunctionCallID())
	return nil, nil
}

func (g *writeGate) voidGrant(ctx agent.Context, t tool.Tool, args map[string]any, reason string) {
	if g.cfg.Grants == nil || ctx == nil {
		return
	}
	sig, err := Signature(t.Name(), args)
	if err != nil {
		return
	}
	raw, ok := stateValue(ctx, GrantStateKey(sig))
	if !ok {
		return
	}
	grant, err := DecodeGrant(raw)
	if err != nil || grant.Signature != sig || grant.VoidedBy != "" {
		return
	}
	grant.VoidedBy = reason
	g.writeGrant(ctx, grant)
	g.cfg.Logger.Info("write gate: a change-set grant was voided and its call re-parked",
		"tool", t.Name(), "call", CallKey(t.Name(), args), "reason", reason,
		"approver", grant.Approver, "origin", grant.Origin,
		"session", ctx.SessionID(), "invocation", ctx.InvocationID())
}

func (g *writeGate) planGrants(ctx agent.Context, t tool.Tool, key string, args map[string]any, v Verdict) ([]Grant, map[string]any) {
	refuse := func(detail string) ([]Grant, map[string]any) {
		g.audit(ctx, t, key, "change_set_scope_refused", detail)
		return nil, map[string]any{
			"error": "approval_scope_refused",
			"detail": "The operator approved this as a change set, but mast would not authorize the set: " + detail +
				" The call was NOT made. Report that the changes must be approved one at a time.",
		}
	}
	if g.cfg.Grants == nil {
		return refuse("this deployment does not issue change-set approvals")
	}
	if v.Verdict == OutcomeEdit {
		return refuse("the verdict edits this call's arguments, and an edit speaks only for the call it edits — the rest of the set is still what the specialist proposed")
	}
	sig, err := Signature(t.Name(), args)
	if err != nil {
		return refuse("this call has no stable signature, so nothing can be bound to it")
	}
	specialist, set, ok := changeSetFor(ctx, sig)
	if !ok {
		return refuse("this call is not one of the calls in any change set this session recorded")
	}
	if err := Legible(set); err != nil {
		return refuse(err.Error())
	}

	now := g.cfg.Grants.now()
	expires := now.Add(g.cfg.Grants.ttl())
	pending := make([]Grant, 0, len(set))
	for _, ch := range set {
		chSig, err := ch.Signature()
		if err != nil {
			return refuse(fmt.Sprintf("a change in the set has no stable signature: %v", err))
		}
		if chSig == sig {
			continue
		}
		snap, err := g.cfg.Grants.snapshot(ctx, ch)
		if err != nil {
			return refuse(fmt.Sprintf("mast could not record what %s assumes about the cluster, so it cannot tell later whether that is still true: %v", chSig, err))
		}
		pending = append(pending, Grant{
			Signature:    chSig,
			Tool:         ch.Tool,
			Arguments:    ch.Arguments,
			Origin:       key,
			Approver:     v.Approver,
			Note:         v.Note,
			MintedAt:     now,
			ExpiresAt:    expires,
			Precondition: snap,
		})
	}
	g.cfg.Logger.Info("change set approved",
		"specialist", specialist, "approver", v.Approver, "approved_call", key,
		"granting", len(pending), "expires_at", expires.UTC(), "note", v.Note,
		"session", ctx.SessionID(), "invocation", ctx.InvocationID())
	return pending, nil
}

func (g *writeGate) commitGrants(ctx agent.Context, t tool.Tool, key string, v Verdict, pending []Grant) {
	sigs := make([]string, 0, len(pending))
	for _, grant := range pending {
		g.writeGrant(ctx, grant)
		sigs = append(sigs, grant.Signature)
	}
	detail := fmt.Sprintf("%d further call(s) authorized", len(pending))
	if len(pending) > 0 {
		detail = fmt.Sprintf("%s until %s: %s", detail,
			pending[0].ExpiresAt.UTC().Format("15:04:05Z"), strings.Join(sigs, "; "))
	} else {
		detail += " — this change set has no calls beyond the one just approved"
	}
	g.audit(ctx, t, key, "change_set_approved", detail)
	g.cfg.Logger.Info("change-set grants minted",
		"approver", v.Approver, "approved_call", key, "granted", len(pending),
		"calls", strings.Join(sigs, "; "),
		"session", ctx.SessionID(), "invocation", ctx.InvocationID())
}

func (g *writeGate) writeGrant(ctx agent.Context, grant Grant) {
	raw, err := EncodeGrant(grant)
	if err != nil {
		g.cfg.Logger.Error("write gate: a change-set grant could not be encoded",
			"tool", grant.Tool, "call", grant.Signature, "error", err.Error())
		return
	}
	a := ctx.Actions()
	if a == nil {
		g.cfg.Logger.Error("write gate: no event actions to record a change-set grant on",
			"tool", grant.Tool, "call", grant.Signature)
		return
	}
	if a.StateDelta == nil {
		a.StateDelta = map[string]any{}
	}
	a.StateDelta[GrantStateKey(grant.Signature)] = raw
}

func changeSetFor(ctx agent.Context, sig string) (specialist string, set []ProposedChange, ok bool) {
	for name, changes := range recordedChangeSets(ctx) {
		for _, ch := range changes {
			chSig, err := ch.Signature()
			if err == nil && chSig == sig {
				return name, changes, true
			}
		}
	}
	return "", nil, false
}

func recordedChangeSets(ctx agent.Context) map[string][]ProposedChange {
	st := ctx.State()
	if st == nil {
		return nil
	}
	out := map[string][]ProposedChange{}
	for k, v := range st.All() {
		if !strings.HasPrefix(k, ChangeSetStateKeyPrefix) {
			continue
		}
		changes, err := DecodeChangeSet(v)
		if err != nil || len(changes) == 0 {
			continue
		}
		out[strings.TrimPrefix(k, ChangeSetStateKeyPrefix)] = changes
	}
	return out
}

func stateValue(ctx agent.Context, key string) (any, bool) {
	st := ctx.State()
	if st == nil {
		return nil, false
	}
	v, err := st.Get(key)
	if err != nil || v == nil {
		return nil, false
	}
	return v, true
}

// ChangeSetContext is what the parked question says about the set the
// call belongs to, so an operator answering one call can see the rest
// and authorize them together.
type ChangeSetContext struct {
	Specialist    string            `json:"specialist"`
	Changes       []ProposedChange  `json:"changes"`
	Grantable     bool              `json:"grantable"`
	Ungrantable   string            `json:"ungrantable,omitempty"`
	TTLSeconds    int               `json:"ttl_seconds,omitempty"`
	Preconditions map[string]string `json:"preconditions,omitempty"`
}

func (g *writeGate) changeSetContextFor(ctx agent.Context, t tool.Tool, args map[string]any) *ChangeSetContext {
	if g.cfg.Grants == nil || ctx == nil {
		return nil
	}
	sig, err := Signature(t.Name(), args)
	if err != nil {
		return nil
	}
	specialist, set, ok := changeSetFor(ctx, sig)
	if !ok {
		return nil
	}
	out := &ChangeSetContext{
		Specialist: specialist,
		Changes:    set,
		Grantable:  true,
		TTLSeconds: int(g.cfg.Grants.ttl().Seconds()),
	}
	if err := Legible(set); err != nil {
		out.Grantable, out.Ungrantable = false, err.Error()
	}
	out.Preconditions = map[string]string{}
	for _, name := range toolNames(set) {
		desc := "none declared — this approval is bounded by time alone"
		if g.cfg.Grants.Precondition != nil {
			pre, err := g.cfg.Grants.Precondition(name)
			switch {
			case err != nil:
				desc = "declared but unreadable: " + err.Error()
				out.Grantable, out.Ungrantable = false, "a tool in this set declares a precondition mast cannot evaluate"
			case pre != nil:
				desc = "re-checked against " + pre.Read + " before the call fires"
			}
		}
		out.Preconditions[name] = desc
	}
	return out
}

func toolNames(set []ProposedChange) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(set))
	for _, ch := range set {
		if !seen[ch.Tool] {
			seen[ch.Tool] = true
			out = append(out, ch.Tool)
		}
	}
	sort.Strings(out)
	return out
}
