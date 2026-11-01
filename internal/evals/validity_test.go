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
	"testing"
)

// validitySchemas is one tool with every declared shape a fixture or
// MCP server realistically advertises, and one that takes no arguments
// at all. The second is not filler: a tool with no properties is the
// case where "the model sent something" and "the schema declares
// nothing" collide, and it is exactly the shape #154 made every tool
// look like.
var validitySchemas = ToolSchemas{
	"read_logs": {
		"type": "object",
		"properties": map[string]any{
			"scope":  map[string]any{"type": "string"},
			"limit":  map[string]any{"type": "integer"},
			"level":  map[string]any{"type": "string", "enum": []any{"info", "warn", "error"}},
			"follow": map[string]any{"type": "boolean"},
		},
		"required": []any{"scope"},
	},
	"cluster_health": {"type": "object"},
}

func done(name string, args, resp map[string]any) Call {
	return Call{Name: name, Args: args, ID: name + "-1", Completed: true, ResponseIndex: 1, Response: resp}
}

func kinds(vs []Violation) []string {
	out := make([]string, 0, len(vs))
	for _, v := range vs {
		out = append(out, v.Kind)
	}
	return out
}

func TestValidateCalls_AWellFormedCallIsSilent(t *testing.T) {
	calls := []Call{
		done("read_logs", map[string]any{"scope": "production/api", "limit": float64(50), "level": "error"}, map[string]any{"reading": "3 lines"}),
		done("cluster_health", map[string]any{}, map[string]any{"reading": "green"}),
	}
	if got := ValidateCalls(validitySchemas, calls, nil); len(got) != 0 {
		t.Errorf("a clean run reported %d violation(s): %v", len(got), got)
	}
}

// TestValidateCalls_EveryMalformedShape walks the kinds one at a time.
// Each case is a real thing a model does — invents a tool, forgets the
// one required argument, sends a string where a number goes, picks an
// enum value that reads sensibly and is not in the list, invents an
// argument for a capability the tool does not have.
func TestValidateCalls_EveryMalformedShape(t *testing.T) {
	for _, tc := range []struct {
		name string
		call Call
		want string
		arg  string
		// detail is a substring the message must carry. A violation a
		// reader cannot act on is one they will learn to scroll past, so
		// the actionable half is asserted rather than assumed.
		detail string
	}{{
		name:   "a tool that does not exist",
		call:   done("kubectl_delete_pod", map[string]any{"scope": "production"}, map[string]any{"ok": true}),
		want:   ViolationUnknownTool,
		detail: "catalog of 2",
	}, {
		name:   "the required argument omitted",
		call:   done("read_logs", map[string]any{"limit": float64(10)}, map[string]any{"reading": "x"}),
		want:   ViolationMissingRequired,
		arg:    "scope",
		detail: "limit=10",
	}, {
		name:   "a string where an integer is declared",
		call:   done("read_logs", map[string]any{"scope": "p", "limit": "fifty"}, map[string]any{"reading": "x"}),
		want:   ViolationWrongType,
		arg:    "limit",
		detail: "declared integer",
	}, {
		name:   "a fractional value for an integer",
		call:   done("read_logs", map[string]any{"scope": "p", "limit": 1.5}, map[string]any{"reading": "x"}),
		want:   ViolationWrongType,
		arg:    "limit",
		detail: "fractional",
	}, {
		name:   "a string where a boolean is declared",
		call:   done("read_logs", map[string]any{"scope": "p", "follow": "yes"}, map[string]any{"reading": "x"}),
		want:   ViolationWrongType,
		arg:    "follow",
		detail: "declared boolean, sent string",
	}, {
		name:   "a plausible value outside the enum",
		call:   done("read_logs", map[string]any{"scope": "p", "level": "verbose"}, map[string]any{"reading": "x"}),
		want:   ViolationNotInEnum,
		arg:    "level",
		detail: "info, warn, error",
	}, {
		name:   "an argument the tool does not declare",
		call:   done("read_logs", map[string]any{"scope": "p", "since": "1h"}, map[string]any{"reading": "x"}),
		want:   ViolationUndeclaredArg,
		arg:    "since",
		detail: "declared: follow, level, limit, scope",
	}, {
		name:   "an argument sent to a tool that declares none",
		call:   done("cluster_health", map[string]any{"scope": "p"}, map[string]any{"reading": "x"}),
		want:   ViolationUndeclaredArg,
		arg:    "scope",
		detail: "the tool declares no arguments",
	}, {
		name:   "a call the tool answered with an error",
		call:   done("read_logs", map[string]any{"scope": "p"}, map[string]any{"error": "namespace p not found"}),
		want:   ViolationErrorResult,
		detail: "namespace p not found",
	}, {
		name:   "a call with no completion",
		call:   Call{Name: "read_logs", Args: map[string]any{"scope": "p"}, ID: "x", ResponseIndex: -1},
		want:   ViolationNoCompletion,
		detail: "declined at a gate",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			got := ValidateCalls(validitySchemas, []Call{tc.call}, nil)
			if len(got) != 1 {
				t.Fatalf("want exactly one %s, got %v", tc.want, got)
			}
			v := got[0]
			if v.Kind != tc.want {
				t.Errorf("kind = %q, want %q (%s)", v.Kind, tc.want, v)
			}
			if v.Arg != tc.arg {
				t.Errorf("arg = %q, want %q", v.Arg, tc.arg)
			}
			if !strings.Contains(v.Detail, tc.detail) {
				t.Errorf("detail does not say %q, so a reader cannot act on it:\n  %s", tc.detail, v)
			}
			if v.Tool != tc.call.Name {
				t.Errorf("tool = %q, want %q", v.Tool, tc.call.Name)
			}
		})
	}
}

