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
	"math"
	"strings"
	"testing"

	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v3/internal/effects"
)

// callsTo builds a Trace of completed read-only calls, the shape almost
// every diagnostic scenario produces.
func callsTo(names ...string) Trace {
	var tr Trace
	for i, n := range names {
		tr.Calls = append(tr.Calls, Call{
			Name:          n,
			ID:            n,
			Class:         effects.ClassReadOnly,
			EventIndex:    2 * i,
			Completed:     true,
			ResponseIndex: 2*i + 1,
		})
	}
	return tr
}

func mutation(name, id string, args map[string]any, callIdx, respIdx int) Call {
	return Call{
		Name:          name,
		Args:          args,
		ID:            id,
		Class:         effects.ClassMutating,
		EventIndex:    callIdx,
		Completed:     respIdx >= 0,
		ResponseIndex: respIdx,
	}
}

func scenario(expectedTools []string, expectedResponse string) Scenario {
	return Scenario{
		ID: "T-01",
		Outputs: ScenarioOutputs{
			ExpectedTools:    expectedTools,
			ExpectedResponse: expectedResponse,
		},
	}
}

func approx(got, want float64) bool { return math.Abs(got-want) < 1e-9 }

func TestIntentCoverage(t *testing.T) {
	tbl := loadTable(t)

	tests := []struct {
		name      string
		expected  []string
		called    []string
		want      float64
		vacuous   bool
		inComment string
	}{
		{
			// The consolidation case the whole metric exists for: LC-22
			// names three upstream tools that a single k8s_triage_workload
			// call answers completely. Name-level overlap scores this 0/3.
			name:     "one lookout call satisfies a three-tool scenario",
			expected: []string{"kubectl_describe_pod", "kubectl_get_events", "kubectl_get_pod_logs"},
			called:   []string{"k8s_triage_workload"},
			want:     1,
		},
		{
			name:      "partial — the logs intent is unanswered",
			expected:  []string{"kubectl_describe_pod", "kubectl_get_pod_logs"},
			called:    []string{"k8s_resource_spec"},
			want:      0.5,
			inComment: "inspect.logs",
		},
		{
			name:     "no tools called covers nothing",
			expected: []string{"kubectl_describe_pod"},
			called:   nil,
			want:     0,
		},
		{
			name:     "a tool outside the table satisfies nothing",
			expected: []string{"kubectl_describe_pod"},
			called:   []string{"some_other_toolset_tool"},
			want:     0,
		},
		{
			// A phantom stays in the denominator: the intent behind it is
			// reachable by lookout even though no upstream run can call
			// the name. Excluding it would credit mast for an upstream bug.
			name:     "a phantom upstream name is still scored",
			expected: []string{"kubectl_describe_node"},
			called:   []string{"k8s_cluster_health"},
			want:     1,
		},
		{
			// An expected name the table has never seen deflates the
			// score instead of silently shrinking the denominator.
			name:      "an unmapped expected tool counts against coverage",
			expected:  []string{"kubectl_describe_pod", "kubectl_invented_tool"},
			called:    []string{"k8s_triage_workload"},
			want:      0.5,
			inComment: "kubectl_invented_tool",
		},
		{
			name:      "duplicate expected names collapse to one intent",
			expected:  []string{"kubectl_get_pod_logs", "kubectl_get_pod_logs"},
			called:    []string{"k8s_triage_logs"},
			want:      1,
			inComment: "all 1 expected intents",
		},
		{
			name:     "nothing expected is vacuous, not earned",
			expected: nil,
			called:   []string{"k8s_cluster_health"},
			want:     1,
			vacuous:  true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := IntentCoverage(tbl, scenario(tc.expected, ""), callsTo(tc.called...))
			if got.Metric != MetricIntentCoverage {
				t.Fatalf("Metric = %q", got.Metric)
			}
			if !approx(got.Score, tc.want) {
				t.Fatalf("Score = %v, want %v (%s)", got.Score, tc.want, got.Comment)
			}
			if got.Vacuous != tc.vacuous {
				t.Fatalf("Vacuous = %v, want %v", got.Vacuous, tc.vacuous)
			}
			if got.Diagnostic {
				t.Fatal("intent_coverage is the primary metric, not a diagnostic")
			}
			if tc.inComment != "" && !strings.Contains(got.Comment, tc.inComment) {
				t.Fatalf("Comment = %q, want it to name %q", got.Comment, tc.inComment)
			}
		})
	}
}

