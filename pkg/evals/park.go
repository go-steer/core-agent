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

package evals

import (
	"strings"
	"time"

	adksession "google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool/toolconfirmation"

	"github.com/go-steer/core-agent/v3/pkg/approval"
)

// ProposedChange re-exports approval.ProposedChange so callers constructing or
// inspecting Park.ChangeSet do not need to import pkg/approval directly.
type ProposedChange = approval.ProposedChange

// Park is the question the write gate wrote into the durable log before
// a mutating call could run.
type Park struct {
	// RequestedAt is when the gate parked, from the event's timestamp.
	RequestedAt time.Time
	// Hint is the one line the gate wrote for a human.
	Hint string
	// Tool and Args are the call as the question described it.
	Tool string
	Args map[string]any
	// Key is approval.CallKey of the same call.
	Key string
	// Agent is the agent whose call was parked.
	Agent string
	// ChangeSet is the set of calls the operator was offered alongside
	// this one, nil when the question carried none.
	ChangeSet []ProposedChange
	// Malformed is set when the confirmation call was core-agent's but its
	// payload would not decode.
	Malformed string
}

// Answer is the gate's durable record of how a parked call was
// resolved: approval.Decision, narrowed to what a check can use.
type Answer struct {
	// At is when the gate adjudicated.
	At time.Time
	// Outcome is what the operator said: approve, reject, edit.
	Outcome string
	// Authority is where the authorization came from: operator_verdict
	// or change_set_grant.
	Authority string
	// Disposition is what the gate then did.
	Disposition string
	// Approver is the authenticated identity the daemon stamped (#194).
	Approver string
	// Origin is the call key of the question a granted call was
	// authorized by (approval.Decision.ChangeSet).
	Origin string
}

// Granted reports whether this call fired on an answer given about a
// different call.
func (a Answer) Granted() bool { return a.Authority == string(approval.AuthorityChangeSetGrant) }

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
		if _, seen := into[d.CallID]; !seen {
			into[d.CallID] = p
		}
	}
}

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
