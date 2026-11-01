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
	"slices"
	"sort"
	"strings"
	"testing"
)

// missOn is the (scenario, intent) key a board would print.
func missOn(rep MissReport) []string {
	out := make([]string, 0, len(rep.Consequential))
	for _, m := range rep.Consequential {
		out = append(out, m.Intent)
	}
	return out
}

// TestClassifyMisses_ARedundantSkipIsNotAMiss is the whole reason #170
// rejected the obvious metric. k8s_cluster_health answers the
// cluster-wide discovery questions on its own; a run that calls it and
// skips the three narrower tools that also answer them has missed
// nothing, and the naive "did it call every tool with data" count would
// flag it.
func TestClassifyMisses_ARedundantSkipIsNotAMiss(t *testing.T) {
	tbl := loadTable(t)

	// Two intents k8s_cluster_health satisfies, each also reachable
	// through a narrower tool — that redundancy is the corpus's shape,
	// not a fixture convenience, so assert it before relying on it.
	const a, b = "discover.cluster_health", "discover.abnormal_pods"
	for _, id := range []string{a, b} {
		if n := len(tbl.ToolsSatisfying(id)); n < 2 {
			t.Fatalf("%s is served by %d tool(s); this test needs an intent with a redundant server", id, n)
		}
	}

	sc := Scenario{ID: "T-01", Outputs: ScenarioOutputs{
		ExpectedTools: []string{"get_cluster_summary", "kubectl_get_pods"},
	}}
	want, unknown := tbl.IntentsFor(sc.Outputs.ExpectedTools)
	if len(unknown) > 0 {
		t.Fatalf("fixture names %v, which the table does not know", unknown)
	}
	if len(want) != 2 {
		t.Fatalf("fixture expects %v; this test needs exactly the two intents above", want)
	}

	rep := ClassifyMisses(tbl, sc, callsTo("k8s_cluster_health"))
	if !rep.Empty() {
		t.Errorf("one call answered both questions, and the report still found %+v", rep)
	}
}

// TestClassifyMisses_NamesTheToolsThatWouldHaveServed: the actionable
// part of a miss is not that it happened, it is which tool would have
// answered it. A miss with no tool named is a line nobody can act on.
func TestClassifyMisses_NamesTheToolsThatWouldHaveServed(t *testing.T) {
	tbl := loadTable(t)
	sc := Scenario{ID: "T-01", Outputs: ScenarioOutputs{
		ExpectedTools: []string{"kubectl_top_nodes"},
	}}

	rep := ClassifyMisses(tbl, sc, callsTo("k8s_triage_logs"))
	if len(rep.Consequential) != 1 {
		t.Fatalf("consequential = %+v, want the one unanswered saturation question", rep.Consequential)
	}
	got := rep.Consequential[0]
	if got.Intent != "inspect.node_saturation" {
		t.Errorf("intent = %q, want inspect.node_saturation", got.Intent)
	}
	if len(got.ServedBy) == 0 {
		t.Fatal("the miss names no tool that would have served it")
	}
	// Every named tool must be one the run did not call — an intent with
	// a called server behind it is satisfied and never reaches here.
	for _, name := range got.ServedBy {
		if name == "k8s_triage_logs" {
			t.Errorf("served_by names %q, which this run did call", name)
		}
		if _, ok := tbl.LookoutTools[name]; !ok {
			t.Errorf("served_by names %q, which is not in the catalog", name)
		}
	}
}

// TestClassifyMisses_TheCatalogsCeilingIsNotTheModelsMiss is the
// distinction the issue insists on: LC-13 expects a rollback, lookout
// excludes write tools by design, and no run can ever satisfy it.
// Counting that as a miss would report a deliberate scope decision as a
// tool-selection failure on every board forever.
func TestClassifyMisses_TheCatalogsCeilingIsNotTheModelsMiss(t *testing.T) {
	tbl := loadTable(t)
	ds := loadLangChain(t)

	var sc Scenario
	for _, s := range ds.Scenarios {
		if strings.HasPrefix(s.ID, "LC-13") {
			sc = s
		}
	}
	if sc.ID == "" {
		t.Fatal("LC-13 is not in the corpus")
	}
	if n := len(tbl.ToolsSatisfying("remediate.rollback")); n != 0 {
		t.Fatalf("remediate.rollback is served by %d lookout tool(s); the ceiling this test pins is gone", n)
	}

	// Call everything the catalog has, so the only thing left unanswered
	// is the thing the catalog cannot answer.
	all := make([]string, 0, len(tbl.LookoutTools))
	for name := range tbl.LookoutTools {
		all = append(all, name)
	}
	rep := ClassifyMisses(tbl, sc, callsTo(all...))

	if len(rep.Consequential) != 0 {
		t.Errorf("a run that called every tool in the catalog was charged with %v", missOn(rep))
	}
	if len(rep.OutOfCatalog) != 1 || rep.OutOfCatalog[0] != "remediate.rollback" {
		t.Errorf("out of catalog = %v, want only remediate.rollback", rep.OutOfCatalog)
	}
}