// TestToolCoverage_PenalisesConsolidation pins the reason the name-level
// metric is a diagnostic: on the exact scenario intent_coverage scores
// 1.0, it scores 0.
//
// It also pins the half of that which #174 changed. The 0 stays — the
// consolidation penalty is the point and is still legible in the
// comment — but it is now marked Vacuous, because no run could have
// produced anything else. Both halves have to hold together: a fix that
// suppressed the 0 would have hidden the penalty, and a fix that left it
// unmarked is what let the reach table call a constant healthy.
func TestToolCoverage_PenalisesConsolidation(t *testing.T) {
	tbl := loadTable(t)
	sc := scenario([]string{"kubectl_describe_pod", "kubectl_get_events", "kubectl_get_pod_logs"}, "")
	tr := callsTo("k8s_triage_workload")

	intent := IntentCoverage(tbl, sc, tr)
	tool := ToolCoverage(tbl, sc, tr)

	if !approx(intent.Score, 1) {
		t.Fatalf("intent_coverage = %v, want 1 (%s)", intent.Score, intent.Comment)
	}
	if !approx(tool.Score, 0) {
		t.Fatalf("tool_coverage = %v, want 0 — the consolidation penalty is the point", tool.Score)
	}
	if !tool.Diagnostic {
		t.Fatal("tool_coverage must be flagged Diagnostic; it is never a comparison number")
	}
	if !tool.Vacuous {
		t.Error("tool_coverage scored 0 on names no runtime can emit and did not mark it Vacuous; " +
			"CorpusReach then counts the row as scorable and the board averages a constant (#174)")
	}
	for _, name := range []string{"kubectl_describe_pod", "kubectl_get_events", "kubectl_get_pod_logs"} {
		if !strings.Contains(tool.Comment, name) {
			t.Errorf("Comment = %q, want it to name the unemittable %q — the penalty has to stay readable", tool.Comment, name)
		}
	}
}

// TestToolCoverage_SatisfiableExpectationsStillReach is the other side of
// #174, and the reason the fix cannot degenerate into "diagnostics never
// reach": a scenario that expects a name this runtime can emit is scored
// normally, whether or not the run emitted it.
func TestToolCoverage_SatisfiableExpectationsStillReach(t *testing.T) {
	tbl := loadTable(t)
	for _, tc := range []struct {
		name  string
		trace Trace
		want  float64
	}{
		{"called", callsTo("k8s_cluster_health"), 1},
		{"not called", callsTo("k8s_triage_logs"), 0},
		{"nothing called at all", Trace{}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ToolCoverage(tbl, scenario([]string{"k8s_cluster_health"}, ""), tc.trace)
			if got.Vacuous {
				t.Fatalf("Vacuous = true on a satisfiable expectation (%s)", got.Comment)
			}
			if !approx(got.Score, tc.want) {
				t.Fatalf("Score = %v, want %v (%s)", got.Score, tc.want, got.Comment)
			}
		})
	}
}

