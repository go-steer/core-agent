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

package evals_test

import (
	"context"
	"iter"
	"strings"
	"testing"

	adksession "google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v3/pkg/evals"
)

type sliceEvents []*adksession.Event

func (s sliceEvents) All() iter.Seq[*adksession.Event] {
	return func(yield func(*adksession.Event) bool) {
		for _, ev := range s {
			if !yield(ev) {
				return
			}
		}
	}
}

func (s sliceEvents) Len() int                   { return len(s) }
func (s sliceEvents) At(i int) *adksession.Event { return s[i] }

func mkEvent(role string, parts ...*genai.Part) *adksession.Event {
	ev := adksession.NewEvent(context.Background(), "inv-1")
	ev.Content = &genai.Content{Role: role, Parts: parts}
	return ev
}

func TestTraceFromEvents_EvaluateAllAndValidateCalls(t *testing.T) {
	t.Parallel()

	events := sliceEvents{
		mkEvent(genai.RoleModel, &genai.Part{
			FunctionCall: &genai.FunctionCall{
				ID:   "call-1",
				Name: "k8s_triage_workload",
				Args: map[string]any{"namespace": "payments"},
			},
		}),
		mkEvent(genai.RoleUser, &genai.Part{
			FunctionResponse: &genai.FunctionResponse{
				ID:       "call-1",
				Name:     "k8s_triage_workload",
				Response: map[string]any{"output": "pod CrashLoopBackOff"},
			},
		}),
		mkEvent(genai.RoleModel, &genai.Part{
			Text: "CRITICAL: payments pod is in CrashLoopBackOff",
		}),
	}

	pred := evals.NewPredicate(map[string]bool{"k8s_triage_workload": false})
	tr := evals.TraceFromEvents(events, pred, nil)
	if len(tr.Calls) != 1 {
		t.Fatalf("len(tr.Calls) = %d, want 1", len(tr.Calls))
	}
	if !tr.Calls[0].Completed || tr.Calls[0].Mutating() {
		t.Errorf("call[0] = %+v, want Completed=true, Mutating=false", tr.Calls[0])
	}
	if got := tr.CalledTools(); len(got) != 1 || got[0] != "k8s_triage_workload" {
		t.Errorf("CalledTools() = %v, want [k8s_triage_workload]", got)
	}
	if tr.FinalText != "CRITICAL: payments pod is in CrashLoopBackOff" {
		t.Errorf("FinalText = %q", tr.FinalText)
	}

	tbl := evals.IntentTable{
		Version: 1,
		Intents: []evals.Intent{{ID: "pod_status", Description: "inspect pod status"}},
		UpstreamTools: map[string]evals.UpstreamTool{
			"kubectl_get_pods": {Intent: "pod_status"},
		},
		LookoutTools: map[string]evals.LookoutTool{
			"k8s_triage_workload": {Satisfies: []string{"pod_status"}},
		},
	}
	sc := evals.Scenario{
		ID: "LC-01",
		Outputs: evals.ScenarioOutputs{
			ExpectedTools:    []string{"kubectl_get_pods"},
			ExpectedResponse: "CRITICAL: payments CrashLoopBackOff",
		},
	}

	results := evals.EvaluateAll(tbl, sc, tr)
	if len(results) != 5 {
		t.Fatalf("len(EvaluateAll) = %d, want 5", len(results))
	}
	for _, r := range results {
		if r.Metric == evals.MetricIntentCoverage && !r.Passed() {
			t.Errorf("intent_coverage failed: %+v", r)
		}
		if r.Metric == evals.MetricSeverityAccuracy && r.Score != 1 {
			t.Errorf("severity_accuracy score = %v, want 1 (%s)", r.Score, r.Comment)
		}
	}

	schemas := evals.ToolSchemas{
		"k8s_triage_workload": {
			"type":     "object",
			"required": []string{"namespace"},
			"properties": map[string]any{
				"namespace": map[string]any{"type": "string"},
			},
		},
	}
	if vs := evals.ValidateCalls(schemas, tr.Calls, nil); len(vs) != 0 {
		t.Errorf("ValidateCalls returned unexpected violations: %v", vs)
	}

	recs := evals.RecordCalls(tr.Calls)
	if len(recs) != 1 || recs[0].Tool != "k8s_triage_workload" || !recs[0].Completed {
		t.Errorf("RecordCalls = %+v", recs)
	}

	// Verify nil events and streaming partial events are handled safely.
	if empty := evals.TraceFromEvents(nil, nil, nil); len(empty.Calls) != 0 || empty.FinalText != "" {
		t.Errorf("TraceFromEvents(nil) = %+v, want empty Trace", empty)
	}
	partialEv := mkEvent(genai.RoleModel, &genai.Part{Text: "partial fragment"})
	partialEv.Partial = true
	withPartial := evals.TraceFromEvents(sliceEvents{partialEv, events[2]}, nil, nil)
	if withPartial.FinalText != "CRITICAL: payments pod is in CrashLoopBackOff" {
		t.Errorf("FinalText with partial = %q", withPartial.FinalText)
	}
}

func TestParseDataset_RoundTrip(t *testing.T) {
	t.Parallel()

	raw := `{"_meta":{"fixture":"test","scenario_count":1}}` + "\n" +
		`{"id":"S1","category":"triage","inputs":{"scenario":"pod down"},"outputs":{"expected_tools":["t1"],"expected_response":"CRITICAL: down"}}` + "\n"
	ds, err := evals.ParseDataset(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("ParseDataset: %v", err)
	}
	if len(ds.Scenarios) != 1 || ds.Scenarios[0].ID != "S1" {
		t.Errorf("Scenarios = %+v", ds.Scenarios)
	}
}
