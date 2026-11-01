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

// Originally derived from go-steer/mast.

package monitor

import (
	"reflect"
	"strings"
	"testing"
)

func TestParse_ValidFullEnvelope(t *testing.T) {
	raw := `{
		"transitions": [
			{
				"kind": "watch_ConditionFalse",
				"edge": "opened",
				"subject_key": "ConditionFalse/Deployment/payments/api/Available",
				"summary": "Deployment payments/api Available=False (MinimumReplicasUnavailable)",
				"attrs": {"namespace": "payments", "kind": "Deployment", "name": "api"}
			},
			{
				"kind": "metric_slo_burn",
				"edge": "closed",
				"subject_key": "metric/slo_burn/payments",
				"summary": "resolved"
			}
		],
		"degraded": true,
		"extra_ignored": 42
	}`
	got, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse() unexpected error: %v", err)
	}
	if !got.Degraded {
		t.Errorf("Degraded = false, want true")
	}
	if len(got.Transitions) != 2 {
		t.Fatalf("len(Transitions) = %d, want 2", len(got.Transitions))
	}
	wantFirst := Transition{
		Kind:       "watch_ConditionFalse",
		Edge:       "opened",
		SubjectKey: "ConditionFalse/Deployment/payments/api/Available",
		Summary:    "Deployment payments/api Available=False (MinimumReplicasUnavailable)",
		Attrs:      map[string]string{"namespace": "payments", "kind": "Deployment", "name": "api"},
	}
	if !reflect.DeepEqual(got.Transitions[0], wantFirst) {
		t.Errorf("Transitions[0] = %+v, want %+v", got.Transitions[0], wantFirst)
	}
	if got.Transitions[1].Edge != "closed" || got.Transitions[1].Kind != "metric_slo_burn" {
		t.Errorf("Transitions[1] = %+v", got.Transitions[1])
	}
}

func TestParse_EmptyTransitionsDefaultsDegradedFalse(t *testing.T) {
	got, err := Parse(`{"transitions": []}`)
	if err != nil {
		t.Fatalf("Parse() unexpected error: %v", err)
	}
	if got.Degraded {
		t.Errorf("Degraded = true, want false")
	}
	if len(got.Transitions) != 0 {
		t.Errorf("len(Transitions) = %d, want 0", len(got.Transitions))
	}
}

func TestParseResult_OutputStringAndDirectMap(t *testing.T) {
	fromOut, err := ParseResult(map[string]any{
		"output": `{"transitions":[{"kind":"k","edge":"opened","subject_key":"s","summary":"m"}]}`,
	})
	if err != nil {
		t.Fatalf("ParseResult(output string) error: %v", err)
	}
	if len(fromOut.Transitions) != 1 || fromOut.Transitions[0].SubjectKey != "s" {
		t.Errorf("ParseResult(output string) = %+v", fromOut)
	}

	fromMap, err := ParseResult(map[string]any{
		"transitions": []any{
			map[string]any{"kind": "k2", "edge": "remained", "subject_key": "s2"},
		},
		"degraded": true,
	})
	if err != nil {
		t.Fatalf("ParseResult(direct map) error: %v", err)
	}
	if !fromMap.Degraded || len(fromMap.Transitions) != 1 || fromMap.Transitions[0].Kind != "k2" {
		t.Errorf("ParseResult(direct map) = %+v", fromMap)
	}
}

func TestParse_MalformedInputsReturnError(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantSub string
	}{
		{name: "empty", raw: "   ", wantSub: "empty collect output"},
		{name: "not json", raw: "not-json", wantSub: "not a JSON object"},
		{name: "missing transitions", raw: `{"degraded": false}`, wantSub: `missing required field "transitions"`},
		{name: "null transitions", raw: `{"transitions": null}`, wantSub: `must be a JSON array, got null`},
		{name: "transitions wrong type", raw: `{"transitions": "nope"}`, wantSub: `decode "transitions"`},
		{name: "degraded wrong type", raw: `{"transitions": [], "degraded": "yes"}`, wantSub: `decode "degraded"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(tc.raw)
			if err == nil {
				t.Fatalf("Parse(%q) succeeded, want error containing %q", tc.raw, tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("Parse(%q) error = %q, want substring %q", tc.raw, err.Error(), tc.wantSub)
			}
		})
	}
	if _, err := ParseResult(nil); err == nil {
		t.Errorf("ParseResult(nil) succeeded, want error")
	}
}
