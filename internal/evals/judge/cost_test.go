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

package judge

import (
	"context"
	"errors"
	"iter"
	"strings"
	"testing"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v3/internal/evals/evalcompose"
	"github.com/go-steer/core-agent/v3/pkg/taskclass"
)

// The check itself needs credentials. Its two halves do not: the roster
// is a value, and the verdicts are arithmetic over numbers. Both are
// tested here, so what the nightly adds is the provider and nothing
// else.

// TestCostSpecs_PricesTwoDifferentRates is the test that keeps the whole
// check from being vacuous. A roster whose two tiers resolve to the same
// model would price correctly every night and prove nothing, and it
// would do that silently — the meter has no opinion about whether the
// two rates it was handed are the same number.
func TestCostSpecs_PricesTwoDifferentRates(t *testing.T) {
	specs := CostSpecs()
	root := evalcompose.DefaultAnthropicModel

	scopes := evalcompose.MeterScopes(specs, "anthropic", root)
	analyst, ok := scopes[costAnalystName]
	if !ok {
		t.Fatalf("MeterScopes gave the analyst no scope; scopes = %v", scopes)
	}
	synth, ok := scopes[evalcompose.SynthesisName]
	if !ok {
		t.Fatalf("MeterScopes gave the synthesizer no scope; scopes = %v", scopes)
	}

	if analyst.RatePer1K <= 0 {
		t.Errorf("the analyst's tier priced at $%.5f/1K; an unpriced scope cannot be checked against anything", analyst.RatePer1K)
	}
	if analyst.RatePer1K >= synth.RatePer1K {
		t.Errorf("the small tier priced at $%.5f/1K and the frontier tier at $%.5f/1K; the check compares two rates and these do not differ in the direction that makes it a check",
			analyst.RatePer1K, synth.RatePer1K)
	}
	if rootRate := evalcompose.RatePer1K("anthropic", root); analyst.RatePer1K == rootRate {
		t.Errorf("the analyst priced at the root's own rate $%.5f/1K, so a run against %s could not tell a working tier from a broken one",
			rootRate, root)
	}
}

// TestCostSpecs_Shape pins the fixture's three load-bearing properties:
// it is the dispatch shape it declares, it reaches nothing outside the
// process, and its two specialists sit on two named tiers.
func TestCostSpecs_Shape(t *testing.T) {
	specs := CostSpecs()
	if len(specs) != 2 {
		t.Fatalf("CostSpecs() returned %d specs, want 2", len(specs))
	}

	// The bundle RunCost builds declares fanout. If the roster's own
	// shape disagreed, the check would be exercising a dispatch nobody
	// runs.
	if got := evalcompose.RosterShape(specs); got != evalcompose.DispatchFanout {
		t.Errorf("RosterShape(CostSpecs()) = %q, want %q — RunCost builds the bundle with fanout",
			got, evalcompose.DispatchFanout)
	}

	tiers := map[string]bool{}
	for _, s := range specs {
		if s.Tier == "" {
			t.Errorf("specialist %q declares no tier, so it prices at the root and measures nothing", s.Name)
		}
		if s.Model != "" {
			t.Errorf("specialist %q declares model %q; this check is about `tier:`, and a concrete id would not exercise the resolution under test",
				s.Name, s.Model)
		}
		tiers[s.Tier] = true

		// A nightly that reaches a cluster is a nightly that fails for
		// reasons that are not about pricing.
		if s.Tools.InheritsAllMCP() {
			t.Errorf("specialist %q inherits every MCP toolset; the cost check must have no live dependency", s.Name)
		}
		if len(s.Tools.MCP) != 0 || len(s.Tools.Builtin) != 0 || len(s.Tools.Skills) != 0 {
			t.Errorf("specialist %q has tools %+v; token spend on tool choice is variance this check does not want", s.Name, s.Tools)
		}
	}
	if len(tiers) != 2 {
		t.Errorf("the roster's specialists sit on %d distinct tier(s), want 2: %v", len(tiers), tiers)
	}
	if !tiers[taskclass.TierSmall] {
		t.Errorf("no specialist on the small tier; the cheap half is the half the check is about")
	}
}

// stubLLM is a model that is never called: the refusals under test all
// happen before the first request.
type stubLLM struct{ name string }

func (s stubLLM) Name() string { return s.name }

