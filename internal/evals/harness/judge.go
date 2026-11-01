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
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"google.golang.org/adk/v2/model"

	"github.com/go-steer/core-agent/v3/internal/evals"
	"github.com/go-steer/core-agent/v3/internal/evals/evalcompose"
	"github.com/go-steer/core-agent/v3/internal/evals/judge"
	"github.com/go-steer/core-agent/v3/internal/kvwindow"
	"github.com/go-steer/core-agent/v3/internal/modelretry"
)

// JudgeSummary is the metered tier's board: the 31 ported LangChain
// scenarios run against a live model over the fixture cluster, scored by
// the same deterministic evaluators the free tier checks the
// measurability of, plus the LLM-as-judge quality grade.
//
// It reports; it does not gate. A low score is a finding to act on, not
// a broken build — the plan's §2 four-tier strategy puts the gate at S,
// U and E precisely so that a model having a bad day cannot turn main
// red. What the judge tier *can* fail on is its own machinery: a
// scenario whose run never happened says nothing about mast, so it is a
// Problem.
type JudgeSummary struct {
	// Model is the model under test, Grader the one that scored
	// response_quality. Both are named because the number is only worth
	// reading next to what produced it — and because nothing can enforce
	// that they differ, which they should.
	Model  string `json:"model"`
	Grader string `json:"grader"`
	// Provider is the credential path, or "auto" when compose picked it
	// from the environment.
	Provider string `json:"provider"`

	// Authored is how many of the scenarios ran against a hand-written
	// fixture rather than one derived from quoted spans of the expected
	// response. Stated on the board because a reader's first question
	// about any judge number is how much of the cluster the fixture
	// author chose.
	Authored int `json:"authored_fixtures"`

	Scenes    []JudgeScenario  `json:"scenarios"`
	Aggregate []MetricSummary  `json:"aggregate"`
	Notes     []string         `json:"notes,omitempty"`
	Ceilings  []CeilingFinding `json:"ceilings,omitempty"`

	// Retries is how many model calls this board only got by waiting out
	// a transient provider error (#239), and RetryWaitSeconds is how long
	// that cost.
	//
	// On the board rather than swallowed by the retry wrapper, because a
	// retry nobody can see is how a measurement quietly stops measuring:
	// a provider under worsening pressure would keep producing complete,
	// green, increasingly slow boards with nothing to point at. Zero on a
	// healthy night, so the delta surfaces the first night it is not.
	Retries          int     `json:"retries,omitempty"`
	RetryWaitSeconds float64 `json:"retry_wait_seconds,omitempty"`

	// Validity is #169's half of the board: not how many tools the run
	// reached for, but whether the calls it made were well formed and
	// whether they found anything.
	Validity ValidityBoard `json:"validity"`

	// Misses is #170's: of the questions intent_coverage marked
	// unanswered, the ones a tool in the catalog would have answered.
	Misses MissBoard `json:"misses"`

	// Cost is J-cost-tier: a two-tier roster run live, then priced off
	// the meter. Nil when the check could not run at all, which is
	// itself reported as a Problem.
	//
	// It rides this board because it needs the same thing the corpus
	// needs and nothing cheaper has — a real provider answering with
	// real usage numbers. It is unlike every other row here in that its
	// verdict is arithmetic rather than judgment, which is why it can
	// gate (see [Summary.Problems]) where the scores cannot.
	Cost *judge.CostBoard `json:"cost,omitempty"`
	// CostSkipped is why there is no cost board, when the reason is that
	// the run had no live model to price. Carried as a field rather than
	// left as a nil Cost so the board states the absence instead of
	// having one fewer section than the reader expected.
	CostSkipped string `json:"cost_skipped,omitempty"`

	// UnderTestUsage and GraderUsage are the tokens each model reported
	// across the whole run, J-cost-tier's calls included. They are what a
	// run costs before anyone has priced it — the number needed to try a
	// model the catalog has no rate for — and what a nightly costs after.
	UnderTestUsage *TokenUsage `json:"under_test_usage,omitempty"`
	GraderUsage    *TokenUsage `json:"grader_usage,omitempty"`

	// KVCache is the model server's KV-cache activity folded over every
	// incident, when the model under test is a provider profile that
	// publishes KV-cache metrics. Fleet-level; never priced.
	KVCache *kvwindow.Window `json:"kv_cache,omitempty"`
}

