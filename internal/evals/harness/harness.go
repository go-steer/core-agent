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

// Package harness is the runnable form of the v0.3 parity eval suite
// (docs/v0.3-plan.md W0.4): the thing scripts/evals.sh invokes and CI
// gates on.
//
// It runs two suites, and the split is the plan's four-tier strategy
// (§2) rather than an implementation detail:
//
//   - The corpus suite loads the 31 ported LangChain scenarios and the
//     intent table and checks that the metrics can score them. It does
//     not run them. Scoring a trajectory requires a model that chooses,
//     and a scripted provider does not choose — replaying a fixture and
//     asserting the tools match the fixture asserts that the script
//     equals itself. Scoring the corpus is therefore the judge tier's
//     job (W0.5), and what the free tier gates is that the measurement
//     is not a constant function.
//
//   - The differentiator suite runs the five scenarios upstream's
//     harness structurally cannot express, each against the composed
//     runtime, and holds each to its declared outcome.
//
// # Why the second suite has an allowlist and the first does not
//
// The differentiators are the scoreboard's red rows in executable form,
// so most of them fail today and must be allowed to. That declaration
// lives in exactly one place — differentiators.Scenario.Expect — and is
// checked in both directions, so a capability that lands without its
// entry being flipped fails the suite. A separate allowlist file was
// considered and rejected: two records of one bit is the drift the
// bidirectional check exists to prevent, and the Go declaration is
// already a diffable artifact whose shrinking a reviewer can see. What
// this package adds is making that state the report's headline instead
// of something a reader has to reconstruct from test output.
package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/adk/v2/model"

	"github.com/go-steer/core-agent/v3/internal/evals"
	"github.com/go-steer/core-agent/v3/internal/evals/differentiators"
	"github.com/go-steer/core-agent/v3/internal/evals/evalcompose"
)

// Tiers, per docs/v0.3-plan.md §2. S (smoke) and U (UAT) have their own
// harnesses — `go test` and scripts/uat-v0.2.sh — so this runner covers
// E and J only.
const (
	// TierDeterministic is the free, credential-free gate. Default.
	TierDeterministic = "deterministic"
	// TierJudge is the metered nightly report: the 31 corpus scenarios
	// scored against a live model over the fixture cluster (W0.5). It
	// reports rather than gates — see [Summary.OK].
	TierJudge = "judge"
)

// Config is what the runner needs from its caller.
type Config struct {
	// Root is the repository root, the base for testdata paths.
	Root string
	// Tier selects the suite. Empty means TierDeterministic.
	Tier string
	// Scratch is a directory the fixtures own for the duration of the
	// run. Empty means one is made under os.TempDir (house rule #5) and
	// removed afterwards.
	Scratch string

	// Model is the model under test in the judge tier. Empty means the
	// Anthropic default. Ignored by the deterministic tier, which has no
	// model at all.
	Model string
	// Grader is the model that scores response_quality. Empty means the
	// small Anthropic default: upstream grades with gpt-4o-mini for the
	// same reason, and a grader is comparing two texts rather than
	// diagnosing a cluster.
	Grader string
	// Provider selects the credential path for both models
	// (`anthropic`, `anthropic-vertex`, or — with gemini-* names in
	// Model/Grader — `gemini`, `vertex`). Empty lets compose pick from
	// the environment.
	Provider string

	// Rows limits the judge tier to these corpus rows, by scenario id.
	// Empty runs the whole corpus. For a cost probe before a full run on
	// a new model, or re-running the one row a nightly lost. The corpus
	// is still validated whole, so a subset cannot hide a broken row.
	Rows []string

	// PromptCache is the Claude prompt caching the judge tier's model
	// under test runs with — a bundle's prompt_cache block, since an eval
	// run has no bundle. Zero is the product default (on, 5 minutes). The
	// grader never caches, so this changes only the row being measured,
	// and an A/B against `off` stays reproducible from the flag alone.
	PromptCache evalcompose.PromptCache

	// Progress, when set, receives a line per scenario as the judge tier
	// works through the corpus. Thirty-one live runs take minutes and the
	// report is written all at once at the end, so without this the tier
	// is indistinguishable from a hang — which is the state an operator
	// is most likely to react to by killing a metered run halfway.
	//
	// Deliberately separate from the report writer: the CLI points this
	// at stderr so the board on stdout stays a clean artifact.
	Progress io.Writer

	// buildModel and retryBackoff are the judge tier's test seam, and
	// they are unexported because that is all they are for. The tier's
	// resilience to a transient provider error (#239) is otherwise only
	// reachable with a provider that can be made to return 429 on
	// demand, which is exactly the thing a unit test cannot have — and
	// an untested wiring is how the retry ends up counted nowhere.
	//
	// Nil buildModel means compose.BuildModel; nil retryBackoff means
	// the judge package's own schedule.
	buildModel   func(ctx context.Context, provider, name string) (model.LLM, error)
	retryBackoff []time.Duration
}

