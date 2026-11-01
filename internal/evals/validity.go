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

// Call validity (#169). The board records which tools were called and
// nothing about how, which leaves two very different failures looking
// identical: the model never called the tool, and the model called the
// tool with a namespace that matched nothing, read the empty result as
// "no problem here", and moved on. The first is a tool-selection
// problem. The second is a tool-*use* problem, and the fix for it is
// not the same one.
//
// This file scores the second. It is deliberately **not** a metric.
// Averaging "three calls were malformed" into a number between 0 and 1
// throws away the only part a reader can act on — which tool, which
// argument, what was sent — and a mean over a corpus whose rows call
// different numbers of tools is not comparable to itself run over run.
// Violations are enumerated and printed. See [Violation].

// Violation kinds. The split is by what a reader would do about it: the
// first four are the model sending something the tool cannot accept
// (a prompt or schema problem), the last two are the call being made
// correctly and the run still learning nothing (a fixture, scope, or
// reasoning problem).
const (
	// ViolationUnknownTool is a call to a name that is not in the
	// catalog. The runtime rejects it, so it costs a turn and teaches
	// the model nothing.
	ViolationUnknownTool = "unknown_tool"
	// ViolationMissingRequired is a required argument the call omitted.
	ViolationMissingRequired = "missing_required"
	// ViolationWrongType is an argument whose value does not match its
	// declared type.
	ViolationWrongType = "wrong_type"
	// ViolationNotInEnum is an argument whose value is outside the
	// declared enum.
	ViolationNotInEnum = "not_in_enum"
	// ViolationUndeclaredArg is an argument the schema does not
	// declare. Reported because a model inventing an argument name is
	// usually reaching for a capability the tool does not have, and
	// because what follows is not the call the model meant either way:
	// ADK's functiontool rejects the whole call ("unexpected additional
	// properties"), while a lenient MCP server drops the argument and
	// answers a narrower question than the one that was asked.
	ViolationUndeclaredArg = "undeclared_argument"

	// ViolationErrorResult is a call the tool answered with an error.
	ViolationErrorResult = "error_result"
	// ViolationEmptyResult is a well-formed call that returned nothing.
	// Not a defect on its own — a triage agent checking a hypothesis
	// and finding nothing is doing its job — but it is the shape of the
	// failure #169 exists to make visible, and per #162's
	// tool-failure-streak a run where the calls look ordinary and the
	// *results* are the story is a real mode we detect at runtime and
	// could not previously see in the eval.
	ViolationEmptyResult = "empty_result"
	// ViolationNoCompletion is a call with no recorded response at all:
	// it never ran, lost its completion to a crash, or was declined at
	// a gate.
	ViolationNoCompletion = "no_completion"
)

// Violation is one thing wrong with one recorded call.
type Violation struct {
	// CallIndex is the call's position in Trace.Calls, so a reader can
	// find it in the recorded log rather than guessing which of three
	// calls to the same tool is meant.
	CallIndex int    `json:"call_index"`
	Tool      string `json:"tool"`
	Kind      string `json:"kind"`
	// Arg is the offending argument, empty for the kinds that are about
	// the call as a whole.
	Arg    string `json:"arg,omitempty"`
	Detail string `json:"detail"`
}

func (v Violation) String() string {
	if v.Arg != "" {
		return fmt.Sprintf("call %d %s(%s): %s — %s", v.CallIndex, v.Tool, v.Arg, v.Kind, v.Detail)
	}
	return fmt.Sprintf("call %d %s: %s — %s", v.CallIndex, v.Tool, v.Kind, v.Detail)
}

// CallRecord is one recorded call in the form a report persists: what
// the model asked for, and a digest of what came back.
//
// A digest rather than the response itself, and that is a judgement
// call worth stating. A judge-tier reading is several hundred words of
// prose; keeping 31 scenarios' worth verbatim would turn the summary
// JSON into a transcript nobody opens, and the transcript already
// exists — the session store holds the full event log. What the board
// needs is enough to answer "did this call see anything", which the
// first line and the empty/error flags carry.
type CallRecord struct {
	Index int            `json:"index"`
	Tool  string         `json:"tool"`
	Args  map[string]any `json:"args,omitempty"`
	// Result is a one-line digest of the response, empty when no
	// completion was recorded.
	Result string `json:"result,omitempty"`
	// Completed distinguishes "the tool answered with nothing" from "the
	// tool never answered", which Result alone cannot.
	Completed bool `json:"completed"`
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

// digestResult is how much of a tool response a CallRecord keeps.
const digestResult = 200

// digestResponse renders a response as one short line.
func digestResponse(resp map[string]any) string {
	if len(resp) == 0 {
		return ""
	}
	if e := errorText(resp); e != "" {
		return truncate("error: " + collapse(e))
	}
	// A single-valued response — which every mast fixture tool and most
	// MCP tools return — reads better as its value than as a JSON object
	// wrapping it.
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
// declaration advertises — the same `properties`/`required`/`type`
// shape that reaches the provider. Held as generic JSON rather than a
// typed schema because that is what both ADK spellings decode to, and
// because a validator that only understood one of them would repeat
// #154's mistake in a new place.
type ToolSchemas map[string]map[string]any

// ValidateCalls checks every recorded call against its declared schema
// and returns the violations in call order, then kind order.
//
// A tool absent from schemas is reported as unknown rather than
// skipped. Skipping would make an incomplete schema map look like a
// clean run, which is the failure mode this whole file is a reaction
// to.
//
// emptyResult decides whether a completed call learned anything. It is
// a callback because "empty" is a property of the tool's own result
// shape, which this package cannot know: the fixture cluster's
// no-findings reading is prose, an MCP server's is likely an empty
// array. Nil means the caller is not making that claim, and no
// ViolationEmptyResult is ever reported — silence rather than a guess.
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

// errorText reports the error a tool response carries, if any. ADK
// puts a handler error under the "error" key of the FunctionResponse
// map (llminternal's base flow and functiontool both do), so that is
// the key read here.
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

// typeMismatch checks a value against a declared JSON Schema type.
//
// Only the types a tool declaration actually uses are checked, and
// anything unrecognized passes: this is a validity check on calls, not
// a JSON Schema implementation, and a validator that reported false
// violations on schema keywords it half-understood would be worse than
// none — the whole value of the report is that every line in it is
// real.
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

// enumMismatch checks a value against a declared enum. Values are
// compared by their rendered form, which is what a tool handler
// effectively does with a string enum and avoids the false negatives
// of comparing a float64 4 against an int 4 across a JSON round trip.
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

// describeArgs renders a call's arguments for a failure line. A
// violation a reader cannot act on is a violation they will learn to
// scroll past, and "which namespace did it ask for" is the whole
// question on an empty read.
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

// ViolationCounts tallies violations by kind, for a board line that
// says how much of a run was malformed without printing every
// instance. Sorted by kind so two runs' summaries line up.
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