// JudgeScenario is one corpus row's run.
type JudgeScenario struct {
	ID        string               `json:"id"`
	Category  string               `json:"category"`
	Tools     []string             `json:"tools_called"`
	Answering []string             `json:"answering_tools"`
	Results   []evals.MetricResult `json:"results"`
	Response  string               `json:"response"`
	Ceiling   float64              `json:"intent_coverage_ceiling"`
	Authored  bool                 `json:"authored_fixture,omitempty"`
	Error     string               `json:"error,omitempty"`
	// LoopStop is why the in-turn loop guard ended this row's run (#514),
	// empty when it did not. The row is scored all the same.
	LoopStop string `json:"loop_stop,omitempty"`
	// KVCache is the model server's KV-cache activity while this row's
	// incident ran; see JudgeSummary.KVCache.
	KVCache *kvwindow.Window `json:"kv_cache,omitempty"`

	// Calls are the row's tool calls with their arguments and a digest
	// of each result, and Violations what was wrong with them. Both are
	// persisted rather than summarized away: Tools above answers "which
	// tools", and #169 exists because that question and "did the run
	// learn anything" have different answers often enough to matter.
	Calls      []evals.CallRecord `json:"calls,omitempty"`
	Violations []evals.Violation  `json:"violations,omitempty"`

	// Misses is this row's unsatisfied intents, partitioned into the ones
	// tool selection could have fixed and the ones the catalog cannot
	// serve. Persisted per row because the board's list is an enumeration
	// and a reader who wants one row's detail should not have to re-derive
	// it from the score.
	Misses evals.MissReport `json:"misses"`
}

// ValidityBoard is the corpus-wide view of how the runs used their
// tools.
//
// Deliberately counts and lists, not a mean. A malformed-call *rate*
// over rows that call different numbers of tools is not comparable to
// itself between runs, and the number a reader can act on is never the
// average — it is which tool, which argument, which scope.
type ValidityBoard struct {
	// Calls is every recorded call across the corpus.
	Calls int `json:"calls"`
	// Malformed are the calls the tool could not accept as sent: an
	// unknown name, a missing or invented argument, a type or enum the
	// schema does not allow, an error result, a call with no completion.
	// Every one is listed — they should be rare, and a rare thing
	// summarized as a count is a thing nobody investigates.
	Malformed []ScenarioViolation `json:"malformed,omitempty"`
	// EmptyReads is how many well-formed calls came back with nothing.
	// Not a defect: checking a hypothesis and ruling it out is the job.
	EmptyReads int `json:"empty_reads"`
	// Blind are scenarios where every completed call came back empty —
	// the run read the cluster, saw nothing anywhere, and reported
	// anyway. This is the shape #169 was opened for: on the old board
	// these rows were indistinguishable from rows that read well.
	Blind []string `json:"blind_runs,omitempty"`
	// Counts is the tally by kind, for a one-line comparison between
	// runs.
	Counts []string `json:"counts,omitempty"`
	// Arguments states how the recorded arguments and results were
	// treated, so a reader who received this file second-hand is told
	// rather than left to assume. See [argumentsNote].
	Arguments string `json:"arguments"`
}

// argumentsNote is #169's redaction answer, and it is W8's answer
// rather than a new one.
//
// W8 settled this for decision exports (pkg/transcript's
// argumentsWarning): tool arguments go out verbatim, because the
// proposed-versus-executed pair is the entire signal, and an export
// with the arguments stripped records only that somebody called
// something. The identical reasoning applies here — an empty read whose
// scope has been redacted is a violation nobody can act on — so the
// answer is the same.
//
// What differs is the exposure, and the artifact says so instead of
// inheriting a warning that would be false. W8's export carries an
// approver identity and a live fleet's arguments, so it digests the
// first and warns about the second. This board's calls are made against
// a synthetic corpus checked into the repo: there is no operator
// identity in an eval run to digest, and the scopes are fixture names.
// Stating that is the point — a warning printed where it cannot be true
// is how readers learn to skip warnings.
const argumentsNote = "recorded verbatim (the W8 answer, pkg/transcript's argumentsWarning); the corpus is a synthetic fixture, " +
	"so this board carries no cluster data — running the tier against real cluster reads would make it as sensitive as that cluster"

// MissBoard is the corpus-wide view of what the runs failed to ask.
//
// A list, not a rate, and for a sharper reason than #169's: the rate was
// measured and it points the wrong way. Counting scenarios that skipped
// a tool holding data flags a third of the Claude board and more than
// half the Gemini one, and the flagged rows score *higher* on
// response_quality than the rows that called everything — because most
// skipped tools were redundant with one that had already answered. See
// the note at the top of internal/evals/misses.go for the numbers.
//
// What survives that filter is small enough to enumerate: four rows
// across both v0.4.0 boards. So the board names each one and says which
// tool would have served it, which is what a reader can act on.
type MissBoard struct {
	// Scenarios is how many rows this board is drawn from — the ones that
	// ran. A row that errored is not a row that missed nothing.
	Scenarios int `json:"scenarios"`
	// Consequential is every unanswered question the catalog could have
	// answered, one entry per (row, intent).
	Consequential []ScenarioMiss `json:"consequential,omitempty"`
	// ByIntent tallies them by intent, because a miss that recurs across
	// rows is a description-or-discoverability problem with one tool
	// rather than three unrelated incidents.
	ByIntent []string `json:"by_intent,omitempty"`
	// OutOfCatalog is how many unsatisfied intents no lookout tool serves.
	// Counted, not listed: they are the structural ceiling, already
	// enumerated under [CeilingFinding], and repeating them here would
	// read as the model's failure rather than the surface's.
	OutOfCatalog int `json:"out_of_catalog"`
	// Untabled is how many expected tool names intents.yaml has never
	// seen. They deflate intent_coverage deliberately, but a gap in the
	// table is not a miss by the model and is not counted as one.
	Untabled int `json:"untabled"`
	// Attribution is #171: the misses above grouped under the one tool
	// that would have answered each, ranked by leverage.
	Attribution []MissAttribution `json:"attribution,omitempty"`
	// Shared is how many consequential misses had more than one tool that
	// would have answered them, so no single tool is responsible.
	// Counted rather than attributed — a miss charged to each of three
	// tools would triple the ranking's numbers and point at none of them.
	Shared int `json:"shared_misses"`
}