// TestValidateCalls_AnAbsentToolIsUnknownNotSkipped is the whole reason
// this validator errs loud. An incomplete schema map that silently
// skipped its gaps would print a clean board — which is precisely how
// #154 shipped.
func TestValidateCalls_AnAbsentToolIsUnknownNotSkipped(t *testing.T) {
	got := ValidateCalls(ToolSchemas{}, []Call{done("read_logs", nil, map[string]any{"reading": "x"})}, nil)
	if len(got) != 1 || got[0].Kind != ViolationUnknownTool {
		t.Fatalf("an empty schema map reported %v, want one unknown_tool", got)
	}
}

// TestValidateCalls_EmptyResultIsTheCallersClaim: "empty" is a property
// of the tool's own result shape, which this package cannot know. With
// no callback it must say nothing rather than guess, because a guessed
// violation on a well-formed call is worse than a missing one.
func TestValidateCalls_EmptyResultIsTheCallersClaim(t *testing.T) {
	call := done("read_logs", map[string]any{"scope": "staging"}, map[string]any{"reading": "no abnormal findings in this scope."})

	if got := ValidateCalls(validitySchemas, []Call{call}, nil); len(got) != 0 {
		t.Errorf("with no emptyResult callback the validator invented %v", got)
	}

	got := ValidateCalls(validitySchemas, []Call{call}, func(Call) bool { return true })
	if len(got) != 1 || got[0].Kind != ViolationEmptyResult {
		t.Fatalf("with the callback saying empty, got %v, want one empty_result", got)
	}
	if !strings.Contains(got[0].Detail, "scope=staging") {
		t.Errorf("an empty read that does not name the scope it read is unactionable: %s", got[0])
	}
}

// TestValidateCalls_AFailedCallIsNotAnEmptyOne. Both come back with
// nothing useful, and the fix is different: an error is the tool
// refusing, an empty read is the tool answering. Reporting one as the
// other sends the reader to the wrong half of the system.
func TestValidateCalls_AFailedCallIsNotAnEmptyOne(t *testing.T) {
	calls := []Call{done("read_logs", map[string]any{"scope": "p"}, map[string]any{"error": "connection refused"})}
	got := ValidateCalls(validitySchemas, calls, func(Call) bool { return true })
	if len(got) != 1 {
		t.Fatalf("want one violation, got %v", got)
	}
	if got[0].Kind != ViolationErrorResult {
		t.Errorf("kind = %s, want %s: an errored call is not an empty read", got[0].Kind, ViolationErrorResult)
	}
}

