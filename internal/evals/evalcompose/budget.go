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

// Package evalcompose provides the runtime wiring helpers used by the
// judge and outcome eval tiers: model construction, per-specialist
// budget metering, fan-out roster assembly, and the in-turn loop guard.
package evalcompose

import (
	"errors"
	"fmt"
	"sync"

	"google.golang.org/adk/v2/session"
)

// ErrBudgetExceeded is returned by Meter.Observe when a ceiling is crossed.
var ErrBudgetExceeded = errors.New("budget exceeded")

// ErrBudgetRefused is returned by Meter.Allow when a ceiling already makes
// another model call impossible.
var ErrBudgetRefused = errors.New("budget refused the call")

// Call is the token breakdown of one model call, in the three disjoint
// input buckets plus output that a price table needs.
type Call struct {
	UncachedInputTokens int64
	CachedInputTokens   int64
	CacheWriteTokens    int64
	CacheWrite1hTokens  int64
	OutputTokens        int64
}

// Pricer prices one model call against a per-model rate table.
type Pricer interface {
	PriceCall(backend, model string, c Call) (costUSD float64, ok bool)
}

// DetailKey is the LLMResponse.CustomMetadata key under which a
// provider adapter stores a Detailer.
const DetailKey = "mast.usage"

// Detailer is the per-call usage detail a provider adapter can report
// beyond genai.GenerateContentResponseUsageMetadata.
type Detailer interface {
	UsageDetail() (cacheRead, cacheWrite, cacheWrite1h, reasoning *int64, servedModel string)
}

// Limits is one set of budget ceilings and the rate used to compute
// cost against them.
type Limits struct {
	MaxTurns   int
	MaxTokens  int64
	MaxCostUSD float64
	RatePer1K  float64
	Backend    string
	Model      string
	Pricer     Pricer
}

// IsZero reports whether l declares neither a ceiling nor a rate.
func (l Limits) IsZero() bool {
	return l.MaxTurns <= 0 && l.MaxTokens <= 0 && l.MaxCostUSD <= 0 && l.RatePer1K <= 0 && l.Model == "" && l.Pricer == nil
}

// BudgetConfig configures a Meter.
type BudgetConfig struct {
	Limits
	Scopes      map[string]Limits
	FinalReport bool
}

// Meter tracks cumulative session usage against a BudgetConfig and
// implements agent.CallGate and agent.FinalReportGate.
type Meter struct {
	cfg BudgetConfig

	mu               sync.Mutex
	turns            int
	tokens           int64
	costUSD          float64
	unpriced         int
	scopes           map[string]*scopeTally
	tripped          error
	finalReportTaken map[string]bool
}

type scopeTally struct {
	turns    int
	tokens   int64
	costUSD  float64
	unpriced int
}

// ScopeSnapshot is a point-in-time reading of one named scope's tally.
type ScopeSnapshot struct {
	Turns    int
	Tokens   int64
	CostUSD  float64
	Unpriced int
}

// NewMeter constructs a Meter for a single session.
func NewMeter(cfg BudgetConfig) *Meter {
	m := &Meter{cfg: cfg}
	if len(cfg.Scopes) > 0 {
		m.scopes = make(map[string]*scopeTally, len(cfg.Scopes))
		for name := range cfg.Scopes {
			m.scopes[name] = &scopeTally{}
		}
	}
	return m
}

// Allow implements agent.CallGate, checking author's own scope first and
// then the session-wide limits before a model call is issued.
func (m *Meter) Allow(author string) error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	if scopeLim, ok := m.cfg.Scopes[author]; ok {
		st := m.scopes[author]
		if err := checkPrecluded(scopeLim, st.turns, st.tokens, st.costUSD); err != nil {
			return fmt.Errorf("%w: specialist %q: %s", ErrBudgetRefused, author, err)
		}
	}
	if err := checkPrecluded(m.cfg.Limits, m.turns, m.tokens, m.costUSD); err != nil {
		return fmt.Errorf("%w: %s", ErrBudgetRefused, err)
	}
	return nil
}

// AllowFinalReport implements agent.FinalReportGate, granting at most one
// final report call per author when BudgetConfig.FinalReport is enabled
// and the author has already spent at least one call.
func (m *Meter) AllowFinalReport(author string) bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.cfg.FinalReport || m.finalReportTaken[author] {
		return false
	}
	calls := m.turns
	if st, ok := m.scopes[author]; ok && st != nil {
		calls = st.turns
	}
	if calls == 0 {
		return false
	}
	if m.finalReportTaken == nil {
		m.finalReportTaken = make(map[string]bool, 1)
	}
	m.finalReportTaken[author] = true
	return true
}

func checkPrecluded(l Limits, turns int, tokens int64, costUSD float64) error {
	if l.MaxTurns > 0 && turns+1 > l.MaxTurns {
		return fmt.Errorf("would be model call (turn) %d of a cap of %d", turns+1, l.MaxTurns)
	}
	if l.MaxTokens > 0 && tokens >= l.MaxTokens {
		return fmt.Errorf("%d tokens already at cap %d; any further call exceeds it", tokens, l.MaxTokens)
	}
	if l.MaxCostUSD > 0 && costUSD >= l.MaxCostUSD {
		return fmt.Errorf("$%.4f already at cap $%.4f (%d tokens over %d calls); any further call exceeds it", costUSD, l.MaxCostUSD, tokens, turns)
	}
	return nil
}