// MissAttribution is one tool's share of the board's consequential
// misses, with what skipping it costs the corpus (#171).
//
// Every entry is sole-source by construction. A miss whose intent has an
// alternative is not attributable: some other tool would have answered,
// so nothing about this one caused it.
//
// The finding this exists to surface: across both v0.4.0 boards, three
// of the four consequential misses are the same tool, k8s_resource_top,
// skipped independently by two unrelated model families. It is the only
// tool that satisfies either saturation intent, so skipping it does not
// risk the miss — it guarantees it. Sole-source × gated-scenario-count
// is what turns "the model skipped something" into a named item, and it
// is what makes the description A/B in judge.toolDescription worth
// running: the competing hypothesis is that the models are reasoning
// past the tool rather than failing to find it, and the same experiment
// separates the two.
type MissAttribution struct {
	Tool string `json:"tool"`
	// Misses are the "<scenario> <intent>" keys charged to this tool.
	Misses []string `json:"misses"`
	// SoleSourceFor is every intent this tool alone answers.
	SoleSourceFor []string `json:"sole_source_for"`
	// Gates is how many corpus scenarios cannot reach full
	// intent_coverage without it — the leverage half of the ranking.
	Gates int `json:"gates_scenarios"`
}

// ScenarioMiss is one consequential miss with the row it came from.
type ScenarioMiss struct {
	Scenario string `json:"scenario"`
	Intent   string `json:"intent"`
	// ServedBy are the catalog tools that would have answered it. Every
	// one is a tool this run did not call — had it called one, the intent
	// would be satisfied and this would not be a miss.
	ServedBy []string `json:"served_by"`
}

// ScenarioViolation is one violation with the row it came from.
type ScenarioViolation struct {
	Scenario  string          `json:"scenario"`
	Violation evals.Violation `json:"violation"`
}

// MetricSummary is one metric's mean across the rows that could score
// it. Vacuous rows are excluded from the mean rather than counted as
// zero — averaging in "there was nothing to check" is how a metric
// starts reporting the corpus's shape as the agent's performance.
type MetricSummary struct {
	Metric string `json:"metric"`
	// Mean is meaningless when Scored is zero; the report says so
	// rather than printing 0.000, which reads as a failing grade.
	Mean    float64 `json:"mean"`
	Scored  int     `json:"scored"`
	Vacuous int     `json:"vacuous"`
	// Diagnostic mirrors evals.Result.Diagnostic: a metric printed for
	// visibility that is not a parity claim. tool_coverage is the
	// standing example — name-level overlap scores mast's consolidated
	// read path as a regression.
	Diagnostic bool `json:"diagnostic,omitempty"`
}

// CeilingFinding names a scenario the read-only surface cannot fully
// cover, with the score it reached and the best it could have. Reported
// beside the board rather than folded into it: adjusting a score for a
// known structural gap is the tuning the plan warns against, but leaving
// the gap unstated would let a reader read a capped row as a miss.
type CeilingFinding struct {
	ID      string  `json:"id"`
	Ceiling float64 `json:"ceiling"`
	Scored  float64 `json:"scored"`
	Why     string  `json:"why"`
}

