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

// Turn-scoped refusal memory (#1074).
//
// #1068 shipped the cheap half of this: a denial and an expiry now tell
// the model, in the tool result it reads, that the answer will not
// change. Run 10 of dev/uat/approval-gate/ measured what that bought on
// a live cluster and the answer was nothing. All seventeen assertions
// passed, including the two proving the sentence reached the model, and
// leg 2 still opened five prompts — the watchdog's tool-failure-streak
// warning quoted the new guidance back as its "Last error" four calls
// into the loop. Eight prompts across a run whose floor is three.
//
// So the gate stops asking. After a deny or an expiry, an identical
// request — same tool, same detail — is refused for the remainder of
// the turn without opening a prompt and without notifying anybody. The
// operator who said no once is not paged five times, and the model's
// fourth attempt fails in microseconds instead of after a 90-second
// approval timeout.
//
// Three scoping decisions, each of which is the answer to a way this
// could be wrong:
//
// The memory is keyed on the request, not the tool. An agent denied one
// `kubectl apply` may legitimately fix its manifest and ask again, and
// that second request is a different detail string and gets a prompt.
// Only the call the operator actually refused is suppressed.
//
// The memory is scoped to the turn, not the session. The model's
// reason for re-issuing is that it has not absorbed the refusal yet;
// by the next turn it has a tool result saying so in its history, and
// the operator's circumstances may have changed. A session-scoped
// refusal would also mean one misclick silently disables a tool for
// hours, which is a support burden nobody would connect to the click.
//
// Only a refusal arms it. An allow-once followed by an identical call
// is an ordinary second call and gets an ordinary prompt: the operator
// said yes to one invocation, and saying yes is not evidence that they
// want the next one auto-answered either way.

package permissions

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// refusalKind records which refusal armed the memory, so
// the repeat can say which. They are not interchangeable to a model: a
// denial is a human decision, an expiry is the absence of one, and the
// useful next action differs (find another approach vs. report that the
// action is still pending).
type refusalKind int

const (
	refusedByDeny refusalKind = iota + 1
	refusedByExpiry

	// refusedByApprover is a deny from ModeAuto's approver model
	// (#1175). It is final for the turn like an operator's, but the
	// repeat must not claim a person said no.
	refusedByApprover

	// refusedNoPerson is a ModeAuto call that needed a person on a
	// gate with no prompter (#1175). Like an expiry, nobody is there to
	// answer a retry; unlike one, no wall clock was spent.
	refusedNoPerson

	// refusedNoPersonAsked is a ModeAuto call that needed a person under
	// permissions.auto.escalation "refuse". Nothing is pending, unlike an
	// expiry: the repeat says so, and says a different call can still
	// be judged, rather than telling the agent to end the turn.
	refusedNoPersonAsked
)

// turnRefusalKeys returns a gated request's two identities for
// suppression purposes. bare is the tool|detail pair the gate uses for
// session allow-grants (see sessionAllowed). full adds a fingerprint of
// the call's full arguments, for every tool but bash, when the gate
// holds them; otherwise it equals bare.
//
// A denial is remembered under full, everything else under bare.
//
// A denial is a verdict on the call's content, by a person or the
// approver, and only bash puts the whole call in its detail. For the
// file tools the detail is the path, so the bare pair made one denied
// edit refuse every other edit to that file for the rest of the turn:
// in A7's second run the approver rightly denied a no-op edit (identical
// old and new text), and the agent's real edit to the same file was then
// refused as "an identical request". That is the "keyed on the request,
// not the tool" rule above, applied to tools whose detail is a target.
//
// An expiry or a no-person refusal is not about the content: nobody is
// there whatever the arguments say, and eligibility reads tool|detail.
// Keying those on the arguments would let a reworded call wait out
// another approval_timeout. Bash keeps the bare pair for a denial too:
// its detail is already the full command, and adding the arguments
// would let a changed timeout_seconds launder a refused command past
// this memory. A request without Args has only the bare pair, which
// errs toward refusing the repeat, this memory's existing direction.
//
// It is an exact match on purpose. pkg/watchdog canonicalizes
// path-shaped arguments before comparing calls, which is right for a
// loop detector — a false positive there costs a warning. Here a false
// positive costs a call the operator would have approved, silently, so
// the key errs toward asking again.
func turnRefusalKeys(req PromptRequest) (bare, full string) {
	bare = req.ToolName + "|" + req.Detail
	if req.ToolName == "bash" || !hasArgs(req.Args) {
		return bare, bare
	}
	sum := sha256.Sum256(req.Args)
	return bare, bare + "|" + hex.EncodeToString(sum[:])
}

