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

	"github.com/google/jsonschema-go/jsonschema"
	"google.golang.org/genai"
)

// Parked describes a mutating call waiting on an operator, read back
// out of the durable session log.
type Parked struct {
	Hint    string
	CallID  string
	Tool    string
	Args    map[string]any
	Request map[string]any
}

// DescribeConfirmation reads a parked mutating call out of the Args of
// an adk_request_confirmation function call.
func DescribeConfirmation(args map[string]any) Parked {
	var p Parked
	switch fc := args["originalFunctionCall"].(type) {
	case *genai.FunctionCall:
		if fc != nil {
			p.CallID, p.Tool, p.Args = fc.ID, fc.Name, fc.Args
		}
	case genai.FunctionCall:
		p.CallID, p.Tool, p.Args = fc.ID, fc.Name, fc.Args
	case map[string]any:
		p.CallID, _ = fc["id"].(string)
		p.Tool, _ = fc["name"].(string)
		p.Args, _ = fc["args"].(map[string]any)
	}
	if tc, ok := args["toolConfirmation"]; ok {
		var decoded struct {
			Hint    string         `json:"hint"`
			Payload map[string]any `json:"payload"`
		}
		if raw, err := json.Marshal(tc); err == nil {
			if err := json.Unmarshal(raw, &decoded); err == nil {
				p.Hint = decoded.Hint
				p.Request = decoded.Payload
			}
		}
	}
	return p
}

// Summary is the operator-facing one-liner for a parked call.
func (p Parked) Summary() string {
	switch {
	case p.Hint != "":
		return p.Hint
	case p.Tool != "":
		return fmt.Sprintf("Approve mutating call %s?", CallKey(p.Tool, p.Args))
	}
	return "Approve a mutating tool call? (the parked call could not be read from the event log)"
}

// VerdictSchema is the JSON schema a resume payload answering a parked
// mutating call must satisfy.
func VerdictSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type:        "object",
		Description: "Operator verdict on a parked mutating tool call.",
		Properties: map[string]*jsonschema.Schema{
			"verdict": {
				Type:        "string",
				Enum:        []any{string(OutcomeApprove), string(OutcomeReject), string(OutcomeEdit)},
				Description: "approve runs the call as proposed; reject refuses it; edit runs it with the arguments in `args`, which must validate against the tool's own input schema and are re-checked against policy before they run.",
			},
			"scope": {
				Type:        "string",
				Enum:        []any{string(ScopeOnce), string(ScopeChangeSet)},
				Description: "How far the approval reaches. `once` authorizes this call. `change_set` additionally authorizes the other calls listed under the request's `change_set`, each bound to its exact arguments, expiring, and re-checked against the cluster before it fires; it is admissible only when the request carries one. Anything else is refused, not narrowed.",
			},
			"args": {
				Type:        "object",
				Description: "edit only: the arguments to run instead of the agent's.",
			},
			"note": {
				Type:        "string",
				Description: "Optional free text; shown to the agent.",
			},
		},
		Required: []string{"verdict"},
	}
}
