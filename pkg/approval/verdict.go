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
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"google.golang.org/adk/v2/tool/toolconfirmation"

	"github.com/go-steer/core-agent/v3/pkg/permissions"
)

// Outcome is the operator's answer to a parked mutating call.
type Outcome string

const (
	// OutcomeApprove runs the call as the agent proposed it.
	OutcomeApprove Outcome = "approve"
	// OutcomeReject refuses the call.
	OutcomeReject Outcome = "reject"
	// OutcomeEdit runs the call with arguments the operator supplied in
	// place of the agent's.
	OutcomeEdit Outcome = "edit"
)

// Scope is how far an approval reaches.
type Scope string

const (
	// ScopeOnce authorizes exactly this call.
	ScopeOnce Scope = "once"
	// ScopeSession asks to authorize this exact request for the session.
	ScopeSession Scope = "session"
	// ScopeSessionTool asks to authorize every call to this tool for the
	// session.
	ScopeSessionTool Scope = "session_tool"
	// ScopeAlways asks to persist a standing allowlist entry.
	ScopeAlways Scope = "always"
	// ScopeChangeSet authorizes this call and the other calls in the
	// change set it belongs to.
	ScopeChangeSet Scope = "change_set"
)

var scopeDecisions = map[Scope]permissions.Decision{
	ScopeOnce:        permissions.DecisionAllowOnce,
	ScopeSession:     permissions.DecisionAllowSession,
	ScopeSessionTool: permissions.DecisionAllowSessionTool,
	ScopeAlways:      permissions.DecisionAllowAlways,
	ScopeChangeSet:   permissions.DecisionAllowOnce,
}

// Verdict is the operator's answer, carried in the Payload of the ADK
// tool confirmation that resumes a parked call.
type Verdict struct {
	Verdict  Outcome        `json:"verdict"`
	Scope    Scope          `json:"scope,omitempty"`
	Args     map[string]any `json:"args,omitempty"`
	Note     string         `json:"note,omitempty"`
	Approver string         `json:"approver,omitempty"`
}

// Decision maps the verdict's scope onto the permissions decision the
// gate adjudicates.
func (v Verdict) Decision() (permissions.Decision, error) {
	if v.Verdict == OutcomeReject {
		return permissions.DecisionDeny, nil
	}
	scope := v.Scope
	if scope == "" {
		scope = ScopeOnce
	}
	d, ok := scopeDecisions[scope]
	if !ok {
		return permissions.DecisionDeny, fmt.Errorf("unknown approval scope %q (want one of %s)", v.Scope, knownScopes())
	}
	return d, nil
}

func knownScopes() string {
	names := make([]string, 0, len(scopeDecisions))
	for s := range scopeDecisions {
		names = append(names, string(s))
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// ConfirmationResponse builds the FunctionResponse payload that answers
// a parked mutating call.
func ConfirmationResponse(v Verdict) map[string]any {
	return map[string]any{
		"confirmed": v.Verdict != OutcomeReject,
		"payload":   v,
	}
}

// DecodeVerdict reads the verdict record out of an ADK tool
// confirmation.
func DecodeVerdict(c *toolconfirmation.ToolConfirmation) (Verdict, error) {
	if c == nil {
		return Verdict{}, fmt.Errorf("approval: no tool confirmation to decode")
	}
	v := Verdict{}
	if c.Payload != nil {
		raw, err := json.Marshal(c.Payload)
		if err != nil {
			return Verdict{}, fmt.Errorf("approval: re-marshalling confirmation payload: %w", err)
		}
		if err := json.Unmarshal(raw, &v); err != nil {
			return Verdict{}, fmt.Errorf("approval: confirmation payload is not a verdict record: %w", err)
		}
	}
	if v.Verdict == "" {
		if c.Confirmed {
			v.Verdict = OutcomeApprove
		} else {
			v.Verdict = OutcomeReject
		}
		return v, nil
	}
	switch v.Verdict {
	case OutcomeApprove, OutcomeEdit:
		if !c.Confirmed {
			return Verdict{}, fmt.Errorf("approval: contradictory verdict: %q with confirmed=false", v.Verdict)
		}
	case OutcomeReject:
		if c.Confirmed {
			return Verdict{}, fmt.Errorf("approval: contradictory verdict: %q with confirmed=true", v.Verdict)
		}
	default:
		return Verdict{}, fmt.Errorf("approval: unknown verdict %q (want approve, reject, or edit)", v.Verdict)
	}
	return v, nil
}