// CorpusSummary is what the corpus suite found.
type CorpusSummary struct {
	Dataset   string              `json:"dataset"`
	Scenarios int                 `json:"scenarios"`
	Intents   int                 `json:"intents"`
	Reach     []evals.MetricReach `json:"reach"`
	Dead      []string            `json:"dead_metrics,omitempty"`
	// DeadDiagnostics names the diagnostic columns that score nothing
	// anywhere. Separate from Dead because it does not gate — a
	// diagnostic is not a parity claim, so a dead one is not a red run —
	// and separate from silence because a constant that nobody names is
	// read as a measurement. tool_coverage lives here permanently: the
	// ported corpus expects upstream's tool names and mast cannot emit
	// them, which is a fact about the two surfaces and not a defect to
	// fix (#174).
	DeadDiagnostics []string `json:"dead_diagnostics,omitempty"`
}

// ScenarioSummary is one differentiator's outcome, flattened so the
// report can be serialized.
type ScenarioSummary struct {
	ID        string   `json:"id"`
	Invariant string   `json:"invariant"`
	Expected  string   `json:"expected"`
	Observed  string   `json:"observed"`
	Matched   bool     `json:"matched"`
	Blocked   string   `json:"blocked,omitempty"`
	Rows      []string `json:"rows"`
	Reason    string   `json:"reason,omitempty"`
	Tools     []string `json:"tools,omitempty"`
	Error     string   `json:"error,omitempty"`
}

// Summary is the whole run.
type Summary struct {
	Tier   string            `json:"tier"`
	Corpus CorpusSummary     `json:"corpus"`
	Scenes []ScenarioSummary `json:"differentiators"`
	// ExpectedFail is the allowlist, in report form: the scenario IDs
	// declared red today. Shrinking it is v0.3's progress metric, so it
	// is a first-class field rather than something to count by eye.
	ExpectedFail []string `json:"expected_fail"`
	// Judge is the metered tier's board. Nil on the deterministic tier.
	Judge *JudgeSummary `json:"judge,omitempty"`
	// Problems are the reasons this run failed. Empty means green.
	//
	// The judge tier records a Problem only when the *measurement* is
	// broken — a metric that scores nothing, a scenario whose run never
	// happened. A low score is never a Problem: J reports, it does not
	// gate (docs/v0.3-plan.md §2).
	//
	// J-cost-tier's findings are Problems, and they are not an exception
	// to that rule but an instance of it. What it checks is arithmetic
	// over the meter's own numbers — a specialist whose tier resolved to
	// the cheap model and whose tokens were billed at the parent's rate
	// is a mast bug with a live reproduction, not a model having a bad
	// day. Nothing about the verdict depends on what the model said.
	Problems []string `json:"problems,omitempty"`
}

// OK reports whether the run gates green.
func (s Summary) OK() bool { return len(s.Problems) == 0 }

// Run executes the configured tier.
//
// It returns an error only when the harness itself could not run — an
// unreadable fixture, an unknown tier, absent credentials. A scenario
// that fails, or a metric that scores nothing, is a Problem on the
// Summary, not an error: the caller wants the whole report, not the
// first thing that went wrong.
func Run(ctx context.Context, cfg Config) (Summary, error) {
	tier := cfg.Tier
	if tier == "" {
		tier = TierDeterministic
	}
	switch tier {
	case TierDeterministic:
		return runDeterministic(ctx, cfg)
	case TierJudge:
		return runJudge(ctx, cfg)
	default:
		return Summary{}, fmt.Errorf("unknown tier %q (want %q or %q)", tier, TierDeterministic, TierJudge)
	}
}