// TestClassifyMisses_AnUntabledNameIsTheTablesGap: an expected tool the
// intent table has never seen deflates intent_coverage on purpose, but
// it is a hole in the fixture rather than a question the model declined
// to ask, and filing it as a miss would send a reader looking at the
// model.
func TestClassifyMisses_AnUntabledNameIsTheTablesGap(t *testing.T) {
	tbl := loadTable(t)
	sc := Scenario{ID: "T-01", Outputs: ScenarioOutputs{
		ExpectedTools: []string{"kubectl_get_pods", "kubectl_invent_a_tool"},
	}}

	rep := ClassifyMisses(tbl, sc, callsTo("k8s_cluster_health"))
	if len(rep.Untabled) != 1 || rep.Untabled[0] != "kubectl_invent_a_tool" {
		t.Errorf("untabled = %v, want the one name intents.yaml does not carry", rep.Untabled)
	}
	if len(rep.Consequential) != 0 || len(rep.OutOfCatalog) != 0 {
		t.Errorf("a table gap was filed as a miss: %+v", rep)
	}
	if rep.Empty() {
		t.Error("a report carrying an untabled name reported itself empty")
	}
}

// TestClassifyMisses_ExplainsExactlyWhatIntentCoverageScored is the
// anti-drift check, run over the whole corpus against four different
// read paths. The list and the number are two readings of one thing; if
// they ever disagree, the board says one story in its score and another
// in its enumeration, and a reader has no way to tell which is wrong.
func TestClassifyMisses_ExplainsExactlyWhatIntentCoverageScored(t *testing.T) {
	tbl := loadTable(t)
	ds := loadLangChain(t)

	paths := map[string][]string{
		"read nothing":    nil,
		"one broad call":  {"k8s_cluster_health"},
		"one narrow call": {"k8s_triage_logs"},
		"the whole belt":  {"k8s_cluster_health", "k8s_triage_workload", "k8s_resource_top", "k8s_event_timeline"},
	}

	classified := 0
	for name, called := range paths {
		for _, sc := range ds.Scenarios {
			tr := callsTo(called...)
			rep := ClassifyMisses(tbl, sc, tr)
			want, unknown := tbl.IntentsFor(sc.Outputs.ExpectedTools)

			denom := len(want) + len(unknown)
			if denom == 0 {
				continue
			}
			missed := len(rep.Consequential) + len(rep.OutOfCatalog)
			classified += missed
			hit := len(want) - missed
			if got := IntentCoverage(tbl, sc, tr).Score; !approx(got, float64(hit)/float64(denom)) {
				t.Errorf("%s/%s: intent_coverage scored %.4f but the report accounts for %d hit of %d",
					name, sc.ID, got, hit, denom)
			}
			if len(rep.Untabled) != len(unknown) {
				t.Errorf("%s/%s: untabled = %v, want %v", name, sc.ID, rep.Untabled, unknown)
			}
		}
	}
	if classified == 0 {
		t.Fatal("no miss was classified on any read path; the check above proved nothing")
	}
}

// TestToolsSatisfying_IsOrderedAndHonestAboutNothing: the list is
// printed on a board, so its order has to be stable, and an intent no
// tool serves must come back empty rather than as some near miss.
func TestToolsSatisfying_IsOrderedAndHonestAboutNothing(t *testing.T) {
	tbl := loadTable(t)

	got := tbl.ToolsSatisfying("inspect.logs")
	if len(got) == 0 {
		t.Fatal("no tool serves inspect.logs")
	}
	for i := 1; i < len(got); i++ {
		if got[i-1] >= got[i] {
			t.Errorf("tools are not in name order: %v", got)
			break
		}
	}
	if out := tbl.ToolsSatisfying("no.such.intent"); len(out) != 0 {
		t.Errorf("an intent no tool declares came back as %v", out)
	}
}

