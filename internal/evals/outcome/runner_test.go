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

package outcome

import (
	"context"
	"errors"
	"iter"
	"strings"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/v2/model"
)

type stubModel struct{ name string }

func (m *stubModel) Name() string { return m.name }

func (m *stubModel) GenerateContent(context.Context, *adkmodel.LLMRequest, bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(func(*adkmodel.LLMResponse, error) bool) {}
}

// wiring is a RunnerConfig that would be accepted, so each test can name
// the one thing it breaks.
func wiring(t *testing.T) RunnerConfig {
	t.Helper()
	tbl := intentTable(t)
	corpus, err := Load(corpusDir, tbl)
	if err != nil {
		t.Fatal(err)
	}
	cl := &Cluster{Name: "mast-outcome-test", Context: "kind-mast-outcome-test"}
	prov, err := NewProvisioner(corpus, cl, corpusDir)
	if err != nil {
		t.Fatal(err)
	}
	return RunnerConfig{
		Corpus:      corpus,
		Table:       tbl,
		Cluster:     cl,
		Provisioner: prov,
		Surface:     &Surface{Version: "lookout v0.0.0-test", Tools: selectTools(tbl)},
		// Not a fake the tier would accept from a caller — cmd/outcome
		// refuses an offline model by name, because a scripted provider
		// cannot be wrong about a diagnosis. Here it is only standing in
		// for "a model was supplied".
		Model:   &stubModel{name: "mast-echo"},
		Scratch: t.TempDir(),
	}
}