// isDenial reports whether kind is a verdict on the call's content.
func (k refusalKind) isDenial() bool {
	return k == refusedByDeny || k == refusedByApprover
}

// turnRefusal reports whether an identical request has already been
// refused in this turn, and how.
//
// It also counts. Every caller that gets ok==true returns the repeat
// refusal on the next line and never reaches a prompt, so this is the
// one chokepoint through which a suppressed call passes, and counting
// anywhere else would be two places that have to agree. The count is
// what the agent reads to decide a turn is over (#1081).
func (g *Gate) turnRefusal(req PromptRequest) (refusalKind, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	bare, full := turnRefusalKeys(req)
	k, ok := g.turnRefusals[full]
	if !ok {
		k, ok = g.turnRefusals[bare]
	}
	if ok {
		g.turnRefusalRepeats++
	}
	return k, ok
}

// TurnRefusalRepeats reports how many calls this turn the gate refused
// without asking anybody, because an identical request had already been
// refused in it.
//
// Counted across keys, not per key. A model alternating between two
// requests the operator has already refused is in the same state as one
// repeating a single request, and every event counted is a call whose
// answer was on record before it was made — so there is no reading of
// the total that is ambiguous, which is what lets the threshold above it
// be low.
//
// Resolves the per-session sub-gate off ctx for the same reason every
// other method does: in a multi-session daemon the refusals belong to
// the session's gate, not to the template the tool wrappers were built
// against. Nil-safe.
func (g *Gate) TurnRefusalRepeats(ctx context.Context) int {
	if g == nil {
		return 0
	}
	g = g.resolveSessionGate(ctx)
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.turnRefusalRepeats
}

// rememberTurnRefusal arms the memory for this exact request. Called on
// the deny and expiry paths only.
func (g *Gate) rememberTurnRefusal(req PromptRequest, kind refusalKind) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.turnRefusals == nil {
		g.turnRefusals = make(map[string]refusalKind)
	}
	bare, full := turnRefusalKeys(req)
	if kind.isDenial() {
		g.turnRefusals[full] = kind
		return
	}
	g.turnRefusals[bare] = kind
}

// ObserveTurnStart clears the turn-scoped refusal memory. The agent
// calls it once per turn, from the pre-turn pipeline's turn-boundary
// step — after the preflights, so a turn that was refused before it
// began is not a boundary and cannot launder a refusal by being
// re-driven.
//
// It resolves the per-session sub-gate off ctx for the same reason
// every Check* method does: in a multi-session daemon the state that
// needs clearing belongs to the session's gate, not to the template the
// tool wrappers were built against.
//
// Nil-safe, because a host may hold a gate it never wired an agent to.
func (g *Gate) ObserveTurnStart(ctx context.Context) {
	if g == nil {
		return
	}
	g = g.resolveSessionGate(ctx)
	g.mu.Lock()
	defer g.mu.Unlock()
	clear(g.turnRefusals)
	g.turnRefusalRepeats = 0
}

// repeatRefusalError is the tool result a suppressed request gets.
//
// It has to say it is a repeat. A model that cannot tell "the human
// said no again" from "the human said no once and you asked twice"
// learns nothing from the second refusal either, which is the lesson
// #1068 already cost a PR to learn. So the message states three
// things the model cannot otherwise know: that this exact request was
// already refused in this turn, that nobody was asked again, and what
// to do instead.
func repeatRefusalError(kind refusalKind, toolName, detail string) error {
	switch kind {
	case refusedByExpiry:
		return fmt.Errorf("%s not attempted: an identical request expired unanswered earlier in this turn, so this one was not put to anybody either (detail=%q). %s",
			toolName, detail, expiryGuidance)
	case refusedNoPerson:
		return fmt.Errorf("%s not attempted: an identical request earlier in this turn needed a person's approval and no person can be asked in this session, so this one was not put to the approver either (detail=%q). %s",
			toolName, detail, expiryGuidance)
	case refusedNoPersonAsked:
		return fmt.Errorf("%s not attempted: an identical request earlier in this turn needed a person's approval, and this deployment asks no person, so this one was refused without asking anybody (detail=%q). %s",
			toolName, detail, refusedGuidance)
	case refusedByApprover:
		return fmt.Errorf("%s not attempted: the approver model denied an identical request earlier in this turn, so this one was not put to the approver or to a person (detail=%q). %s",
			toolName, detail, denyGuidance)
	default:
		return fmt.Errorf("%s not attempted: an identical request was denied earlier in this turn, so this one was not put to a human again (detail=%q). %s",
			toolName, detail, denyGuidance)
	}
}
