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

// Package costbench implements the multi-turn cost-per-passing-task benchmark
// across the four canonical cost-stack configurations (#1378):
//
//  1. single_uncached — single reasoning model, prompt caching off
//  2. single_cached   — single reasoning model, prompt caching on
//  3. split_uncached  — frontier parent + small-tier subtasks, prompt caching off
//  4. split_cached    — frontier parent + small-tier subtasks, prompt caching on
//
// Every model call is priced through pkg/usage.PriceOnCatalog (backed by
// pkg/pricing.Catalog) so the benchmark uses the exact same layered rate table
// and cache-read / cache-write TTL arithmetic as the production runtime.
//
// The primary headline metric is CostPerPassingTaskUSD = TotalCostUSD / TasksPassed:
// failed tasks still contribute their token spend to TotalCostUSD without
// incrementing TasksPassed, so a configuration that cuts token spend by
// degrading task completion pays for those failures in the headline number.
package costbench

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/go-steer/core-agent/v3/pkg/pricing"
	"github.com/go-steer/core-agent/v3/pkg/usage"
)

// ConfigID names one of the four benchmark configurations.
type ConfigID string

const (
	// ConfigSingleUncached runs every turn on the frontier reasoning model with
	// prompt caching disabled.
	ConfigSingleUncached ConfigID = "single_uncached"
	// ConfigSingleCached runs every turn on the frontier reasoning model with
	// prompt caching enabled.
	ConfigSingleCached ConfigID = "single_cached"
	// ConfigSplitUncached routes bulk tool reads and mechanical subtasks to the
	// small-tier model while keeping prompt caching disabled.
	ConfigSplitUncached ConfigID = "split_uncached"
	// ConfigSplitCached combines the frontier+small model split with prompt
	// caching on the parent session.
	ConfigSplitCached ConfigID = "split_cached"
)

// AllConfigs returns the four benchmark configurations in canonical comparison order.
func AllConfigs() []ConfigID {
	return []ConfigID{
		ConfigSingleUncached,
		ConfigSingleCached,
		ConfigSplitUncached,
		ConfigSplitCached,
	}
}

// CallRecord captures one LLM call inside a multi-turn task execution.
type CallRecord struct {
	Backend                    string `json:"backend,omitempty"`
	Model                      string `json:"model"`
	Role                       string `json:"role"` // "parent" or "subtask"
	InputTokens                int    `json:"input_tokens"`
	CachedInputTokens          int    `json:"cached_input_tokens,omitempty"`
	CacheCreationInputTokens   int    `json:"cache_creation_input_tokens,omitempty"`
	CacheCreation1hInputTokens int    `json:"cache_creation_1h_input_tokens,omitempty"`
	OutputTokens               int    `json:"output_tokens"`
	ThoughtsTokens             int    `json:"thoughts_tokens,omitempty"`
}

// EffectiveUsage returns the TurnUsage for c under cfg. For uncached
// configurations (*_uncached), cache read and write counters are zeroed so the
// entire prompt bills at the uncached base input rate.
func (c CallRecord) EffectiveUsage(cfg ConfigID) usage.TurnUsage {
	u := usage.TurnUsage{
		InputTokens:                c.InputTokens,
		CachedInputTokens:          c.CachedInputTokens,
		CacheCreationInputTokens:   c.CacheCreationInputTokens,
		CacheCreation1hInputTokens: c.CacheCreation1hInputTokens,
		OutputTokens:               c.OutputTokens,
		ThoughtsTokens:             c.ThoughtsTokens,
	}
	if cfg == ConfigSingleUncached || cfg == ConfigSplitUncached {
		u.CachedInputTokens = 0
		u.CacheCreationInputTokens = 0
		u.CacheCreation1hInputTokens = 0
	}
	return u.Clamped()
}

// TaskSpec defines a benchmark task and its deterministic completion oracle.
type TaskSpec struct {
	ID                string   `json:"id"`
	Title             string   `json:"title"`
	RequiredFragments []string `json:"required_fragments"`
}

