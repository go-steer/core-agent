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

import "fmt"

// MetricReach reports how much of the corpus one metric can actually score.
type MetricReach struct {
	Metric     string
	Diagnostic bool
	Scenarios  int
	Reaches    int
}

// Dead reports a metric that can score nothing anywhere in the corpus.
func (r MetricReach) Dead() bool { return r.Scenarios > 0 && r.Reaches == 0 }

// String renders one row of the reach table.
func (r MetricReach) String() string {
	s := fmt.Sprintf("%-18s %d/%d scenarios scorable", r.Metric, r.Reaches, r.Scenarios)
	if r.Diagnostic {
		s += " (diagnostic)"
	}
	if r.Dead() {
		s += "  <- DEAD: scores nothing, anywhere"
	}
	return s
}

// CorpusReach measures the corpus-side metrics against the loaded dataset and
// intent table.
func CorpusReach(tbl IntentTable, ds Dataset) []MetricReach {
	reach := []MetricReach{
		{Metric: MetricIntentCoverage},
		{Metric: MetricToolCoverage, Diagnostic: true},
		{Metric: MetricSeverityAccuracy, Diagnostic: true},
	}
	for _, sc := range ds.Scenarios {
		probes := []MetricResult{
			IntentCoverage(tbl, sc, Trace{}),
			ToolCoverage(tbl, sc, Trace{}),
			SeverityAccuracy(sc, Trace{}),
		}
		for i := range reach {
			reach[i].Scenarios++
			if !probes[i].Vacuous {
				reach[i].Reaches++
			}
		}
	}
	return reach
}

// DeadMetrics returns the gating metrics that score nothing anywhere.
func DeadMetrics(reach []MetricReach) []string {
	var dead []string
	for _, r := range reach {
		if r.Diagnostic || !r.Dead() {
			continue
		}
		dead = append(dead, r.Metric)
	}
	return dead
}