func (s stubLLM) GenerateContent(context.Context, *adkmodel.LLMRequest, bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	panic("judge: cost: the stub model was asked to generate; the refusal under test did not happen")
}

func TestRunCost_Refuses(t *testing.T) {
	cases := []struct {
		name    string
		model   adkmodel.LLM
		root    string
		scratch string
		want    string
		// sentinel marks the refusal the harness must be able to tell
		// apart from a failure: this configuration cannot answer the
		// question, which is a stated skip rather than a Problem.
		sentinel bool
	}{
		{
			name:    "no model",
			root:    evalcompose.DefaultAnthropicModel,
			scratch: t.TempDir(),
			want:    "no model",
		},
		{
			name:  "no scratch dir",
			model: stubLLM{name: evalcompose.DefaultAnthropicModel},
			root:  evalcompose.DefaultAnthropicModel,
			want:  "no scratch dir",
		},
		{
			// The one that matters. A green J-cost-tier obtained under
			// echo would be exactly the fiction the check exists to
			// detect, one level up: the fake collapses every tier back
			// to itself, so the two rates it would compare are the same
			// rate and the row would pass by construction.
			name:     "offline fake root",
			model:    stubLLM{name: "echo"},
			root:     "echo",
			scratch:  t.TempDir(),
			want:     `root model "echo" collapses every tier back to itself`,
			sentinel: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			board, err := RunCost(context.Background(), tc.model, tc.root, "anthropic", tc.scratch)
			if err == nil {
				t.Fatalf("RunCost returned a board and no error; want a refusal mentioning %q", tc.want)
			}
			if board != nil {
				t.Errorf("RunCost returned a non-nil board alongside its error; a caller that reads the board on error reads a pass")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("RunCost error = %q, want it to mention %q", err, tc.want)
			}
			if got := errors.Is(err, ErrCostNeedsLiveModel); got != tc.sentinel {
				t.Errorf("errors.Is(err, ErrCostNeedsLiveModel) = %v, want %v — the harness routes on this to tell a skip from a Problem: %v",
					got, tc.sentinel, err)
			}
		})
	}
}