// Grade reports whether output satisfies every required fragment in the task's
// completion oracle.
func (t TaskSpec) Grade(output string) bool {
	if len(t.RequiredFragments) == 0 {
		return strings.TrimSpace(output) != ""
	}
	lower := strings.ToLower(output)
	for _, frag := range t.RequiredFragments {
		if !strings.Contains(lower, strings.ToLower(frag)) {
			return false
		}
	}
	return true
}

// TaskTrace is the recorded or live execution of one task under one configuration.
type TaskTrace struct {
	TaskID string       `json:"task_id"`
	Config ConfigID     `json:"config"`
	Output string       `json:"output"`
	Calls  []CallRecord `json:"calls"`
}

// TaskExecutor executes (or replays) a single task under cfg.
type TaskExecutor func(ctx context.Context, task TaskSpec, cfg ConfigID) (TaskTrace, error)

// TaskResult holds the priced and graded outcome of one task under one configuration.
type TaskResult struct {
	TaskID                   string   `json:"task_id"`
	Config                   ConfigID `json:"config"`
	Passed                   bool     `json:"passed"`
	Turns                    int      `json:"turns"`
	ParentTurns              int      `json:"parent_turns"`
	SubtaskTurns             int      `json:"subtask_turns"`
	InputTokens              int      `json:"input_tokens"`
	UncachedInputTokens      int      `json:"uncached_input_tokens"`
	CachedInputTokens        int      `json:"cached_input_tokens"`
	CacheCreationInputTokens int      `json:"cache_creation_input_tokens"`
	OutputTokens             int      `json:"output_tokens"`
	ThoughtsTokens           int      `json:"thoughts_tokens"`
	CostUSD                  float64  `json:"cost_usd"`
	UnpricedCalls            int      `json:"unpriced_calls,omitempty"`
}

// ConfigSummary aggregates task results for one configuration across the task suite.
type ConfigSummary struct {
	Config                      ConfigID `json:"config"`
	TasksTotal                  int      `json:"tasks_total"`
	TasksPassed                 int      `json:"tasks_passed"`
	PassRate                    float64  `json:"pass_rate"`
	TurnsTotal                  int      `json:"turns_total"`
	ParentTurns                 int      `json:"parent_turns"`
	SubtaskTurns                int      `json:"subtask_turns"`
	InputTokens                 int      `json:"input_tokens"`
	UncachedInputTokens         int      `json:"uncached_input_tokens"`
	CachedInputTokens           int      `json:"cached_input_tokens"`
	CacheCreationInputTokens    int      `json:"cache_creation_input_tokens"`
	OutputTokens                int      `json:"output_tokens"`
	ThoughtsTokens              int      `json:"thoughts_tokens"`
	TotalCostUSD                float64  `json:"total_cost_usd"`
	CostPerTaskUSD              float64  `json:"cost_per_task_usd"`
	CostPerPassingTaskUSD       float64  `json:"cost_per_passing_task_usd"`
	SavingsMultiplierVsBaseline float64  `json:"savings_multiplier_vs_baseline"`
	UnpricedCalls               int      `json:"unpriced_calls,omitempty"`
}

// MarshalJSON encodes ConfigSummary while replacing a non-finite
// CostPerPassingTaskUSD (+Inf when TasksPassed == 0) with null so
// encoding/json never fails with "unsupported value: +Inf".
func (s ConfigSummary) MarshalJSON() ([]byte, error) {
	type Alias ConfigSummary
	var costPerPassing *float64
	if !math.IsInf(s.CostPerPassingTaskUSD, 0) && !math.IsNaN(s.CostPerPassingTaskUSD) {
		v := s.CostPerPassingTaskUSD
		costPerPassing = &v
	}
	aux := struct {
		Alias
		CostPerPassingTaskUSD *float64 `json:"cost_per_passing_task_usd"`
	}{
		Alias:                 Alias(s),
		CostPerPassingTaskUSD: costPerPassing,
	}
	return json.Marshal(aux)
}