func TestToolCoverage(t *testing.T) {
	tbl := loadTable(t)
	// Real lookout names, because since #174 the metric asks whether the
	// runtime could emit the name at all and a made-up "a" cannot.
	const (
		a = "k8s_cluster_health"
		b = "k8s_triage_logs"
		z = "k8s_resource_top"
	)
	tests := []struct {
		name      string
		expected  []string
		called    []string
		want      float64
		vacuous   bool
		inComment string
	}{
		{name: "all names called verbatim", expected: []string{a, b}, called: []string{a, b}, want: 1},
		{name: "half", expected: []string{a, b}, called: []string{a, z}, want: 0.5},
		{name: "extra calls are not penalised", expected: []string{a}, called: []string{a, b, z}, want: 1},
		{name: "duplicate expected names count once", expected: []string{a, a, b}, called: []string{a}, want: 0.5},
		{name: "nothing expected is vacuous", expected: nil, called: []string{a}, want: 1, vacuous: true},
		{
			// One emittable name is enough to make the row scorable; the
			// unemittable one stays in the denominator and is named, the
			// same way intent_coverage keeps an unmapped name in its own.
			name:      "a mixed expectation is scored, not skipped",
			expected:  []string{a, "kubectl_get_pods"},
			called:    []string{a},
			want:      0.5,
			inComment: "kubectl_get_pods",
		},
		{
			name:      "no emittable name is vacuous at zero",
			expected:  []string{"kubectl_get_pods", "kubectl_top_nodes"},
			called:    []string{a},
			want:      0,
			vacuous:   true,
			inComment: "no expected name is one this runtime can emit",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ToolCoverage(tbl, scenario(tc.expected, ""), callsTo(tc.called...))
			if !approx(got.Score, tc.want) {
				t.Fatalf("Score = %v, want %v (%s)", got.Score, tc.want, got.Comment)
			}
			if got.Vacuous != tc.vacuous {
				t.Fatalf("Vacuous = %v, want %v (%s)", got.Vacuous, tc.vacuous, got.Comment)
			}
			if tc.inComment != "" && !strings.Contains(got.Comment, tc.inComment) {
				t.Fatalf("Comment = %q, want it to name %q", got.Comment, tc.inComment)
			}
		})
	}
}

func TestSeverityAccuracy(t *testing.T) {
	tests := []struct {
		name       string
		expected   string
		final      string
		structured string
		want       float64
		vacuous    bool
	}{
		{
			name:     "bare prefix, the shape every corpus row uses",
			expected: "CRITICAL: api-server is CrashLoopBackOff (18 restarts).",
			final:    "CRITICAL: the api-server pod is crash-looping.",
			want:     1,
		},
		{
			name:     "wrong severity",
			expected: "CRITICAL: node is NotReady.",
			final:    "WARNING: the node looks degraded.",
			want:     0,
		},
		{
			// Upstream's own regex is bracket-only, which is why it scores
			// 0 on all 31 rows. Both forms must read as the same claim.
			name:     "bracketed actual against bare expected",
			expected: "WARNING: HPA is saturated.",
			final:    "[WARNING] the HPA is pinned at max replicas.",
			want:     1,
		},
		{
			name:     "markdown bold",
			expected: "OK: cluster healthy.",
			final:    "**OK** — all 3 nodes Ready.",
			want:     1,
		},
		{
			name:     "labelled form",
			expected: "INFO: HPA is over-provisioned.",
			final:    "Severity: INFO\nThe HPA holds 12 replicas at 5% CPU.",
			want:     1,
		},
		{
			name:     "heading form after a preamble line",
			expected: "CRITICAL: CoreDNS is down.",
			final:    "## Diagnosis\nCRITICAL: CoreDNS has 0/2 pods ready.",
			want:     1,
		},
		{
			// The four shapes below are transcribed from the 2026-08-19
			// boards, where the extractor read every one of them as "the
			// run declared no severity". Each carries decoration *and* the
			// label on one line, which the old strip order could not see
			// through: 7 of 31 rows on one board, 0 of 31 on another, so
			// the metric was scoring markdown style and reporting it as a
			// cross-family capability gap.
			name:     "heading and label on one line (LC-09)",
			expected: "CRITICAL: node is NotReady.",
			final:    "## SEVERITY: CRITICAL\n\nNode ip-10-0-1-42 has been NotReady for 8 minutes.",
			want:     1,
		},
		{
			name:     "bold and label on one line (LC-17)",
			expected: "WARNING: the namespace quota is exhausted.",
			final:    "**SEVERITY: WARNING**\n\nScaling batch-runner is blocked by team-a-quota.",
			want:     1,
		},
		{
			name:     "decorated label after a heading (LC-11)",
			expected: "WARNING: pods were evicted for ephemeral storage.",
			final:    "## Report\n\n**SEVERITY: WARNING**\n\n10 pods were evicted.",
			want:     1,
		},
		{
			// The anchor's job: a severity word in running prose is not a
			// verdict. An unanchored search reads "critical" here and
			// scores a run that declared nothing as a correct CRITICAL.
			name:     "prose mention is not a verdict",
			expected: "CRITICAL: node is NotReady.",
			final:    "Everything checks out; nothing critical was found.",
			want:     0,
		},
		{
			name:     "no severity at all",
			expected: "CRITICAL: pods are OOMKilled.",
			final:    "The pods are being killed for memory.",
			want:     0,
		},
		{
			// W1.3's typed report wins over the prose, which may hedge.
			name:       "structured severity takes precedence",
			expected:   "CRITICAL: image pull failure.",
			final:      "WARNING: I could not pull the image.",
			structured: "critical",
			want:       1,
		},
		{
			name:     "scenario declares no severity",
			expected: "The deployment looks fine.",
			final:    "CRITICAL: everything is on fire.",
			want:     1,
			vacuous:  true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tr := Trace{FinalText: tc.final, StructuredSeverity: tc.structured}
			got := SeverityAccuracy(scenario(nil, tc.expected), tr)
			if !approx(got.Score, tc.want) {
				t.Fatalf("Score = %v, want %v (%s)", got.Score, tc.want, got.Comment)
			}
			if got.Vacuous != tc.vacuous {
				t.Fatalf("Vacuous = %v, want %v", got.Vacuous, tc.vacuous)
			}
		})
	}
}

