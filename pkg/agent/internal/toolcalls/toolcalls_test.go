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

package toolcalls

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/adk/session"
	"google.golang.org/genai"
)

// callEvent builds a consolidated model event carrying tool calls.
func callEvent(calls ...*genai.FunctionCall) *session.Event {
	ev := session.NewEvent("inv")
	parts := []*genai.Part{{Text: "calling a tool"}}
	for _, fc := range calls {
		parts = append(parts, &genai.Part{FunctionCall: fc})
	}
	ev.LLMResponse = adkmodel.LLMResponse{
		Content: &genai.Content{Role: genai.RoleModel, Parts: parts},
	}
	return ev
}

// responseEvent builds the user-role event carrying tool results.
func responseEvent(resps ...*genai.FunctionResponse) *session.Event {
	ev := session.NewEvent("inv")
	var parts []*genai.Part
	for _, fr := range resps {
		parts = append(parts, &genai.Part{FunctionResponse: fr})
	}
	ev.LLMResponse = adkmodel.LLMResponse{
		Content: &genai.Content{Role: genai.RoleUser, Parts: parts},
	}
	return ev
}

func TestRecordsCallsInOrderWithArgs(t *testing.T) {
	var r Recorder
	r.Observe(callEvent(
		&genai.FunctionCall{ID: "a", Name: "kubectl_get", Args: map[string]any{"kind": "deployment", "name": "emailservice"}},
		&genai.FunctionCall{ID: "b", Name: "kubectl_logs", Args: map[string]any{"pod": "emailservice-abc"}},
	))

	got := r.Calls()
	if len(got) != 2 {
		t.Fatalf("recorded %d calls, want 2: %+v", len(got), got)
	}
	if got[0].Tool != "kubectl_get" || got[1].Tool != "kubectl_logs" {
		t.Errorf("calls out of order or misnamed: %+v", got)
	}
	if got[0].Args["name"] != "emailservice" {
		t.Errorf("first call lost its arguments: %+v", got[0].Args)
	}
	if got[0].Error != "" {
		t.Errorf("a call with no response yet reads as failed: %q", got[0].Error)
	}
}

func TestOutcomeIsMatchedToItsCallByID(t *testing.T) {
	var r Recorder
	r.Observe(callEvent(
		&genai.FunctionCall{ID: "a", Name: "kubectl_get"},
		&genai.FunctionCall{ID: "b", Name: "kubectl_logs"},
	))
	// Deliberately answered out of order: matching by position would
	// attach the 429 to the wrong call, and a citation that names the
	// wrong failed read is worse than one that names none.
	r.Observe(responseEvent(
		&genai.FunctionResponse{ID: "b", Response: map[string]any{"error": "429 Resource Exhausted"}},
		&genai.FunctionResponse{ID: "a", Response: map[string]any{"output": "ok"}},
	))

	got := r.Calls()
	if got[0].Error != "" {
		t.Errorf("kubectl_get reported an error it did not have: %q", got[0].Error)
	}
	if !strings.Contains(got[1].Error, "429") {
		t.Errorf("kubectl_logs lost its failure, got %q", got[1].Error)
	}
}

func TestPartialEventsAreNotCounted(t *testing.T) {
	// A streaming provider emits a call as chunks and then again
	// consolidated. Counting the chunks would report one read as
	// several, which inflates exactly the number #1014 is measured by.
	chunk := callEvent(&genai.FunctionCall{ID: "a", Name: "kubectl_get"})
	chunk.Partial = true

	var r Recorder
	r.Observe(chunk)
	r.Observe(chunk)
	r.Observe(callEvent(&genai.FunctionCall{ID: "a", Name: "kubectl_get"}))

	if got := r.Calls(); len(got) != 1 {
		t.Fatalf("recorded %d calls from one streamed call, want 1: %+v", len(got), got)
	}
}

func TestSkippedToolsAreNotRecorded(t *testing.T) {
	// The done tool's argument IS the delegation's output. Recording it
	// would ship the same prose twice in one result, which is the
	// opposite of the saving this package exists for.
	r := Recorder{Skip: []string{"return_result", "schedule_next_turn"}}
	r.Observe(callEvent(
		&genai.FunctionCall{ID: "a", Name: "kubectl_get"},
		&genai.FunctionCall{ID: "b", Name: "RETURN_RESULT", Args: map[string]any{"result": "a long prose report"}},
	))

	got := r.Calls()
	if len(got) != 1 {
		t.Fatalf("recorded %d calls, want 1 (the done tool must be skipped): %+v", len(got), got)
	}
	if got[0].Tool != "kubectl_get" {
		t.Errorf("skipped the wrong call, kept %q", got[0].Tool)
	}
}