// UnmarshalJSON decodes ConfigSummary and restores +Inf when
// cost_per_passing_task_usd is null and TasksPassed == 0.
func (s *ConfigSummary) UnmarshalJSON(data []byte) error {
	type Alias ConfigSummary
	aux := struct {
		*Alias
		CostPerPassingTaskUSD *float64 `json:"cost_per_passing_task_usd"`
	}{
		Alias: (*Alias)(s),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if aux.CostPerPassingTaskUSD != nil {
		s.CostPerPassingTaskUSD = *aux.CostPerPassingTaskUSD
	} else if s.TasksPassed == 0 {
		s.CostPerPassingTaskUSD = math.Inf(1)
	}
	return nil
}

// Report is the complete output of a four-configuration benchmark run.
type Report struct {
	SuiteName   string                     `json:"suite_name"`
	ConfigOrder []ConfigID                 `json:"config_order,omitempty"`
	Catalog     *pricing.Catalog           `json:"-"`
	Configs     map[ConfigID]ConfigSummary `json:"configs"`
	TaskResults map[ConfigID][]TaskResult  `json:"task_results"`
}

// SuiteOptions configures RunSuite.
type SuiteOptions struct {
	SuiteName string
	Tasks     []TaskSpec
	Configs   []ConfigID
	Catalog   *pricing.Catalog
}

// RunSuite executes opts.Tasks across opts.Configs (defaulting to AllConfigs),
// prices every model call against opts.Catalog (defaulting to the builtin
// pkg/pricing catalog), grades each task output, and computes
// CostPerPassingTaskUSD and relative savings vs ConfigSingleUncached.
func RunSuite(ctx context.Context, opts SuiteOptions, exec TaskExecutor) (*Report, error) {
	if exec == nil {
		return nil, fmt.Errorf("costbench: TaskExecutor must not be nil")
	}
	if len(opts.Tasks) == 0 {
		return nil, fmt.Errorf("costbench: Tasks must not be empty")
	}
	configs := opts.Configs
	if len(configs) == 0 {
		configs = AllConfigs()
	}
	cat := opts.Catalog
	if cat == nil {
		var err error
		cat, err = pricing.NewCatalog(pricing.Options{})
		if err != nil {
			return nil, fmt.Errorf("costbench: init pricing catalog: %w", err)
		}
	}
	suiteName := opts.SuiteName
	if suiteName == "" {
		suiteName = "default"
	}

	report := &Report{
		SuiteName:   suiteName,
		ConfigOrder: append([]ConfigID(nil), configs...),
		Catalog:     cat,
		Configs:     make(map[ConfigID]ConfigSummary, len(configs)),
		TaskResults: make(map[ConfigID][]TaskResult, len(configs)),
	}

	for _, cfg := range configs {
		summary := ConfigSummary{
			Config:     cfg,
			TasksTotal: len(opts.Tasks),
		}
		results := make([]TaskResult, 0, len(opts.Tasks))

		for _, task := range opts.Tasks {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			trace, err := exec(ctx, task, cfg)
			if err != nil {
				return nil, fmt.Errorf("costbench: task %q under %q: %w", task.ID, cfg, err)
			}
			tr := priceAndGradeTask(cat, task, cfg, trace)
			results = append(results, tr)

			if tr.Passed {
				summary.TasksPassed++
			}
			summary.TurnsTotal += tr.Turns
			summary.ParentTurns += tr.ParentTurns
			summary.SubtaskTurns += tr.SubtaskTurns
			summary.InputTokens += tr.InputTokens
			summary.UncachedInputTokens += tr.UncachedInputTokens
			summary.CachedInputTokens += tr.CachedInputTokens
			summary.CacheCreationInputTokens += tr.CacheCreationInputTokens
			summary.OutputTokens += tr.OutputTokens
			summary.ThoughtsTokens += tr.ThoughtsTokens
			summary.TotalCostUSD += tr.CostUSD
			summary.UnpricedCalls += tr.UnpricedCalls
		}

		if summary.TasksTotal > 0 {
			summary.PassRate = float64(summary.TasksPassed) / float64(summary.TasksTotal)
			summary.CostPerTaskUSD = summary.TotalCostUSD / float64(summary.TasksTotal)
		}
		if summary.TasksPassed > 0 {
			summary.CostPerPassingTaskUSD = summary.TotalCostUSD / float64(summary.TasksPassed)
		} else {
			summary.CostPerPassingTaskUSD = math.Inf(1)
		}

		report.Configs[cfg] = summary
		report.TaskResults[cfg] = results
	}

	baselineCost := report.Configs[ConfigSingleUncached].CostPerPassingTaskUSD
	for _, cfg := range configs {
		s := report.Configs[cfg]
		if baselineCost > 0 && !math.IsInf(baselineCost, 0) && s.CostPerPassingTaskUSD > 0 && !math.IsInf(s.CostPerPassingTaskUSD, 0) {
			s.SavingsMultiplierVsBaseline = baselineCost / s.CostPerPassingTaskUSD
		}
		report.Configs[cfg] = s
	}

	return report, nil
}

func priceAndGradeTask(cat *pricing.Catalog, task TaskSpec, cfg ConfigID, trace TaskTrace) TaskResult {
	res := TaskResult{
		TaskID: task.ID,
		Config: cfg,
		Passed: task.Grade(trace.Output),
		Turns:  len(trace.Calls),
	}
	for _, call := range trace.Calls {
		if call.Role == "subtask" {
			res.SubtaskTurns++
		} else {
			res.ParentTurns++
		}
		u := call.EffectiveUsage(cfg)
		res.InputTokens += u.InputTokens
		res.UncachedInputTokens += u.UncachedInputTokens()
		res.CachedInputTokens += u.CachedInputTokens
		res.CacheCreationInputTokens += u.CacheCreationInputTokens
		res.OutputTokens += u.OutputTokens
		res.ThoughtsTokens += u.ThoughtsTokens

		p := usage.PriceOnCatalog(cat, call.Backend, call.Model)
		if p.Unpriced {
			res.UnpricedCalls++
		}
		res.CostUSD += p.CostUSDForTurn(u)
	}
	return res
}

// FormatMarkdown renders the benchmark report as a Markdown table ordered by
// r.ConfigOrder (falling back to AllConfigs() when empty).
func (r *Report) FormatMarkdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "### Cost-per-passing-task benchmark (`%s`)\n\n", r.SuiteName)
	b.WriteString("| Configuration | Pass rate | Turns (parent / subtask) | Input tokens (uncached / cached / write) | Output (+thoughts) | Total cost | Cost / passing task | vs `single_uncached` |\n")
	b.WriteString("|---|---:|---:|---:|---:|---:|---:|---:|\n")
	order := r.ConfigOrder
	if len(order) == 0 {
		order = AllConfigs()
	}
	for _, cfg := range order {
		s, ok := r.Configs[cfg]
		if !ok {
			continue
		}
		costPerSuccess := "∞ (0 passed)"
		if !math.IsInf(s.CostPerPassingTaskUSD, 0) {
			costPerSuccess = fmt.Sprintf("$%.4f", s.CostPerPassingTaskUSD)
		}
		mult := "—"
		if s.SavingsMultiplierVsBaseline > 0 {
			mult = fmt.Sprintf("%.2f×", s.SavingsMultiplierVsBaseline)
		}
		fmt.Fprintf(&b, "| `%s` | %d/%d (%.0f%%) | %d (%d / %d) | %d (%d / %d / %d) | %d (+%d) | $%.4f | %s | %s |\n",
			s.Config,
			s.TasksPassed, s.TasksTotal, s.PassRate*100,
			s.TurnsTotal, s.ParentTurns, s.SubtaskTurns,
			s.InputTokens, s.UncachedInputTokens, s.CachedInputTokens, s.CacheCreationInputTokens,
			s.OutputTokens, s.ThoughtsTokens,
			s.TotalCostUSD,
			costPerSuccess,
			mult,
		)
	}
	return b.String()
}

// FormatJSON serializes the summary report as indented JSON.
func (r *Report) FormatJSON() ([]byte, error) {
	return json.MarshalIndent(r, "", "  ")
}
