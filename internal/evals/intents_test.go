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
	"reflect"
	"sort"
	"strings"
	"testing"
)

func loadTable(t *testing.T) IntentTable {
	t.Helper()
	tbl, err := LoadIntentTable(intentsPath)
	if err != nil {
		t.Fatalf("LoadIntentTable: %v", err)
	}
	return tbl
}

// TestIntentTable_Completeness is the W0.1a done-when: every
// expected_tools entry across all 31 scenarios resolves to an intent.
// An unresolved name is a hole in the primary metric — intent_coverage
// would score against a denominator that silently omits it.
func TestIntentTable_Completeness(t *testing.T) {
	tbl := loadTable(t)
	ds := loadLangChain(t)

	distinct := make(map[string]bool)
	for _, s := range ds.Scenarios {
		intents, unknown := tbl.IntentsFor(s.Outputs.ExpectedTools)
		if len(unknown) > 0 {
			t.Errorf("%s: expected_tools not in the intent table: %v", s.ID, unknown)
		}
		if len(intents) == 0 {
			t.Errorf("%s: resolved to no intents at all", s.ID)
		}
		for _, name := range s.Outputs.ExpectedTools {
			distinct[name] = true
		}
	}

	// The table must cover exactly the dataset's surface: 23 distinct
	// names. An entry for a name no scenario uses is dead weight that
	// will drift out of sync with upstream.
	if len(distinct) != 23 {
		t.Errorf("dataset uses %d distinct tool names, want 23", len(distinct))
	}
	for name := range tbl.UpstreamTools {
		if !distinct[name] {
			t.Errorf("intent table maps %q, which no scenario expects", name)
		}
	}
	if len(tbl.UpstreamTools) != len(distinct) {
		t.Errorf("table has %d upstream tools, dataset has %d distinct names",
			len(tbl.UpstreamTools), len(distinct))
	}
}

// TestIntentTable_Annotations pins the two annotations W0.1a exists to
// make explicit: the deliberate write-tool exclusion, and the 7 names
// upstream's own registry cannot serve. Both adjust how any number
// compared against upstream must be read, so both are asserted rather
// than left to the YAML.
func TestIntentTable_Annotations(t *testing.T) {
	tbl := loadTable(t)

	// 1. The single deliberate non-mapping.
	var excluded []string
	for name, ut := range tbl.UpstreamTools {
		if ut.LookoutExcluded {
			excluded = append(excluded, name)
		}
	}
	sort.Strings(excluded)
	if want := []string{"kubectl_rollback_deployment"}; !reflect.DeepEqual(excluded, want) {
		t.Errorf("lookout-excluded tools = %v, want %v", excluded, want)
	}
	if ut := tbl.UpstreamTools["kubectl_rollback_deployment"]; !ut.Write || ut.ExclusionReason == "" {
		t.Error("the write-tool exclusion must be marked write:true and carry a reason")
	}

	// 2. The phantoms.
	var phantom []string
	for name, ut := range tbl.UpstreamTools {
		if ut.UnreachableUpstream {
			phantom = append(phantom, name)
		}
	}
	sort.Strings(phantom)
	want := []string{
		"kubectl_describe_hpa",
		"kubectl_describe_ingress",
		"kubectl_describe_node",
		"kubectl_describe_service",
		"kubectl_get_pvcs",
		"kubectl_get_secrets",
		"kubectl_get_service",
	}
	if !reflect.DeepEqual(phantom, want) {
		t.Errorf("unreachable_upstream tools = %v, want %v", phantom, want)
	}
	for _, name := range phantom {
		if !tbl.Unreachable(name) {
			t.Errorf("Unreachable(%q) = false", name)
		}
	}
	if tbl.Unreachable("kubectl_get_pods") {
		t.Error("Unreachable(kubectl_get_pods) = true, want false")
	}

	// The ~23% reference ceiling the plan records. Asserted as a count
	// so a future edit to the corpus cannot move it unnoticed.
	ds := loadLangChain(t)
	total, unreachable := 0, 0
	for _, s := range ds.Scenarios {
		for _, name := range s.Outputs.ExpectedTools {
			total++
			if tbl.Unreachable(name) {
				unreachable++
			}
		}
	}
	if total != 71 || unreachable != 16 {
		t.Errorf("tool references: %d total / %d unreachable, want 71/16", total, unreachable)
	}
}