// TestGatingBy_IsTheCatalogsFactNotTheBoards: gating is computed from the
// intent table and the corpus alone, so it is the same number on a night
// when every tool was called and on a night when none was. That is what
// makes it the leverage half of the ranking — the miss count is one
// model on one night, this is a property of the fixture.
func TestGatingBy_IsTheCatalogsFactNotTheBoards(t *testing.T) {
	tbl := loadTable(t)
	ds := loadLangChain(t)

	g := GatingBy(tbl, ds)
	if len(g) == 0 {
		t.Fatal("no tool in the catalog is the sole answer to anything; #171 has nothing to rank")
	}

	top, ok := g["k8s_resource_top"]
	if !ok {
		t.Fatalf("k8s_resource_top is not sole-source for anything; the finding #171 exists to surface is gone (have %v)", keysOf(g))
	}
	// Three of the four consequential misses across both v0.4.0 boards are
	// this tool, and it is sole-source for both saturation intents. If a
	// second tool ever declares one of them, the attribution stops being
	// attribution and this test should be the thing that says so.
	want := []string{"inspect.node_saturation", "inspect.pod_saturation"}
	if !slices.Equal(top.SoleSource, want) {
		t.Errorf("k8s_resource_top sole-source for %v, want %v", top.SoleSource, want)
	}
	if len(top.Gates) == 0 {
		t.Error("k8s_resource_top gates no scenario, so skipping it costs the corpus nothing")
	}

	for name, entry := range g {
		if entry.Tool != name {
			t.Errorf("%s is keyed under %s", entry.Tool, name)
		}
		if len(entry.SoleSource) == 0 {
			t.Errorf("%s is in the ranking with no sole-source intent — it can be skipped for free and can never move", name)
		}
		for _, id := range entry.SoleSource {
			if servers := tbl.ToolsSatisfying(id); len(servers) != 1 || servers[0] != name {
				t.Errorf("%s claims sole source of %s, but %v serve it", name, id, servers)
			}
		}
		for i := 1; i < len(entry.Gates); i++ {
			if entry.Gates[i-1] >= entry.Gates[i] {
				t.Errorf("%s gates are not ordered and deduplicated: %v", name, entry.Gates)
				break
			}
		}
	}
}

// TestGatingBy_CountsAScenarioOnceHoweverManyWaysItNeedsTheTool: a row
// that expects both saturation intents is one row that cannot be covered
// without k8s_resource_top, not two. Double-counting would inflate the
// leverage of exactly the tools the ranking puts on top.
func TestGatingBy_CountsAScenarioOnceHoweverManyWaysItNeedsTheTool(t *testing.T) {
	tbl := loadTable(t)

	sole := tbl.ToolsSatisfying("inspect.node_saturation")
	if len(sole) != 1 {
		t.Fatalf("inspect.node_saturation is served by %v; this test needs a sole-source intent", sole)
	}
	other := tbl.ToolsSatisfying("inspect.pod_saturation")
	if len(other) != 1 || other[0] != sole[0] {
		t.Fatalf("the two saturation intents no longer share one sole source: %v and %v", sole, other)
	}

	// One scenario expecting both saturation intents, named the way the
	// corpus names them: upstream kubectl tools, one intent each.
	ds := Dataset{Scenarios: []Scenario{{
		ID:      "T-01",
		Outputs: ScenarioOutputs{ExpectedTools: []string{"kubectl_top_nodes", "kubectl_top_pods"}},
	}}}
	if want, unknown := tbl.IntentsFor(ds.Scenarios[0].Outputs.ExpectedTools); len(unknown) != 0 || len(want) != 2 {
		t.Fatalf("the fixture no longer maps both saturation intents: want %v, unknown %v", want, unknown)
	}
	g := GatingBy(tbl, ds)
	if got := g[sole[0]].Gates; len(got) != 1 {
		t.Errorf("gates = %v, want the one scenario counted once", got)
	}
}

func keysOf(m map[string]ToolGating) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
