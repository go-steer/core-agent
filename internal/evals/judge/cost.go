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
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/glebarez/sqlite"
	"google.golang.org/genai"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	adkagent "google.golang.org/adk/v2/agent"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/session/database"

	"github.com/go-steer/core-agent/v3/internal/evals/evalcompose"
	"github.com/go-steer/core-agent/v3/pkg/taskclass"
)

// J-cost-tier: does a tiered roster's cheap analyst actually get billed
// at the cheap rate?
//
// This measurement cannot be made anywhere but here, and that is the
// whole reason it exists as a judge-tier check rather than a Go test.
// W1.2 finding (b): an offline-fake root collapses every `model:` and
// `tier:` back to itself, so under `echo` the tiers this asks about do
// not exist — the E tier can assert the wiring and never the price. Unit
// tests cover the arithmetic (TestMeterScopes_OverridePricesAtItsOwnTier,
// TestMeterScopes_TierPricesAtItsResolvedModel); what neither can cover
// is a real provider answering with real usage numbers on a roster whose
// two halves ran on two different models.
//
// So: one incident, two specialists, two tiers, live. Then read the
// meter's per-scope snapshot and ask whether the analyst's dollars are
// its own.
const (
	// costAnalystName is the cheap branch. Named for what it is in the
	// board, not for a cluster concept — this roster diagnoses nothing.
	costAnalystName = "analyst"

	// costSessionID keeps the check's session out of the corpus's
	// namespace; the scratch dir is shared.
	costSessionID = "j-cost-tier"

	// costAlert is deliberately trivial. This check measures pricing,
	// not diagnosis: every token spent making the model think harder is
	// a token spent on a question the other 31 scenarios already ask,
	// and a nightly that costs more than it must is a nightly somebody
	// eventually turns off.
	costAlert = "Alert: pod api-7d9f in namespace shop is OOMKilled, 5 restarts in 10 minutes."
)

// costRateTolerance is how far, as a fraction, a row's effective rate
// may sit from the rate its own model charges for the same tokens
// before the check calls it a mismatch.
//
// Relative, and measured against the exact price of the row's own token
// mix rather than a flat blend. It was once an absolute 0.005/1K against
// compose.RatePer1K's (input+output)/2, which was exact while the meter
// billed scopes at that flat rate. Since #339 a scope is billed at the
// catalog's exact input and output prices, so its effective rate moves
// with how much of a call was output: a ~1K-token, mostly-input opus
// call lands near $0.009–0.011/1K against a $0.015 blend, straddling
// the old tolerance, and the nightly went red on about half its nights
// with nothing mispriced.
//
// Not zero, because both sides are float sums over several calls. Small
// enough that the failure this exists to catch — an analyst billed at
// the parent's rate — is far outside it: haiku and opus differ by more
// than 4x on every bucket.
const costRateTolerance = 0.01

// ErrCostNeedsLiveModel is returned when the root is one of mast's
// offline fakes.
//
// A sentinel because the caller must treat it differently from every
// other failure: it means the check was asked a question that has no
// answer in this configuration, not that mast priced anything wrongly.
// The harness turns it into a stated skip; a real error stays a Problem.
var ErrCostNeedsLiveModel = errors.New("judge: cost: J-cost-tier needs a live model")

// ScopeCost is one specialist's metered spend, with everything a reader
// needs to tell "priced correctly" from "priced plausibly".
type ScopeCost struct {
	Name string `json:"name"`
	Tier string `json:"tier"`
	// Resolved is the model id the tier became for the provider this
	// run used. What a tier costs is a fact about the run, not about
	// the roster, so the board carries it.
	Resolved string `json:"resolved_model"`
	// Ran is the distinct model versions the provider reported on this
	// specialist's events. Resolution says which client was built; this
	// says which model answered — and only the second one is evidence.
	Ran []string `json:"ran_as,omitempty"`

	Calls   int     `json:"calls"`
	Tokens  int64   `json:"tokens"`
	CostUSD float64 `json:"cost_usd"`

	// WantRate is what Resolved charges for this row's own token mix,
	// per 1K: the same events priced by a reference meter that knows
	// only that model (compose.ModelLimits). GotRate is
	// CostUSD/Tokens*1000, read back off what the meter actually accrued.
	WantRate float64 `json:"want_rate_per_1k"`
	GotRate  float64 `json:"got_rate_per_1k"`

	// AtParentRate is the counterfactual: what these same tokens would
	// have cost billed at the root model's prices, in USD. This is the number the
	// row is about — "the analyst's tokens are not billed at the
	// synthesizer's" is only meaningful next to what that would have
	// been.
	AtParentRate float64 `json:"at_parent_rate_usd"`
}