func TestSkippedCallsOutcomeCannotLandOnAnotherCall(t *testing.T) {
	// Regression guard on the byID map: a skipped call records no
	// index, so its response must find nothing rather than falling
	// through onto whichever call happens to be there.
	r := Recorder{Skip: []string{"return_result"}}
	r.Observe(callEvent(
		&genai.FunctionCall{ID: "a", Name: "kubectl_get"},
		&genai.FunctionCall{ID: "b", Name: "return_result"},
	))
	r.Observe(responseEvent(&genai.FunctionResponse{ID: "b", Response: map[string]any{"error": "boom"}}))

	if got := r.Calls(); got[0].Error != "" {
		t.Errorf("a skipped call's failure was attributed to %q: %q", got[0].Tool, got[0].Error)
	}
}

func TestOversizedArgumentsAreShortenedAndSaySo(t *testing.T) {
	big := strings.Repeat("x", 4000)
	var r Recorder
	r.Observe(callEvent(&genai.FunctionCall{
		ID: "a", Name: "bash",
		Args: map[string]any{"command": big, "timeout": 30},
	}))

	got := r.Calls()[0]
	cmd, ok := got.Args["command"].(string)
	if !ok {
		t.Fatalf("command argument is %T, want a (truncated) string", got.Args["command"])
	}
	if len(cmd) > DefaultMaxArgBytes+len(truncationMarker) {
		t.Errorf("truncated command is %d bytes, over the %d limit", len(cmd), DefaultMaxArgBytes)
	}
	if !strings.HasSuffix(cmd, truncationMarker) {
		t.Errorf("shortened value does not say it was shortened: %q", cmd)
	}
	// A small value must survive untouched, or the record stops being
	// a faithful account of small calls to protect against large ones.
	if got.Args["timeout"] != 30 {
		t.Errorf("small argument was altered: %#v", got.Args["timeout"])
	}
}

func TestTruncationLeavesValidUTF8(t *testing.T) {
	// A multi-byte rune straddling the cut. Mangled bytes in an audit
	// record read as corruption rather than as truncation.
	var r Recorder
	r.Observe(callEvent(&genai.FunctionCall{
		ID: "a", Name: "bash",
		Args: map[string]any{"command": strings.Repeat("é", 4000)},
	}))

	cmd := r.Calls()[0].Args["command"].(string)
	if !utf8.ValidString(cmd) {
		t.Errorf("truncation produced invalid UTF-8: %q", cmd)
	}
}

func TestRecordedArgumentsDoNotAliasTheEvent(t *testing.T) {
	// The map belongs to the event, which other observers on the same
	// stream are still reading. Mutating it here would corrupt them.
	args := map[string]any{"name": "emailservice"}
	var r Recorder
	r.Observe(callEvent(&genai.FunctionCall{ID: "a", Name: "kubectl_get", Args: args}))

	r.Calls()[0].Args["name"] = "tampered"
	if args["name"] != "emailservice" {
		t.Errorf("recorder handed out a view onto the event's own map; it now reads %q", args["name"])
	}
}

func TestCallsPastTheCapAreCountedNotSilentlyDropped(t *testing.T) {
	r := Recorder{MaxCalls: 2}
	for i := 0; i < 5; i++ {
		r.Observe(callEvent(&genai.FunctionCall{ID: "x", Name: "kubectl_get"}))
	}

	if got := len(r.Calls()); got != 2 {
		t.Errorf("kept %d calls against a cap of 2", got)
	}
	// The count is the whole point: a truncated provenance record that
	// does not admit it is truncated invites "not in the list" being
	// read as "did not happen".
	if r.Dropped() != 3 {
		t.Errorf("dropped count is %d, want 3", r.Dropped())
	}
}

