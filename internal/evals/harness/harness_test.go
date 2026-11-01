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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/genai"

	adkmodel "google.golang.org/adk/v2/model"

	"github.com/go-steer/core-agent/v3/internal/evals"
	"github.com/go-steer/core-agent/v3/internal/evals/differentiators"
	"github.com/go-steer/core-agent/v3/internal/evals/evalcompose"
	"github.com/go-steer/core-agent/v3/internal/evals/judge"
)

const repoRoot = "../../.."

// loadIntentTable reads the same table the harness reads, so a test that
// checks an attribution against the catalog is checking the shipped
// catalog rather than a copy of it that can drift.
func loadIntentTable(t *testing.T) evals.IntentTable {
	t.Helper()
	tbl, err := evals.LoadIntentTable(filepath.Join(repoRoot, "testdata", "evals", "intents.yaml"))
	if err != nil {
		t.Fatalf("loading the intent table: %v", err)
	}
	return tbl
}

// TestRun_Deterministic is the W0.4 done-when: the gating tier runs
// green against today's tree, with the known-red scenarios accounted for
// rather than hidden.
func TestRun_Deterministic(t *testing.T) {
	sum, err := Run(context.Background(), Config{Root: repoRoot, Scratch: t.TempDir()})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !sum.OK() {
		t.Fatalf("suite is red:\n  - %s", strings.Join(sum.Problems, "\n  - "))
	}
	if sum.Corpus.Scenarios != 31 {
		t.Errorf("corpus loaded %d scenarios, want 31", sum.Corpus.Scenarios)
	}
	if len(sum.Corpus.Dead) != 0 {
		t.Errorf("dead metrics %v; a constant function is a harness failure", sum.Corpus.Dead)
	}
	if got, want := len(sum.Scenes), len(differentiators.All()); got != want {
		t.Errorf("reported %d differentiators, want %d", got, want)
	}
	// Every scene must carry the evidence a reader needs to act on it.
	for _, sc := range sum.Scenes {
		if sc.Reason == "" {
			t.Errorf("%s: no reason reported", sc.ID)
		}
		if len(sc.Rows) == 0 {
			t.Errorf("%s: names no scoreboard row", sc.ID)
		}
		if sc.Observed == differentiators.Fail.String() && sc.Blocked == "" {
			t.Errorf("%s: red with no blocking workstream named", sc.ID)
		}
	}
	// W0.3 left three scenarios red; W2.5 retired the last of them. An
	// empty allowlist is now the expected state, and re-growing one is
	// the thing to notice: a scenario may only go back to declared-Fail
	// with a named blocking workstream, which the loop above enforces.
	if len(sum.ExpectedFail) != 0 {
		t.Errorf("expected-fail allowlist has re-grown to %v; shrinking it is the progress metric, so a new entry "+
			"needs a plan row saying why (docs/v0.3-plan.md W0.4)", sum.ExpectedFail)
	}
	for _, sc := range sum.Scenes {
		if sc.Observed != differentiators.Pass.String() {
			t.Errorf("%s observed %s: %s", sc.ID, sc.Observed, sc.Reason)
		}
	}
}

// TestRun_JudgeTierUnbuildableModelFailsLoudly pins the tier boundary.
// Silently falling back to the free tier when the metered one cannot
// start would report a deterministic result as a LangChain-comparable
// one, and it is the runner's exit-2 case rather than exit 1: nothing
// was measured.
func TestRun_JudgeTierUnbuildableModelFailsLoudly(t *testing.T) {
	_, err := Run(context.Background(), Config{
		Root: repoRoot, Tier: TierJudge, Scratch: t.TempDir(),
		Model: "no-such-model-9000", Grader: "echo",
	})
	if err == nil {
		t.Fatal("the judge tier started with a model that cannot be built")
	}
	if !strings.Contains(err.Error(), "unknown model") {
		t.Fatalf("error = %v, want it to name the unbuildable model", err)
	}
}

