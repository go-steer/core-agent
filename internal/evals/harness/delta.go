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

package harness

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strings"

	"github.com/go-steer/core-agent/v3/internal/evals"
	"github.com/go-steer/core-agent/v3/internal/evals/judge"
)

// LoadSummary reads a board written by [Summary.WriteJSON]. The nightly
// uses it to compare a run against the previous one.
func LoadSummary(path string) (Summary, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Summary{}, err
	}
	var s Summary
	if err := json.Unmarshal(b, &s); err != nil {
		return Summary{}, fmt.Errorf("harness: %s is not a board: %w", path, err)
	}
	return s, nil
}

// noiseFloor is the change below which a metric is reported as flat.
//
// The judge tier's numbers move a little between runs on their own —
// the model under test is sampling, and the grader is a model too — so a
// delta that flags every hundredth would train its reader to skip it.
// One-fiftieth of the 0-1 range is roughly a quarter of a rubric point,
// which is the smallest change a human reading two responses would
// agree was a change.
const noiseFloor = 0.02

// WriteDelta reports what changed between a previous board and this one.
//
// It never decides anything: the nightly posts the delta and a human
// reads it. Both tiers are handled, because both have a state worth
// diffing — the deterministic tier's is the expected-fail allowlist,
// whose shrinking is v0.3's progress metric, and the judge tier's is the
// scores.
func (s Summary) WriteDelta(w io.Writer, prev Summary) {
	p := func(format string, args ...any) { fmt.Fprintf(w, format+"\n", args...) }

	p("")
	p("delta vs the previous board")
	if prev.Tier != s.Tier {
		p("  the previous board is tier %s and this one is tier %s; only the parts they share are compared",
			prev.Tier, s.Tier)
	}

	// The allowlist. Its direction carries meaning the numbers do not:
	// an entry leaving means a capability landed.
	gone, arrived := diffSets(prev.ExpectedFail, s.ExpectedFail)
	switch {
	case len(gone) == 0 && len(arrived) == 0:
		p("  expected-fail allowlist: unchanged (%d)", len(s.ExpectedFail))
	default:
		if len(gone) > 0 {
			p("  expected-fail allowlist: %s no longer declared red — a capability landed", strings.Join(gone, ", "))
		}
		if len(arrived) > 0 {
			p("  expected-fail allowlist: %s newly declared red — check this was deliberate", strings.Join(arrived, ", "))
		}
	}

	if s.Judge == nil || prev.Judge == nil {
		p("")
		return
	}

	if prev.Judge.Model != s.Judge.Model || prev.Judge.Grader != s.Judge.Grader {
		p("  models changed (%s/%s → %s/%s) — the scores below are not comparable, they are two different measurements",
			prev.Judge.Model, prev.Judge.Grader, s.Judge.Model, s.Judge.Grader)
	}

	// Retries night over night, because the number that matters is the
	// trend and not the night. One retry is weather; a count that climbs
	// every night is a quota the tier is about to run out of, and the
	// board that reports it is green right up until the night it isn't
	// (#239).
	if prev.Judge.Retries != s.Judge.Retries {
		p("  provider retries: %d → %d (%.0fs → %.0fs waiting)",
			prev.Judge.Retries, s.Judge.Retries, prev.Judge.RetryWaitSeconds, s.Judge.RetryWaitSeconds)
	}

	p("")
	prevAgg := byMetricName(prev.Judge.Aggregate)
	for _, cur := range s.Judge.Aggregate {
		old, ok := prevAgg[cur.Metric]
		switch {
		case !ok:
			p("  %-20s %.3f (new)", cur.Metric, cur.Mean)
		case cur.Scored == 0 && old.Scored == 0:
			// Two means of nothing are not a flat trend. Same reasoning
			// as the board itself: 0.000 reads as every row failing.
			p("  %-20s n/a both runs — no scenario had anything to score", cur.Metric)
		case cur.Scored == 0:
			p("  %-20s n/a this run (was %.3f over %d) — the metric stopped reaching the corpus",
				cur.Metric, old.Mean, old.Scored)
		case old.Scored == 0:
			p("  %-20s %.3f over %d (the previous run had nothing to score)", cur.Metric, cur.Mean, cur.Scored)
		default:
			p("  %-20s %s", cur.Metric, move(old.Mean, cur.Mean))
		}
	}

	// Per-row movement, so a flat mean that hides two rows swapping
	// places is still visible.
	prevRows := byRowID(prev.Judge.Scenes)
	var moved []string
	for _, cur := range s.Judge.Scenes {
		old, ok := prevRows[cur.ID]
		if !ok {
			moved = append(moved, fmt.Sprintf("  %s: new row", cur.ID))
			continue
		}
		switch {
		case old.Error == "" && cur.Error != "":
			moved = append(moved, fmt.Sprintf("  %s: ran last time, did not run this time — %s", cur.ID, cur.Error))
			continue
		case old.Error != "" && cur.Error == "":
			moved = append(moved, fmt.Sprintf("  %s: did not run last time, ran this time", cur.ID))
		}
		for _, metric := range []string{
			evals.MetricIntentCoverage,
			evals.MetricSeverityAccuracy,
			judge.MetricResponseQuality,
		} {
			before, hadBefore := scoreIn(old.Results, metric)
			after, hadAfter := scoreIn(cur.Results, metric)
			if !hadBefore || !hadAfter || math.Abs(after-before) < noiseFloor {
				continue
			}
			moved = append(moved, fmt.Sprintf("  %s %s: %s", cur.ID, metric, move(before, after)))
		}
	}
	for _, id := range missing(prev.Judge.Scenes, s.Judge.Scenes) {
		moved = append(moved, fmt.Sprintf("  %s: on the previous board, absent from this one", id))
	}

	p("")
	if len(moved) == 0 {
		p("  no scenario moved by more than %.2f", noiseFloor)
	} else {
		sort.Strings(moved)
		for _, line := range moved {
			p("%s", line)
		}
	}

	writeValidityDelta(p, prev.Judge.Validity, s.Judge.Validity)
	writeMissDelta(p, prev.Judge.Misses, s.Judge.Misses)
	p("")
}

