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
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestWireContract_OutcomeVocabulary(t *testing.T) {
	for _, tc := range []struct {
		got  Outcome
		want string
	}{
		{OutcomeApprove, "approve"},
		{OutcomeReject, "reject"},
		{OutcomeEdit, "edit"},
	} {
		if string(tc.got) != tc.want {
			t.Errorf("outcome = %q, want %q", tc.got, tc.want)
		}
	}
}

func TestWireContract_ScopeVocabulary(t *testing.T) {
	for _, tc := range []struct {
		got  Scope
		want string
	}{
		{ScopeOnce, "once"},
		{ScopeSession, "session"},
		{ScopeSessionTool, "session_tool"},
		{ScopeAlways, "always"},
		{ScopeChangeSet, "change_set"},
	} {
		if string(tc.got) != tc.want {
			t.Errorf("scope = %q, want %q", tc.got, tc.want)
		}
	}
}

func TestWireContract_VerdictFields(t *testing.T) {
	rt := reflect.TypeOf(Verdict{})
	var got []string
	for i := 0; i < rt.NumField(); i++ {
		name, _, _ := strings.Cut(rt.Field(i).Tag.Get("json"), ",")
		got = append(got, name)
	}
	sort.Strings(got)
	want := []string{"approver", "args", "note", "scope", "verdict"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Verdict wire fields = %v, want %v", got, want)
	}
}

func TestWireContract_ConfirmationEnvelope(t *testing.T) {
	env := ConfirmationResponse(Verdict{Verdict: OutcomeEdit, Args: map[string]any{"replicas": 2}})
	confirmed, ok := env["confirmed"].(bool)
	if !ok {
		t.Fatalf("envelope[confirmed] = %T, want bool", env["confirmed"])
	}
	if !confirmed {
		t.Error("an edit came back confirmed=false")
	}
	if _, ok := env["payload"]; !ok {
		t.Fatalf("envelope has no payload key; got %v", keysOf(env))
	}
	if len(env) != 2 {
		t.Errorf("envelope has %d keys (%v), want exactly confirmed and payload", len(env), keysOf(env))
	}
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	var back struct {
		Confirmed bool    `json:"confirmed"`
		Payload   Verdict `json:"payload"`
	}
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if back.Payload.Verdict != OutcomeEdit {
		t.Errorf("payload verdict = %q after a round trip, want edit", back.Payload.Verdict)
	}
	if got := back.Payload.Args["replicas"]; got == nil {
		t.Error("the edited arguments did not survive the round trip")
	}
}

func TestWireContract_RejectIsNotConfirmed(t *testing.T) {
	env := ConfirmationResponse(Verdict{Verdict: OutcomeReject})
	if env["confirmed"] != false {
		t.Fatalf("envelope[confirmed] = %v for a reject, want false", env["confirmed"])
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