// TestExtractSeverity_RealBoardVerdicts pins the extractor against
// verdicts transcribed verbatim from the 2026-08-19 judge boards.
//
// Every one of these was read as no severity at all until the strip
// order was fixed, and the failure was invisible in the aggregate: a
// missing verdict and a wrong verdict both score 0, so the column read
// as a model that could not classify rather than a harness that could
// not parse. The two shapes that still yield nothing are here for the
// same reason — the anchor has to keep holding, or the fix trades a
// false miss for a false hit.
func TestExtractSeverity_RealBoardVerdicts(t *testing.T) {
	tests := []struct {
		name string
		text string
		want string
	}{
		{"LC-09 heading and label", "## SEVERITY: CRITICAL", "CRITICAL"},
		{"LC-17 bold and label", "**SEVERITY: WARNING**", "WARNING"},
		{"LC-11 label under a heading", "## Report\n\n**SEVERITY: WARNING**", "WARNING"},
		{"LC-13 label with an em-dash qualifier", "## SEVERITY: WARNING — symptom unconfirmed from cluster reads", "WARNING"},
		{"LC-29 label with a parenthetical", "## SEVERITY: CRITICAL (reported impact) — root cause **unconfirmed**", "CRITICAL"},
		{"lowercase label", "### severity: info", "INFO"},
		{"label with an equals sign", "> SEVERITY = OK", "OK"},

		// The anchor: decoration is stripped from the front of a line, so
		// a severity word that is not the line's subject stays out.
		{"prose mention", "The situation is not critical.", ""},
		{"label naming a severity mid-sentence", "## Findings\nWe rate the severity as CRITICAL below.", ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractSeverity(tc.text); got != tc.want {
				t.Errorf("extractSeverity(%q) = %q, want %q", tc.text, got, tc.want)
			}
		})
	}
}

// TestSeverityAccuracy_IsDiagnostic pins #179's disposition. The corpus
// states no severity definition for the buckets 20 of its 31 rows are
// labelled with (judge.TestSeverityRubricDoesNotSpanCorpus measures
// that), so the score cannot be acted on and is not a claim about mast.
// Re-promoting it means fixing the corpus, not this line.
func TestSeverityAccuracy_IsDiagnostic(t *testing.T) {
	tr := Trace{FinalText: "CRITICAL: the api-server pod is crash-looping."}
	for _, expected := range []string{
		"CRITICAL: api-server is CrashLoopBackOff.", // scores 1
		"WARNING: api-server is unhappy.",           // scores 0
		"the deployment looks fine.",                // vacuous
	} {
		got := SeverityAccuracy(scenario(nil, expected), tr)
		if !got.Diagnostic {
			t.Errorf("SeverityAccuracy(%q).Diagnostic = false, want true (#179)", expected)
		}
	}
}