func TestAppendEnforcesTheRunLevelCap(t *testing.T) {
	dst := []Call{{Tool: "a"}, {Tool: "b"}}
	src := []Call{{Tool: "c"}, {Tool: "d"}, {Tool: "e"}}

	out, dropped := Append(dst, src, 4)
	if len(out) != 4 || dropped != 1 {
		t.Errorf("Append(len 2, len 3, max 4) = len %d, dropped %d; want len 4, dropped 1", len(out), dropped)
	}
	if out[3].Tool != "d" {
		t.Errorf("kept the wrong calls: %+v", out)
	}

	// Already full: everything is refused and counted, and nothing
	// slices out of range.
	if out, dropped := Append(out, src, 4); len(out) != 4 || dropped != 3 {
		t.Errorf("appending to a full list = len %d, dropped %d; want len 4, dropped 3", len(out), dropped)
	}
	if out, dropped := Append(dst, src, -1); len(out) != 5 || dropped != 0 {
		t.Errorf("a negative max should be unbounded, got len %d dropped %d", len(out), dropped)
	}

	// dst already past the cap. Unreachable from the run loop today —
	// which is exactly why it needs a test: without one the clamp is
	// invisible to the suite, and a caller that lowers max between
	// calls gets a bounds panic instead of a truncated list.
	over := []Call{{Tool: "a"}, {Tool: "b"}, {Tool: "c"}}
	if out, dropped := Append(over, src, 2); len(out) != 3 || dropped != 3 {
		t.Errorf("appending to an over-cap list = len %d, dropped %d; want len 3 (unchanged), dropped 3", len(out), dropped)
	}
}

func TestNoteSaysNothingWhenThereIsNothingToSay(t *testing.T) {
	if got := Note(0, 0); got != "" {
		t.Errorf("Note(0, 0) = %q, want empty — a result with no calls must not carry a sentence about calls", got)
	}
}

func TestNoteLicensesTheFreshnessReadAndOnlyThat(t *testing.T) {
	got := Note(3, 0)
	// The distinction this sentence has to carry: citing is free,
	// checking whether state changed is not forbidden. Collapsing them
	// would trade #1014's redundancy for ungroundedness.
	for _, want := range []string{"3 call(s)", "Cite them", "CURRENT value"} {
		if !strings.Contains(got, want) {
			t.Errorf("Note is missing %q: %q", want, got)
		}
	}
	if strings.Contains(got, "not recorded") {
		t.Errorf("Note claims a truncation that did not happen: %q", got)
	}
}

func TestNoteAdmitsTruncation(t *testing.T) {
	got := Note(2, 7)
	if !strings.Contains(got, "7 further call(s)") {
		t.Errorf("Note does not report the 7 dropped calls: %q", got)
	}
	if !strings.Contains(got, "does not mean it did not happen") {
		t.Errorf("Note does not warn against reading absence as evidence: %q", got)
	}
}

func TestResponseErrorShapes(t *testing.T) {
	cases := []struct {
		name string
		resp map[string]any
		want string
	}{
		{"clean", map[string]any{"output": "ok"}, ""},
		{"absent", nil, ""},
		{"nil value", map[string]any{"error": nil}, ""},
		{"empty string", map[string]any{"error": ""}, ""},
		{"string", map[string]any{"error": "429"}, "429"},
		// An unrecognized shape counts as a failure: a tool returning a
		// structured error object is failing, and reading that as
		// success drops exactly the observations this exists to make.
		{"structured", map[string]any{"error": map[string]any{"code": 429}}, "map[code:429]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResponseError(tc.resp); got != tc.want {
				t.Errorf("ResponseError(%v) = %q, want %q", tc.resp, got, tc.want)
			}
		})
	}
}

func TestARecordedCallIsSmallerThanTheReadItReplaces(t *testing.T) {
	// The premise of the whole package. A repeated read in the archived
	// corpus averaged ~3,120 bytes of re-pulled payload; if a call
	// record approached that there would be no saving to bank.
	var r Recorder
	r.Observe(callEvent(&genai.FunctionCall{
		ID: "a", Name: "kubectl_get",
		Args: map[string]any{"kind": "deployment", "name": "emailservice", "namespace": "online-boutique", "outputFormat": "yaml"},
	}))
	encoded, err := json.Marshal(r.Calls()[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if len(encoded) > 512 {
		t.Errorf("one call record is %d bytes; it is meant to cost a fraction of the ~3,120-byte read it replaces", len(encoded))
	}
}
