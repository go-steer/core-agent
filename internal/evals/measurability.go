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

// MetricReach reports how much of the corpus one metric can actually
// score — the guard against a metric that is green because it never ran.
//
// Upstream's harness is the cautionary case, twice over: its
// tool_coverage reads a key no scenario carries and returns 1.0 on all
// 31 rows, and its severity_accuracy extracts a bracketed token the
// corpus never writes and returns 0 on all 31. Both are constant
// functions, and nothing in either output says so. Result.Vacuous marks
// the individual case; this is the corpus-wide roll-up that turns it
// into a harness failure instead of a green board.
//
// Reach is necessary and not sufficient, which severity_accuracy shows
// from both ends. mast's extractor reads all 31 expected responses where
// upstream's reads none, so the metric has reached the whole corpus
// since v0.3 — and it was still wrong in two ways this guard cannot see.
// It silently failed to read a decorated verdict on the *actual* side
// (7 of 31 rows of one 2026-08-19 board, 0 of 31 of another, so the
// published gap between two model families was partly markdown style;
// fixed in extractSeverity), and the corpus states no severity
// definition for the buckets 20 of its 31 rows are labelled with, so the
// score it produces cannot be acted on however completely it reaches
// (#179, which demoted it to diagnostic). A metric that scores every row
// has cleared this bar and no other.
//
// tool_coverage is the same lesson from the other side: this table
// reported it 31/31 scorable while it returned 0.000 on every row of
// every board, because reach asked whether a scenario had *declared* an
// expectation rather than whether the expectation could be *met*. A
// guard that measures the wrong property is the defect it exists to
// catch, so it caught nothing about itself. Fixed in ToolCoverage (#174)
// rather than here, because vacuity is the evaluator's judgement and
// this table only counts it.
type MetricReach struct {
	Metric string
	// Diagnostic mirrors Result.Diagnostic: a diagnostic metric is
	// reported but never gates, because it is not a claim about mast.
	Diagnostic bool
	// Scenarios is how many the metric was probed against.
	Scenarios int
	// Reaches is how many of them give it something to score.
	Reaches int
}

// Dead reports a metric that can score nothing anywhere in the corpus.
// Its score carries no information, whatever it happens to be.
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

// CorpusReach measures the corpus-side metrics against the loaded
// dataset and intent table.
//
// The three metrics here are the ones whose vacuity is a property of the
// *expectation* rather than of any run: a scenario that declares no
// expected tools gives intent_coverage nothing to score no matter what
// the agent does, and one whose expected response carries no severity
// token gives severity_accuracy nothing to compare. That is why the
// probe passes an empty Trace — it isolates the corpus half, and it uses
// the real evaluators so the guard follows any change to what vacuity
// means. TestCorpusReach_IsTraceIndependent pins the property the probe
// relies on.
//
// Since #174 the expectation is read against the intent table's tool
// catalog as well as against the scenario, which is why the table is a
// parameter rather than only intent_coverage's business. That is still a
// fact about the corpus and not about the run: which names exist is
// fixed before any agent starts, so the empty-trace probe holds.
//
// effect_ordering and exactly_once are deliberately absent: their
// vacuity is a property of the run (no mutating effects to order), not
// of the corpus, so there is nothing here for them to be measured
// against. The differentiator tier is what proves those two are not
// constants — E-exactly-once asserts both score 1.00 on a run that
// actually mutates, and would catch an evaluator that had degenerated
// into returning a fixed value.
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
// A non-empty result is a harness failure, not a red scoreboard row:
// the measurement is broken, so the board says nothing either way.
//
// Diagnostics are deliberately absent, and that omission is load-bearing
// in both directions. A diagnostic is not a claim about mast, so a dead
// one must not fail the build — tool_coverage has been dead by
// construction since the corpus was ported and always will be. But a
// dead column still has to be *said*, or the reader takes a constant for
// a measurement, so harness.summarizeCorpus reports it on a non-gating
// line of its own.
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