// TestSeverityAccuracy_CorpusIsReadable is the guard against shipping
// upstream's defect: their bracketed regex matches 0 of 31 expected
// responses, so the metric is a constant regardless of agent behaviour.
func TestSeverityAccuracy_CorpusIsReadable(t *testing.T) {
	ds := loadLangChain(t)
	counts := map[string]int{}
	for _, sc := range ds.Scenarios {
		sev := extractSeverity(sc.Outputs.ExpectedResponse)
		if sev == "" {
			t.Errorf("%s: no severity extractable from %q", sc.ID,
				firstLine(sc.Outputs.ExpectedResponse))
			continue
		}
		counts[sev]++
	}
	// A corpus that were all one severity would make the metric
	// satisfiable by a constant answer.
	if len(counts) < 3 {
		t.Fatalf("expected severities span only %v; the metric is too easy", counts)
	}
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

func TestEffectOrdering(t *testing.T) {
	tests := []struct {
		name      string
		calls     []Call
		want      float64
		vacuous   bool
		inComment string
	}{
		{
			name:  "intent recorded before the effect completes",
			calls: []Call{mutation("scale_deployment", "c1", nil, 0, 1)},
			want:  1,
		},
		{
			name: "a dangling intent is not a violation — it is the ambiguous window",
			calls: []Call{
				mutation("scale_deployment", "c1", nil, 0, -1),
			},
			want: 1,
		},
		{
			name: "completion with no recorded intent",
			calls: []Call{
				{Name: "scale_deployment", ID: "ghost", Class: effects.ClassMutating,
					EventIndex: -1, Completed: true, ResponseIndex: 3},
			},
			want:      0,
			inComment: "no recorded intent",
		},
		{
			name: "completion in the same event as the intent is not durably ordered",
			calls: []Call{
				mutation("scale_deployment", "c1", nil, 4, 4),
			},
			want:      0,
			inComment: "not after its intent",
		},
		{
			name: "spawning calls are guarded like mutations",
			calls: []Call{
				{Name: "invoke_specialist", ID: "s1", Class: effects.ClassSpawning,
					EventIndex: -1, Completed: true, ResponseIndex: 2},
			},
			want: 0,
		},
		{
			name: "read-only calls are not scored",
			calls: []Call{
				{Name: "k8s_cluster_health", ID: "r1", Class: effects.ClassReadOnly,
					EventIndex: 3, Completed: true, ResponseIndex: 2},
			},
			want:    1,
			vacuous: true,
		},
		{
			name: "an orphaned read-only completion is reported but not scored",
			calls: []Call{
				mutation("scale_deployment", "c1", nil, 0, 1),
				{Name: "k8s_cluster_health", ID: "r1", Class: effects.ClassReadOnly,
					EventIndex: -1, Completed: true, ResponseIndex: 2},
			},
			want:      1,
			inComment: "orphaned read-only",
		},
		{
			name:    "no mutations at all",
			calls:   callsTo("k8s_cluster_health").Calls,
			want:    1,
			vacuous: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := EffectOrdering(Trace{Calls: tc.calls})
			if !approx(got.Score, tc.want) {
				t.Fatalf("Score = %v, want %v (%s)", got.Score, tc.want, got.Comment)
			}
			if got.Vacuous != tc.vacuous {
				t.Fatalf("Vacuous = %v, want %v (%s)", got.Vacuous, tc.vacuous, got.Comment)
			}
			if tc.inComment != "" && !strings.Contains(got.Comment, tc.inComment) {
				t.Fatalf("Comment = %q, want it to mention %q", got.Comment, tc.inComment)
			}
		})
	}
}