func runDeterministic(ctx context.Context, cfg Config) (Summary, error) {
	sum := Summary{Tier: TierDeterministic}
	ds, tbl, err := loadFixtures(cfg.Root)
	if err != nil {
		return Summary{}, err
	}
	sum.Corpus, sum.Problems = summarizeCorpus(tbl, ds)

	scratch, cleanup, err := scratchDir(cfg)
	if err != nil {
		return Summary{}, err
	}
	defer cleanup()

	scenarios := differentiators.All()
	if err := differentiators.Validate(scenarios); err != nil {
		// A malformed registry invalidates the both-directions check, so
		// it is a harness failure and the suite does not run.
		return Summary{}, err
	}
	for _, rep := range differentiators.RunAll(ctx, scenarios, scratch) {
		s, problems := summarize(rep)
		sum.Scenes = append(sum.Scenes, s)
		sum.Problems = append(sum.Problems, problems...)
		if rep.Scenario.Expect == differentiators.Fail {
			sum.ExpectedFail = append(sum.ExpectedFail, rep.Scenario.ID)
		}
	}
	return sum, nil
}

// scratchDir resolves the run's scratch directory, returning a cleanup
// that removes it only when the harness made it. A caller-supplied
// directory is the caller's to delete.
func scratchDir(cfg Config) (string, func(), error) {
	if cfg.Scratch != "" {
		return cfg.Scratch, func() {}, nil
	}
	dir, err := os.MkdirTemp("", "mast-evals-")
	if err != nil {
		return "", nil, fmt.Errorf("harness: scratch dir: %w", err)
	}
	return dir, func() { _ = os.RemoveAll(dir) }, nil
}

func loadFixtures(root string) (evals.Dataset, evals.IntentTable, error) {
	dsPath := filepath.Join(root, "testdata", "evals", "scenarios", "langchain-sre.jsonl")
	ds, err := evals.LoadDataset(dsPath)
	if err != nil {
		return evals.Dataset{}, evals.IntentTable{}, err
	}
	tbl, err := evals.LoadIntentTable(filepath.Join(root, "testdata", "evals", "intents.yaml"))
	if err != nil {
		return evals.Dataset{}, evals.IntentTable{}, err
	}
	return ds, tbl, nil
}

// summarizeCorpus reports whether the metrics can score this corpus, and
// names any that cannot. Both tiers run it: on the deterministic tier it
// is the whole corpus check, and on the judge tier it is what keeps a
// board of real numbers from being read as meaningful when one of the
// columns is a constant.
func summarizeCorpus(tbl evals.IntentTable, ds evals.Dataset) (CorpusSummary, []string) {
	reach := evals.CorpusReach(tbl, ds)
	sum := CorpusSummary{
		Dataset:   ds.Meta.Fixture,
		Scenarios: len(ds.Scenarios),
		Intents:   len(tbl.Intents),
		Reach:     reach,
		Dead:      evals.DeadMetrics(reach),
	}
	var problems []string
	for _, m := range sum.Dead {
		problems = append(problems, fmt.Sprintf(
			"metric %q scores nothing anywhere in the corpus: it is a constant function, so no board it appears on means anything", m))
	}
	// A diagnostic never gates the parity claim — that is what makes it a
	// diagnostic — but a diagnostic that can score nothing is still a
	// broken measurement, and evals.DeadMetrics deliberately does not
	// name one. Reported separately so both readings hold: the column is
	// not a red parity row, and the harness still says it measures
	// nothing. severity_accuracy is why this is not hypothetical — it was
	// demoted to diagnostic by #179 while remaining the metric whose
	// extractor has broken twice.
	//
	// This list is deliberately not appended to problems. #179 landed it
	// there while describing it as non-gating, which was true only
	// because no diagnostic was dead at the time; Summary.OK reads
	// problems, so the first dead diagnostic would have turned a
	// permanent property of the ported corpus into a red suite. #174's
	// tool_coverage fix is that first one.
	for _, r := range reach {
		if r.Diagnostic && r.Dead() {
			sum.DeadDiagnostics = append(sum.DeadDiagnostics, r.Metric)
		}
	}
	return sum, problems
}

