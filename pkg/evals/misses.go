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

import "sort"

// ConsequentialMiss is one expected intent the run left unanswered that
// a tool in the catalog would have answered.
type ConsequentialMiss struct {
	Intent   string   `json:"intent"`
	ServedBy []string `json:"served_by"`
}

// MissReport partitions one scenario's unsatisfied expectations by whose
// problem each one is: the model's, the catalog's, or the table's.
type MissReport struct {
	Consequential []ConsequentialMiss `json:"consequential,omitempty"`
	OutOfCatalog  []string            `json:"out_of_catalog,omitempty"`
	Untabled      []string            `json:"untabled,omitempty"`
}

// Empty reports whether the scenario left nothing unsatisfied at all.
func (m MissReport) Empty() bool {
	return len(m.Consequential) == 0 && len(m.OutOfCatalog) == 0 && len(m.Untabled) == 0
}

// ClassifyMisses explains what [IntentCoverage] scored: for every
// expected intent the run did not satisfy, whether a tool in the catalog
// would have satisfied it.
func ClassifyMisses(tbl IntentTable, sc Scenario, tr Trace) MissReport {
	cov := readCoverage(tbl, sc, tr)
	rep := MissReport{Untabled: cov.unknown}
	for _, id := range cov.missing {
		served := tbl.ToolsSatisfying(id)
		if len(served) == 0 {
			rep.OutOfCatalog = append(rep.OutOfCatalog, id)
			continue
		}
		rep.Consequential = append(rep.Consequential, ConsequentialMiss{Intent: id, ServedBy: served})
	}
	return rep
}

type coverage struct {
	want    []string
	unknown []string
	missing []string
}

func readCoverage(tbl IntentTable, sc Scenario, tr Trace) coverage {
	cov := coverage{}
	cov.want, cov.unknown = tbl.IntentsFor(sc.Outputs.ExpectedTools)

	got := make(map[string]bool)
	for _, id := range tbl.SatisfiedBy(tr.CalledTools()) {
		got[id] = true
	}
	for _, id := range cov.want {
		if !got[id] {
			cov.missing = append(cov.missing, id)
		}
	}
	return cov
}

// ToolGating is what one tool's absence costs the corpus (#171).
type ToolGating struct {
	Tool       string   `json:"tool"`
	SoleSource []string `json:"sole_source_for"`
	Gates      []string `json:"gates"`
}

// GatingBy indexes the corpus by sole-source tool.
func GatingBy(tbl IntentTable, ds Dataset) map[string]ToolGating {
	sole := make(map[string]string, len(tbl.Intents))
	out := make(map[string]ToolGating)
	for _, in := range tbl.Intents {
		servers := tbl.ToolsSatisfying(in.ID)
		if len(servers) != 1 {
			continue
		}
		sole[in.ID] = servers[0]
		g := out[servers[0]]
		g.Tool = servers[0]
		g.SoleSource = append(g.SoleSource, in.ID)
		out[servers[0]] = g
	}

	for _, sc := range ds.Scenarios {
		want, _ := tbl.IntentsFor(sc.Outputs.ExpectedTools)
		counted := make(map[string]bool, len(want))
		for _, id := range want {
			name, ok := sole[id]
			if !ok || counted[name] {
				continue
			}
			counted[name] = true
			g := out[name]
			g.Gates = append(g.Gates, sc.ID)
			out[name] = g
		}
	}

	for name, g := range out {
		sort.Strings(g.SoleSource)
		sort.Strings(g.Gates)
		out[name] = g
	}
	return out
}

// ToolsSatisfying returns the catalog tools that satisfy an intent, in
// name order.
func (t IntentTable) ToolsSatisfying(intent string) []string {
	var out []string
	for name, lt := range t.LookoutTools {
		for _, id := range lt.Satisfies {
			if id == intent {
				out = append(out, name)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}