// TestRun_JudgeTierRunsTheWholeCorpus exercises the metered tier's
// plumbing end to end without credentials or cost, by pointing it at
// compose's offline echo model. It is not a quality check — echo does
// not choose tools, so every score is meaningless — it checks that all
// 31 rows reach the board, that both model names are recorded, and that
// a grader which cannot produce a grade costs one column rather than the
// row.
func TestRun_JudgeTierRunsTheWholeCorpus(t *testing.T) {
	sum, err := Run(context.Background(), Config{
		Root: repoRoot, Tier: TierJudge, Scratch: t.TempDir(),
		Model: "echo", Grader: "echo",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Judge == nil {
		t.Fatal("the judge tier produced no board")
	}
	if got := len(sum.Judge.Scenes); got != 31 {
		t.Fatalf("board has %d rows, want 31", got)
	}
	for _, s := range sum.Judge.Scenes {
		if s.Error != "" {
			t.Errorf("%s: did not run: %s", s.ID, s.Error)
		}
	}
	if sum.Judge.Model != "echo" || sum.Judge.Grader != "echo" {
		t.Errorf("board records model %q / grader %q, want both named", sum.Judge.Model, sum.Judge.Grader)
	}
	// Grading with the model under test is a real methodological
	// problem, so the board has to say so rather than leave it to be
	// noticed.
	if len(sum.Judge.Notes) == 0 || !strings.Contains(sum.Judge.Notes[0], "grading its own output") {
		t.Errorf("notes = %v, want the same-model warning", sum.Judge.Notes)
	}
	// The echo model returns the prompt, which is not a grade. That
	// must show up as a missing column, not as a zero.
	if len(sum.Problems) == 0 {
		t.Error("an ungradeable reply from the grader was recorded as a score")
	}
	for _, p := range sum.Problems {
		if !strings.Contains(p, "response_quality was not graded") {
			t.Errorf("unexpected problem: %s", p)
		}
	}
	// LC-13 is the one structurally-capped row; the rest must not be.
	if len(sum.Judge.Ceilings) != 1 || sum.Judge.Ceilings[0].ID != "LC-13-rollback-needed-after-bad" {
		t.Errorf("ceilings = %+v, want only LC-13", sum.Judge.Ceilings)
	}
	// Under echo there is nothing to price: the fake collapses every
	// tier back onto itself. That is a skip the board has to state — an
	// absent cost section would read as one that passed — and it is not
	// a Problem, because the question, not mast, is what this
	// configuration cannot answer.
	if sum.Judge.Cost != nil {
		t.Errorf("the offline fake produced a cost board: %+v", sum.Judge.Cost)
	}
	if sum.Judge.CostSkipped == "" {
		t.Error("J-cost-tier was skipped without saying so; a silent skip reads as a pass")
	}
	// echo answers with the prompt and never calls a tool, so the
	// validity board is empty — and has to say so out loud. "No
	// malformed calls" and "no calls at all" are the same JSON and very
	// different facts, and this configuration is the one that produces
	// the second.
	if v := sum.Judge.Validity; v.Calls != 0 || len(v.Malformed) != 0 {
		t.Errorf("echo produced tool calls: %+v", v)
	}
	// A run that calls nothing answers nothing, so every reachable
	// expected intent is a consequential miss — and LC-13's rollback is
	// not one of them, on a board where literally every other question
	// went unasked. That is the partition working at its least
	// forgiving.
	m := sum.Judge.Misses
	if m.Scenarios != len(sum.Judge.Scenes) {
		t.Errorf("miss board drawn from %d rows, want all %d", m.Scenarios, len(sum.Judge.Scenes))
	}
	if len(m.Consequential) == 0 {
		t.Error("a board where nothing was called reported no consequential miss")
	}
	if m.OutOfCatalog != 1 {
		t.Errorf("out of catalog = %d, want only LC-13's rollback", m.OutOfCatalog)
	}
	for _, c := range m.Consequential {
		if c.Intent == "remediate.rollback" {
			t.Errorf("%s: the write intent lookout excludes by design was charged as a miss", c.Scenario)
		}
		if len(c.ServedBy) == 0 {
			t.Errorf("%s %s: listed as consequential with no tool that would have served it", c.Scenario, c.Intent)
		}
	}

	// #171 on the real table: a board that called nothing misses every
	// sole-source tool, so the ranking is the catalog's own list of
	// choke points and every entry on it has to be genuinely sole-source.
	if len(m.Attribution) == 0 {
		t.Error("a board that called nothing attributed no miss to any tool")
	}
	tbl := loadIntentTable(t)
	for _, a := range m.Attribution {
		if len(a.SoleSourceFor) == 0 {
			t.Errorf("%s is in the ranking with no intent it alone answers", a.Tool)
		}
		for _, id := range a.SoleSourceFor {
			if got := tbl.ToolsSatisfying(id); len(got) != 1 || got[0] != a.Tool {
				t.Errorf("%s is credited as sole answer to %s, but %v serve it", a.Tool, id, got)
			}
		}
		if a.Gates < len(a.Misses) {
			t.Errorf("%s gates %d scenario(s) but was charged %d miss(es); a miss it does not gate is not its own",
				a.Tool, a.Gates, len(a.Misses))
		}
	}
	if len(m.Attribution)+m.Shared == 0 {
		t.Error("no miss was either attributed or counted as shared; the partition dropped them")
	}

	var buf bytes.Buffer
	sum.WriteText(&buf)
	out := buf.String()
	if !strings.Contains(out, "no calls were recorded at all") {
		t.Errorf("a board with no tool calls did not say so:\n%s", out)
	}
	if !strings.Contains(out, "the structural ceiling below, not a miss by the model") {
		t.Errorf("the miss section did not separate the ceiling from the misses:\n%s", out)
	}
	if !strings.Contains(out, "attributable to one tool (#171), most leverage first:") {
		t.Errorf("the board did not rank the misses by tool:\n%s", out)
	}
	if !strings.Contains(out, "it guarantees them: no other tool in the catalog answers") {
		t.Errorf("the ranking did not say why a sole-source skip is different:\n%s", out)
	}
}

// TestAggregate_ExcludesVacuousRows pins the averaging rule. Counting a
// row that had nothing to score as a zero reports the corpus's shape as
// the agent's performance.
func TestAggregate_ExcludesVacuousRows(t *testing.T) {
	scenes := []JudgeScenario{
		{ID: "a", Results: []evals.MetricResult{{Metric: evals.MetricIntentCoverage, Score: 1}}},
		{ID: "b", Results: []evals.MetricResult{{Metric: evals.MetricIntentCoverage, Score: 0.5}}},
		{ID: "c", Results: []evals.MetricResult{{Metric: evals.MetricIntentCoverage, Score: 0, Vacuous: true}}},
	}
	got := aggregate(scenes)
	if len(got) != 1 {
		t.Fatalf("aggregate = %+v, want one metric", got)
	}
	if got[0].Mean != 0.75 || got[0].Scored != 2 || got[0].Vacuous != 1 {
		t.Errorf("aggregate = %+v, want mean 0.75 over 2 scored with 1 vacuous", got[0])
	}
}

// validityScenes is one board of each shape #169 distinguishes: a row
// that read well, a row whose every read came back empty, and a row
// that called a tool that does not exist.
func validityScenes() []JudgeScenario {
	return []JudgeScenario{{
		ID: "read-well",
		Calls: []evals.CallRecord{
			{Index: 0, Tool: "k8s_triage_workload", Args: map[string]any{"scope": "production/api"}, Result: "restart count 18", Completed: true},
			{Index: 1, Tool: "k8s_triage_logs", Args: map[string]any{"scope": "production/api"}, Result: "no abnormal findings", Completed: true},
		},
		Violations: []evals.Violation{
			{CallIndex: 1, Tool: "k8s_triage_logs", Kind: evals.ViolationEmptyResult, Detail: "scope=production/api"},
		},
	}, {
		ID: "ran-blind",
		Calls: []evals.CallRecord{
			{Index: 0, Tool: "k8s_cloud_quota", Args: map[string]any{"scope": "kube-system"}, Result: "no abnormal findings", Completed: true},
		},
		Violations: []evals.Violation{
			{CallIndex: 0, Tool: "k8s_cloud_quota", Kind: evals.ViolationEmptyResult, Detail: "scope=kube-system"},
		},
	}, {
		ID: "invented-a-tool",
		Calls: []evals.CallRecord{
			{Index: 0, Tool: "kubectl_delete_pod", Args: map[string]any{"scope": "production"}, Completed: true},
		},
		Violations: []evals.Violation{
			{CallIndex: 0, Tool: "kubectl_delete_pod", Kind: evals.ViolationUnknownTool, Detail: "no such tool in the catalog"},
		},
	}, {
		ID: "did-not-run", Error: "provider refused",
	}}
}

// TestSummarizeValidity_SeparatesMalformedFromEmpty pins the split the
// board is built on. An empty read is ordinary — a hypothesis ruled out
// — and a malformed call is a defect; folding them into one number
// would bury the rare actionable thing under the common expected one.
func TestSummarizeValidity_SeparatesMalformedFromEmpty(t *testing.T) {
	got := summarizeValidity(validityScenes())

	if got.Calls != 4 {
		t.Errorf("counted %d calls, want 4 (the row that did not run contributes none)", got.Calls)
	}
	if got.EmptyReads != 2 {
		t.Errorf("empty reads = %d, want 2", got.EmptyReads)
	}
	if len(got.Malformed) != 1 || got.Malformed[0].Scenario != "invented-a-tool" {
		t.Fatalf("malformed = %+v, want the one unknown-tool call attributed to its row", got.Malformed)
	}
	if got.Malformed[0].Violation.Kind != evals.ViolationUnknownTool {
		t.Errorf("an empty read was filed as malformed: %+v", got.Malformed[0])
	}
	// ran-blind only: read-well found something on its first call, so
	// its diagnosis rests on evidence even though its second read was
	// clean.
	if len(got.Blind) != 1 || got.Blind[0] != "ran-blind" {
		t.Errorf("blind runs = %v, want only ran-blind", got.Blind)
	}
	if strings.Join(got.Counts, " ") != "empty_result=2 unknown_tool=1" {
		t.Errorf("counts = %v", got.Counts)
	}
}

// TestSummarizeValidity_ARowThatDidNotRunIsNotACleanRow. A board that
// lost rows to a provider outage must not read as better than one that
// finished — the missing rows have no calls to be right or wrong about.
func TestSummarizeValidity_ARowThatDidNotRunIsNotACleanRow(t *testing.T) {
	got := summarizeValidity([]JudgeScenario{{
		ID:    "lost",
		Error: "provider refused",
		// A row can carry both an error and whatever it managed to
		// record before failing; neither may be counted.
		Calls:      []evals.CallRecord{{Index: 0, Tool: "k8s_triage_logs", Completed: true}},
		Violations: []evals.Violation{{CallIndex: 0, Kind: evals.ViolationEmptyResult}},
	}})
	if got.Calls != 0 || got.EmptyReads != 0 || len(got.Counts) != 0 {
		t.Errorf("a row that did not run contributed to the tally: %+v", got)
	}
}

// TestJudgeSummary_WriteValidity checks the section says the things a
// reader acts on: every malformed call named, and the blind rows called
// out as rows whose score is not measuring what it appears to.
func TestJudgeSummary_WriteValidity(t *testing.T) {
	j := &JudgeSummary{Scenes: validityScenes()}
	j.Validity = summarizeValidity(j.Scenes)

	var buf bytes.Buffer
	j.writeValidity(func(format string, args ...any) { fmt.Fprintf(&buf, format+"\n", args...) })
	out := buf.String()
	for _, want := range []string{
		"call validity (#169)",
		"4 call(s) recorded",
		"empty_result=2",
		"malformed calls (1)",
		"invented-a-tool call 0 kubectl_delete_pod: unknown_tool",
		"empty reads: 2 of 4",
		"ran blind (1)",
		"ran-blind",
		"rests on no evidence",
		// W8's answer, restated on the artifact rather than left to be
		// assumed: a consumer who received this file second-hand is told
		// what was kept verbatim and what the exposure is.
		"recorded verbatim",
		"synthetic fixture",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the validity section is missing %q:\n%s", want, out)
		}
	}

	// A clean board must say so rather than dropping the section: an
	// absent section reads as one that passed, which is the same
	// mistake J-cost-tier's skip line exists to avoid.
	clean := &JudgeSummary{Scenes: []JudgeScenario{{
		ID:    "fine",
		Calls: []evals.CallRecord{{Index: 0, Tool: "k8s_triage_workload", Result: "restart count 18", Completed: true}},
	}}}
	clean.Validity = summarizeValidity(clean.Scenes)
	buf.Reset()
	clean.writeValidity(func(format string, args ...any) { fmt.Fprintf(&buf, format+"\n", args...) })
	if out := buf.String(); !strings.Contains(out, "malformed: none") {
		t.Errorf("a clean board did not state that it was clean:\n%s", out)
	}
}

// missScenes is one board of each shape #170 distinguishes: a row that
// left a catalog-answerable question unasked, a row whose only gap is
// the write tool lookout excludes, a clean row, and a row that never
// ran.
func missScenes() []JudgeScenario {
	return []JudgeScenario{{
		ID: "missed-saturation",
		Misses: evals.MissReport{Consequential: []evals.ConsequentialMiss{
			{Intent: "inspect.node_saturation", ServedBy: []string{"k8s_resource_top"}},
		}},
	}, {
		ID: "missed-it-too",
		Misses: evals.MissReport{Consequential: []evals.ConsequentialMiss{
			{Intent: "inspect.node_saturation", ServedBy: []string{"k8s_resource_top"}},
			{Intent: "inspect.events", ServedBy: []string{"k8s_event_timeline", "k8s_triage_workload"}},
		}},
	}, {
		ID:     "capped-by-the-catalog",
		Misses: evals.MissReport{OutOfCatalog: []string{"remediate.rollback"}},
	}, {
		ID: "clean",
	}, {
		ID:    "did-not-run",
		Error: "provider refused",
		// Whatever this row managed to classify before failing is not a
		// measurement, the same way its calls are not.
		Misses: evals.MissReport{Consequential: []evals.ConsequentialMiss{
			{Intent: "inspect.logs", ServedBy: []string{"k8s_triage_logs"}},
		}},
	}}
}

// missGating is the catalog fact #171 reads: which tools are the only
// answer to something, and how much of the corpus that gates. It is a
// property of the table and the dataset, so it names tools the board
// above never charged — k8s_triage_logs is sole-source too, and the only
// row that missed it never ran.
func missGating() map[string]evals.ToolGating {
	return map[string]evals.ToolGating{
		"k8s_resource_top": {
			Tool:       "k8s_resource_top",
			SoleSource: []string{"inspect.node_saturation", "inspect.pod_saturation"},
			Gates:      []string{"LC-03", "LC-05", "LC-20", "LC-22", "LC-29"},
		},
		"k8s_triage_logs": {
			Tool:       "k8s_triage_logs",
			SoleSource: []string{"inspect.logs"},
			Gates:      []string{"LC-01", "LC-02"},
		},
	}
}

// TestSummarizeMisses_KeepsTheCatalogsGapOutOfTheModelsColumn is the
// partition #170 turns on, at board level: three unsatisfied intents
// across the rows that ran, and only two of them are the model's.
func TestSummarizeMisses_KeepsTheCatalogsGapOutOfTheModelsColumn(t *testing.T) {
	got := summarizeMisses(missScenes(), missGating())

	if got.Scenarios != 4 {
		t.Errorf("board drawn from %d rows, want 4 (the row that did not run contributes none)", got.Scenarios)
	}
	if len(got.Consequential) != 3 {
		t.Fatalf("consequential = %+v, want the three from the rows that ran", got.Consequential)
	}
	if got.OutOfCatalog != 1 {
		t.Errorf("out of catalog = %d, want 1", got.OutOfCatalog)
	}
	for _, c := range got.Consequential {
		if c.Scenario == "did-not-run" {
			t.Error("a row that did not run contributed a miss")
		}
		if c.Intent == "remediate.rollback" {
			t.Error("the structural ceiling was filed as a tool-selection miss")
		}
	}
	// Sorted by row then intent, because this is a list a human reads
	// and an unstable order makes two boards impossible to compare.
	if got.Consequential[0].Scenario != "missed-it-too" || got.Consequential[0].Intent != "inspect.events" {
		t.Errorf("first entry = %+v, want missed-it-too/inspect.events", got.Consequential[0])
	}
	// The recurring intent is the actionable pattern: one tool nobody
	// reaches for, not two unrelated rows.
	if strings.Join(got.ByIntent, " ") != "inspect.events=1 inspect.node_saturation=2" {
		t.Errorf("by intent = %v, want the recurring intent tallied", got.ByIntent)
	}
}

// TestSummarizeMisses_ChargesOnlyWhatOneToolCouldHaveAnswered is #171's
// attribution rule. Two of the three misses are the same sole-source
// tool and are charged to it; the third had two possible answers, so
// naming either as the cause would be a guess and the board counts it
// instead.
func TestSummarizeMisses_ChargesOnlyWhatOneToolCouldHaveAnswered(t *testing.T) {
	got := summarizeMisses(missScenes(), missGating())

	if len(got.Attribution) != 1 {
		t.Fatalf("attribution = %+v, want only the sole-source tool", got.Attribution)
	}
	a := got.Attribution[0]
	if a.Tool != "k8s_resource_top" {
		t.Errorf("charged tool = %s, want k8s_resource_top", a.Tool)
	}
	if want := []string{"missed-it-too inspect.node_saturation", "missed-saturation inspect.node_saturation"}; !slices.Equal(a.Misses, want) {
		t.Errorf("misses = %v, want both rows named in order %v", a.Misses, want)
	}
	// The leverage half comes from the catalog, not from this board: the
	// tool is the only answer to a second intent no row here missed, and
	// it gates five scenarios of which only two show up as misses.
	if !slices.Equal(a.SoleSourceFor, []string{"inspect.node_saturation", "inspect.pod_saturation"}) {
		t.Errorf("sole source for = %v, want both saturation intents", a.SoleSourceFor)
	}
	if a.Gates != 5 {
		t.Errorf("gates = %d, want the corpus count (5), not the count of misses on this board", a.Gates)
	}
	if got.Shared != 1 {
		t.Errorf("shared = %d, want the two-server miss counted rather than charged", got.Shared)
	}
	// A tool that is sole-source but was only missed by a row that never
	// ran has nothing to answer for. Ranking it at zero would put a tool
	// on the list that no evidence points at.
	for _, x := range got.Attribution {
		if x.Tool == "k8s_triage_logs" {
			t.Error("a tool charged only by a row that did not run was ranked")
		}
	}
}

// TestRankAttribution_LeadsWithTheCorpusNotTheNight orders by the
// durable fact first. How many misses a tool collected is one model on
// one night; how much of the corpus it gates is a property of the corpus,
// and it is what says whether fixing the tool is worth the afternoon.
func TestRankAttribution_LeadsWithTheCorpusNotTheNight(t *testing.T) {
	charged := map[string][]string{
		"noisy": {"a x", "b x", "c x"},
		"heavy": {"d y"},
		"tied":  {"e z"},
	}
	gating := map[string]evals.ToolGating{
		"noisy": {Tool: "noisy", Gates: []string{"LC-01"}},
		"heavy": {Tool: "heavy", Gates: []string{"LC-01", "LC-02", "LC-03"}},
		"tied":  {Tool: "tied", Gates: []string{"LC-01"}},
	}
	got := rankAttribution(charged, gating)

	var order []string
	for _, a := range got {
		order = append(order, a.Tool)
	}
	// heavy first on gating despite one miss; then noisy over tied on
	// misses at equal gating; name breaks nothing here, but two boards
	// with identical numbers must still render identically.
	if want := []string{"heavy", "noisy", "tied"}; !slices.Equal(order, want) {
		t.Errorf("order = %v, want %v", order, want)
	}
}

// TestJudgeSummary_WriteMisses checks the section answers the question
// its reader arrives with. Everyone comes to it expecting "tools the run
// skipped", which is a bigger number pointing the other way, so the
// section has to say which question it answers before it lists anything.
func TestJudgeSummary_WriteMisses(t *testing.T) {
	j := &JudgeSummary{Scenes: missScenes()}
	j.Misses = summarizeMisses(j.Scenes, missGating())

	var buf bytes.Buffer
	j.writeMisses(func(format string, args ...any) { fmt.Fprintf(&buf, format+"\n", args...) })
	out := buf.String()
	for _, want := range []string{
		"consequential misses (#170) — 3 across 4 scenario(s)",
		"that a tool in the catalog would have answered, that the run never asked",
		"a skipped tool is not one of these",
		"missed-saturation inspect.node_saturation — k8s_resource_top would have answered it",
		"missed-it-too inspect.events — k8s_event_timeline, k8s_triage_workload would have answered it",
		"by intent: inspect.events=1  inspect.node_saturation=2",
		"1 further unsatisfied intent(s) no read-only tool serves",
		"attributable to one tool (#171), most leverage first:",
		"k8s_resource_top — sole answer to inspect.node_saturation and inspect.pod_saturation, gates 5 of the corpus's scenarios, missed 2 time(s) here",
		"skipping it does not risk those misses, it guarantees them",
		"1 miss(es) had more than one tool that would have answered, so none is attributable to a tool",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the miss section is missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "remediate.rollback") {
		t.Errorf("the section named the excluded write intent as a miss:\n%s", out)
	}

	// A board with nothing to report says so, for the same reason the
	// validity section does: an absent section reads as a pass, and this
	// one is meant to be empty most nights.
	clean := &JudgeSummary{Scenes: []JudgeScenario{{ID: "fine"}}}
	clean.Misses = summarizeMisses(clean.Scenes, missGating())
	buf.Reset()
	clean.writeMisses(func(format string, args ...any) { fmt.Fprintf(&buf, format+"\n", args...) })
	if out := buf.String(); !strings.Contains(out, "none across 1 scenario(s)") {
		t.Errorf("a board with no misses did not state it:\n%s", out)
	}
}

// TestSummary_WriteTextJudgeTier keeps the metered board honest about
// what it is: a report, with its ceilings stated and its models named.
func TestSummary_WriteTextJudgeTier(t *testing.T) {
	sum := Summary{
		Tier: TierJudge,
		Corpus: CorpusSummary{
			Dataset: "langchain-sre", Scenarios: 31, Intents: 19,
			Reach: []evals.MetricReach{{Metric: evals.MetricIntentCoverage, Scenarios: 31, Reaches: 31}},
		},
		Judge: &JudgeSummary{
			Model: "claude-opus-4-7", Grader: "claude-haiku-4-5", Provider: "anthropic-vertex",
			Scenes: []JudgeScenario{{
				ID: "LC-13-rollback-needed-after-bad", Ceiling: 0.5, Tools: []string{"k8s_recent_changes"},
				Results: []evals.MetricResult{{Metric: evals.MetricIntentCoverage, Score: 0.5}},
			}},
			Aggregate: []MetricSummary{{Metric: evals.MetricIntentCoverage, Mean: 0.5, Scored: 1}},
			Ceilings: []CeilingFinding{{
				ID: "LC-13-rollback-needed-after-bad", Ceiling: 0.5, Scored: 0.5, Why: "the corpus expects a write tool",
			}},
		},
	}

	var buf bytes.Buffer
	sum.WriteText(&buf)
	out := buf.String()
	for _, want := range []string{
		"tier judge",
		"model under test claude-opus-4-7",
		"grader claude-haiku-4-5",
		"LC-13-rollback-needed-after-bad",
		"ceiling 0.50",
		"structural ceilings",
		"does not gate",
		"REPORTED",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report is missing %q:\n%s", want, out)
		}
	}
	// The differentiator section belongs to the free tier; printing an
	// empty one here would read as five scenarios having vanished.
	if strings.Contains(out, "differentiators:") {
		t.Errorf("the judge board printed an empty differentiator section:\n%s", out)
	}

	sum.Problems = []string{"LC-02: the run did not complete"}
	buf.Reset()
	sum.WriteText(&buf)
	if out := buf.String(); !strings.Contains(out, "INCOMPLETE") || strings.Contains(out, "REPORTED") {
		t.Errorf("an incomplete board reported itself as complete:\n%s", out)
	}
}

