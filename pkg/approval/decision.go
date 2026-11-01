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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// DecisionStateKeyPrefix namespaces the durable record of one
// adjudication in the session's state, one key per function call.
const DecisionStateKeyPrefix = "mast_decision_"

// DecisionStateKey is the state key holding the Decision record for one
// function call.
func DecisionStateKey(functionCallID string) string {
	return DecisionStateKeyPrefix + functionCallID
}

// DecisionSchema names the record shape in an export's provenance
// header.
const DecisionSchema = "mast.decision/v1"

// Disposition is what the gate did with the call, as distinct from what
// the operator asked for.
type Disposition string

const (
	// DispositionAuthorized means the gate let the call through.
	DispositionAuthorized Disposition = "authorized"

	// DispositionRefusedByOperator means a person said no.
	DispositionRefusedByOperator Disposition = "refused_by_operator"

	// DispositionRefusedByMast means the write gate refused a verdict a
	// person gave.
	DispositionRefusedByMast Disposition = "refused_by_mast"
)

// Authority is where the authorization for this call came from.
type Authority string

const (
	// AuthorityVerdict is the ordinary case: a person was asked about
	// this exact call and answered it.
	AuthorityVerdict Authority = "operator_verdict"

	// AuthorityChangeSetGrant is a call that fired on an answer given
	// earlier about a different call in the same change set (W7).
	AuthorityChangeSetGrant Authority = "change_set_grant"
)

// Decision is the durable record of one adjudication of one mutating
// call: what was proposed, what the operator answered, what actually
// ran, and who decided.
type Decision struct {
	DecidedAt      time.Time      `json:"decided_at"`
	Session        string         `json:"session"`
	Workload       string         `json:"workload,omitempty"`
	Specialist     string         `json:"specialist,omitempty"`
	Invocation     string         `json:"invocation,omitempty"`
	FunctionCallID string         `json:"function_call_id,omitempty"`
	Tool           string         `json:"tool"`
	Outcome        Outcome        `json:"outcome,omitempty"`
	Scope          Scope          `json:"scope,omitempty"`
	Authority      Authority      `json:"authority"`
	Disposition    Disposition    `json:"disposition"`
	Refusal        string         `json:"refusal,omitempty"`
	ChangeSet      string         `json:"change_set,omitempty"`
	ProposedKey    string         `json:"proposed_key"`
	ProposedArgs   map[string]any `json:"proposed_args,omitempty"`
	ExecutedKey    string         `json:"executed_key,omitempty"`
	ExecutedArgs   map[string]any `json:"executed_args,omitempty"`
	Approver       string         `json:"approver,omitempty"`
	Note           string         `json:"note,omitempty"`
}

// Edited reports whether the operator substituted their own arguments.
func (d Decision) Edited() bool { return d.ExecutedKey != "" && d.ExecutedKey != d.ProposedKey }

// EncodeDecision renders the record for storage.
func EncodeDecision(d Decision) (string, error) {
	raw, err := json.Marshal(d)
	if err != nil {
		return "", fmt.Errorf("approval: encoding decision record: %w", err)
	}
	return string(raw), nil
}

// DecodeDecision parses a record written under a DecisionStateKey.
func DecodeDecision(v any) (Decision, error) {
	var raw []byte
	switch t := v.(type) {
	case string:
		raw = []byte(t)
	case []byte:
		raw = t
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return Decision{}, fmt.Errorf("approval: re-marshalling decision record: %w", err)
		}
		raw = b
	}
	var d Decision
	if err := json.Unmarshal(raw, &d); err != nil {
		return Decision{}, fmt.Errorf("approval: decision record is not one: %w", err)
	}
	return d, nil
}

// MachineApproverPrefix marks an identity that names a mechanism rather
// than a person.
const MachineApproverPrefix = "mast:"

// RedactedApproverPrefix marks a digested identity in an export.
const RedactedApproverPrefix = "sha256:"

// RedactApprover replaces an approver identity with a stable digest of
// it.
func RedactApprover(approver string) string {
	if approver == "" {
		return ""
	}
	if strings.HasPrefix(approver, MachineApproverPrefix) || strings.HasPrefix(approver, "core-agent:") {
		return approver
	}
	sum := sha256.Sum256([]byte(approver))
	return RedactedApproverPrefix + hex.EncodeToString(sum[:])[:16]
}

// Redacted returns a copy of the record with the approver digested.
func (d Decision) Redacted() Decision {
	d.Approver = RedactApprover(d.Approver)
	return d
}
