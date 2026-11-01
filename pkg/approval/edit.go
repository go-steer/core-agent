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

	"github.com/google/jsonschema-go/jsonschema"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"
)

// EditStateKeyPrefix namespaces the durable record of an applied edit in
// the session's state, one key per function call.
const EditStateKeyPrefix = "mast_approval_edit_"

// EditStateKey is the state key holding the AppliedEdit record for one
// function call.
func EditStateKey(functionCallID string) string {
	return EditStateKeyPrefix + functionCallID
}

// AppliedEdit is the durable record of an operator's edit being applied:
// what the model asked for, what actually ran, and who authorized the
// substitution.
type AppliedEdit struct {
	Tool         string         `json:"tool"`
	Approver     string         `json:"approver"`
	ProposedKey  string         `json:"proposed_key"`
	ExecutedKey  string         `json:"executed_key"`
	ProposedArgs map[string]any `json:"proposed_args"`
	ExecutedArgs map[string]any `json:"executed_args"`
	Note         string         `json:"note,omitempty"`
}

// String renders the record for an operator-facing listing.
func (e AppliedEdit) String() string {
	s := e.ExecutedKey + " (proposed " + e.ProposedKey + ")"
	if e.Approver != "" {
		s += " approved by " + e.Approver
	}
	if e.Note != "" {
		s += ": " + e.Note
	}
	return s
}

// DecodeAppliedEdit parses a record written under an EditStateKey.
func DecodeAppliedEdit(v any) (AppliedEdit, error) {
	var raw []byte
	switch t := v.(type) {
	case string:
		raw = []byte(t)
	case []byte:
		raw = t
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return AppliedEdit{}, fmt.Errorf("approval: re-marshalling applied-edit record: %w", err)
		}
		raw = b
	}
	var e AppliedEdit
	if err := json.Unmarshal(raw, &e); err != nil {
		return AppliedEdit{}, fmt.Errorf("approval: applied-edit record is not one: %w", err)
	}
	return e, nil
}

type declarer interface {
	Declaration() *genai.FunctionDeclaration
}

func normalizeEdit(t tool.Tool, edited map[string]any) (map[string]any, error) {
	if len(edited) == 0 {
		return nil, fmt.Errorf("the verdict is %q but carries no arguments", OutcomeEdit)
	}
	schema, err := InputSchema(t)
	if err != nil {
		return nil, err
	}
	norm, err := NormalizeArgs(t.Name(), schema, edited)
	if err != nil {
		return nil, fmt.Errorf("edited %w", err)
	}
	return norm, nil
}

// NormalizeArgs validates one call's arguments against the tool's
// declared input schema and returns them in the shape the tool will
// receive.
func NormalizeArgs(toolName string, schema *jsonschema.Schema, args map[string]any) (map[string]any, error) {
	if schema == nil {
		return nil, fmt.Errorf("tool %q has no input schema here, so its arguments cannot be checked against anything", toolName)
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return nil, fmt.Errorf("arguments for tool %q are not JSON: %w", toolName, err)
	}
	var norm map[string]any
	if err := json.Unmarshal(raw, &norm); err != nil {
		return nil, fmt.Errorf("arguments for tool %q are not a JSON object: %w", toolName, err)
	}
	if norm == nil {
		norm = map[string]any{}
	}
	if err := checkDeclaredKeys(schema, norm); err != nil {
		return nil, fmt.Errorf("arguments %w", err)
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		return nil, fmt.Errorf("tool %q input schema does not resolve: %w", toolName, err)
	}
	if err := resolved.Validate(norm); err != nil {
		return nil, fmt.Errorf("arguments do not satisfy tool %q's input schema: %w", toolName, err)
	}
	return norm, nil
}

// InputSchema reads a tool's declared parameters as a JSON Schema.
func InputSchema(t tool.Tool) (*jsonschema.Schema, error) {
	d, ok := t.(declarer)
	if !ok {
		return nil, fmt.Errorf("tool %q does not declare its arguments, so its arguments cannot be checked against anything", t.Name())
	}
	decl := d.Declaration()
	if decl == nil {
		return nil, fmt.Errorf("tool %q has no declaration, so its arguments cannot be checked against anything", t.Name())
	}
	var src any
	switch {
	case decl.ParametersJsonSchema != nil:
		src = decl.ParametersJsonSchema
	case decl.Parameters != nil:
		src = decl.Parameters
	default:
		return nil, fmt.Errorf("tool %q declares no input schema, so its arguments cannot be checked against anything", t.Name())
	}
	raw, err := json.Marshal(src)
	if err != nil {
		return nil, fmt.Errorf("tool %q input schema is not marshallable: %w", t.Name(), err)
	}
	schema := &jsonschema.Schema{}
	if err := json.Unmarshal(raw, schema); err != nil {
		return nil, fmt.Errorf("tool %q input schema is not a JSON Schema: %w", t.Name(), err)
	}
	return schema, nil
}

func checkDeclaredKeys(schema *jsonschema.Schema, args map[string]any) error {
	if len(schema.Properties) == 0 {
		return nil
	}
	var unknown []string
	for k := range args {
		if _, ok := schema.Properties[k]; !ok {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	declared := make([]string, 0, len(schema.Properties))
	for k := range schema.Properties {
		declared = append(declared, k)
	}
	sort.Strings(declared)
	return fmt.Errorf("name %s, which the tool does not declare (it declares %s)",
		strings.Join(unknown, ", "), strings.Join(declared, ", "))
}

func applyArgs(args, edited map[string]any) {
	for k := range args {
		delete(args, k)
	}
	for k, v := range edited {
		args[k] = v
	}
}
