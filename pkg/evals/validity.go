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
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
)

// Violation kinds for ValidateCalls (#169).
const (
	ViolationUnknownTool     = "unknown_tool"
	ViolationMissingRequired = "missing_required"
	ViolationWrongType       = "wrong_type"
	ViolationNotInEnum       = "not_in_enum"
	ViolationUndeclaredArg   = "undeclared_argument"
	ViolationErrorResult     = "error_result"
	ViolationEmptyResult     = "empty_result"
	ViolationNoCompletion    = "no_completion"
)

// Violation is one thing wrong with one recorded call.
type Violation struct {
	CallIndex int    `json:"call_index"`
	Tool      string `json:"tool"`
	Kind      string `json:"kind"`
	Arg       string `json:"arg,omitempty"`
	Detail    string `json:"detail"`
}

func (v Violation) String() string {
	if v.Arg != "" {
		return fmt.Sprintf("call %d %s(%s): %s — %s", v.CallIndex, v.Tool, v.Arg, v.Kind, v.Detail)
	}
	return fmt.Sprintf("call %d %s: %s — %s", v.CallIndex, v.Tool, v.Kind, v.Detail)
}

// CallRecord is one recorded call in the form a report persists: what
// the model asked for, and a digest of what came back.
type CallRecord struct {
	Index     int            `json:"index"`
	Tool      string         `json:"tool"`
	Args      map[string]any `json:"args,omitempty"`
	Result    string         `json:"result,omitempty"`
	Completed bool           `json:"completed"`
}

// RecordCalls renders a trace's calls for persistence.
func RecordCalls(calls []Call) []CallRecord {
	if len(calls) == 0 {
		return nil
	}
	out := make([]CallRecord, 0, len(calls))
	for i, c := range calls {
		out = append(out, CallRecord{
			Index:     i,
			Tool:      c.Name,
			Args:      c.Args,
			Result:    digestResponse(c.Response),
			Completed: c.Completed,
		})
	}
	return out
}

const digestResult = 200

func digestResponse(resp map[string]any) string {
	if len(resp) == 0 {
		return ""
	}
	if e := errorText(resp); e != "" {
		return truncate("error: " + collapse(e))
	}
	if len(resp) == 1 {
		for _, v := range resp {
			if s, ok := v.(string); ok {
				return truncate(collapse(s))
			}
		}
	}
	b, err := json.Marshal(resp)
	if err != nil {
		return truncate(collapse(fmt.Sprint(resp)))
	}
	return truncate(collapse(string(b)))
}

func collapse(s string) string { return strings.Join(strings.Fields(s), " ") }

func truncate(s string) string {
	r := []rune(s)
	if len(r) <= digestResult {
		return s
	}
	return string(r[:digestResult]) + "…"
}

// ToolSchemas maps a tool name to the JSON Schema object its
// declaration advertises.
type ToolSchemas map[string]map[string]any

// ValidateCalls checks every recorded call against its declared schema
// and returns the violations in call order, then kind order.
func ValidateCalls(schemas ToolSchemas, calls []Call, emptyResult func(Call) bool) []Violation {
	var out []Violation
	add := func(i int, tool, kind, arg, detail string) {
		out = append(out, Violation{CallIndex: i, Tool: tool, Kind: kind, Arg: arg, Detail: detail})
	}

	for i, c := range calls {
		schema, known := schemas[c.Name]
		if !known {
			add(i, c.Name, ViolationUnknownTool, "",
				fmt.Sprintf("no such tool in the catalog of %d; the runtime rejects the call and the turn is spent", len(schemas)))
			continue
		}

		props, _ := schema["properties"].(map[string]any)
		for _, want := range requiredOf(schema) {
			if _, sent := c.Args[want]; !sent {
				add(i, c.Name, ViolationMissingRequired, want,
					fmt.Sprintf("declared required; the call sent %s", describeArgs(c.Args)))
			}
		}
		for _, arg := range sortedArgNames(c.Args) {
			spec, declared := props[arg].(map[string]any)
			if !declared {
				add(i, c.Name, ViolationUndeclaredArg, arg,
					fmt.Sprintf("not in the declared schema (%s); the tool rejects the call or drops the argument, so it is not the call the model meant", describeProps(props)))
				continue
			}
			if detail, bad := typeMismatch(spec, c.Args[arg]); bad {
				add(i, c.Name, ViolationWrongType, arg, detail)
				continue
			}
			if detail, bad := enumMismatch(spec, c.Args[arg]); bad {
				add(i, c.Name, ViolationNotInEnum, arg, detail)
			}
		}

		switch {
		case !c.Completed:
			add(i, c.Name, ViolationNoCompletion, "",
				"no response was recorded: the call never ran, lost its completion, or was declined at a gate")
		case errorText(c.Response) != "":
			add(i, c.Name, ViolationErrorResult, "",
				fmt.Sprintf("the tool answered with an error: %s", errorText(c.Response)))
		case emptyResult != nil && emptyResult(c):
			add(i, c.Name, ViolationEmptyResult, "",
				fmt.Sprintf("well-formed call, nothing found: %s. The run learned nothing here, which reads on the board as an unsatisfied intent and is not one",
					describeArgs(c.Args)))
		}
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].CallIndex != out[j].CallIndex {
			return out[i].CallIndex < out[j].CallIndex
		}
		return out[i].Kind < out[j].Kind
	})
	return out
}

