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
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
)

// ChangeSetField is the report property a change set travels in.
const ChangeSetField = "proposed_change"

// ChangeSetStateKeyPrefix namespaces the durable record of the change
// set one specialist proposed, one key per specialist.
const ChangeSetStateKeyPrefix = "mast_change_set_"

// ChangeSetStateKey is the state key holding a specialist's proposed
// change set.
func ChangeSetStateKey(specialist string) string {
	return ChangeSetStateKeyPrefix + specialist
}

// ProposedChange is one machine-executable remediation: a tool the
// workload declares, and the arguments to call it with.
type ProposedChange struct {
	Tool      string         `json:"tool"`
	Arguments map[string]any `json:"arguments"`
}

// UnmarshalJSON accepts `arguments` as either a JSON object or a JSON
// string holding one.
func (c *ProposedChange) UnmarshalJSON(raw []byte) error {
	var wire struct {
		Tool      string          `json:"tool"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return fmt.Errorf("approval: %s entry is not an object: %w", ChangeSetField, err)
	}
	c.Tool = wire.Tool
	c.Arguments = nil
	if len(wire.Arguments) == 0 || string(wire.Arguments) == "null" {
		return nil
	}
	body := wire.Arguments
	var asString string
	if err := json.Unmarshal(body, &asString); err == nil {
		trimmed := strings.TrimSpace(asString)
		if trimmed == "" {
			return nil
		}
		body = []byte(trimmed)
	}
	if err := json.Unmarshal(body, &c.Arguments); err != nil {
		return fmt.Errorf("approval: arguments for tool %q are not a JSON object: %w", c.Tool, err)
	}
	return nil
}

// Signature renders a change as a byte-stable identity for the exact
// call it proposes.
func Signature(toolName string, args map[string]any) (string, error) {
	if len(args) == 0 {
		return toolName + "({})", nil
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return "", fmt.Errorf("approval: arguments for tool %q cannot be encoded, so the call has no stable signature: %w", toolName, err)
	}
	return toolName + "(" + string(raw) + ")", nil
}

// Signature is the change's call signature. See Signature.
func (c ProposedChange) Signature() (string, error) { return Signature(c.Tool, c.Arguments) }

// ParseChangeSet reads the change set out of a specialist's structured
// report.
func ParseChangeSet(report map[string]any) ([]ProposedChange, error) {
	v, ok := report[ChangeSetField]
	if !ok || v == nil {
		return nil, nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("approval: %s is not encodable: %w", ChangeSetField, err)
	}
	var changes []ProposedChange
	if err := json.Unmarshal(raw, &changes); err != nil {
		return nil, fmt.Errorf("approval: %s is not a list of {tool, arguments} entries: %w", ChangeSetField, err)
	}
	return changes, nil
}

// ChangeSetChecker validates a proposed change set against the
// workload's catalog and the named tools' declared input schemas.
type ChangeSetChecker struct {
	Declares func(toolName string) bool
	Schema   func(toolName string) (*jsonschema.Schema, error)
}

// Check validates every entry and returns the change set with each
// entry's arguments normalized into the shape the tool will receive.
func (c ChangeSetChecker) Check(changes []ProposedChange) ([]ProposedChange, error) {
	if c.Declares == nil || c.Schema == nil {
		return nil, fmt.Errorf("approval: ChangeSetChecker needs both Declares and Schema")
	}
	out := make([]ProposedChange, 0, len(changes))
	for i, ch := range changes {
		name := strings.TrimSpace(ch.Tool)
		if name == "" {
			return nil, fmt.Errorf("%s[%d] names no tool", ChangeSetField, i)
		}
		if !c.Declares(name) {
			return nil, fmt.Errorf("%s[%d] names tool %q, which this workload's tool_catalog does not declare; propose a call this workload can make, or return an empty %s and say why in escalate",
				ChangeSetField, i, name, ChangeSetField)
		}
		schema, err := c.Schema(name)
		if err != nil {
			return nil, fmt.Errorf("%s[%d]: %w", ChangeSetField, i, err)
		}
		norm, err := NormalizeArgs(name, schema, ch.Arguments)
		if err != nil {
			return nil, fmt.Errorf("%s[%d]: %w", ChangeSetField, i, err)
		}
		out = append(out, ProposedChange{Tool: name, Arguments: norm})
	}
	return out, nil
}

// FinishTaskToolName is the completion tool ADK auto-installs on every
// Task-mode agent.
const FinishTaskToolName = "finish_task"

func (g *writeGate) checkChangeSet(ctx agent.Context, t tool.Tool, args map[string]any) map[string]any {
	if g.cfg.ChangeSet == nil || t.Name() != FinishTaskToolName {
		return nil
	}
	changes, err := ParseChangeSet(args)
	if err == nil && len(changes) > 0 {
		changes, err = g.cfg.ChangeSet.Check(changes)
	}
	if err != nil {
		g.audit(ctx, t, FinishTaskToolName, "change_set_refused", err.Error())
		return map[string]any{
			"error": "invalid_proposed_change",
			"detail": "Your report was NOT accepted: " + err.Error() + ". " +
				"Fix that one entry and call finish_task again. `" + ChangeSetField + "` must name a tool this workload declares and carry that tool's own arguments, " +
				"encoded as a JSON object in a string. If you cannot name an exact executable call, send an empty list and say what is missing in `escalate` — " +
				"that is a valid report, and inventing a plausible-looking call is not.",
		}
	}
	if len(changes) == 0 {
		return nil
	}
	g.recordChangeSet(ctx, changes)
	return nil
}

func (g *writeGate) recordChangeSet(ctx agent.Context, changes []ProposedChange) {
	raw, err := EncodeChangeSet(changes)
	if err != nil {
		g.cfg.Logger.Error("write gate: a specialist proposed a change set that could not be recorded",
			"specialist", ctx.AgentName(), "error", err.Error())
		return
	}
	if a := ctx.Actions(); a != nil {
		if a.StateDelta == nil {
			a.StateDelta = map[string]any{}
		}
		a.StateDelta[ChangeSetStateKey(ctx.AgentName())] = raw
	}
	sigs := make([]string, 0, len(changes))
	for _, ch := range changes {
		if sig, err := ch.Signature(); err == nil {
			sigs = append(sigs, sig)
		}
	}
	g.cfg.Logger.Info("change set proposed",
		"specialist", ctx.AgentName(), "changes", len(changes),
		"calls", strings.Join(sigs, "; "),
		"session", ctx.SessionID(), "invocation", ctx.InvocationID())
}

// DescribeChangeSet renders a change set for a prompt or an operator
// message: one canonical signature per line, in list order.
func DescribeChangeSet(changes []ProposedChange) string {
	lines := make([]string, 0, len(changes))
	for i, ch := range changes {
		sig, err := ch.Signature()
		if err != nil {
			sig = ch.Tool + "(<unencodable arguments>)"
		}
		lines = append(lines, fmt.Sprintf("%d. %s", i+1, sig))
	}
	return strings.Join(lines, "\n")
}

// DecodeChangeSet reads a change set back out of durable state.
func DecodeChangeSet(v any) ([]ProposedChange, error) {
	if v == nil {
		return nil, nil
	}
	var raw []byte
	switch t := v.(type) {
	case string:
		if strings.TrimSpace(t) == "" {
			return nil, nil
		}
		raw = []byte(t)
	case []byte:
		raw = t
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("approval: re-marshalling change-set record: %w", err)
		}
		raw = b
	}
	var changes []ProposedChange
	if err := json.Unmarshal(raw, &changes); err != nil {
		return nil, fmt.Errorf("approval: change-set record is not one: %w", err)
	}
	return changes, nil
}

// EncodeChangeSet renders a change set for durable state.
func EncodeChangeSet(changes []ProposedChange) (string, error) {
	raw, err := json.Marshal(changes)
	if err != nil {
		return "", fmt.Errorf("approval: encoding change-set record: %w", err)
	}
	return string(raw), nil
}