// writeMissDelta reports #170's list run over run.
//
// Named individually in both directions, because a consequential miss is
// small-N by construction: four across two whole boards at v0.4.0. At
// that size "3 → 4" tells a reader nothing and "LC-05 inspect.pod_
// saturation is new" tells them where to look. A miss that cleared is
// worth the same line — otherwise the only news this section ever
// carries is bad, and a section like that stops being read.
func writeMissDelta(p func(string, ...any), before, after MissBoard) {
	if before.Scenarios == 0 && after.Scenarios == 0 {
		return
	}
	gone, arrived := diffSets(missKeys(before), missKeys(after))
	if len(gone) == 0 && len(arrived) == 0 {
		p("")
		p("  consequential misses: unchanged (%d)", len(after.Consequential))
		return
	}
	p("")
	if len(arrived) > 0 {
		p("  newly missed, and a tool in the catalog would have answered: %s", strings.Join(arrived, "; "))
	}
	if len(gone) > 0 {
		p("  no longer missed: %s", strings.Join(gone, "; "))
	}
	writeAttributionDelta(p, before, after)
}

// writeAttributionDelta reports #171's per-tool counts when the list
// moved.
//
// This is the readout for the experiment the attribution layer exists to
// make possible: judge.toolDescription is in this repo rather than in
// k8s-lookout, so a sole-source tool that keeps getting skipped can have
// its description changed and the board re-run. "k8s_resource_top: 3
// miss(es) → 0" is that experiment's answer, and it is invisible in the
// scores — a description fix moves intent_coverage by a fraction of one
// row's worth of mean.
func writeAttributionDelta(p func(string, ...any), before, after MissBoard) {
	was := missCounts(before)
	now := missCounts(after)
	for _, tool := range unionKeys(was, now) {
		if was[tool] == now[tool] {
			continue
		}
		p("    %s: %d miss(es) → %d", tool, was[tool], now[tool])
	}
}