// throws429Once wraps a model and fails its very first call with the
// error the 2026-08-21 nightly received, so a credential-free run can
// reach the judge tier's retry wiring. Only once: the point is a blip.
type throws429Once struct {
	inner adkmodel.LLM
	once  sync.Once
}

func (m *throws429Once) Name() string { return m.inner.Name() }

func (m *throws429Once) GenerateContent(ctx context.Context, req *adkmodel.LLMRequest, stream bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	var fail bool
	m.once.Do(func() { fail = true })
	if fail {
		return func(yield func(*adkmodel.LLMResponse, error) bool) {
			yield(nil, fmt.Errorf("failed to call model: %w", genai.APIError{
				Code: 429, Status: "RESOURCE_EXHAUSTED", Message: "Resource exhausted. Please try again later.",
			}))
		}
	}
	return m.inner.GenerateContent(ctx, req, stream)
}

// TestRun_JudgeTierWaitsOutATransient429AndPutsItOnTheBoard is the
// wiring half of #239, and it is here rather than in the judge package
// because the retry only helps if runJudge actually wraps the models it
// builds and reports what they spent. Before this, a 429 cost a corpus
// row, three of them cost the night, and the report blamed mast for a
// provider's quota.
func TestRun_JudgeTierWaitsOutATransient429AndPutsItOnTheBoard(t *testing.T) {
	sum, err := Run(context.Background(), Config{
		Root: repoRoot, Tier: TierJudge, Scratch: t.TempDir(),
		Model: "echo", Grader: "echo",
		buildModel: func(ctx context.Context, provider, name string) (adkmodel.LLM, error) {
			m, err := evalcompose.BuildModel(ctx, provider, name, evalcompose.ModelOptions{})
			if err != nil {
				return nil, err
			}
			return &throws429Once{inner: m}, nil
		},
		// A real schedule would make this test sleep for three seconds
		// to prove something about arithmetic.
		retryBackoff: []time.Duration{time.Millisecond},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Judge == nil {
		t.Fatal("the judge tier produced no board")
	}
	for _, s := range sum.Judge.Scenes {
		if s.Error != "" {
			t.Fatalf("%s did not run: %s — a transient 429 still cost a row", s.ID, s.Error)
		}
	}
	if got := len(sum.Judge.Scenes); got != 31 {
		t.Errorf("board has %d rows, want all 31", got)
	}
	// Two, not one: the tier builds two models and each is wrapped, so
	// this also pins that the board sums both rather than reporting the
	// corpus's retries and quietly dropping the grader's.
	if sum.Judge.Retries != 2 {
		t.Errorf("board records %d retries, want 2 (one per model) — a retry nobody counted is a provider under pressure nobody sees",
			sum.Judge.Retries)
	}
	// The wait it actually served, not the one it was configured with:
	// two millisecond waits, so anything near the default schedule's six
	// seconds means the board is reporting a number it made up.
	if sum.Judge.RetryWaitSeconds <= 0 || sum.Judge.RetryWaitSeconds > 1 {
		t.Errorf("board records %v seconds waiting, want the two millisecond waits it served", sum.Judge.RetryWaitSeconds)
	}
	var buf bytes.Buffer
	sum.WriteText(&buf)
	if out := buf.String(); !strings.Contains(out, "only completed on a retry") {
		t.Errorf("the printed board did not mention the retry:\n%s", out)
	}
}

// TestSummary_WriteTextSaysWhenTheBoardOnlyCompletedByWaiting. A retry
// nobody can see is how a measurement quietly stops measuring: a
// provider under worsening pressure keeps producing complete, green,
// increasingly slow boards with nothing for a reader to point at
// (#239). Silent at zero, so the line means something when it appears.
func TestSummary_WriteTextSaysWhenTheBoardOnlyCompletedByWaiting(t *testing.T) {
	board := func(retries int, wait float64) Summary {
		return Summary{Tier: TierJudge, Judge: &JudgeSummary{
			Model: "gemini-3.7-flash", Grader: "gemini-3.5-flash-lite", Provider: "vertex",
			Scenes:           []JudgeScenario{{ID: "LC-01", Results: []evals.MetricResult{{Metric: evals.MetricIntentCoverage, Score: 1}}}},
			Aggregate:        []MetricSummary{{Metric: evals.MetricIntentCoverage, Mean: 1, Scored: 1}},
			Retries:          retries,
			RetryWaitSeconds: wait,
		}}
	}

	var buf bytes.Buffer
	board(3, 39).WriteText(&buf)
	out := buf.String()
	if !strings.Contains(out, "3 call(s) only completed on a retry, 39s spent waiting") {
		t.Errorf("the board did not say it had been retried:\n%s", out)
	}
	// It stays a report: waiting out a 429 is not a broken board.
	if !strings.Contains(out, "REPORTED") {
		t.Errorf("a retried-but-complete board reported itself as incomplete:\n%s", out)
	}

	buf.Reset()
	board(0, 0).WriteText(&buf)
	if out := buf.String(); strings.Contains(out, "only completed on a retry") {
		t.Errorf("a clean board printed a retry line:\n%s", out)
	}
}

func TestRun_RejectsUnknownTier(t *testing.T) {
	_, err := Run(context.Background(), Config{Root: repoRoot, Tier: "vibes", Scratch: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "unknown tier") {
		t.Fatalf("error = %v, want an unknown-tier rejection", err)
	}
}

// TestRun_MissingFixtureIsAHarnessError, not a red board: a suite that
// cannot find its corpus has measured nothing.
func TestRun_MissingFixtureIsAHarnessError(t *testing.T) {
	_, err := Run(context.Background(), Config{Root: t.TempDir(), Scratch: t.TempDir()})
	if err == nil {
		t.Fatal("Run succeeded with no fixtures on disk")
	}
	if !strings.Contains(err.Error(), "langchain-sre.jsonl") {
		t.Errorf("error = %v, want it to name the missing fixture", err)
	}
}

// TestSummarize_BothDirections is the mapping from an observed outcome
// to a gate decision, checked in every combination. The two mismatch
// directions are different defects and must read differently: one says a
// capability landed, the other says one regressed.
func TestSummarize_BothDirections(t *testing.T) {
	scene := func(expect differentiators.Outcome, blocked string) differentiators.Scenario {
		return differentiators.Scenario{
			ID:        "E-example",
			Invariant: "the thing holds",
			Expect:    expect,
			Blocked:   blocked,
			Rows:      []string{"L1"},
		}
	}
	live := differentiators.Result{Held: true, Reason: "observed something", Trace: evals.Trace{
		Calls: []evals.Call{{Name: "k8s_triage_workload", ID: "c1"}},
	}}

	tests := []struct {
		name        string
		rep         differentiators.Report
		wantProblem string
	}{
		{
			name: "declared pass, passes",
			rep:  differentiators.Report{Scenario: scene(differentiators.Pass, ""), Result: live, Outcome: differentiators.Pass},
		},
		{
			name: "declared fail, fails",
			rep:  differentiators.Report{Scenario: scene(differentiators.Fail, "W2.5 — no channel"), Result: live, Outcome: differentiators.Fail},
		},
		{
			name:        "declared fail, now passes",
			rep:         differentiators.Report{Scenario: scene(differentiators.Fail, "W2.5 — no channel"), Result: live, Outcome: differentiators.Pass},
			wantProblem: "the capability landed",
		},
		{
			name:        "declared pass, now fails",
			rep:         differentiators.Report{Scenario: scene(differentiators.Pass, ""), Result: live, Outcome: differentiators.Fail},
			wantProblem: "regression in shipped behaviour",
		},
		{
			name: "broken beats its declaration",
			rep: differentiators.Report{
				Scenario: scene(differentiators.Fail, "W2.5 — no channel"),
				Outcome:  differentiators.Broken,
				Err:      errors.New("empty trace"),
			},
			wantProblem: "BROKEN",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, problems := summarize(tc.rep)
			if tc.wantProblem == "" {
				if len(problems) != 0 {
					t.Fatalf("problems = %v, want none", problems)
				}
				return
			}
			if len(problems) != 1 {
				t.Fatalf("problems = %v, want exactly one", problems)
			}
			if !strings.Contains(problems[0], tc.wantProblem) {
				t.Errorf("problem = %q, want it to mention %q", problems[0], tc.wantProblem)
			}
		})
	}
}

// TestSummarize_BrokenIsNotAllowlistable is the load-bearing half of the
// three-valued outcome: a fixture defect declared as an expected fail
// must still fail the gate. Kept separate from the table above because
// it is the property the whole design rests on.
func TestSummarize_BrokenIsNotAllowlistable(t *testing.T) {
	rep := differentiators.Report{
		Scenario: differentiators.Scenario{
			ID: "E-example", Invariant: "x", Expect: differentiators.Broken, Rows: []string{"L1"},
		},
		Outcome: differentiators.Broken,
		Err:     errors.New("fixture never ran"),
	}
	// Even when the declaration matches the observation exactly, Broken
	// is a problem. Report.Matches() is true here, and that must not be
	// enough to pass.
	if !rep.Matches() {
		t.Fatal("premise: this report's outcome matches its declaration")
	}
	_, problems := summarize(rep)
	if len(problems) != 1 || !strings.Contains(problems[0], "BROKEN") {
		t.Fatalf("problems = %v, want a Broken report to fail the gate even when declared", problems)
	}
}

func TestSummary_WriteText(t *testing.T) {
	sum := Summary{
		Tier: TierDeterministic,
		Corpus: CorpusSummary{
			Dataset: "langchain-sre", Scenarios: 31, Intents: 19,
			Reach: []evals.MetricReach{{Metric: evals.MetricIntentCoverage, Scenarios: 31, Reaches: 31}},
		},
		Scenes: []ScenarioSummary{
			{ID: "E-approval-edited", Expected: "FAIL", Observed: "FAIL", Matched: true,
				Blocked: "W2.5 — no channel", Reason: "the model's arguments executed"},
		},
		ExpectedFail: []string{"E-approval-edited"},
	}

	var buf bytes.Buffer
	sum.WriteText(&buf)
	out := buf.String()
	for _, want := range []string{
		"tier deterministic",
		"intent_coverage",
		"E-approval-edited",
		"blocked on W2.5",
		"expected-fail allowlist: 1 of 1",
		"progress metric",
		"PASS",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report is missing %q:\n%s", want, out)
		}
	}

	sum.Problems = []string{"E-approval-edited: something"}
	buf.Reset()
	sum.WriteText(&buf)
	if out := buf.String(); !strings.Contains(out, "FAIL") || !strings.Contains(out, "E-approval-edited: something") {
		t.Errorf("failing report does not say so:\n%s", out)
	}
}

// TestSummary_WriteJSON keeps the machine-readable shape usable by
// W0.5's nightly delta, which diffs one run's expected-fail list against
// the last.
func TestSummary_WriteJSON(t *testing.T) {
	sum, err := Run(context.Background(), Config{Root: repoRoot, Scratch: t.TempDir()})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var buf bytes.Buffer
	if err := sum.WriteJSON(&buf); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	var back Summary
	if err := json.Unmarshal(buf.Bytes(), &back); err != nil {
		t.Fatalf("the report does not round-trip: %v", err)
	}
	if back.Tier != sum.Tier || len(back.Scenes) != len(sum.Scenes) {
		t.Errorf("round-trip lost data: %+v", back)
	}
	if len(back.ExpectedFail) != len(sum.ExpectedFail) {
		t.Errorf("expected-fail list = %v, want %v", back.ExpectedFail, sum.ExpectedFail)
	}
}

// TestJudgeSummary_WriteCost covers the three states the cost section
// can be in, because each one is a different claim to a reader skimming
// a nightly: priced and correct, priced and wrong, or not priced at all.
func TestJudgeSummary_WriteCost(t *testing.T) {
	rows := []judge.ScopeCost{
		{
			Name: "analyst", Tier: "small", Resolved: "claude-haiku-4-5",
			Ran:   []string{"claude-haiku-4-5-20251001"},
			Calls: 3, Tokens: 4200, CostUSD: 0.0042,
			WantRate: 0.001, GotRate: 0.001, AtParentRate: 0.315,
		},
		{
			Name: "_synthesis", Tier: "frontier", Resolved: "claude-opus-4-7",
			Ran:   []string{"claude-opus-4-7"},
			Calls: 1, Tokens: 900, CostUSD: 0.0675,
			WantRate: 0.075, GotRate: 0.075, AtParentRate: 0.0675,
		},
	}

	t.Run("priced", func(t *testing.T) {
		out := renderJudge(t, &JudgeSummary{
			Model: "claude-opus-4-7", Cost: &judge.CostBoard{
				Provider: "anthropic-vertex", RootModel: "claude-opus-4-7", RootRate: 0.075,
				Scopes: rows,
			},
		})
		for _, want := range []string{
			"J-cost-tier",
			"every tiered specialist was billed at its own rate",
			"claude-haiku-4-5",
			// The counterfactual is the number the row is about: what
			// these tokens would have cost at the parent's rate.
			"$0.31500",
			"ran as claude-haiku-4-5-20251001",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("cost section is missing %q:\n%s", want, out)
			}
		}
	})

	t.Run("mispriced", func(t *testing.T) {
		bad := &judge.CostBoard{
			Provider: "anthropic-vertex", RootModel: "claude-opus-4-7", RootRate: 0.075,
			Scopes:   rows,
			Findings: []string{"analyst ran on claude-haiku-4-5 but was billed at $0.07500/1K"},
		}
		out := renderJudge(t, &JudgeSummary{Model: "claude-opus-4-7", Cost: bad})
		if !strings.Contains(out, "a tiered specialist was not billed at its own rate") {
			t.Errorf("a mispriced board did not say so in its headline:\n%s", out)
		}
		if !strings.Contains(out, "PROBLEM: analyst ran on") {
			t.Errorf("the finding was not printed:\n%s", out)
		}
	})

	t.Run("skipped", func(t *testing.T) {
		out := renderJudge(t, &JudgeSummary{
			Model:       "echo",
			CostSkipped: "judge: cost: J-cost-tier needs a live model: root model \"echo\" collapses every tier back to itself",
		})
		if !strings.Contains(out, "J-cost-tier: SKIPPED") {
			t.Errorf("a skipped check left no trace in the report:\n%s", out)
		}
		if !strings.Contains(out, "not evidence that tiered pricing works") {
			t.Errorf("the skip did not say what it is not evidence of:\n%s", out)
		}
	})
}

