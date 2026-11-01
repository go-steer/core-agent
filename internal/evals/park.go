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

// The write gate's adjudication of a mutating call, projected onto the
// trace so an evaluator can score it (#295).
//
// Nothing here is a new record. The gate already writes both halves and
// has since v0.2 and v0.4 respectively: the question is an
// adk_request_confirmation call carrying an approval.Request, and the
// answer is an approval.Decision on the state delta of the event ADK
// appends for the same call. What did not exist is a way to ask for
// them together, which is why #295 is an exposure and not a recording.
//
// # Why they are two fields and not one
//
// The question and the answer have different authors. mast writes the
// question, before any answer exists, keyed by the call it gates. The
// answer is mast's record of something that arrived from outside —
// an operator over the resume boundary, or a grant given earlier about
// a different call.
//
// Folding them into one struct would let a check read "an adjudication
// happened" and believe it had measured the gate. It has not: in an
// eval the harness supplies the answer, so an answer is evidence that
// the harness ran. Only the question is evidence that mast asked. The
// two stay separable because the whole point of the projection is that
// a predicate can be written over one of them and not the other.
//
// Either can be absent, and each absence means something different:
//
//   - no question, no answer — the call ran ungated. Policy was apply,
//     or the gate was not registered, or the call went out through a
//     path the gate does not sit on.
//   - no question, an answer under change_set_grant — legitimate: an
//     operator authorized this call by answering about another one in
//     the same set (see approval.AuthorityChangeSetGrant). The
//     authorizing question is elsewhere in the same log.
//   - no question, an answer under operator_verdict — an answer to a
//     question the log does not carry. Nothing in mast produces this.
//   - a question, no answer — the run ended parked. Ordinary.

package evals

import (
	"strings"
	"time"

	adksession "google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool/toolconfirmation"

	"github.com/go-steer/core-agent/v3/pkg/approval"
)

// Park is the question the write gate wrote into the durable log before
// a mutating call could run.
//
// Tool and Args are what the gate recorded, not what the trace records:
// they are read out of the confirmation payload rather than off the
// FunctionCall part beside it. That is the difference a safeguard needs
// — "the operator was shown this call" is a claim about the question,
// and comparing it against the call that ran is the only way to catch a
// gate that asked about a proxy.
type Park struct {
	// RequestedAt is when the gate parked, from the event's timestamp.
	RequestedAt time.Time
	// Hint is the one line the gate wrote for a human.
	Hint string
	// Tool and Args are the call as the question described it.
	Tool string
	Args map[string]any
	// Key is approval.CallKey of the same call. It is what a change-set
	// grant names its origin by, and therefore the only way to find the
	// question a granted call was authorized by.
	Key string
	// Agent is the agent whose call was parked.
	Agent string
	// ChangeSet is the set of calls the operator was offered alongside
	// this one, nil when the question carried none. A granted call is
	// authorized by appearing here, so this is read rather than
	// decorative.
	ChangeSet []approval.ProposedChange
	// Malformed is set when the confirmation call was mast's but its
	// payload would not decode. The park is still reported: an
	// unreadable question is not the same finding as no question, and
	// collapsing them would let a payload regression read as a gate
	// that never fired.
	Malformed string
}

// Answer is the gate's durable record of how a parked call was
// resolved: approval.Decision, narrowed to what a check can use.
//
// It is the answer as mast recorded it, which is not the same as the
// answer the operator gave — Disposition and Outcome come apart when
// mast refuses a verdict a person gave.
type Answer struct {
	// At is when the gate adjudicated. mast does not see when the
	// operator answered.
	At time.Time
	// Outcome is what the operator said: approve, reject, edit. Empty
	// when the payload was not readable.
	Outcome string
	// Authority is where the authorization came from: operator_verdict
	// for a question about this exact call, change_set_grant for one
	// answered earlier about another call in the same set.
	Authority string
	// Disposition is what the gate then did.
	Disposition string
	// Approver is the authenticated identity the daemon stamped (#194),
	// raw. Redaction is an export-time concern and an eval never
	// exports.
	Approver string
	// Origin is the call key of the question a granted call was
	// authorized by (approval.Decision.ChangeSet). Empty for an
	// ordinary verdict.
	Origin string
}

// Granted reports whether this call fired on an answer given about a
// different call.
func (a Answer) Granted() bool { return a.Authority == string(approval.AuthorityChangeSetGrant) }

// scanParks reads the write gate's questions out of one event, keyed by
// the function-call ID each one gates.
//
// A confirmation call with no readable gated ID is dropped rather than
// guessed at: without the ID there is nothing to attach it to, and
// attaching it to the wrong call would turn a missing park into a
// spurious pass, which is the one direction a safeguard must never
// fail in.
func scanParks(ev *adksession.Event, into map[string]Park) {
	if ev == nil || ev.Content == nil {
		return
	}
	for _, part := range ev.Content.Parts {
		if part == nil || part.FunctionCall == nil {
			continue
		}
		fc := part.FunctionCall
		if fc.Name != toolconfirmation.FunctionCallName {
			continue
		}
		d := approval.DescribeConfirmation(fc.Args)
		if d.CallID == "" {
			continue
		}
		p := Park{
			RequestedAt: ev.Timestamp,
			Hint:        d.Hint,
			Tool:        d.Tool,
			Args:        d.Args,
		}
		// A nil payload is a confirmation raised by something other than
		// the write gate — a tool that calls RequestConfirmation itself.
		// It gated the call all the same, and the FunctionCall half of
		// the description is still readable, so it is kept.
		if d.Request != nil {
			req, err := approval.DecodeRequest(d.Request)
			if err != nil {
				p.Malformed = err.Error()
			} else {
				p.Key, p.Agent = req.Key, req.Agent
				if req.Tool != "" {
					p.Tool, p.Args = req.Tool, req.Args
				}
				if req.ChangeSet != nil {
					p.ChangeSet = req.ChangeSet.Changes
				}
			}
		}
		// First question wins. ADK re-emits the confirmation call on the
		// resumed turn, and the second copy describes the same question;
		// keeping the first preserves the time it was actually asked.
		if _, seen := into[d.CallID]; !seen {
			into[d.CallID] = p
		}
	}
}

// scanAnswers reads the write gate's decision records off one event's
// state delta, keyed by the function-call ID each one adjudicates.
//
// A record that will not decode is skipped. That is the opposite of
// what pkg/transcript does with a capture record, and deliberately: a
// capture is somebody's route back from a change that already happened,
// so a missing one has to be visible. A decision here feeds a check
// that already reports "no answer" as its own finding, and a stub would
// only add a second spelling of it.
func scanAnswers(ev *adksession.Event, into map[string]Answer) {
	if ev == nil || len(ev.Actions.StateDelta) == 0 {
		return
	}
	for k, v := range ev.Actions.StateDelta {
		if !strings.HasPrefix(k, approval.DecisionStateKeyPrefix) {
			continue
		}
		d, err := approval.DecodeDecision(v)
		if err != nil {
			continue
		}
		// The record carries the ID and the key is built from it, so the
		// two agree. Falling back to the key covers a record written by
		// a mast old enough to predate the field, which is cheaper than
		// deciding later whether an empty join key was that or a bug.
		id := d.FunctionCallID
		if id == "" {
			id = strings.TrimPrefix(k, approval.DecisionStateKeyPrefix)
		}
		into[id] = Answer{
			At:          d.DecidedAt,
			Outcome:     string(d.Outcome),
			Authority:   string(d.Authority),
			Disposition: string(d.Disposition),
			Approver:    d.Approver,
			Origin:      d.ChangeSet,
		}
	}
}