// judgeCost's verdicts, over boards written by hand. Rates here are
// round numbers rather than the real table's: what is under test is the
// arithmetic, and a test that recomputed the table would pass whatever
// the table said.
func TestJudgeCost(t *testing.T) {
	const (
		rootModel = "big-model"
		rootRate  = 0.075
		smallRate = 0.005
	)
	// priced returns a row billed at rate, for tokens tokens.
	priced := func(name, tier, resolved string, tokens int64, want, at float64) ScopeCost {
		return ScopeCost{
			Name: name, Tier: tier, Resolved: resolved, Ran: []string{resolved},
			Calls: 1, Tokens: tokens,
			CostUSD:      float64(tokens) / 1000 * at,
			WantRate:     want,
			GotRate:      at,
			AtParentRate: float64(tokens) / 1000 * rootRate,
		}
	}
	good := func() *CostBoard {
		return &CostBoard{
			RootModel: rootModel, RootRate: rootRate,
			Scopes: []ScopeCost{
				priced("analyst", "small", "small-model", 2000, smallRate, smallRate),
				priced("synthesis", "frontier", rootModel, 1000, rootRate, rootRate),
			},
		}
	}

	cases := []struct {
		name  string
		board func() *CostBoard
		// want is a substring every expected finding must contain, or ""
		// when the board is expected to hold.
		want string
		// note, when set, must appear in the notes.
		note string
	}{
		{
			name:  "correctly priced",
			board: good,
			// The frontier row resolving to the root is a control, not a
			// failure, and the board says so rather than staying quiet.
			note: "control, not a measurement",
		},
		{
			name: "analyst billed at the parent's rate",
			board: func() *CostBoard {
				b := good()
				b.Scopes[0] = priced("analyst", "small", "small-model", 2000, smallRate, rootRate)
				return b
			},
			want: "the parent's (big-model)",
		},
		{
			name: "analyst billed at some third rate",
			board: func() *CostBoard {
				b := good()
				b.Scopes[0] = priced("analyst", "small", "small-model", 2000, smallRate, 0.02)
				return b
			},
			// Not the parent's rate, so the message must not claim it is.
			want: "not its own",
		},
		{
			name: "never ran",
			board: func() *CostBoard {
				b := good()
				b.Scopes[0] = ScopeCost{Name: "analyst", Tier: "small", Resolved: "small-model", WantRate: smallRate}
				return b
			},
			want: "made no model call",
		},
		{
			name: "ran but reported no tokens",
			board: func() *CostBoard {
				b := good()
				b.Scopes[0] = ScopeCost{Name: "analyst", Tier: "small", Resolved: "small-model", Calls: 2, WantRate: smallRate}
				return b
			},
			want: "no tokens",
		},
		{
			name: "resolved to a model with no rate",
			board: func() *CostBoard {
				b := good()
				b.Scopes[0] = priced("analyst", "small", "unpriced-model", 2000, 0, 0.004)
				return b
			},
			want: "no rate for",
		},
		{
			name: "priced as one model, ran as another",
			board: func() *CostBoard {
				b := good()
				s := priced("analyst", "small", "small-model", 2000, smallRate, smallRate)
				s.Ran = []string{"big-model-20260801"}
				b.Scopes[0] = s
				return b
			},
			want: "the provider reported the call ran as",
		},
		{
			name: "every tier collapsed onto the root",
			board: func() *CostBoard {
				b := good()
				b.Scopes[0] = priced("analyst", "small", rootModel, 2000, rootRate, rootRate)
				return b
			},
			want: "no row compares two rates",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := tc.board()
			findings, notes := judgeCost(b)
			joined := strings.Join(findings, "\n")

			if tc.want == "" {
				if len(findings) != 0 {
					t.Errorf("a correctly priced board produced findings:\n%s", joined)
				}
			} else if !strings.Contains(joined, tc.want) {
				t.Errorf("findings did not mention %q; got:\n%s", tc.want, joined)
			}
			if tc.note != "" && !strings.Contains(strings.Join(notes, "\n"), tc.note) {
				t.Errorf("notes did not mention %q; got:\n%s", tc.note, strings.Join(notes, "\n"))
			}

			b.Findings = findings
			if ok := b.OK(); ok != (tc.want == "") {
				t.Errorf("board.OK() = %v with findings:\n%s", ok, joined)
			}
		})
	}
}

// TestScopeCost_PricesTheTokenMixNotABlend drives the real meters with a
// mostly-input call, the shape every J-cost-tier turn has. The meter
// bills a scope at the catalog's exact input and output prices, so a
// row's effective rate sits well under (input+output)/2; the check used
// to compare against that blend and failed about half the nightlies on
// a correctly priced opus row (2026-09-26 → 2026-10-05). The second case
// is the failure the check exists for — the analyst's scope wired to the
// root's model — and must still be named as the parent's. It needs a
// provider that reports no model version: one that does is priced by
// what it reports, whatever the scope says.
func TestScopeCost_PricesTheTokenMixNotABlend(t *testing.T) {
	const provider, root = "anthropic", "claude-opus-5"
	specs := CostSpecs()

	findings := func(t *testing.T, scopes map[string]evalcompose.Limits, reportVersion bool) []string {
		t.Helper()
		meter := evalcompose.NewMeter(evalcompose.BudgetConfig{
			Limits: evalcompose.Limits{RatePer1K: evalcompose.RatePer1K(provider, root)},
			Scopes: scopes,
		})
		ref := newRefMeters(specs, provider, root)
		seen := &modelVersions{}
		for _, s := range specs {
			ev := &session.Event{
				Author: s.Name,
				LLMResponse: adkmodel.LLMResponse{
					UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
						PromptTokenCount:     900,
						CandidatesTokenCount: 100,
						TotalTokenCount:      1000,
					},
				},
			}
			if reportVersion {
				ev.ModelVersion = evalcompose.SpecModelName(s, provider, root)
			}
			seen.record(ev)
			if err := errors.Join(meter.Observe(ev), ref.observe(ev)); err != nil {
				t.Fatal(err)
			}
		}
		b := &CostBoard{RootModel: root, RootRate: evalcompose.RatePer1K(provider, root)}
		for _, s := range specs {
			b.Scopes = append(b.Scopes, scopeCost(s, meter, ref, seen, provider, root))
		}
		f, _ := judgeCost(b)
		return f
	}

	for _, reported := range []bool{true, false} {
		if f := findings(t, evalcompose.MeterScopes(specs, provider, root), reported); len(f) != 0 {
			t.Errorf("version reported=%v: a correctly priced roster reported a mispricing:\n%s",
				reported, strings.Join(f, "\n"))
		}
	}

	scopes := evalcompose.MeterScopes(specs, provider, root)
	scopes[costAnalystName] = evalcompose.ModelLimits(provider, root)
	if f := findings(t, scopes, false); len(f) != 1 || !strings.Contains(f[0], "the parent's") {
		t.Errorf("analyst wired to the root's model: want one finding naming the parent's rate, got %d:\n%s",
			len(f), strings.Join(f, "\n"))
	}
}