func errorText(resp map[string]any) string {
	v, ok := resp["error"]
	if !ok || v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		if strings.TrimSpace(t) == "" {
			return ""
		}
		return t
	case error:
		return t.Error()
	default:
		return fmt.Sprint(t)
	}
}

func typeMismatch(spec map[string]any, v any) (string, bool) {
	want, _ := spec["type"].(string)
	if want == "" || v == nil {
		return "", false
	}
	got := jsonTypeOf(v)
	switch want {
	case "string", "boolean", "object", "array":
		if got != want {
			return fmt.Sprintf("declared %s, sent %s (%v)", want, got, v), true
		}
	case "number":
		if got != "number" {
			return fmt.Sprintf("declared number, sent %s (%v)", got, v), true
		}
	case "integer":
		f, ok := v.(float64)
		if !ok {
			if n, isInt := v.(int); isInt {
				f, ok = float64(n), true
			}
		}
		if !ok {
			return fmt.Sprintf("declared integer, sent %s (%v)", got, v), true
		}
		if f != math.Trunc(f) {
			return fmt.Sprintf("declared integer, sent the fractional value %v", v), true
		}
	}
	return "", false
}

func enumMismatch(spec map[string]any, v any) (string, bool) {
	raw, ok := spec["enum"].([]any)
	if !ok || len(raw) == 0 {
		return "", false
	}
	got := fmt.Sprint(v)
	allowed := make([]string, 0, len(raw))
	for _, a := range raw {
		s := fmt.Sprint(a)
		if s == got {
			return "", false
		}
		allowed = append(allowed, s)
	}
	return fmt.Sprintf("sent %q; declared values are %s", got, strings.Join(allowed, ", ")), true
}

func jsonTypeOf(v any) string {
	switch v.(type) {
	case string:
		return "string"
	case bool:
		return "boolean"
	case float64, float32, int, int32, int64:
		return "number"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case nil:
		return "null"
	default:
		return fmt.Sprintf("%T", v)
	}
}

func requiredOf(schema map[string]any) []string {
	var out []string
	switch req := schema["required"].(type) {
	case []any:
		for _, v := range req {
			if s, ok := v.(string); ok {
				out = append(out, s)
			}
		}
	case []string:
		out = append(out, req...)
	}
	sort.Strings(out)
	return out
}

func sortedArgNames(args map[string]any) []string {
	out := make([]string, 0, len(args))
	for k := range args {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func describeArgs(args map[string]any) string {
	if len(args) == 0 {
		return "no arguments"
	}
	parts := make([]string, 0, len(args))
	for _, k := range sortedArgNames(args) {
		parts = append(parts, fmt.Sprintf("%s=%v", k, args[k]))
	}
	return strings.Join(parts, " ")
}

func describeProps(props map[string]any) string {
	if len(props) == 0 {
		return "the tool declares no arguments"
	}
	names := make([]string, 0, len(props))
	for k := range props {
		names = append(names, k)
	}
	sort.Strings(names)
	return "declared: " + strings.Join(names, ", ")
}

// ViolationCounts tallies violations by kind, sorted by kind.
func ViolationCounts(vs []Violation) []string {
	if len(vs) == 0 {
		return nil
	}
	by := map[string]int{}
	for _, v := range vs {
		by[v.Kind]++
	}
	kinds := make([]string, 0, len(by))
	for k := range by {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	out := make([]string, 0, len(kinds))
	for _, k := range kinds {
		out = append(out, fmt.Sprintf("%s=%d", k, by[k]))
	}
	return out
}