// TestValidateCalls_OrderedByCall keeps the report readable when one
// run does several things wrong: a reader scans it against the trace,
// which is in call order.
func TestValidateCalls_OrderedByCall(t *testing.T) {
	calls := []Call{
		done("read_logs", map[string]any{"scope": "p", "level": "verbose", "since": "1h"}, map[string]any{"reading": "x"}),
		done("nope", nil, map[string]any{}),
		done("read_logs", map[string]any{}, map[string]any{"reading": "x"}),
	}
	got := ValidateCalls(validitySchemas, calls, nil)
	want := []string{ViolationNotInEnum, ViolationUndeclaredArg, ViolationUnknownTool, ViolationMissingRequired}
	if diff := strings.Join(kinds(got), ","); diff != strings.Join(want, ",") {
		t.Errorf("kinds = %s, want %s", diff, strings.Join(want, ","))
	}
	for i, v := range got {
		wantIdx := []int{0, 0, 1, 2}[i]
		if v.CallIndex != wantIdx {
			t.Errorf("violation %d has CallIndex %d, want %d — a reader could not find it in the trace", i, v.CallIndex, wantIdx)
		}
	}
}

// TestValidateCalls_UnknownKeywordsAreNotGuessedAt. The validator sees
// whatever JSON Schema a tool author wrote, and half-understanding a
// keyword produces false violations — which cost more than the missed
// real ones, because a report with noise in it stops being read.
func TestValidateCalls_UnknownKeywordsAreNotGuessedAt(t *testing.T) {
	schemas := ToolSchemas{"exotic": {
		"type": "object",
		"properties": map[string]any{
			"when":  map[string]any{"type": "string", "format": "date-time"},
			"any":   map[string]any{"anyOf": []any{map[string]any{"type": "string"}, map[string]any{"type": "number"}}},
			"count": map[string]any{"type": "integer", "minimum": float64(10)},
		},
	}}
	calls := []Call{done("exotic", map[string]any{"when": "yesterday", "any": float64(3), "count": float64(1)}, map[string]any{"ok": true})}
	if got := ValidateCalls(schemas, calls, nil); len(got) != 0 {
		t.Errorf("keywords the validator does not implement produced violations: %v", got)
	}
}

func TestViolationCounts(t *testing.T) {
	vs := []Violation{
		{Kind: ViolationEmptyResult}, {Kind: ViolationUnknownTool}, {Kind: ViolationEmptyResult},
	}
	got := strings.Join(ViolationCounts(vs), " ")
	if got != "empty_result=2 unknown_tool=1" {
		t.Errorf("counts = %q", got)
	}
	if ViolationCounts(nil) != nil {
		t.Error("a clean run should tally to nothing, not to an empty line")
	}
}

func TestRecordCalls_KeepsArgumentsAndDigestsTheResult(t *testing.T) {
	long := strings.Repeat("k8s_triage_workload: reading production/api\n", 40)
	calls := []Call{
		done("read_logs", map[string]any{"scope": "production/api"}, map[string]any{"reading": long}),
		{Name: "read_logs", Args: map[string]any{"scope": "staging"}, ID: "b", ResponseIndex: -1},
		done("read_logs", map[string]any{"scope": "p"}, map[string]any{"error": "boom"}),
	}
	got := RecordCalls(calls)
	if len(got) != 3 {
		t.Fatalf("recorded %d calls, want 3", len(got))
	}

	if got[0].Args["scope"] != "production/api" {
		t.Errorf("the arguments did not survive: %v", got[0].Args)
	}
	if n := len([]rune(got[0].Result)); n > digestResult+1 {
		t.Errorf("digest is %d runes; a board that inlines whole readings is a transcript nobody opens", n)
	}
	if strings.Contains(got[0].Result, "\n") {
		t.Errorf("digest spans lines, so it breaks the board: %q", got[0].Result)
	}
	if !strings.HasSuffix(got[0].Result, "…") {
		t.Errorf("a truncated digest must say it was truncated: %q", got[0].Result)
	}

	if got[1].Completed || got[1].Result != "" {
		t.Errorf("a call with no completion recorded a result: %+v", got[1])
	}
	if !strings.HasPrefix(got[2].Result, "error: boom") {
		t.Errorf("an errored call's digest must lead with the error: %q", got[2].Result)
	}
	if RecordCalls(nil) != nil {
		t.Error("no calls should record as nothing, not as an empty list")
	}
}