func summarize(rep differentiators.Report) (ScenarioSummary, []string) {
	s := rep.Scenario
	out := ScenarioSummary{
		ID:        s.ID,
		Invariant: s.Invariant,
		Expected:  s.Expect.String(),
		Observed:  rep.Outcome.String(),
		Matched:   rep.Matches(),
		Blocked:   s.Blocked,
		Rows:      s.Rows,
		Reason:    rep.Result.Reason,
		Tools:     rep.Result.Trace.CalledTools(),
	}
	if rep.Err != nil {
		out.Error = rep.Err.Error()
	}

	var problems []string
	switch {
	case rep.Outcome == differentiators.Broken:
		problems = append(problems, fmt.Sprintf(
			"%s: BROKEN — the fixture did not produce a run, so this says nothing about mast: %v", s.ID, rep.Err))
	case rep.Matches():
	case rep.Outcome == differentiators.Pass:
		problems = append(problems, fmt.Sprintf(
			"%s: declared FAIL (blocked on %s) but the invariant now holds — the capability landed. "+
				"Flip Expect to Pass and drop Blocked; shrinking the expected-fail list is v0.3's progress metric.", s.ID, s.Blocked))
	default:
		problems = append(problems, fmt.Sprintf(
			"%s: declared PASS but the invariant no longer holds — regression in shipped behaviour: %s", s.ID, rep.Result.Reason))
	}
	return out, problems
}

// WriteText renders the operator-facing report.
func (s Summary) WriteText(w io.Writer) {
	p := func(format string, args ...any) { fmt.Fprintf(w, format+"\n", args...) }

	p("")
	p("mast v0.3 parity evals — tier %s", s.Tier)
	p("")
	p("corpus: %s (%d scenarios, %d intents)", s.Corpus.Dataset, s.Corpus.Scenarios, s.Corpus.Intents)
	for _, r := range s.Corpus.Reach {
		p("  %s", r)
	}
	for _, m := range s.Corpus.DeadDiagnostics {
		// Not under FAIL, and not silent. The reach row above already
		// carries the number; this is the sentence that keeps a reader
		// from taking the column for a measurement.
		p("  note: %s is a dead diagnostic — it does not gate, and any value it prints is a constant", m)
	}

	if s.Judge != nil {
		// The judge tier scores the corpus rather than running the
		// differentiators, which are deterministic and free and belong
		// on the gating tier.
		s.Judge.write(p)
		p("")
		if s.OK() {
			p("REPORTED")
			return
		}
		p("INCOMPLETE")
		for _, prob := range s.Problems {
			p("  - %s", prob)
		}
		return
	}

	p("")
	p("differentiators:")
	for _, sc := range s.Scenes {
		mark := "ok"
		if !sc.Matched {
			mark = "!!"
		}
		p("  %s %-22s %-6s (declared %s)", mark, sc.ID, sc.Observed, sc.Expected)
		if sc.Reason != "" {
			p("       %s", wrapIndent(sc.Reason, 7))
		}
		if sc.Blocked != "" {
			p("       blocked on %s", wrapIndent(sc.Blocked, 7))
		}
	}
	p("")
	if len(s.ExpectedFail) == 0 {
		p("expected-fail allowlist: empty — every differentiator holds")
	} else {
		p("expected-fail allowlist: %d of %d (%s)", len(s.ExpectedFail), len(s.Scenes), strings.Join(s.ExpectedFail, ", "))
		p("  shrinking this list is v0.3's progress metric; each entry names the workstream that removes it")
	}
	p("")
	if s.OK() {
		p("PASS")
		return
	}
	p("FAIL")
	for _, prob := range s.Problems {
		p("  - %s", prob)
	}
}

// WriteJSON renders the machine-readable report — the shape W0.5's
// nightly diffs one run against the last.
func (s Summary) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(s)
}

// wrapIndent hard-wraps a reason at a readable width so a long
// observation stays legible in a CI log.
func wrapIndent(s string, indent int) string {
	const width = 68
	words := strings.Fields(s)
	if len(words) == 0 {
		return ""
	}
	var b strings.Builder
	line := 0
	for i, word := range words {
		if i > 0 && line+1+len(word) > width {
			b.WriteString("\n" + strings.Repeat(" ", indent))
			line = 0
		} else if i > 0 {
			b.WriteString(" ")
			line++
		}
		b.WriteString(word)
		line += len(word)
	}
	return b.String()
}