// CostBoard is the J-cost-tier result.
type CostBoard struct {
	Provider  string `json:"provider"`
	RootModel string `json:"root_model"`
	// RootRate is the parent's rate, the thing a tiered specialist must
	// not be billed at.
	RootRate float64 `json:"root_rate_per_1k"`

	Scopes []ScopeCost `json:"scopes"`

	// Findings are the reasons this check did not hold. Empty means the
	// tiered roster priced itself correctly.
	//
	// A finding here is not "the model had a bad day" — the distinction
	// the judge tier draws everywhere else. A tier that resolved and
	// then billed at the wrong rate is a mast bug with a live
	// reproduction, so the harness raises these as Problems while
	// leaving corpus scores as report-only.
	Findings []string `json:"findings,omitempty"`

	// Notes are true things that are not failures — most often that a
	// tier resolved to the root model, which makes that one scope's
	// comparison vacuous rather than wrong.
	Notes []string `json:"notes,omitempty"`
}

// OK reports whether the tiered roster priced itself correctly.
func (b *CostBoard) OK() bool { return b != nil && len(b.Findings) == 0 }

// CostSpecs is the roster J-cost-tier runs: one small-tier analyst under
// one frontier-tier synthesis.
//
// Written in Go rather than loaded from examples/ on purpose. The
// shipped tiered bundle (ns-audit) needs the GKE MCP server and four
// branches to say what two say here, and a check that drags a live
// dependency into a nightly is a check that starts failing for reasons
// that are not about pricing.
//
// It is still the real path: these Specs go through compose.BuildRoot
// and compose.MeterScopes exactly as a loaded roster does, so the tier
// resolution and the scope rates under test are the shipped ones.
func CostSpecs() []evalcompose.Spec {
	return []evalcompose.Spec{
		{
			Name:        costAnalystName,
			Description: "reads one alert and reports what it says",
			Mode:        evalcompose.ModeTask,
			Tier:        taskclass.TierSmall,
			Instruction: "You are given one alert line. Restate what it says in one sentence — " +
				"which object, which symptom — and nothing else. Do not speculate about causes. " +
				"Report by calling finish_task.",
			// No tools at all: an analyst with a tool belt spends its
			// tokens choosing tools, which is measured elsewhere and only
			// adds variance here.
			Tools: evalcompose.ToolAllowlist{MCP: []evalcompose.MCPAllowlist{}},
		},
		{
			Name:        evalcompose.SynthesisName,
			Description: "merges the analyst's finding into one line for the operator",
			Mode:        evalcompose.ModeTask,
			Tier:        taskclass.TierFrontier,
			Instruction: "You are given one analyst's finding. Return it as a single " +
				"severity-prefixed line: \"CRITICAL: <what is wrong>. Recommended action: <what to do>.\" " +
				"Report by calling finish_task.",
			Tools: evalcompose.ToolAllowlist{MCP: []evalcompose.MCPAllowlist{}},
		},
	}
}

