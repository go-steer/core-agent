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
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Metric names. These strings are the scoreboard's column keys and the
// expected-fail allowlist's vocabulary, so treat them as stable.
const (
	MetricIntentCoverage   = "intent_coverage"
	MetricToolCoverage     = "tool_coverage"
	MetricSeverityAccuracy = "severity_accuracy"
	MetricEffectOrdering   = "effect_ordering"
	MetricExactlyOnce      = "exactly_once"
)

// MetricResult is one evaluator's verdict on one scenario.
type MetricResult struct {
	Metric     string  `json:"metric"`
	Score      float64 `json:"score"`
	Comment    string  `json:"comment,omitempty"`
	Diagnostic bool    `json:"diagnostic,omitempty"`
	Vacuous    bool    `json:"vacuous,omitempty"`
}

// Passed reports whether a non-diagnostic result is a full score.
func (r MetricResult) Passed() bool { return r.Score >= 1 }

// EvaluateAll runs every deterministic evaluator against one recorded
// run. Order is stable so scoreboard rows line up.
func EvaluateAll(tbl IntentTable, sc Scenario, tr Trace) []MetricResult {
	return []MetricResult{
		IntentCoverage(tbl, sc, tr),
		ToolCoverage(tbl, sc, tr),
		SeverityAccuracy(sc, tr),
		EffectOrdering(tr),
		ExactlyOnce(tr),
	}
}

// IntentCoverage measures what fraction of the diagnostic questions a
// scenario expects answered were actually answered by the run.
func IntentCoverage(tbl IntentTable, sc Scenario, tr Trace) MetricResult {
	cov := readCoverage(tbl, sc, tr)
	unknown, missing := cov.unknown, cov.missing
	denom := len(cov.want) + len(unknown)
	if denom == 0 {
		return MetricResult{
			Metric:  MetricIntentCoverage,
			Score:   1,
			Vacuous: true,
			Comment: "scenario declares no expected tools; nothing to cover",
		}
	}
	hit := len(cov.want) - len(missing)

	res := MetricResult{
		Metric: MetricIntentCoverage,
		Score:  float64(hit) / float64(denom),
	}
	switch {
	case len(missing) == 0 && len(unknown) == 0:
		res.Comment = fmt.Sprintf("all %d expected intents satisfied by %d call(s)", denom, len(tr.Calls))
	default:
		var parts []string
		if len(missing) > 0 {
			parts = append(parts, "unsatisfied intents: "+strings.Join(missing, ", "))
		}
		if len(unknown) > 0 {
			parts = append(parts, "expected tools missing from intents.yaml: "+strings.Join(unknown, ", "))
		}
		res.Comment = fmt.Sprintf("%d/%d — %s", hit, denom, strings.Join(parts, "; "))
	}
	return res
}

// ToolCoverage is the diagnostic name-level trajectory metric.
func ToolCoverage(tbl IntentTable, sc Scenario, tr Trace) MetricResult {
	want := sc.Outputs.ExpectedTools
	if len(want) == 0 {
		return MetricResult{
			Metric:     MetricToolCoverage,
			Score:      1,
			Diagnostic: true,
			Vacuous:    true,
			Comment:    "scenario declares no expected tools; nothing to cover",
		}
	}
	called := make(map[string]bool)
	for _, name := range tr.CalledTools() {
		called[name] = true
	}
	seen := make(map[string]bool, len(want))
	hit, denom := 0, 0
	var unemittable []string
	for _, name := range want {
		if seen[name] {
			continue
		}
		seen[name] = true
		denom++
		if !tbl.Emittable(name) {
			unemittable = append(unemittable, name)
		}
		if called[name] {
			hit++
		}
	}
	if len(unemittable) == denom {
		return MetricResult{
			Metric:     MetricToolCoverage,
			Score:      0,
			Diagnostic: true,
			Vacuous:    true,
			Comment: fmt.Sprintf(
				"0/%d — no expected name is one this runtime can emit (%s), so no run can move this score; excluded from the mean, not earned",
				denom, strings.Join(unemittable, ", ")),
		}
	}
	comment := fmt.Sprintf("%d/%d expected tool names called verbatim (diagnostic only)", hit, denom)
	if len(unemittable) > 0 {
		comment += fmt.Sprintf("; %d of the %d cannot be emitted by this runtime (%s)",
			len(unemittable), denom, strings.Join(unemittable, ", "))
	}
	return MetricResult{
		Metric:     MetricToolCoverage,
		Score:      float64(hit) / float64(denom),
		Diagnostic: true,
		Comment:    comment,
	}
}

