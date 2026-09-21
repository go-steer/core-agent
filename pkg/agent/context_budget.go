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

// Watching the context window grow INSIDE a turn (#975).
//
// Every context check in this package used to sit on a turn boundary,
// and the thing that overflows a context window does not. The sequence
// is: a model call completes and commits its usage, a tool runs and
// returns a payload, the next model call is built with that payload in
// it. Nothing evaluated context between the second and third steps, so
// one large tool result — a full pod log, a wide `kubectl get -o json`,
// an MCP response over a big cluster — took the request over the wall
// while the utilization figure compaction and the UI both read still
// described the comfortable state before it arrived. On the GKE path
// large read results are the normal case, not an edge case.
//
// Two arms, both fed by usage.Tracker's estimate of the unmeasured tail:
//
//  1. Trigger early. Once the estimate crosses the compactor's own
//     threshold, compaction is marked pending immediately instead of at
//     the end of an agentic turn that may still have a dozen tool calls
//     to run. Nothing about the threshold changes; only when it is read.
//
//  2. Cut before the wall. Past ContextBudgetHardCeiling the turn is
//     stopped, with compaction already pending, so the next turn reduces
//     context and carries on. The alternative is not "the turn
//     succeeds" — it is the provider rejecting an oversized request with
//     an error that classifies badly and points nowhere near compaction.
//     A cut we chose is legible; a 400 from the wall is the symptom
//     nobody could trace in #974.
//
// This is deliberately NOT modelled as a guardrail trip. `cost_ceiling`
// and `watchdog` latch the session and wait for an operator to reset;
// this one heals itself on the next turn, and borrowing their vocabulary
// would render a "go reset your session" affordance for a condition
// with nothing to reset. It rides #974's context-reduction-degraded row
// instead, which already means "context reduction happened, in a worse
// way than usual" — which is exactly what a cut turn is.

package agent

import (
	"context"
	"fmt"
	"log"

	"google.golang.org/adk/session"

	"github.com/go-steer/core-agent/v2/pkg/attach"
)

// ContextBudgetHardCeiling is the fraction of the context window past
// which an in-flight turn is cut rather than allowed to build its next
// request.
//
// 0.95 sits above every compaction threshold that ships (0.85 frontier,
// 0.65 mid, 0.35 small) so the ordinary path always gets to act first:
// by the time this fires, compaction has already been marked pending and
// simply has not had a turn boundary to run at. It is a backstop for the
// case the first arm cannot cover — a single result large enough to
// cross both lines at once — and not a second trigger.
//
// Leaving 5% rather than cutting at the wall itself is deliberate. The
// number being compared is an estimate for exactly the bytes that have
// not been measured yet, the request also carries a system instruction
// and tool declarations this does not count, and being slightly early
// costs one cut turn while being late costs the provider rejection this
// exists to prevent.
const ContextBudgetHardCeiling = 0.95

// observeContextGrowth folds the size of anything this event appends to
// the conversation into the tracker's unmeasured tail, then re-checks
// the budget. Called from Run's in-turn tap for every event, alongside
// the cost-ceiling and watchdog arms that already enforce there.
//
// Cheap when it cannot matter: an event carrying no function response
// adds nothing, and the check short-circuits on an empty tail.
func (a *Agent) observeContextGrowth(ev *session.Event) {
	if a == nil || a.tracker == nil || ev == nil {
		return
	}
	if n := contextGrowthBytes(ev); n > 0 {
		a.tracker.AddPendingContextBytes(n)
	}
	a.enforceContextBudgetInTurn()
}

// contextGrowthBytes measures what an event adds to the conversation
// that the next request will have to re-send.
//
// Function responses only. Model text is already counted by the usage
// record for the call that produced it — output tokens become input
// tokens on the next call, and the provider's own number is better than
// anything measured here. Tool results are the gap: they enter history
// between two measurements and nothing reports their size.
func contextGrowthBytes(ev *session.Event) int {
	if ev == nil || ev.Content == nil {
		return 0
	}
	total := 0
	for _, p := range ev.Content.Parts {
		if p == nil || p.FunctionResponse == nil {
			continue
		}
		total += functionResponseBytes(p.FunctionResponse.Response)
	}
	return total
}

// functionResponseBytes approximates the serialized size of a tool
// result map. fmt of the map rather than json.Marshal: the value may
// hold types that do not marshal, and this is feeding an estimate whose
// own conversion ratio is a rounded guess — a marshalling error path
// would be more precision than the number can carry, and returning 0 on
// one would under-count exactly the oversized payloads that matter.
func functionResponseBytes(resp map[string]any) int {
	if len(resp) == 0 {
		return 0
	}
	total := 0
	for k, v := range resp {
		total += len(k)
		switch s := v.(type) {
		case string:
			total += len(s)
		case nil:
			// nothing
		default:
			total += len(fmt.Sprint(v))
		}
	}
	return total
}