// RunCost runs the tiered roster against the live root model and reads
// the meter back.
//
// root/rootName/provider are the harness's model under test and the
// strings it was built from — the same three values cmd/mast hands
// BuildRoot, so the tiers resolve here the way they resolve in the
// daemon.
func RunCost(ctx context.Context, root adkmodel.LLM, rootName, provider, scratch string) (*CostBoard, error) {
	if root == nil {
		return nil, fmt.Errorf("judge: cost: no model")
	}
	if scratch == "" {
		return nil, fmt.Errorf("judge: cost: no scratch dir")
	}
	if evalcompose.IsOfflineFake(rootName) {
		// Refused rather than answered: a green J-cost-tier row obtained
		// under `echo` would be the exact fiction this check exists to
		// detect, one level up. The caller is expected to tell this
		// refusal apart from a real failure (hence the sentinel) and to
		// print the skip rather than swallow it — a check that quietly
		// does not run is indistinguishable from one that passed.
		return nil, fmt.Errorf("%w: root model %q collapses every tier back to itself, so there is nothing to price",
			ErrCostNeedsLiveModel, rootName)
	}

	specs := CostSpecs()
	board := &CostBoard{
		Provider:  providerOrAuto(provider),
		RootModel: rootName,
		RootRate:  evalcompose.RatePer1K(provider, rootName),
	}

	svc, err := database.NewSessionService(
		sqlite.Open(filepath.Join(scratch, "j-cost-tier.db")),
		&gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)},
	)
	if err != nil {
		return nil, fmt.Errorf("judge: cost: open session store: %w", err)
	}
	if err := database.AutoMigrate(svc); err != nil {
		return nil, fmt.Errorf("judge: cost: migrate session store: %w", err)
	}

	agent, _, err := evalcompose.BuildRoot(ctx, evalcompose.RootConfig{
		Bundle: evalcompose.Bundle{
			Name:        "j-cost-tier",
			Description: "two specialists on two tiers, to price them",
			Specialists: []string{costAnalystName, evalcompose.SynthesisName},
			Dispatch:    string(evalcompose.DispatchFanout),
		},
		Specs:     specs,
		Model:     root,
		ModelName: rootName,
		Provider:  provider,
	})
	if err != nil {
		return nil, fmt.Errorf("judge: cost: compose root: %w", err)
	}

	run, err := runner.New(runner.Config{
		AppName:           appName,
		Agent:             agent,
		SessionService:    svc,
		AutoCreateSession: true,
	})
	if err != nil {
		return nil, fmt.Errorf("judge: cost: construct runner: %w", err)
	}

	// The meter is built from the same two inputs the daemon builds it
	// from — the root's flat rate as the session default, and
	// compose.MeterScopes for the roster — because a hand-written scope
	// table would make this check evidence about the fixture.
	meter := evalcompose.NewMeter(evalcompose.BudgetConfig{
		Limits: evalcompose.Limits{RatePer1K: evalcompose.RatePer1K(provider, rootName)},
		Scopes: evalcompose.MeterScopes(specs, provider, rootName),
	})
	ref := newRefMeters(specs, provider, rootName)
	seen := &modelVersions{}

	msg := genai.NewContentFromText(costAlert, genai.RoleUser)
	for ev, rerr := range run.Run(ctx, userID, costSessionID, msg, adkagent.RunConfig{
		StreamingMode: adkagent.StreamingModeNone,
	}) {
		if rerr != nil {
			return nil, fmt.Errorf("judge: cost: run: %w", rerr)
		}
		seen.record(ev)
		if berr := errors.Join(meter.Observe(ev), ref.observe(ev)); berr != nil {
			// No ceilings are configured, so this cannot be a budget
			// stop; if it ever is, the run is not measuring what it says.
			return nil, fmt.Errorf("judge: cost: meter: %w", berr)
		}
	}

	for _, s := range specs {
		board.Scopes = append(board.Scopes, scopeCost(s, meter, ref, seen, provider, rootName))
	}
	board.Findings, board.Notes = judgeCost(board)
	return board, nil
}

// refMeters are J-cost-tier's expected side: the run's events priced
// with each scope at its own resolved model, and at the root's.
type refMeters struct{ own, atRoot *evalcompose.Meter }

// newRefMeters builds the expected side from the resolved names rather
// than through compose.MeterScopes, on purpose: the meter built from
// MeterScopes is what is under test, and only these say what it should
// have charged for the token mix the run actually produced.
func newRefMeters(specs []evalcompose.Spec, provider, rootName string) refMeters {
	own, atRoot := map[string]evalcompose.Limits{}, map[string]evalcompose.Limits{}
	for _, s := range specs {
		own[s.Name] = evalcompose.ModelLimits(provider, evalcompose.SpecModelName(s, provider, rootName))
		atRoot[s.Name] = evalcompose.ModelLimits(provider, rootName)
	}
	return refMeters{
		own:    evalcompose.NewMeter(evalcompose.BudgetConfig{Scopes: own}),
		atRoot: evalcompose.NewMeter(evalcompose.BudgetConfig{Scopes: atRoot}),
	}
}

// observe feeds one event to both reference meters with the reported
// model version stripped. The meter prices a call by the version the
// provider reports before the scope's configured model, which is right
// for billing and wrong for a reference: left in, all three meters
// would price the same id and agree by construction. Stripped, these
// price strictly at the resolved and root names, and whether the
// version that ran is the one that resolved is ranAs's question.
func (r refMeters) observe(ev *session.Event) error {
	bare := *ev
	bare.ModelVersion = ""
	return errors.Join(r.own.Observe(&bare), r.atRoot.Observe(&bare))
}

// scopeCost reads one specialist's meter snapshot into a board row.
func scopeCost(s evalcompose.Spec, m *evalcompose.Meter, ref refMeters, seen *modelVersions, provider, rootName string) ScopeCost {
	resolved := evalcompose.SpecModelName(s, provider, rootName)
	row := ScopeCost{
		Name:     s.Name,
		Tier:     s.Tier,
		Resolved: resolved,
		Ran:      seen.forAuthor(s.Name),
		WantRate: evalcompose.RatePer1K(provider, resolved),
	}
	tokens, cost, calls, ok := m.ScopeSnapshot(s.Name)
	if !ok {
		return row
	}
	row.Calls, row.Tokens, row.CostUSD = calls, tokens, cost
	if tokens > 0 {
		_, want, _, _ := ref.own.ScopeSnapshot(s.Name)
		_, atRoot, _, _ := ref.atRoot.ScopeSnapshot(s.Name)
		row.WantRate = want / float64(tokens) * 1000
		row.GotRate = cost / float64(tokens) * 1000
		row.AtParentRate = atRoot
	}
	return row
}

