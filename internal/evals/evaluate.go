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
// W0.4 expected-fail allowlist's vocabulary, so treat them as stable.
const (
	MetricIntentCoverage   = "intent_coverage"
	MetricToolCoverage     = "tool_coverage"
	MetricSeverityAccuracy = "severity_accuracy"
	MetricEffectOrdering   = "effect_ordering"
	MetricExactlyOnce      = "exactly_once"
)

// MetricResult is one evaluator's verdict on one scenario.
type MetricResult struct {
	// The json tags matter: a MetricResult is serialized into the judge
	// board, which is a durable artifact the nightly diffs and a human
	// reads. Untagged, it was the one PascalCase island in an otherwise
	// snake_case document.
	Metric string `json:"metric"`
	// Score is in [0,1]. For the two invariants it is binary: an
	// invariant is not partially held.
	Score float64 `json:"score"`
	// Comment says why, in operator-readable terms. It is the part a
	// human reads when a scoreboard cell goes red, so it names the
	// specific intent, tool, or call rather than reporting a bare count.
	Comment string `json:"comment,omitempty"`

	// Diagnostic marks a metric emitted for visibility only. A
	// diagnostic score is never a comparison number and never gates
	// CI — see tool_coverage, which scores mast's consolidated read
	// path as a regression by construction.
	Diagnostic bool `json:"diagnostic,omitempty"`

	// Vacuous marks a score that carries no information about the run:
	// its value is fixed by the corpus and the tool surface, not by
	// anything the agent did. Upstream's tool_coverage is 1.0 on all 31
	// rows for exactly this reason (it reads a key no row has), and a
	// harness that cannot tell the two apart reports a perfect score for
	// a metric that never ran.
	//
	// The value is usually 1.0 and for a while it was only ever that.
	// It is not always: mast's own tool_coverage is pinned at 0 on a
	// scenario whose expected names the runtime cannot emit (#174). Both
	// are constants. Which direction a constant points is not the
	// property this field is about.
	Vacuous bool `json:"vacuous,omitempty"`
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

// IntentCoverage is v0.3's primary trajectory metric: of the diagnostic
// questions the scenario expects answered, what fraction did the run
// actually answer?
//
// The denominator is the scenario's expected intents, including intents
// reachable only through upstream tool names that upstream itself cannot
// call (the 7 phantoms). Those names are an upstream-side artifact, and
// the intents behind them are reachable by lookout, so excluding them
// would hand mast credit for upstream's dataset bug. The unreachability
// is recorded as an annotation in intents.yaml, not folded into a score.
//
// An expected tool name the intent table has never seen also stays in
// the denominator: a table gap should deflate the metric visibly rather
// than quietly shrink what is being measured.
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

// ToolCoverage is upstream's name-level trajectory metric, reimplemented
// with the semantics upstream intended (it reads expected_trajectory, a
// key no row carries, so it returns 1.0 unconditionally).
//
// It is emitted as a diagnostic and must never be reported as a
// comparison number. Name-level set overlap scores a better-factored
// read path as a regression: LC-22 names three upstream tools that one
// k8s_triage_workload call answers completely, and this metric scores
// that 0/3. Keeping it visible keeps the consolidation penalty legible
// instead of scored.
//
// # Vacuity is satisfiability, not declaration (#174)
//
// A scenario reaches this metric only when at least one name it expects
// is a name the runtime can emit. That is a stronger test than "the
// scenario declared something", and the difference is the whole corpus:
// the ported dataset names upstream's kubectl_* surface, mast's is
// k8s_*, and the intersection is empty on all 31 rows. Under the
// declaration-shaped test every row reached, so CorpusReach reported
// 31/31 scorable for a metric that is 0.000 by construction — the guard
// against a constant function calling a constant function healthy.
//
// The neighbouring intent_coverage never had this problem, because it
// builds its denominator after mapping names through the intent table.
// Two definitions of reach sat side by side in one file and only the
// declaration-shaped one was wrong; that asymmetry was the bug, rather
// than any single line.
//
// Note what does not change: the score. An unsatisfiable expectation
// still reports 0/N and still names the tools, because the consolidation
// penalty is the reason this metric exists. What changes is that the
// harness stops averaging that 0 into a board, and says out loud that
// the column is a constant.
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
//
// The extractor is deliberately not upstream's. Upstream matches
// \[(CRITICAL|WARNING|INFO|OK)\] — bracketed — against data that writes
// a bare "CRITICAL: " prefix, so it scores 0 on all 31 rows regardless
// of what any agent does. Here the expected side is read the way the
// corpus is actually written, and the actual side accepts the formats a
// model plausibly emits for the same claim.
//
// # Why this is diagnostic and not a comparison number (#179)
//
// Every board since v0.3 has read near half here, with the misses almost
// all one direction — expected WARNING, got CRITICAL — on two unrelated
// model families. Two readings fit that equally well: the corpus labels
// contradict the severity definitions the corpus itself ships, or mast's
// rubric escalates past them. #179 asked for the misses to be
// partitioned between the two, against the corpus's own stated
// definitions rather than its labels.
//
// That partition is not derivable from this corpus. The definitions are
// twelve enumerated conditions (judge.systemInstruction, taken verbatim
// from upstream): CRITICAL is "service down, crash loops, OOM kills, 0
// ready endpoints", WARNING is "no PDB, missing probes, :latest images,
// wildcard RBAC", INFO is "right-sizing, orphaned PVs, suspended
// CronJobs". Over the 31 scenarios, the CRITICAL conditions occur in 5;
// the WARNING and INFO conditions occur in none at all. Twenty of the 31
// rows are nonetheless labelled WARNING or INFO — buckets the rubric
// describes with four hygiene examples the corpus never once presents.
// judge.TestSeverityRubricDoesNotSpanCorpus pins that measurement.
//
// So on almost every row that misses, there is no stated definition to
// partition against, and the two readings are indistinguishable in
// principle rather than merely by this metric. Where the rubric does
// decide a row it contradicts the label twice in five — LC-03 is an
// OOMKill labelled WARNING, LC-10 has 0 endpoints and is labelled
// WARNING — and both families miss both rows by answering what they were
// told to answer.
//
// Tuning the rubric until the number moves was rejected in v0.3 as
// teaching to the test, and that still holds: there is nothing here to
// tune against. The number is reported, and the per-row comment stays
// legible, but it is not a claim about mast — the same disposition
// tool_coverage has, for a sibling reason. Re-promoting it needs a
// corpus that states a basis for the buckets it labels with.
func SeverityAccuracy(sc Scenario, tr Trace) MetricResult {
	want := extractSeverity(sc.Outputs.ExpectedResponse)
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
		got, source = extractSeverity(tr.FinalText), "final response text"
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

// extractSeverity pulls a severity token out of a response.
//
// It matches only at the start of a line, after stripping markdown
// decoration and an optional "Severity:" label. The anchor is what keeps
// prose out: "the pod is not critical" never begins a line with the
// token, while every format a model actually uses for the verdict does —
// "CRITICAL: ...", "[CRITICAL] ...", "**CRITICAL**", "## CRITICAL",
// "Severity: CRITICAL". The first line that yields a token wins.
//
// Decoration is stripped on both sides of the label because a real
// response carries both at once. Until 2026-08-19 the label was stripped
// first and the decoration second, so "## SEVERITY: CRITICAL" matched
// nothing: the label pattern is anchored and cannot reach past "## ",
// and once the decoration is gone the line opens with "SEVERITY:" rather
// than the token. That is one line of a decorated heading — the single
// most common way a model writes this verdict — silently scoring as "the
// run declared no severity", on 7 of 31 rows of one board and 0 of 31 of
// another. Trimming decoration, then the label, then decoration again
// costs nothing and admits the combination.
func extractSeverity(s string) string {
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

// trimDecoration removes the markdown a model wraps a verdict in.
func trimDecoration(line string) string {
	return strings.TrimLeft(line, " \t#*_[(\"'`>-")
}

// EffectOrdering checks the outbox invariant on the recorded log: every
// mutating effect that completed has its intent recorded durably first,
// at a strictly lower event index.
//
// This is mast-only — upstream has no equivalent, because it has no
// durable record of an in-flight mutation. It is the invariant that makes
// crash recovery decidable: a call with an intent and no completion is
// the ambiguous window the operator gets asked about, while a completion
// with no preceding intent means the log cannot tell what ran.
//
// Read-only calls are not scored (their re-execution is free), but an
// orphaned read-only completion is reported in the comment as a
// log-integrity signal.
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

// ExactlyOnce checks that no mutating effect completed twice.
//
// Identity is tool name plus canonicalized arguments: scaling two
// different deployments is two effects, scaling one deployment twice is
// the violation. This is the metric that catches a blind resume
// re-firing a mutation whose completion was lost to a crash — the
// failure the recorded-effect outbox exists to prevent, and the one
// upstream's harness has no way to observe.
func ExactlyOnce(tr Trace) MetricResult {
	counts := make(map[string]int)
	labels := make(map[string]string)
	completed := 0

	for _, c := range tr.Calls {
		if !c.Mutating() || !c.Completed {
			continue
		}
		completed++
		id := c.identity()
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