// Everything NewRunner can refuse, it refuses at construction. The
// tier's expensive failure is discovering after eleven metered runs that
// the twelfth could never have been graded.
func TestNewRunnerRefusals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		break_ func(*RunnerConfig)
		want   string
	}{
		{"no cases", func(c *RunnerConfig) { c.Corpus.Cases = nil }, "no cases"},
		{"no cluster", func(c *RunnerConfig) { c.Cluster = nil }, "needs a cluster"},
		{"no provisioner", func(c *RunnerConfig) { c.Provisioner = nil }, "needs a provisioner"},
		{"no surface", func(c *RunnerConfig) { c.Surface = nil }, "needs a tool surface"},
		{"no model", func(c *RunnerConfig) { c.Model = nil }, "needs a model"},
		{"no scratch", func(c *RunnerConfig) { c.Scratch = "" }, "needs a scratch dir"},
		{
			// The run-time half of "never ship a rung that cannot fire".
			// The loader refuses an intent no lookout tool satisfies per
			// the table; this is the same rule against the surface the
			// model is actually shown.
			"an intent no tool on the surface satisfies",
			func(c *RunnerConfig) { c.Surface = &Surface{Tools: []string{"k8s_cloud_quota"}} },
			"would measure nothing",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := wiring(t)
			tc.break_(&cfg)
			_, err := NewRunner(cfg)
			if err == nil {
				t.Fatalf("accepted a config with %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error is %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestNewRunnerDefaults(t *testing.T) {
	r, err := NewRunner(wiring(t))
	if err != nil {
		t.Fatal(err)
	}
	if r.ceiling != DefaultCeiling {
		t.Errorf("ceiling = %s, want %s", r.ceiling, DefaultCeiling)
	}
	if r.concurrency != DefaultConcurrency {
		t.Errorf("concurrency = %d, want %d", r.concurrency, DefaultConcurrency)
	}
	if r.note == nil {
		t.Error("a nil Progress left a nil note, which the first progress line would panic on")
	}
}

// The read-only/mutating split is §8's runner obligation, honoured
// before any case sets the flag: the first mutating case rewrites the
// exact field all three admitted cases pin as a catastrophic safeguard.
func TestMutatingCasesAreSplitOut(t *testing.T) {
	cfg := wiring(t)
	// The shipped roster is read-only, so the mutating half has to be
	// synthesised — which is the point: this code has to be right before
	// there is a case to be wrong about.
	mut := cfg.Corpus.Cases[0]
	mut.ID = "crashloop-remediate-and-verify"
	mut.Mutating = true
	cfg.Corpus.Cases = append(cfg.Corpus.Cases, mut)

	r, err := NewRunner(cfg)
	if err != nil {
		t.Fatal(err)
	}
	readOnly, mutating := r.split()
	if len(mutating) != 1 || mutating[0].ID != "crashloop-remediate-and-verify" {
		t.Fatalf("mutating = %v, want the one synthesised case", ids(mutating))
	}
	if len(readOnly) != len(cfg.Corpus.Cases)-1 {
		t.Errorf("read-only = %v, want everything else", ids(readOnly))
	}
	for _, cs := range readOnly {
		if cs.Mutating {
			t.Errorf("%s is mutating and landed in the concurrent pool", cs.ID)
		}
	}
}

// The shipped roster is entirely read-only, so nothing reaches the
// mutating loop. Asserted rather than assumed: if this ever stops being
// true without a restore path, three catastrophic safeguards trip for a
// reason that has nothing to do with the agent.
func TestTheAdmittedRosterIsReadOnly(t *testing.T) {
	corpus, err := Load(corpusDir, intentTable(t))
	if err != nil {
		t.Fatal(err)
	}
	if mut := corpus.Mutating(); len(mut) > 0 {
		t.Fatalf("the admitted roster now has mutating cases (%v) — the runner sequences them last and alone and restores after each, and that path has never run against a real cluster", mut)
	}
}

// restore only touches a role that names the case in
// restore_required_after. A mutating case whose roles name nothing needs
// no restore, and inventing one would tear down a namespace the corpus
// did not ask to have rebuilt.
func TestRestorableReadsTheCatalog(t *testing.T) {
	cfg := wiring(t)
	r, err := NewRunner(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cs := cfg.Corpus.Cases[0]
	if got := r.restorable(cs); len(got) != 0 {
		t.Errorf("restorable(%s) = %v, want none — fixtures.yaml's restore_required_after is empty", cs.ID, got)
	}

	role, ok := cfg.Corpus.RoleFor("crashloop-workload")
	if !ok {
		t.Fatal("no crashloop-workload role")
	}
	role.RestoreRequiredAfter = []string{cs.ID}
	cfg.Corpus.Catalog.Roles["crashloop-workload"] = role
	r, err = NewRunner(cfg)
	if err != nil {
		t.Fatal(err)
	}
	got := r.restorable(cs)
	if len(got) != 1 || got[0] != "crashloop-workload" {
		t.Errorf("restorable(%s) = %v, want [crashloop-workload]", cs.ID, got)
	}
}

func TestOutcomeOf(t *testing.T) {
	for _, tc := range []struct {
		name     string
		run      Run
		verdicts []Verdict
		want     string
	}{
		{"clean", Run{}, []Verdict{{Check: "a", Passed: true}}, "passed"},
		{"one failed", Run{}, []Verdict{{Check: "a", Passed: true}, {Check: "b"}}, "failed: b"},
		{
			// An errored run still had its checks graded, and the line
			// says so: a provider timeout is not a regression in mast,
			// and a reader should not go looking for one.
			"errored", Run{Err: errors.New("deadline exceeded")},
			[]Verdict{{Check: "a", Passed: true}}, "errored (checks still graded)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := outcomeOf(tc.run, tc.verdicts); got != tc.want {
				t.Errorf("outcomeOf = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPassTimedOut(t *testing.T) {
	if (Pass{Elapsed: 3 * time.Minute, Ceiling: 20 * time.Minute}).TimedOut() {
		t.Error("a pass well inside its ceiling reported a timeout")
	}
	if !(Pass{Elapsed: 20 * time.Minute, Ceiling: 20 * time.Minute}).TimedOut() {
		t.Error("a pass that reached its ceiling did not report a timeout")
	}
}

// The instruction is written to be true of the tier, not of the corpus.
// A prompt that supplied a noun a check demands back would turn a
// diagnosis into a transcription, and the check would stop
// discriminating without going red.
func TestTheInstructionGivesNothingAway(t *testing.T) {
	contains := func(needle string) bool {
		return strings.Contains(strings.ToLower(systemInstruction), strings.ToLower(needle))
	}
	// The control. A test made of negative assertions passes just as
	// cheerfully against an empty string, so one positive one proves the
	// matcher is reading the instruction it claims to be reading.
	if !contains("kubernetes") {
		t.Fatal("the matcher found nothing in an instruction that is about Kubernetes; every assertion below is vacuous")
	}
	for _, leak := range []string{"payments-api", "OOMKilled", "64Mi", "seeded-debug", "exit 137", "memory limit"} {
		if contains(leak) {
			t.Errorf("the system instruction contains %q, which the corpus requires the agent to produce from a cluster read", leak)
		}
	}
}

var _ adkmodel.LLM = &stubModel{name: "x"}

func ids(cases []Case) []string {
	out := make([]string, 0, len(cases))
	for _, cs := range cases {
		out = append(out, cs.ID)
	}
	return out
}
