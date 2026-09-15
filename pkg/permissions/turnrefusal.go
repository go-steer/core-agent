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
	"fmt"
)

// refusalKind records which of the two refusals armed the memory, so
// the repeat can say which. They are not interchangeable to a model: a
// denial is a human decision, an expiry is the absence of one, and the
// useful next action differs (find another approach vs. report that the
// action is still pending).
type refusalKind int

const (
	refusedByDeny refusalKind = iota + 1
	refusedByExpiry
)

// turnRefusalKey is the identity of a gated request for suppression
// purposes. It is deliberately the same tool|detail pair the gate
// already uses for session allow-grants (see sessionAllowed): the gate
// has exactly one notion of "the same request", and a refusal must not
// be allowed to disagree with an approval about what that means.
//
// It is an exact match on purpose. pkg/watchdog canonicalizes
// path-shaped arguments before comparing calls, which is right for a
// loop detector — a false positive there costs a warning. Here a false
// positive costs a call the operator would have approved, silently, so
// the key errs toward asking again.
func turnRefusalKey(toolName, detail string) string {
	return toolName + "|" + detail
}

// turnRefusal reports whether an identical request has already been
// refused in this turn, and how.
func (g *Gate) turnRefusal(toolName, detail string) (refusalKind, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	k, ok := g.turnRefusals[turnRefusalKey(toolName, detail)]
	return k, ok
}

// rememberTurnRefusal arms the memory for this exact request. Called on
// the deny and expiry paths only.
func (g *Gate) rememberTurnRefusal(toolName, detail string, kind refusalKind) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.turnRefusals == nil {
		g.turnRefusals = make(map[string]refusalKind)
	}
	g.turnRefusals[turnRefusalKey(toolName, detail)] = kind
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
	default:
		return fmt.Errorf("%s not attempted: an identical request was denied earlier in this turn, so this one was not put to a human again (detail=%q). %s",
			toolName, detail, denyGuidance)
	}
}