// runJudge scores the whole corpus against a live model.
//
// Sequential on purpose. Thirty-one runs against a frontier model take
// minutes, which is fine for a nightly, and a rate-limit error in the
// middle of a fan-out would land on the board as a scenario that "said
// nothing about mast" — noise indistinguishable from a real fixture
// failure. Cost and wall-clock are the plan's accepted price for this
// tier (~$5-15 a run).
func runJudge(ctx context.Context, cfg Config) (Summary, error) {
	sum := Summary{Tier: TierJudge}
	ds, tbl, err := loadFixtures(cfg.Root)
	if err != nil {
		return Summary{}, err
	}
	sum.Corpus, sum.Problems = summarizeCorpus(tbl, ds)

	overrides, err := judge.LoadOverrides(filepath.Join(cfg.Root, "testdata", "evals", "judge", "overrides.yaml"))
	if err != nil {
		return Summary{}, err
	}
	fixtures, err := judge.Fixtures(ds, overrides)
	if err != nil {
		return Summary{}, err
	}
	// After the fixtures, which check every override against the whole
	// corpus: a subset run must not make a valid override look orphaned.
	if ds, err = selectRows(ds, cfg.Rows); err != nil {
		return Summary{}, err
	}

	modelName := cfg.Model
	if modelName == "" {
		modelName = evalcompose.DefaultAnthropicModel
	}
	graderName := cfg.Grader
	if graderName == "" {
		graderName = evalcompose.DefaultAnthropicSmallModel
	}

	note := progressFn(cfg.Progress)

	build, buildUnder := cfg.buildModel, cfg.buildModel
	if build == nil {
		// No bundle on an eval run, so the provider's server-side
		// built-ins stay at mast's default-off baseline (#324) — the
		// same posture a shipped workload gets unless it asks
		// otherwise, which is the posture these tiers are measuring.
		// Prompt caching follows --prompt-cache (the product default
		// when unset), for the model under test only; the grader never
		// caches.
		build = func(ctx context.Context, provider, name string) (model.LLM, error) {
			return evalcompose.BuildModel(ctx, provider, name, graderModelOptions())
		}
		buildUnder = func(ctx context.Context, provider, name string) (model.LLM, error) {
			return evalcompose.BuildModel(ctx, provider, name, underTestModelOptions(cfg))
		}
	}
	rawUnder, err := buildUnder(ctx, cfg.Provider, modelName)
	if err != nil {
		return Summary{}, fmt.Errorf("harness: build model under test: %w", err)
	}
	rawGrading, err := build(ctx, cfg.Provider, graderName)
	if err != nil {
		return Summary{}, fmt.Errorf("harness: build grading model: %w", err)
	}
	// Both models, so the rig, the grader and J-cost-tier all survive the
	// same blip. The tier's expensive failure is a row that never ran, and
	// a 429 is the provider asking us to wait rather than telling us
	// anything about mast (#239) — see internal/modelretry.
	//
	// ONE policy for both, because the board already sums their counters
	// (below) and because the tier's numbers are per-run rather than
	// per-model. It is the eval policy rather than the production one on
	// purpose: this tier can afford to wait out half a minute to keep a
	// row, and an unattended turn cannot. The two are side by side in
	// modelretry so the divergence stays a diff.
	retryCfg := modelretry.JudgeConfig()
	if cfg.retryBackoff != nil {
		retryCfg.Backoff = cfg.retryBackoff
	}
	retryCfg.OnWait = func(model string, attempt int, wait time.Duration, err error) {
		who := "grader"
		if model == modelName {
			who = "model under test"
		}
		note("[retry] %s: %v — waiting %s before attempt %d", who, err, wait, attempt+1)
	}
	retries := modelretry.New(retryCfg)
	underUsage := &usageCounter{inner: retries.Wrap(rawUnder)}
	gradingUsage := &usageCounter{inner: retries.Wrap(rawGrading)}
	under, grading := model.LLM(underUsage), model.LLM(gradingUsage)

	scratch, cleanup, err := scratchDir(cfg)
	if err != nil {
		return Summary{}, err
	}
	defer cleanup()

	rig, err := judge.NewRig(tbl, fixtures, under, scratch)
	if err != nil {
		return Summary{}, err
	}
	grader, err := judge.NewJudge(grading)
	if err != nil {
		return Summary{}, err
	}

	board := &JudgeSummary{
		Model:    modelName,
		Grader:   graderName,
		Provider: providerLabel(cfg.Provider),
	}
	if modelName == graderName {
		board.Notes = append(board.Notes, fmt.Sprintf(
			"the model under test and the grader are both %s — a model grading its own output flatters it; pass --grader to separate them", modelName))
	}

	// J-cost-tier runs first. It is two specialists and one turn against
	// the same credentials the corpus is about to spend minutes on, and
	// its failures are machinery failures — a roster that will not
	// compose, a provider that will not answer. Finding that out now
	// rather than after thirty-one metered runs is worth the ordering.
	note("[cost] pricing a two-tier roster against %s", modelName)
	switch cost, err := judge.RunCost(ctx, under, modelName, cfg.Provider, scratch); {
	case errors.Is(err, judge.ErrCostNeedsLiveModel):
		// Not a Problem: the check was asked a question this
		// configuration cannot answer. Under an offline fake every score
		// on this board is machinery-shaped anyway, and calling the skip
		// a failure would train a reader to ignore the field that
		// matters when it is real. It is printed, loudly, so that the
		// skip is never read as a pass.
		board.CostSkipped = err.Error()
	case err != nil:
		sum.Problems = append(sum.Problems, fmt.Sprintf(
			"J-cost-tier did not run, so nothing checked that a tiered roster is billed at its own rates: %v", err))
	default:
		board.Cost = cost
		sum.Problems = append(sum.Problems, cost.Findings...)
	}

	// KV-cache sampling around each incident's run, for a model under
	// test served by a self-hosted server whose profile publishes
	// metrics. Nil for every other model; a sampler that cannot be built
	// is a note, not a failure.
	kvs, kvErr := evalcompose.KVSampler(ctx, cfg.Provider, modelName)
	if kvErr != nil {
		note("[kv] no KV-cache sampler for %s: %v", cfg.Provider, kvErr)
	}
	kv := &kvSampling{s: kvs, note: note}
	var kvFold kvwindow.Fold

	for i, sc := range ds.Scenarios {
		if err := ctx.Err(); err != nil {
			return Summary{}, err
		}
		note("[%2d/%2d] %s", i+1, len(ds.Scenarios), sc.ID)
		row := JudgeScenario{ID: sc.ID, Category: sc.Category}
		kvRun := kv.start(ctx)
		out, err := rig.Run(ctx, sc)
		row.KVCache = kv.stop(ctx, kvRun)
		kvFold.Add(row.KVCache)
		board.KVCache = kvFold.Window()
		if err != nil {
			// The run never happened, so this row is not a score. Saying
			// so is a Problem: an incomplete board read as a complete one
			// understates or overstates whatever it is missing.
			row.Error = err.Error()
			board.Scenes = append(board.Scenes, row)
			sum.Problems = append(sum.Problems, fmt.Sprintf(
				"%s: the run did not complete, so the board is missing a row rather than scoring one: %v", sc.ID, err))
			continue
		}

		grade, err := grader.Grade(ctx, sc, out.Response)
		if err != nil {
			// A grader failure costs one column, not the row: the
			// deterministic metrics are already scored and are the
			// primary ones.
			sum.Problems = append(sum.Problems, fmt.Sprintf(
				"%s: response_quality was not graded, so that column is short a row: %v", sc.ID, err))
		} else {
			out.Quality = &grade
			out.Results = append(out.Results, grade.Result())
		}

		row.Tools = out.Tools
		row.Answering = out.Answering
		row.Results = out.Results
		row.Response = out.Response
		row.Ceiling = out.Ceiling
		row.Authored = out.Authored
		row.Calls = out.Calls
		row.Violations = out.Violations
		row.Misses = out.Misses
		row.LoopStop = out.LoopStop
		if out.LoopStop != "" {
			note("[loop] %s: %s", sc.ID, out.LoopStop)
		}
		if out.Authored {
			board.Authored++
		}
		board.Scenes = append(board.Scenes, row)

		if out.Ceiling < 1 {
			board.Ceilings = append(board.Ceilings, CeilingFinding{
				ID:      sc.ID,
				Ceiling: out.Ceiling,
				Scored:  scoreOf(out.Results, evals.MetricIntentCoverage),
				Why:     "the corpus expects a write tool, which lookout's read-only surface excludes by design; remediation is mast's effect-outbox path",
			})
		}
	}

	// Counted across both models: from the board's point of view a
	// grader retry and a corpus retry are the same fact about the
	// provider, and splitting them would only invite reading one and
	// missing the other. One shared policy is what makes that a single
	// read rather than an addition that can drift.
	retryCount, _, retryWait := retries.Stats()
	board.Retries = retryCount
	board.RetryWaitSeconds = retryWait.Seconds()
	board.UnderTestUsage = underUsage.snapshot()
	board.GraderUsage = gradingUsage.snapshot()

	board.Aggregate = aggregate(board.Scenes)
	board.Validity = summarizeValidity(board.Scenes)
	board.Misses = summarizeMisses(board.Scenes, evals.GatingBy(tbl, ds))
	sum.Judge = board
	return sum, nil
}