// TestSummarizeCorpus_DiagnosticDeathIsReported covers the gap #179's
// demotion opened. evals.DeadMetrics names gating metrics only, so a
// diagnostic that can score nothing anywhere would have gone from a
// harness failure to a silent column — and severity_accuracy, whose
// extractor has now broken twice, is exactly such a metric. It is not a
// red parity row, but the harness has to say it measures nothing.
func TestSummarizeCorpus_DiagnosticDeathIsReported(t *testing.T) {
	ds, tbl, err := loadFixtures(repoRoot)
	if err != nil {
		t.Fatalf("loadFixtures: %v", err)
	}
	for i := range ds.Scenarios {
		ds.Scenarios[i].Outputs.ExpectedResponse = "the cluster looks unwell"
	}

	sum, _ := summarizeCorpus(tbl, ds)
	if len(sum.Dead) != 0 {
		t.Errorf("Dead = %v, want empty: no gating metric died here", sum.Dead)
	}
	if !slices.Contains(sum.DeadDiagnostics, evals.MetricSeverityAccuracy) {
		t.Errorf("DeadDiagnostics = %v, want it to name %s", sum.DeadDiagnostics, evals.MetricSeverityAccuracy)
	}
}

// TestSummarizeCorpus_DeadDiagnosticDoesNotGate is the half of the line
// above that #179 described and did not implement. It appended the dead
// diagnostic to problems, which Summary.OK reads, so the report said
// "does not gate" while the code gated. Nothing caught it because no
// diagnostic was dead yet; #174's tool_coverage fix makes one dead
// permanently, so the whole E tier would have gone red on a fact about
// the ported corpus.
func TestSummarizeCorpus_DeadDiagnosticDoesNotGate(t *testing.T) {
	ds, tbl, err := loadFixtures(repoRoot)
	if err != nil {
		t.Fatalf("loadFixtures: %v", err)
	}

	sum, problems := summarizeCorpus(tbl, ds)
	if !slices.Contains(sum.DeadDiagnostics, evals.MetricToolCoverage) {
		t.Fatalf("DeadDiagnostics = %v, want it to name %s on the shipped corpus", sum.DeadDiagnostics, evals.MetricToolCoverage)
	}
	for _, p := range problems {
		if strings.Contains(p, evals.MetricToolCoverage) {
			t.Errorf("problem %q gates on a dead diagnostic; a diagnostic is not a parity claim, so it cannot redden the suite", p)
		}
	}
	if len(problems) != 0 {
		t.Errorf("problems = %v, want none on the shipped corpus", problems)
	}

	// And the report still says it, on a line of its own.
	var buf bytes.Buffer
	Summary{Tier: TierDeterministic, Corpus: sum}.WriteText(&buf)
	out := buf.String()
	if !strings.Contains(out, "dead diagnostic") || !strings.Contains(out, evals.MetricToolCoverage) {
		t.Errorf("report does not name the dead diagnostic:\n%s", out)
	}
	if strings.Contains(out, "FAIL") {
		t.Errorf("a dead diagnostic failed the report:\n%s", out)
	}
}

func renderJudge(t *testing.T, j *JudgeSummary) string {
	t.Helper()
	var buf bytes.Buffer
	Summary{Tier: TierJudge, Judge: j}.WriteText(&buf)
	return buf.String()
}