// judgeCost turns the metered rows into findings and notes.
//
// Split from RunCost so the verdicts are testable without a provider:
// everything above this line needs credentials, everything in it is
// arithmetic over numbers a test can write down.
func judgeCost(b *CostBoard) (findings, notes []string) {
	for _, s := range b.Scopes {
		switch {
		case s.Calls == 0:
			// Nothing ran, so nothing was priced. This is a finding and
			// not a note: a board that says "the analyst was billed
			// correctly" on the strength of zero calls is worse than one
			// that says nothing.
			findings = append(findings, fmt.Sprintf(
				"%s made no model call, so its rate was never exercised — the roster ran, but this row measures nothing", s.Name))
			continue
		case s.Tokens == 0:
			findings = append(findings, fmt.Sprintf(
				"%s made %d model call(s) but reported no tokens, so there is nothing to price", s.Name, s.Calls))
			continue
		}

		if s.Resolved == b.RootModel {
			notes = append(notes, fmt.Sprintf(
				"%s's tier %q resolved to %s, which is also the root model — its rate cannot disagree with the parent's, so this row is a control, not a measurement",
				s.Name, s.Tier, s.Resolved))
		}
		if s.WantRate <= 0 {
			findings = append(findings, fmt.Sprintf(
				"%s resolved to %s, which internal/pricing has no rate for — a priced run cannot be checked against an unpriced model", s.Name, s.Resolved))
			continue
		}
		if !withinRate(s.GotRate, s.WantRate) {
			at := "its own"
			if parent := s.AtParentRate / float64(s.Tokens) * 1000; s.Resolved != b.RootModel && withinRate(s.GotRate, parent) {
				// The specific failure the row exists to catch, named as
				// such rather than left for the reader to spot.
				at = fmt.Sprintf("the parent's (%s)", b.RootModel)
			}
			findings = append(findings, fmt.Sprintf(
				"%s ran on %s but was billed at $%.5f/1K, not %s $%.5f/1K — %d tokens cost $%.5f where they should have cost $%.5f",
				s.Name, s.Resolved, s.GotRate, at, s.WantRate,
				s.Tokens, s.CostUSD, float64(s.Tokens)/1000*s.WantRate))
		}
		if len(s.Ran) > 0 && !ranAs(s.Ran, s.Resolved) {
			// Resolution and execution are two claims, and only the
			// second one is what the operator pays for. A tier that
			// resolves to haiku and runs on opus would price correctly
			// and cost wrongly.
			findings = append(findings, fmt.Sprintf(
				"%s's tier resolved to %s but the provider reported the call ran as %s",
				s.Name, s.Resolved, strings.Join(s.Ran, ", ")))
		}
	}

	// A roster where every tier collapsed onto the root is well-priced
	// and proves nothing. Say so, once, rather than per row.
	tiered := false
	for _, s := range b.Scopes {
		if s.Resolved != "" && s.Resolved != b.RootModel {
			tiered = true
		}
	}
	if !tiered && len(b.Scopes) > 0 {
		findings = append(findings, fmt.Sprintf(
			"every tier in the roster resolved to the root model %s, so no row compares two rates; run with a root model that is not the frontier tier",
			b.RootModel))
	}
	return findings, notes
}

// withinRate reports whether got is within costRateTolerance of want,
// as a fraction of want.
func withinRate(got, want float64) bool {
	return math.Abs(got-want) <= costRateTolerance*want
}

// ranAs reports whether any reported model version names the resolved
// id. Prefix, not equality: providers append a build date
// (claude-haiku-4-5 → claude-haiku-4-5-20251001) and a check that
// insisted on the bare id would fail on a correct run.
func ranAs(ran []string, resolved string) bool {
	if resolved == "" {
		return true
	}
	for _, v := range ran {
		if strings.HasPrefix(v, resolved) || strings.HasPrefix(resolved, v) {
			return true
		}
	}
	return false
}

// modelVersions collects the model versions a provider reported, per
// event author. Concurrent because fan-out branches stream at once.
type modelVersions struct {
	mu sync.Mutex
	in map[string]map[string]bool
}

func (m *modelVersions) record(ev *session.Event) {
	if ev == nil || ev.ModelVersion == "" || ev.Author == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.in == nil {
		m.in = map[string]map[string]bool{}
	}
	if m.in[ev.Author] == nil {
		m.in[ev.Author] = map[string]bool{}
	}
	m.in[ev.Author][ev.ModelVersion] = true
}

func (m *modelVersions) forAuthor(name string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.in[name]))
	for v := range m.in[name] {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func providerOrAuto(p string) string {
	if p == "" {
		return "auto (from the environment)"
	}
	return p
}