// progressFn returns a printer for the per-scenario line, or a no-op
// when the caller did not ask for one. Nil-checking once here rather
// than at each call site keeps the loop readable.
func progressFn(w io.Writer) func(string, ...any) {
	if w == nil {
		return func(string, ...any) {}
	}
	return func(format string, args ...any) { fmt.Fprintf(w, format+"\n", args...) }
}

func providerLabel(p string) string {
	if p == "" {
		return "auto (from the environment)"
	}
	return p
}

func scoreOf(results []evals.MetricResult, metric string) float64 {
	for _, r := range results {
		if r.Metric == metric {
			return r.Score
		}
	}
	return 0
}

// aggregate means each metric over the rows that scored it, in a stable
// order: intent_coverage, then severity_accuracy, then response_quality,
// then the rest alphabetically. The order is the board's reading order
// and is deliberately not the gating set — severity_accuracy sits second
// because that is where a reader has looked for it since v0.3, and it
// stopped being a parity claim when #179 demoted it. What gates is
// marked on each row, not inferred from its column.
func aggregate(scenes []JudgeScenario) []MetricSummary {
	type acc struct {
		sum        float64
		scored     int
		vacuous    int
		diagnostic bool
	}
	byMetric := map[string]*acc{}
	for _, s := range scenes {
		for _, r := range s.Results {
			a, ok := byMetric[r.Metric]
			if !ok {
				a = &acc{}
				byMetric[r.Metric] = a
			}
			a.diagnostic = a.diagnostic || r.Diagnostic
			if r.Vacuous {
				a.vacuous++
				continue
			}
			a.sum += r.Score
			a.scored++
		}
	}

	rank := map[string]int{
		evals.MetricIntentCoverage:   0,
		evals.MetricSeverityAccuracy: 1,
		judge.MetricResponseQuality:  2,
	}
	out := make([]MetricSummary, 0, len(byMetric))
	for metric, a := range byMetric {
		m := MetricSummary{Metric: metric, Scored: a.scored, Vacuous: a.vacuous, Diagnostic: a.diagnostic}
		if a.scored > 0 {
			m.Mean = a.sum / float64(a.scored)
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		ri, oki := rank[out[i].Metric]
		rj, okj := rank[out[j].Metric]
		if oki != okj {
			return oki
		}
		if oki && ri != rj {
			return ri < rj
		}
		return out[i].Metric < out[j].Metric
	})
	return out
}