func TestExactlyOnce(t *testing.T) {
	scale := func(id, ns string, idx int) Call {
		return mutation("scale_deployment", id, map[string]any{"ns": ns, "replicas": 3.0}, idx, idx+1)
	}

	tests := []struct {
		name      string
		calls     []Call
		want      float64
		vacuous   bool
		inComment string
	}{
		{
			name:  "one effect, one completion",
			calls: []Call{scale("c1", "prod", 0)},
			want:  1,
		},
		{
			// The failure the outbox exists to prevent: a completion lost
			// to a crash, then a blind resume re-firing the same mutation.
			name:      "the same mutation completed twice",
			calls:     []Call{scale("c1", "prod", 0), scale("c2", "prod", 2)},
			want:      0,
			inComment: "completed 2 times",
		},
		{
			name:  "the same tool against different targets is two effects",
			calls: []Call{scale("c1", "prod", 0), scale("c2", "staging", 2)},
			want:  1,
		},
		{
			// Only completions count. A dangling re-issue whose result was
			// never recorded has not fired twice as far as the log knows.
			name: "a duplicate intent that never completed is not a violation",
			calls: []Call{
				scale("c1", "prod", 0),
				mutation("scale_deployment", "c2", map[string]any{"ns": "prod", "replicas": 3.0}, 2, -1),
			},
			want: 1,
		},
		{
			name: "read-only repetition is free",
			calls: append(callsTo("k8s_cluster_health").Calls,
				callsTo("k8s_cluster_health").Calls...),
			want:    1,
			vacuous: true,
		},
		{
			name:    "no mutations at all",
			calls:   nil,
			want:    1,
			vacuous: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ExactlyOnce(Trace{Calls: tc.calls})
			if !approx(got.Score, tc.want) {
				t.Fatalf("Score = %v, want %v (%s)", got.Score, tc.want, got.Comment)
			}
			if got.Vacuous != tc.vacuous {
				t.Fatalf("Vacuous = %v, want %v (%s)", got.Vacuous, tc.vacuous, got.Comment)
			}
			if tc.inComment != "" && !strings.Contains(got.Comment, tc.inComment) {
				t.Fatalf("Comment = %q, want it to mention %q", got.Comment, tc.inComment)
			}
		})
	}
}

// TestEvaluateAll_OrderAndCoverage keeps the scoreboard's column set
// stable and every evaluator wired in.
func TestEvaluateAll_OrderAndCoverage(t *testing.T) {
	got := EvaluateAll(loadTable(t), scenario([]string{"kubectl_get_pod_logs"}, "INFO: fine."),
		callsTo("k8s_triage_logs"))

	want := []string{
		MetricIntentCoverage, MetricToolCoverage, MetricSeverityAccuracy,
		MetricEffectOrdering, MetricExactlyOnce,
	}
	if len(got) != len(want) {
		t.Fatalf("EvaluateAll returned %d results, want %d", len(got), len(want))
	}
	for i, r := range got {
		if r.Metric != want[i] {
			t.Fatalf("result %d is %q, want %q", i, r.Metric, want[i])
		}
		if r.Comment == "" {
			t.Fatalf("%s has no comment; a red cell must say why", r.Metric)
		}
	}
}

// TestEvaluators_RunAgainstRecordedLog closes W0.2's done-when: the
// evaluators score a durable event log end to end, with no provider.
func TestEvaluators_RunAgainstRecordedLog(t *testing.T) {
	events := eventList{
		userEvent(genai.NewPartFromText("api-server pods keep restarting")),
		modelEvent(callPart("k8s_triage_workload", "c1", map[string]any{"workload": "api-server"})),
		userEvent(respPart("k8s_triage_workload", "c1")),
		modelEvent(genai.NewPartFromText(
			"CRITICAL: api-server is CrashLoopBackOff (18 restarts) — bad config mount.")),
	}
	tr := TraceFromEvents(events, readOnlyPred(), nil)

	sc := scenario(
		[]string{"kubectl_describe_pod", "kubectl_get_events", "kubectl_get_pod_logs"},
		"CRITICAL: api-server-7d8f9c-xkp2v is CrashLoopBackOff (18 restarts).")

	for _, r := range EvaluateAll(loadTable(t), sc, tr) {
		switch r.Metric {
		case MetricToolCoverage:
			if r.Score != 0 || !r.Diagnostic {
				t.Fatalf("tool_coverage = %v diagnostic=%v, want 0 and diagnostic", r.Score, r.Diagnostic)
			}
		default:
			if !r.Passed() {
				t.Fatalf("%s = %v, want 1 (%s)", r.Metric, r.Score, r.Comment)
			}
		}
	}
}