// TestJudgeCost_NamesTheParentOnlyWhenItIsTheParent guards the one
// message a reader will act on directly. "Billed at the parent's rate"
// is a specific accusation with a specific fix; saying it about a rate
// that is not the parent's would send somebody looking at the wrong
// code.
func TestJudgeCost_NamesTheParentOnlyWhenItIsTheParent(t *testing.T) {
	b := &CostBoard{
		RootModel: "big-model", RootRate: 0.075,
		Scopes: []ScopeCost{
			{Name: "analyst", Tier: "small", Resolved: "small-model", Ran: []string{"small-model"},
				Calls: 1, Tokens: 1000, CostUSD: 0.02, WantRate: 0.005, GotRate: 0.02},
			{Name: "synthesis", Tier: "frontier", Resolved: "small-model", Ran: []string{"small-model"},
				Calls: 1, Tokens: 1000, CostUSD: 0.005, WantRate: 0.005, GotRate: 0.005},
		},
	}
	findings, _ := judgeCost(b)
	if len(findings) != 1 {
		t.Fatalf("want exactly the one mispricing, got %d:\n%s", len(findings), strings.Join(findings, "\n"))
	}
	if strings.Contains(findings[0], "the parent's") {
		t.Errorf("the finding blamed the parent's rate for a rate that is not the parent's:\n%s", findings[0])
	}
}

func TestRanAs(t *testing.T) {
	cases := []struct {
		name     string
		ran      []string
		resolved string
		want     bool
	}{
		{"exact", []string{"claude-haiku-4-5"}, "claude-haiku-4-5", true},
		// The case this function exists for: providers append a build
		// date, and a check that insisted on the bare id would fail
		// every correct run.
		{"provider appended a build date", []string{"claude-haiku-4-5-20251001"}, "claude-haiku-4-5", true},
		{"reported id is the shorter one", []string{"claude-haiku-4-5"}, "claude-haiku-4-5-20251001", true},
		{"different model", []string{"claude-opus-4-7"}, "claude-haiku-4-5", false},
		{"one of several matches", []string{"claude-opus-4-7", "claude-haiku-4-5-20251001"}, "claude-haiku-4-5", true},
		// Nothing to compare against is not evidence of a mismatch.
		{"nothing resolved", []string{"claude-opus-4-7"}, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ranAs(tc.ran, tc.resolved); got != tc.want {
				t.Errorf("ranAs(%v, %q) = %v, want %v", tc.ran, tc.resolved, got, tc.want)
			}
		})
	}
}

func TestCostSpecsAreBuildable(t *testing.T) {
	// specialists.Build is what compose.BuildRoot calls; a spec that
	// cannot build would turn J-cost-tier into a Problem every night.
	// The tier resolver stands in for compose's, which needs a provider:
	// what is under test here is that the specs are well-formed, not
	// what their tiers resolve to.
	var asked []string
	opts := evalcompose.BuildOptions{
		Model: stubLLM{name: "big-model"},
		ResolveTier: func(tier string) (adkmodel.LLM, error) {
			asked = append(asked, tier)
			return stubLLM{name: "resolved-" + tier}, nil
		},
	}
	for _, s := range CostSpecs() {
		if _, err := evalcompose.Build(s, opts); err != nil {
			t.Errorf("specialist %q does not build: %v", s.Name, err)
		}
	}
	// Both specs must actually route through the tier resolver — a spec
	// that quietly built on the root model would price at the root and
	// the check would compare a rate against itself.
	if len(asked) != 2 {
		t.Errorf("the tier resolver was asked %d time(s) for a two-spec tiered roster: %v", len(asked), asked)
	}
}