// summarizeValidity collects #169's counts across the board.
//
// A row that did not run contributes nothing rather than a zero: it has
// no calls to be right or wrong about, and counting it as clean would
// make a board that lost half its rows look better than one that
// finished.
func summarizeValidity(scenes []JudgeScenario) ValidityBoard {
	var b ValidityBoard
	var all []evals.Violation
	for _, s := range scenes {
		if s.Error != "" {
			continue
		}
		b.Calls += len(s.Calls)
		all = append(all, s.Violations...)

		empties := 0
		for _, v := range s.Violations {
			switch v.Kind {
			case evals.ViolationEmptyResult:
				empties++
				b.EmptyReads++
			default:
				b.Malformed = append(b.Malformed, ScenarioViolation{Scenario: s.ID, Violation: v})
			}
		}

		// Blind counts against the calls that completed, not against all
		// of them: a run whose one call was declined at a gate never saw
		// the cluster, and calling that "read everything and found
		// nothing" would blame the model for the harness.
		completed := 0
		for _, c := range s.Calls {
			if c.Completed {
				completed++
			}
		}
		if completed > 0 && empties == completed {
			b.Blind = append(b.Blind, s.ID)
		}
	}
	b.Counts = evals.ViolationCounts(all)
	b.Arguments = argumentsNote
	return b
}

// summarizeMisses collects #170's list across the board.
//
// Rows that did not run are skipped for the same reason
// [summarizeValidity] skips them, and it matters more here: a row with
// no run has no unsatisfied intents to classify, so counting it would
// make a board that lost rows read as a board that missed nothing.
func summarizeMisses(scenes []JudgeScenario, gating map[string]evals.ToolGating) MissBoard {
	var b MissBoard
	byIntent := map[string]int{}
	charged := map[string][]string{}
	for _, s := range scenes {
		if s.Error != "" {
			continue
		}
		b.Scenarios++
		b.OutOfCatalog += len(s.Misses.OutOfCatalog)
		b.Untabled += len(s.Misses.Untabled)
		for _, m := range s.Misses.Consequential {
			b.Consequential = append(b.Consequential, ScenarioMiss{
				Scenario: s.ID, Intent: m.Intent, ServedBy: m.ServedBy,
			})
			byIntent[m.Intent]++
			// Attributable only when one tool could have answered. With
			// two or more, charging the miss to each would triple the
			// ranking's numbers and name none of them as the cause.
			if len(m.ServedBy) != 1 {
				b.Shared++
				continue
			}
			name := m.ServedBy[0]
			charged[name] = append(charged[name], s.ID+" "+m.Intent)
		}
	}
	b.Attribution = rankAttribution(charged, gating)
	sort.Slice(b.Consequential, func(i, j int) bool {
		if b.Consequential[i].Scenario != b.Consequential[j].Scenario {
			return b.Consequential[i].Scenario < b.Consequential[j].Scenario
		}
		return b.Consequential[i].Intent < b.Consequential[j].Intent
	})
	for _, id := range sortedKeys(byIntent) {
		b.ByIntent = append(b.ByIntent, fmt.Sprintf("%s=%d", id, byIntent[id]))
	}
	return b
}