// Observe inspects a session event and updates the running tallies if
// it carries UsageMetadata.
func (m *Meter) Observe(ev *session.Event) error {
	if ev == nil || ev.UsageMetadata == nil {
		return nil
	}
	u := ev.UsageMetadata
	delta := int64(u.TotalTokenCount)
	if delta == 0 {
		delta = int64(u.PromptTokenCount) + int64(u.CandidatesTokenCount)
	}
	call, reportedModel := extractCall(ev)

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.tripped != nil {
		return m.tripped
	}

	m.turns++
	m.tokens += delta

	scopeLim, hasScope := m.cfg.Scopes[ev.Author]
	var st *scopeTally
	if hasScope {
		st = m.scopes[ev.Author]
		st.turns++
		st.tokens += delta
	}

	priceFrom := m.cfg.Limits
	if hasScope && (scopeLim.RatePer1K > 0 || scopeLim.Pricer != nil) {
		priceFrom = scopeLim
	}
	cost, priced := priceEvent(priceFrom, reportedModel, delta, call)
	m.costUSD += cost
	if !priced {
		m.unpriced++
	}
	if st != nil {
		st.costUSD += cost
		if !priced {
			st.unpriced++
		}
		if err := m.checkScopeLimits(ev.Author, scopeLim, st); err != nil {
			m.tripped = err
			return m.tripped
		}
	}
	if err := m.checkSessionLimits(); err != nil {
		m.tripped = err
		return m.tripped
	}
	return nil
}

func (m *Meter) checkScopeLimits(author string, scopeLim Limits, st *scopeTally) error {
	if scopeLim.MaxTurns > 0 && st.turns > scopeLim.MaxTurns {
		return fmt.Errorf("%w: specialist %q exceeded max_turns (%d > %d)", ErrBudgetExceeded, author, st.turns, scopeLim.MaxTurns)
	}
	if scopeLim.MaxTokens > 0 && st.tokens > scopeLim.MaxTokens {
		return fmt.Errorf("%w: specialist %q exceeded max_tokens (%d > %d)", ErrBudgetExceeded, author, st.tokens, scopeLim.MaxTokens)
	}
	if scopeLim.MaxCostUSD > 0 && st.costUSD > scopeLim.MaxCostUSD {
		return fmt.Errorf("%w: specialist %q exceeded max_cost_usd ($%.4f > $%.4f)", ErrBudgetExceeded, author, st.costUSD, scopeLim.MaxCostUSD)
	}
	return nil
}

func (m *Meter) checkSessionLimits() error {
	if m.cfg.MaxTurns > 0 && m.turns > m.cfg.MaxTurns {
		return fmt.Errorf("%w: exceeded max_turns (%d > %d)", ErrBudgetExceeded, m.turns, m.cfg.MaxTurns)
	}
	if m.cfg.MaxTokens > 0 && m.tokens > m.cfg.MaxTokens {
		return fmt.Errorf("%w: exceeded max_tokens (%d > %d)", ErrBudgetExceeded, m.tokens, m.cfg.MaxTokens)
	}
	if m.cfg.MaxCostUSD > 0 && m.costUSD > m.cfg.MaxCostUSD {
		return fmt.Errorf("%w: exceeded max_cost_usd ($%.4f > $%.4f)", ErrBudgetExceeded, m.costUSD, m.cfg.MaxCostUSD)
	}
	return nil
}

// Snapshot returns the current session totals without mutating state.
func (m *Meter) Snapshot() (turns int, tokens int64, costUSD float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.turns, m.tokens, m.costUSD
}

// ScopeSnapshot returns the current tally for the named scope and true,
// or zero values and false if name was not declared in BudgetConfig.Scopes.
func (m *Meter) ScopeSnapshot(name string) (tokens int64, costUSD float64, turns int, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st, ok := m.scopes[name]
	if !ok {
		return 0, 0, 0, false
	}
	return st.tokens, st.costUSD, st.turns, true
}

func priceEvent(lim Limits, reportedModel string, delta int64, call Call) (cost float64, priced bool) {
	if lim.Pricer != nil {
		model := reportedModel
		if model == "" {
			model = lim.Model
		}
		if model != "" {
			if c, ok := lim.Pricer.PriceCall(lim.Backend, model, call); ok {
				return c, true
			}
		}
	}
	if lim.RatePer1K > 0 {
		return float64(delta) / 1000.0 * lim.RatePer1K, true
	}
	return 0, false
}

func extractCall(ev *session.Event) (Call, string) {
	u := ev.UsageMetadata
	prompt := max(int64(u.PromptTokenCount), 0)
	cached := clamp(int64(u.CachedContentTokenCount), prompt)
	out := max(int64(u.CandidatesTokenCount), 0) + max(int64(u.ThoughtsTokenCount), 0)

	var write, write1h int64
	var served string
	if d, ok := ev.CustomMetadata[DetailKey].(Detailer); ok && d != nil {
		var cr, cw, cw1h *int64
		cr, cw, cw1h, _, served = d.UsageDetail()
		if cr != nil && cached == 0 {
			cached = clamp(*cr, prompt)
		}
		if cw != nil {
			write = clamp(*cw, prompt-cached)
		}
		if cw1h != nil {
			write1h = clamp(*cw1h, write)
		}
	}
	reported := ev.ModelVersion
	if reported == "" {
		reported = served
	}
	return Call{
		UncachedInputTokens: prompt - cached - write,
		CachedInputTokens:   cached,
		CacheWriteTokens:    write,
		CacheWrite1hTokens:  write1h,
		OutputTokens:        out,
	}, reported
}

func clamp(v, ceiling int64) int64 {
	if v < 0 {
		return 0
	}
	if v > ceiling {
		return ceiling
	}
	return v
}