// SeverityAccuracy is an exact match on the severity the run declared
// against the severity the scenario expects.
func SeverityAccuracy(sc Scenario, tr Trace) MetricResult {
	want := ExtractSeverity(sc.Outputs.ExpectedResponse)
	if want == "" {
		return MetricResult{
			Metric:     MetricSeverityAccuracy,
			Score:      1,
			Diagnostic: true,
			Vacuous:    true,
			Comment:    "scenario declares no expected severity",
		}
	}

	got, source := tr.StructuredSeverity, "structured report"
	if got == "" {
		got, source = ExtractSeverity(tr.FinalText), "final response text"
	}
	got = strings.ToUpper(strings.TrimSpace(got))

	switch {
	case got == "":
		return MetricResult{
			Metric:     MetricSeverityAccuracy,
			Score:      0,
			Diagnostic: true,
			Comment:    fmt.Sprintf("expected %s; run declared no severity in its %s", want, source),
		}
	case got != want:
		return MetricResult{
			Metric:     MetricSeverityAccuracy,
			Score:      0,
			Diagnostic: true,
			Comment:    fmt.Sprintf("expected %s, got %s (from %s)", want, got, source),
		}
	}
	return MetricResult{
		Metric:     MetricSeverityAccuracy,
		Score:      1,
		Diagnostic: true,
		Comment:    fmt.Sprintf("%s (from %s)", got, source),
	}
}

var (
	severityLabel = regexp.MustCompile(`(?i)^\s*severity\s*[:=]\s*`)
	severityToken = regexp.MustCompile(`(?i)^(CRITICAL|WARNING|INFO|OK)\b`)
)

// ExtractSeverity pulls a severity token (CRITICAL, WARNING, INFO, OK) out of
// a response string.
func ExtractSeverity(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = trimDecoration(line)
		line = severityLabel.ReplaceAllString(line, "")
		line = trimDecoration(line)
		if m := severityToken.FindStringSubmatch(line); m != nil {
			return strings.ToUpper(m[1])
		}
	}
	return ""
}

func trimDecoration(line string) string {
	return strings.TrimLeft(line, " \t#*_[(\"'`>-")
}

// EffectOrdering checks the outbox invariant on the recorded log: every
// mutating effect that completed has its intent recorded durably first,
// at a strictly lower event index.
func EffectOrdering(tr Trace) MetricResult {
	var violations []string
	readOnlyOrphans := 0
	mutating := 0

	for _, c := range tr.Calls {
		orphan := c.EventIndex < 0
		if !c.Mutating() {
			if orphan {
				readOnlyOrphans++
			}
			continue
		}
		mutating++
		switch {
		case orphan:
			violations = append(violations, fmt.Sprintf(
				"%s (%s) completed with no recorded intent", c.Name, c.ID))
		case c.Completed && c.ResponseIndex <= c.EventIndex:
			violations = append(violations, fmt.Sprintf(
				"%s (%s) completed at event %d, not after its intent at event %d",
				c.Name, c.ID, c.ResponseIndex, c.EventIndex))
		}
	}

	if mutating == 0 {
		return MetricResult{
			Metric:  MetricEffectOrdering,
			Score:   1,
			Vacuous: true,
			Comment: orphanNote("run recorded no mutating effects", readOnlyOrphans),
		}
	}
	if len(violations) > 0 {
		return MetricResult{
			Metric:  MetricEffectOrdering,
			Score:   0,
			Comment: orphanNote(strings.Join(violations, "; "), readOnlyOrphans),
		}
	}
	return MetricResult{
		Metric:  MetricEffectOrdering,
		Score:   1,
		Comment: orphanNote(fmt.Sprintf("%d mutating effect(s), each preceded by its intent", mutating), readOnlyOrphans),
	}
}

func orphanNote(base string, readOnlyOrphans int) string {
	if readOnlyOrphans == 0 {
		return base
	}
	return fmt.Sprintf("%s (plus %d orphaned read-only completion(s))", base, readOnlyOrphans)
}

// ExactlyOnce checks that no mutating effect completed twice with identical
// arguments.
func ExactlyOnce(tr Trace) MetricResult {
	counts := make(map[string]int)
	labels := make(map[string]string)
	completed := 0

	for _, c := range tr.Calls {
		if !c.Mutating() || !c.Completed {
			continue
		}
		completed++
		id := c.Identity()
		counts[id]++
		labels[id] = c.Name
	}

	var dupes []string
	for id, n := range counts {
		if n > 1 {
			dupes = append(dupes, fmt.Sprintf("%s completed %d times with identical arguments", labels[id], n))
		}
	}
	sort.Strings(dupes)

	if completed == 0 {
		return MetricResult{
			Metric:  MetricExactlyOnce,
			Score:   1,
			Vacuous: true,
			Comment: "run completed no mutating effects",
		}
	}
	if len(dupes) > 0 {
		return MetricResult{
			Metric:  MetricExactlyOnce,
			Score:   0,
			Comment: strings.Join(dupes, "; "),
		}
	}
	return MetricResult{
		Metric:  MetricExactlyOnce,
		Score:   1,
		Comment: fmt.Sprintf("%d mutating effect(s), each completed once", completed),
	}
}