// rankAttribution orders the charged tools by leverage: how much of the
// corpus each one gates first, then how often it was actually skipped,
// then by name so two identical boards render identically.
//
// Gates leads because it is the durable fact. How many misses a tool
// collected on one board is a sample of one model on one night; how many
// scenarios it is the only answer for is a property of the corpus, and
// it is the number that says whether fixing this tool is worth an
// afternoon.
func rankAttribution(charged map[string][]string, gating map[string]evals.ToolGating) []MissAttribution {
	out := make([]MissAttribution, 0, len(charged))
	for name, misses := range charged {
		sort.Strings(misses)
		g := gating[name]
		out = append(out, MissAttribution{
			Tool:          name,
			Misses:        misses,
			SoleSourceFor: g.SoleSource,
			Gates:         len(g.Gates),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		switch {
		case out[i].Gates != out[j].Gates:
			return out[i].Gates > out[j].Gates
		case len(out[i].Misses) != len(out[j].Misses):
			return len(out[i].Misses) > len(out[j].Misses)
		}
		return out[i].Tool < out[j].Tool
	})
	return out
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// writeJudge renders the metered board.
func (j *JudgeSummary) write(p func(string, ...any)) {
	p("")
	p("judge tier — model under test %s, grader %s, provider %s", j.Model, j.Grader, j.Provider)
	p("  fixtures: %d of %d hand-authored, the rest derived from quoted spans of the expected response (marked *)",
		j.Authored, len(j.Scenes))
	if j.Retries > 0 {
		p("  provider: %d call(s) only completed on a retry, %.0fs spent waiting — this board is complete, but the provider was under pressure (#239)",
			j.Retries, j.RetryWaitSeconds)
	}
	p("")
	p("  %-42s %-9s %-9s %-9s %s", "scenario", "intent", "severity", "quality", "tools")
	for _, s := range j.Scenes {
		if s.Error != "" {
			p("  %-42s %s", s.ID, "DID NOT RUN — "+s.Error)
			continue
		}
		capped := ""
		if s.Ceiling < 1 {
			capped = fmt.Sprintf(" (ceiling %.2f)", s.Ceiling)
		}
		id := s.ID
		if s.Authored {
			id += "*"
		}
		p("  %-42s %-9s %-9s %-9s %s%s",
			id,
			cell(s.Results, evals.MetricIntentCoverage),
			cell(s.Results, evals.MetricSeverityAccuracy),
			cell(s.Results, judge.MetricResponseQuality),
			strings.Join(s.Tools, ","),
			capped)
	}

	p("")
	for _, m := range j.Aggregate {
		var line string
		if m.Scored == 0 {
			// A mean of nothing is not zero. Printing 0.000 here would
			// read as every row failing the metric.
			line = fmt.Sprintf("  mean %-20s n/a — no scenario had anything to score", m.Metric)
		} else {
			line = fmt.Sprintf("  mean %-20s %.3f over %d scenario(s)", m.Metric, m.Mean, m.Scored)
			if m.Vacuous > 0 {
				line += fmt.Sprintf("; %d had nothing to score and are excluded", m.Vacuous)
			}
		}
		if m.Diagnostic {
			line += " [diagnostic, not a parity claim]"
		}
		p("%s", line)
	}

	j.writeValidity(p)
	j.writeMisses(p)

	if len(j.Ceilings) > 0 {
		p("")
		p("structural ceilings (reported, never folded into the score):")
		for _, c := range j.Ceilings {
			p("  %s scored %.2f against a ceiling of %.2f — %s", c.ID, c.Scored, c.Ceiling, c.Why)
		}
	}
	j.writeLoopStops(p)
	j.writeKVCache(p)
	j.writeCost(p)
	j.writeUsage(p)

	for _, n := range j.Notes {
		p("")
		p("note: %s", n)
	}
	p("")
	p("a low score here is a finding, not a red build: this tier's scoring reports and does not gate.")
	p("J-cost-tier is the exception — its verdict is arithmetic, so a mispriced tier is a Problem.")
}

// writeLoopStops lists the rows the in-turn loop guard ended. Silent
// when there are none, which is the common case.
func (j *JudgeSummary) writeLoopStops(p func(string, ...any)) {
	var stopped []JudgeScenario
	for _, s := range j.Scenes {
		if s.LoopStop != "" {
			stopped = append(stopped, s)
		}
	}
	if len(stopped) == 0 {
		return
	}
	p("")
	p("loop stops (#514) — %d run(s) ended by the in-turn loop guard, scored on what they did before it:", len(stopped))
	for _, s := range stopped {
		p("  %s: %s", s.ID, s.LoopStop)
	}
}

// writeValidity renders how the runs used their tools.
//
// Two sections, because the two halves are read differently. A
// malformed call is something to fix and every one is printed. An empty
// read is ordinary — a hypothesis checked and ruled out — right up
// until every read in a row is empty, at which point the row's
// diagnosis rests on nothing and the score above it is not measuring
// what it appears to.
func (j *JudgeSummary) writeValidity(p func(string, ...any)) {
	v := j.Validity
	p("")
	p("call validity (#169) — %d call(s) recorded across the board", v.Calls)
	if v.Calls == 0 {
		p("  no calls were recorded at all; nothing on this board was scored against a tool the model actually used")
		return
	}
	if len(v.Counts) > 0 {
		p("  %s", strings.Join(v.Counts, "  "))
	}

	if len(v.Malformed) == 0 {
		p("  malformed: none — every call named a declared tool, and matched its schema")
	} else {
		p("  malformed calls (%d) — enumerated, never averaged into a score:", len(v.Malformed))
		for _, m := range v.Malformed {
			p("    %s %s", m.Scenario, m.Violation)
		}
	}

	p("  empty reads: %d of %d call(s) were well formed and found nothing", v.EmptyReads, v.Calls)
	if len(v.Blind) > 0 {
		p("  ran blind (%d) — every completed call came back empty, so the row's diagnosis rests on no evidence:", len(v.Blind))
		p("    %s", strings.Join(v.Blind, ", "))
		p("    a score on these rows measures what the model already knew about Kubernetes, not what it read")
	}
	p("  arguments: %s", v.Arguments)
}

// writeMisses renders what the runs did not ask.
//
// The line under the heading is doing real work. Every reader of this
// section arrives expecting "tools the run skipped", which is a much
// larger number pointing the other way, so the section says up front
// which question it answers and prints the out-of-catalog count beside
// it — the difference between the two is the whole content of #170.
func (j *JudgeSummary) writeMisses(p func(string, ...any)) {
	m := j.Misses
	p("")
	if m.Scenarios == 0 {
		p("consequential misses (#170) — no scenario ran, so there is nothing to classify")
		return
	}
	if len(m.Consequential) == 0 {
		p("consequential misses (#170) — none across %d scenario(s)", m.Scenarios)
	} else {
		p("consequential misses (#170) — %d across %d scenario(s)", len(m.Consequential), m.Scenarios)
	}
	p("  a question the corpus expected answered, that a tool in the catalog would have answered, that the run never asked")
	p("  a skipped tool is not one of these: most are redundant with a tool that already answered, and the rows that skip them score higher")
	for _, c := range m.Consequential {
		p("    %s %s — %s would have answered it", c.Scenario, c.Intent, strings.Join(c.ServedBy, ", "))
	}
	if len(m.ByIntent) > 0 {
		p("  by intent: %s", strings.Join(m.ByIntent, "  "))
		p("  an intent that recurs here is one tool's description or discoverability, not several unrelated rows")
	}
	if len(m.Attribution) > 0 {
		p("  attributable to one tool (#171), most leverage first:")
		for _, a := range m.Attribution {
			p("    %s — sole answer to %s, gates %d of the corpus's scenarios, missed %d time(s) here",
				a.Tool, strings.Join(a.SoleSourceFor, " and "), a.Gates, len(a.Misses))
			p("      skipping it does not risk those misses, it guarantees them: no other tool in the catalog answers")
		}
	}
	if m.Shared > 0 {
		p("  %d miss(es) had more than one tool that would have answered, so none is attributable to a tool", m.Shared)
	}
	if m.OutOfCatalog > 0 {
		p("  %d further unsatisfied intent(s) no read-only tool serves — the structural ceiling below, not a miss by the model", m.OutOfCatalog)
	}
	if m.Untabled > 0 {
		p("  %d expected tool name(s) intents.yaml has never seen — a gap in the table; it deflates intent_coverage but is not a miss", m.Untabled)
	}
}

// writeCost renders the cost board, or says why there isn't one. The
// absent case gets a line of its own for the same reason the check
// exists at all: a section that silently disappears reads as a section
// that passed.
func (j *JudgeSummary) writeCost(p func(string, ...any)) {
	b := j.Cost
	p("")
	if b == nil {
		if j.CostSkipped != "" {
			p("J-cost-tier: SKIPPED — %s", j.CostSkipped)
			p("  no tier was priced this run; this is not evidence that tiered pricing works")
			return
		}
		p("J-cost-tier: DID NOT RUN — see problems above; no tier was priced this run")
		return
	}

	verdict := "every tiered specialist was billed at its own rate"
	if !b.OK() {
		verdict = "a tiered specialist was not billed at its own rate"
	}
	p("J-cost-tier — %s (root %s at $%.5f/1K, provider %s)", verdict, b.RootModel, b.RootRate, b.Provider)
	p("  %-14s %-9s %-24s %-6s %-8s %-11s %-11s %s",
		"specialist", "tier", "resolved", "calls", "tokens", "billed", "at root", "rate/1K")
	for _, s := range b.Scopes {
		billed := fmt.Sprintf("$%.5f", s.CostUSD)
		saved := fmt.Sprintf("$%.5f", s.AtParentRate)
		rate := fmt.Sprintf("$%.5f", s.GotRate)
		if s.Calls == 0 {
			billed, saved, rate = "—", "—", "—"
		}
		p("  %-14s %-9s %-24s %-6d %-8d %-11s %-11s %s",
			s.Name, s.Tier, s.Resolved, s.Calls, s.Tokens, billed, saved, rate)
		if len(s.Ran) > 0 {
			p("  %-14s ran as %s", "", strings.Join(s.Ran, ", "))
		}
	}
	for _, n := range b.Notes {
		p("  note: %s", n)
	}
	for _, f := range b.Findings {
		p("  PROBLEM: %s", f)
	}
}

func cell(results []evals.MetricResult, metric string) string {
	for _, r := range results {
		if r.Metric == metric {
			if r.Vacuous {
				return "n/a"
			}
			return fmt.Sprintf("%.2f", r.Score)
		}
	}
	return "-"
}

// writeUsage renders the token totals.
func (j *JudgeSummary) writeUsage(p func(string, ...any)) {
	for _, u := range []struct {
		who string
		u   *TokenUsage
	}{{"model under test (" + j.Model + ")", j.UnderTestUsage}, {"grader (" + j.Grader + ")", j.GraderUsage}} {
		if u.u == nil {
			continue
		}
		p("")
		p("tokens, %s: %d calls, %d prompt (%d cached, %d cache writes), %d output, %d thoughts",
			u.who, u.u.Calls, u.u.PromptTokens, u.u.CachedTokens, u.u.CacheWriteTokens, u.u.OutputTokens, u.u.ThoughtsTokens)
	}
}

// underTestModelOptions is what the model under test is built with: no
// bundle, so built-ins at mast's default-off baseline, and the prompt
// cache --prompt-cache asked for (the product default when unset).
func underTestModelOptions(cfg Config) evalcompose.ModelOptions {
	return evalcompose.ModelOptions{PromptCache: cfg.PromptCache}
}

// graderModelOptions is what the grader is built with. Caching is off
// explicitly, because the zero block now means the product default (on):
// a grader whose cost moved with --prompt-cache would muddy the A/B the
// flag exists for.
func graderModelOptions() evalcompose.ModelOptions {
	off := false
	return evalcompose.ModelOptions{PromptCache: evalcompose.PromptCache{Enabled: &off}}
}