// TestIntentTable_Consolidation is the reason intent_coverage replaced
// tool_coverage. LC-22 names three upstream tools; one k8s_triage_workload
// call answers all three. Name-level set overlap scores that 0/3 and
// reports a better-factored read path as a total miss.
func TestIntentTable_Consolidation(t *testing.T) {
	tbl := loadTable(t)
	ds := loadLangChain(t)

	var target *Scenario
	for i := range ds.Scenarios {
		if ds.Scenarios[i].ID == "LC-22-coredns-down" {
			target = &ds.Scenarios[i]
		}
	}
	if target == nil {
		t.Fatal("scenario LC-22-coredns-down not found")
	}

	expected, unknown := tbl.IntentsFor(target.Outputs.ExpectedTools)
	if len(unknown) != 0 {
		t.Fatalf("unresolved tool names: %v", unknown)
	}
	if len(expected) != 3 {
		t.Fatalf("expected intents = %v, want 3 distinct", expected)
	}

	satisfied := tbl.SatisfiedBy([]string{"k8s_triage_workload"})
	set := make(map[string]bool, len(satisfied))
	for _, id := range satisfied {
		set[id] = true
	}
	for _, id := range expected {
		if !set[id] {
			t.Errorf("one k8s_triage_workload call does not satisfy %q", id)
		}
	}

	// The metric this replaces: zero name-level overlap for a trace
	// that answered every question the scenario asked.
	for _, name := range target.Outputs.ExpectedTools {
		if _, isLookout := tbl.LookoutTools[name]; isLookout {
			t.Errorf("%q is both an upstream name and a lookout tool; the "+
				"consolidation contrast no longer holds", name)
		}
	}
}

// TestIntentTable_SatisfiedByIgnoresUnknownTools keeps a roster's
// unrelated tools from being treated as an error: rosters legitimately
// carry tools this dataset does not exercise.
func TestIntentTable_SatisfiedByIgnoresUnknownTools(t *testing.T) {
	tbl := loadTable(t)
	got := tbl.SatisfiedBy([]string{"finish_task", "k8s_triage_logs", "send_slack_notification"})
	if want := []string{"inspect.logs"}; !reflect.DeepEqual(got, want) {
		t.Errorf("SatisfiedBy = %v, want %v", got, want)
	}
}

// TestIntentTable_ValidationRejects checks the strictness that keeps a
// typo in the table from becoming a silent hole in the score.
func TestIntentTable_ValidationRejects(t *testing.T) {
	tests := []struct {
		name  string
		table IntentTable
		want  string
	}{
		{"no intents", IntentTable{}, "no intents defined"},
		{
			"duplicate intent",
			IntentTable{Intents: []Intent{{ID: "a"}, {ID: "a"}}},
			"duplicate intent id",
		},
		{
			"upstream tool with no intent",
			IntentTable{
				Intents:       []Intent{{ID: "a"}},
				UpstreamTools: map[string]UpstreamTool{"t": {}},
			},
			"maps to no intent",
		},
		{
			"upstream tool to undefined intent",
			IntentTable{
				Intents:       []Intent{{ID: "a"}},
				UpstreamTools: map[string]UpstreamTool{"t": {Intent: "b"}},
			},
			"undefined intent",
		},
		{
			"lookout tool to undefined intent",
			IntentTable{
				Intents:      []Intent{{ID: "a"}},
				LookoutTools: map[string]LookoutTool{"k": {Satisfies: []string{"b"}}},
			},
			"undefined intent",
		},
		{
			"lookout tool satisfying nothing",
			IntentTable{
				Intents:      []Intent{{ID: "a"}},
				LookoutTools: map[string]LookoutTool{"k": {}},
			},
			"satisfies no intents",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.table.validate()
			if err == nil {
				t.Fatalf("validate accepted a bad table, want error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}