func missCounts(b MissBoard) map[string]int {
	out := make(map[string]int, len(b.Attribution))
	for _, a := range b.Attribution {
		out[a.Tool] = len(a.Misses)
	}
	return out
}

func unionKeys(a, b map[string]int) []string {
	seen := make(map[string]bool, len(a)+len(b))
	var out []string
	for _, m := range []map[string]int{a, b} {
		for k := range m {
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	sort.Strings(out)
	return out
}

func missKeys(b MissBoard) []string {
	out := make([]string, 0, len(b.Consequential))
	for _, m := range b.Consequential {
		out = append(out, m.Scenario+" "+m.Intent)
	}
	return out
}

// writeValidityDelta reports #169's counts run over run.
//
// Call validity is not a score, so none of the means above move when it
// changes — which is exactly why it belongs in the delta. A run that
// started making malformed calls, or started running blind on rows it
// used to read, can hold its intent_coverage steady while the reason
// behind the number has changed completely.
func writeValidityDelta(p func(string, ...any), before, after ValidityBoard) {
	if before.Calls == 0 && after.Calls == 0 {
		return
	}
	p("")
	p("  calls %d → %d, malformed %d → %d, empty reads %d → %d",
		before.Calls, after.Calls,
		len(before.Malformed), len(after.Malformed),
		before.EmptyReads, after.EmptyReads)
	gone, arrived := diffSets(before.Blind, after.Blind)
	if len(arrived) > 0 {
		p("  started running blind — every completed call came back empty: %s", strings.Join(arrived, ", "))
	}
	if len(gone) > 0 {
		p("  no longer running blind: %s", strings.Join(gone, ", "))
	}
}

// move renders one number's change, with the direction spelled out
// rather than colour-coded: this is read in a CI log and in a job
// summary, and neither is a terminal.
func move(before, after float64) string {
	d := after - before
	switch {
	case math.Abs(d) < noiseFloor:
		return fmt.Sprintf("%.3f (was %.3f, flat)", after, before)
	case d > 0:
		return fmt.Sprintf("%.3f (was %.3f, up %.3f)", after, before, d)
	default:
		return fmt.Sprintf("%.3f (was %.3f, down %.3f)", after, before, -d)
	}
}

func byMetricName(ms []MetricSummary) map[string]MetricSummary {
	out := make(map[string]MetricSummary, len(ms))
	for _, m := range ms {
		out[m.Metric] = m
	}
	return out
}

func byRowID(rows []JudgeScenario) map[string]JudgeScenario {
	out := make(map[string]JudgeScenario, len(rows))
	for _, r := range rows {
		out[r.ID] = r
	}
	return out
}

func scoreIn(results []evals.MetricResult, metric string) (float64, bool) {
	for _, r := range results {
		if r.Metric == metric {
			if r.Vacuous {
				return 0, false
			}
			return r.Score, true
		}
	}
	return 0, false
}

func missing(prev, cur []JudgeScenario) []string {
	have := byRowID(cur)
	var out []string
	for _, r := range prev {
		if _, ok := have[r.ID]; !ok {
			out = append(out, r.ID)
		}
	}
	return out
}

// diffSets returns what left a and what arrived in b.
func diffSets(a, b []string) (gone, arrived []string) {
	inB := make(map[string]bool, len(b))
	for _, s := range b {
		inB[s] = true
	}
	inA := make(map[string]bool, len(a))
	for _, s := range a {
		inA[s] = true
		if !inB[s] {
			gone = append(gone, s)
		}
	}
	for _, s := range b {
		if !inA[s] {
			arrived = append(arrived, s)
		}
	}
	sort.Strings(gone)
	sort.Strings(arrived)
	return gone, arrived
}
