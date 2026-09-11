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

package trajectory

import (
	"encoding/json"
	"strings"
	"testing"
)

// The wants below are not hand-written. Each is the literal output of
// CPython 3.12's `json.dumps(json.loads(input))` for the input beside
// it, captured once and pasted in. If pyDumps ever stops reproducing
// them, score.py's 200-character error-prose window and this package's
// have parted company and the two will start disagreeing about which
// tool calls failed.
//
// To regenerate a row:
//
//	python3 -c 'import json,sys; print(json.dumps(json.loads(sys.argv[1])))' '<input>'
func TestPyDumpsMatchesPythonJSONDumps(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"{\"call_id\":\"x\",\"digest\":\"{\\\"latency_ms\\\":662,\\\"output\\\":{\\\"errors\\\":[]}}\"}", "{\"call_id\": \"x\", \"digest\": \"{\\\"latency_ms\\\":662,\\\"output\\\":{\\\"errors\\\":[]}}\"}"},
		{"{\"a\":1,\"b\":[1,2,{\"c\":null}],\"d\":true,\"e\":false}", "{\"a\": 1, \"b\": [1, 2, {\"c\": null}], \"d\": true, \"e\": false}"},
		{"{\"z\":\"first\",\"a\":\"second\"}", "{\"z\": \"first\", \"a\": \"second\"}"},
		{"{\"unicode\":\"héllo → wörld ✓\",\"emoji\":\"🚀 launch\"}", "{\"unicode\": \"h\\u00e9llo \\u2192 w\\u00f6rld \\u2713\", \"emoji\": \"\\ud83d\\ude80 launch\"}"},
		{"{\"nested\":{\"deep\":{\"deeper\":[{\"k\":\"v\"},[],{}]}}}", "{\"nested\": {\"deep\": {\"deeper\": [{\"k\": \"v\"}, [], {}]}}}"},
		{"{\"escapes\": \"line\\nbreak\\ttab \\\"quoted\\\" back\\\\slash\", \"controls\": \"\\u0001\\u0007\\u001f\\u007f\", \"crlf\": \"\\r\\n\", \"formfeed\": \"\\f\\b\"}", "{\"escapes\": \"line\\nbreak\\ttab \\\"quoted\\\" back\\\\slash\", \"controls\": \"\\u0001\\u0007\\u001f\\u007f\", \"crlf\": \"\\r\\n\", \"formfeed\": \"\\f\\b\"}"},
		{"[]", "[]"},
		{"{}", "{}"},
		{"{\"empty_list\":[],\"empty_obj\":{},\"num\":0,\"neg\":-1.5,\"exp\":1e3}", "{\"empty_list\": [], \"empty_obj\": {}, \"num\": 0, \"neg\": -1.5, \"exp\": 1000.0}"},
		{"{\"logs\":\"Error from server (Forbidden): pods is forbidden\"}", "{\"logs\": \"Error from server (Forbidden): pods is forbidden\"}"},
		{"{\"error\":\"\",\"result\":\"fine\"}", "{\"error\": \"\", \"result\": \"fine\"}"},
		{"{\"isError\":false,\"result\":\"fine\"}", "{\"isError\": false, \"result\": \"fine\"}"},
		{"\"just a string\"", "\"just a string\""},
		{"42", "42"},
		{"null", "null"},
		{"{\"a\":1e3,\"b\":1e15,\"c\":1e16,\"d\":1e17,\"e\":1e-4,\"f\":1e-5,\"g\":0.0001,\"h\":0.00001}", "{\"a\": 1000.0, \"b\": 1000000000000000.0, \"c\": 1e+16, \"d\": 1e+17, \"e\": 0.0001, \"f\": 1e-05, \"g\": 0.0001, \"h\": 1e-05}"},
		{"{\"a\":1.0,\"b\":-0.0,\"c\":0.0,\"d\":1.5e20,\"e\":-1.5e-20,\"f\":123456789012345.6,\"g\":1234567890123456.0}", "{\"a\": 1.0, \"b\": -0.0, \"c\": 0.0, \"d\": 1.5e+20, \"e\": -1.5e-20, \"f\": 123456789012345.6, \"g\": 1234567890123456.0}"},
		{"{\"cost\":0.00052125,\"latency_ms\":662,\"big\":9007199254740993}", "{\"cost\": 0.00052125, \"latency_ms\": 662, \"big\": 9007199254740993}"},
		{"{\"mixed\":[1,2.5,\"three\",null,true,{\"four\":4}],\"tail\":\"x\"}", "{\"mixed\": [1, 2.5, \"three\", null, true, {\"four\": 4}], \"tail\": \"x\"}"},
		{"{\"deep\":[[[[[1]]]]]}", "{\"deep\": [[[[[1]]]]]}"},
		{"{\"k\":\"value with / slash and <html> & ampersand\"}", "{\"k\": \"value with / slash and <html> & ampersand\"}"},
		{"{\"\":\"empty key\",\"dup\":\"only once\"}", "{\"\": \"empty key\", \"dup\": \"only once\"}"},
		{"{\"tab_in_key\\t\":\"v\"}", "{\"tab_in_key\\t\": \"v\"}"},
	}
	for _, tc := range cases {
		got := pyDumps(json.RawMessage(tc.in), 1<<20)
		if got != tc.want {
			t.Errorf("pyDumps(%s)\n got  %s\n want %s", tc.in, got, tc.want)
		}
	}
}

// The window is the whole point of pyDumps: it exists so that the cut
// lands where Python's would.
func TestPyDumpsStopsAtTheLimit(t *testing.T) {
	in := json.RawMessage(`{"a":"` + strings.Repeat("x", 1000) + `"}`)
	got := pyDumps(in, 20)
	if len(got) != 20 {
		t.Errorf("len = %d, want 20: %q", len(got), got)
	}
	if want := `{"a": "xxxxxxxxxxxxx`; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// A truncated or corrupt payload must not panic or spin; it renders what
// it could parse and stops.
func TestPyDumpsOnMalformedInput(t *testing.T) {
	for _, in := range []string{"", "{", `{"a":`, `{"a":1,`, "[[[", "not json at all"} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("pyDumps(%q) panicked: %v", in, r)
				}
			}()
			_ = pyDumps(json.RawMessage(in), 200)
		}()
	}
}