// enforceContextBudgetInTurn is the in-turn half of the context check.
//
// Returns without touching anything when the tail is empty: that is the
// state every boundary check already covers, and re-deciding it here
// would mean a turn with no tool calls paying for a threshold evaluation
// on every streamed text delta.
func (a *Agent) enforceContextBudgetInTurn() {
	if a == nil || a.tracker == nil {
		return
	}
	used, estimated := a.tracker.ContextWindowUsedEstimated()
	if !estimated {
		return
	}
	size, _ := a.compactionWindowSize()
	if size == 0 {
		return
	}

	// Arm 1: bring the boundary decision forward. ShouldCompact reads
	// ContextWindowUsed, which now includes the tail, so the compactor
	// needs no changes to become estimate-aware — it just gets asked at a
	// moment it was never asked before.
	if a.compactor != nil && a.compactor.ShouldCompact(context.Background(), a) {
		a.mu.Lock()
		a.compactionPending = true
		a.mu.Unlock()
	}

	// Arm 2: the backstop.
	if float64(used)/float64(size) < ContextBudgetHardCeiling {
		return
	}
	a.cutTurnForContextBudget(used, size)
}

// cutTurnForContextBudget stops the turn in flight, leaving compaction
// pending so the next one starts by reducing context.
//
// Latched per turn, not per process. A cut describes an attempt, not a
// state, and #974's rule is that states announce once while attempts
// announce every time — a session cutting a turn every turn is a very
// different report from a session that cut one, and collapsing the two
// would hide the worse of them.
func (a *Agent) cutTurnForContextBudget(used, size int) {
	a.mu.Lock()
	if a.contextBudgetCut {
		a.mu.Unlock()
		return
	}
	a.contextBudgetCut = true
	// Pending regardless of what the compactor's own threshold said: at
	// this fill the next request does not fit, so the next turn has to
	// reduce before it does anything else.
	a.compactionPending = true
	pending := a.tracker.PendingContextBytes()
	a.mu.Unlock()

	detail := fmt.Sprintf(
		"a tool result took the estimated context to %d of %d tokens (%.0f%%, including %d unmeasured bytes); "+
			"cut the turn before building a request the provider would reject, and compaction runs first on any further turn.",
		used, size, 100*float64(used)/float64(size), pending)
	// The session goes on the log line only, not into detail (#1136).
	// detail is also the durable degraded row's text, and that row is
	// already filed under its session — repeating the id inside it would
	// be noise in the one place it is redundant. Leading rather than
	// mid-sentence because detail is a whole sentence of its own; the
	// guardrail lines can splice theirs in after the guardrail's name
	// because they build the sentence around it.
	//
	// turnCutNoNextTurn rides the log line for the same reason and not
	// the same one (#1140). "Compaction will run first on the next
	// turn" promised a turn a one-shot never takes — that half is fixed
	// in detail itself, in the subjunctive, because a stale promise is
	// wrong in the durable row too. Naming the one-shot is the half
	// that is log-only: the row is read by a surface that queried the
	// session, so it has a caller by construction and is exactly the
	// reader for whom "a -p run ends here" is noise.
	log.Printf("agent:%s %s %s", a.logSessionSuffix(), detail, turnCutNoNextTurn)
	a.recordContextReductionDegraded(attach.ContextReductionTurnCut, detail)
	a.Interrupt()
}

// clearContextBudgetCut resets the per-turn latch at turn start, the
// same belt-and-braces as clearGuardrailHalt: a turn that never reaches
// its cleanup must not leave the next one unable to protect itself.
func (a *Agent) clearContextBudgetCut() {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.contextBudgetCut = false
	a.mu.Unlock()
}

// ContextWindowUsedEstimated is the reporting accessor for a surface
// that renders a utilization percentage. Returns the token count and
// whether any of it is estimated, so the surface can say which of the
// two it has rather than presenting a guess as a measurement — the
// assessment's note on this is right that a number which can only be
// stale is worse than one that admits it is estimated.
//
// The bundled TUI segment does not show the distinction yet: that is a
// client-side change, and #891's lesson is that shipping one side of one
// of those regresses the other. The substrate reports it now so the
// client has something to read when it does.
func (a *Agent) ContextWindowUsedEstimated() (used int, estimated bool) {
	if a == nil || a.tracker == nil {
		return 0, false
	}
	return a.tracker.ContextWindowUsedEstimated()
}
