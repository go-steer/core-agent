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

package costbench_test

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v3/internal/evals/costbench"
)

func TestRunSuiteFourConfigsAnthropicAndGemini(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		pair costbench.ModelPair
	}{
		{name: "anthropic_opus_haiku", pair: costbench.DefaultAnthropicPair()},
		{name: "gemini_pro_flash", pair: costbench.DefaultGeminiPair()},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			report, err := costbench.RunSuite(context.Background(), costbench.SuiteOptions{
				SuiteName: tc.name,
				Tasks:     costbench.DefaultTasks(),
			}, costbench.NewReplayExecutor(tc.pair))
			if err != nil {
				t.Fatalf("RunSuite(%s): %v", tc.name, err)
			}

			t.Logf("\n%s", report.FormatMarkdown())

			for _, cfg := range costbench.AllConfigs() {
				s, ok := report.Configs[cfg]
				if !ok {
					t.Fatalf("missing config %q in report", cfg)
				}
				if s.UnpricedCalls != 0 {
					t.Errorf("%s: UnpricedCalls = %d, want 0 (all benchmark models must resolve in pkg/pricing)", cfg, s.UnpricedCalls)
				}
				if s.TasksPassed != s.TasksTotal {
					t.Errorf("%s: TasksPassed = %d/%d, want 100%% pass on reference replay", cfg, s.TasksPassed, s.TasksTotal)
				}
				if s.CostPerPassingTaskUSD <= 0 || math.IsInf(s.CostPerPassingTaskUSD, 0) {
					t.Errorf("%s: CostPerPassingTaskUSD = %v, want finite positive USD", cfg, s.CostPerPassingTaskUSD)
				}
			}

			singleUncached := report.Configs[costbench.ConfigSingleUncached]
			singleCached := report.Configs[costbench.ConfigSingleCached]
			splitUncached := report.Configs[costbench.ConfigSplitUncached]
			splitCached := report.Configs[costbench.ConfigSplitCached]

			if singleCached.CostPerPassingTaskUSD >= singleUncached.CostPerPassingTaskUSD {
				t.Errorf("single_cached ($%.4f) must cost less per passing task than single_uncached ($%.4f)",
					singleCached.CostPerPassingTaskUSD, singleUncached.CostPerPassingTaskUSD)
			}
			if splitUncached.CostPerPassingTaskUSD >= singleUncached.CostPerPassingTaskUSD {
				t.Errorf("split_uncached ($%.4f) must cost less per passing task than single_uncached ($%.4f)",
					splitUncached.CostPerPassingTaskUSD, singleUncached.CostPerPassingTaskUSD)
			}
			if splitCached.CostPerPassingTaskUSD >= splitUncached.CostPerPassingTaskUSD {
				t.Errorf("split_cached ($%.4f) must cost less per passing task than split_uncached ($%.4f)",
					splitCached.CostPerPassingTaskUSD, splitUncached.CostPerPassingTaskUSD)
			}
			if splitCached.CostPerPassingTaskUSD >= singleCached.CostPerPassingTaskUSD {
				t.Errorf("split_cached ($%.4f) must cost less per passing task than single_cached ($%.4f)",
					splitCached.CostPerPassingTaskUSD, singleCached.CostPerPassingTaskUSD)
			}
			if math.Abs(singleUncached.SavingsMultiplierVsBaseline-1.0) > 1e-9 {
				t.Errorf("single_uncached multiplier = %v, want 1.0", singleUncached.SavingsMultiplierVsBaseline)
			}
			if splitCached.SavingsMultiplierVsBaseline <= 1.0 {
				t.Errorf("split_cached multiplier = %v, want > 1.0", splitCached.SavingsMultiplierVsBaseline)
			}
		})
	}
}

func TestFailedTaskPenalizesCostPerPassingTask(t *testing.T) {
	t.Parallel()

	tasks := []costbench.TaskSpec{
		{ID: "t1", Title: "pass task", RequiredFragments: []string{"fixed"}},
		{ID: "t2", Title: "fail task on split", RequiredFragments: []string{"exact_line_42"}},
	}

	exec := func(_ context.Context, task costbench.TaskSpec, cfg costbench.ConfigID) (costbench.TaskTrace, error) {
		output := "fixed at exact_line_42"
		if task.ID == "t2" && (cfg == costbench.ConfigSplitUncached || cfg == costbench.ConfigSplitCached) {
			// Simulate a subtask digest dropping a required detail so the task fails.
			output = "fixed approximately"
		}
		return costbench.TaskTrace{
			TaskID: task.ID,
			Config: cfg,
			Output: output,
			Calls: []costbench.CallRecord{
				{
					Backend:      "anthropic",
					Model:        "claude-opus-4-6",
					Role:         "parent",
					InputTokens:  20000,
					OutputTokens: 500,
				},
			},
		}, nil
	}

	report, err := costbench.RunSuite(context.Background(), costbench.SuiteOptions{
		SuiteName: "failure_penalty",
		Tasks:     tasks,
	}, exec)
	if err != nil {
		t.Fatalf("RunSuite: %v", err)
	}

	single := report.Configs[costbench.ConfigSingleUncached]
	split := report.Configs[costbench.ConfigSplitUncached]

	if single.TasksPassed != 2 || split.TasksPassed != 1 {
		t.Fatalf("TasksPassed: single=%d, split=%d; want 2 and 1", single.TasksPassed, split.TasksPassed)
	}
	// Total spend is identical because both ran the same calls, so split's
	// CostPerPassingTaskUSD must be 2x single's because 1 of 2 tasks failed.
	ratio := split.CostPerPassingTaskUSD / single.CostPerPassingTaskUSD
	if math.Abs(ratio-2.0) > 1e-6 {
		t.Errorf("split.CostPerPassingTaskUSD / single.CostPerPassingTaskUSD = %v, want 2.0", ratio)
	}
}

func TestUncachedConfigsStripCacheCounters(t *testing.T) {
	t.Parallel()

	call := costbench.CallRecord{
		Backend:                  "anthropic",
		Model:                    "claude-opus-4-6",
		Role:                     "parent",
		InputTokens:              10000,
		CachedInputTokens:        8000,
		CacheCreationInputTokens: 1000,
		OutputTokens:             200,
	}

	uUncached := call.EffectiveUsage(costbench.ConfigSingleUncached)
	if uUncached.CachedInputTokens != 0 || uUncached.CacheCreationInputTokens != 0 {
		t.Errorf("ConfigSingleUncached kept cache counters: %+v", uUncached)
	}
	if uUncached.UncachedInputTokens() != 10000 {
		t.Errorf("UncachedInputTokens = %d, want 10000", uUncached.UncachedInputTokens())
	}

	uCached := call.EffectiveUsage(costbench.ConfigSingleCached)
	if uCached.CachedInputTokens != 8000 || uCached.CacheCreationInputTokens != 1000 || uCached.UncachedInputTokens() != 1000 {
		t.Errorf("ConfigSingleCached unexpected breakdown: %+v (uncached=%d)", uCached, uCached.UncachedInputTokens())
	}
}

func TestReportFormatMarkdownAndJSON(t *testing.T) {
	t.Parallel()

	report, err := costbench.RunSuite(context.Background(), costbench.SuiteOptions{
		SuiteName: "format_check",
		Tasks:     costbench.DefaultTasks()[:2],
	}, costbench.NewReplayExecutor(costbench.DefaultAnthropicPair()))
	if err != nil {
		t.Fatalf("RunSuite: %v", err)
	}

	md := report.FormatMarkdown()
	for _, want := range []string{
		"single_uncached",
		"single_cached",
		"split_uncached",
		"split_cached",
		"Cost / passing task",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("FormatMarkdown() missing %q:\n%s", want, md)
		}
	}

	raw, err := report.FormatJSON()
	if err != nil {
		t.Fatalf("FormatJSON: %v", err)
	}
	var decoded costbench.Report
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("Unmarshal FormatJSON output: %v", err)
	}
	if len(decoded.Configs) != 4 {
		t.Errorf("decoded Configs len = %d, want 4", len(decoded.Configs))
	}
}

func TestZeroPassingTasksJSONAndMarkdownAndCancellation(t *testing.T) {
	t.Parallel()

	tasks := []costbench.TaskSpec{
		{ID: "t1", Title: "impossible", RequiredFragments: []string{"never_emitted"}},
	}
	exec := func(_ context.Context, task costbench.TaskSpec, cfg costbench.ConfigID) (costbench.TaskTrace, error) {
		return costbench.TaskTrace{
			TaskID: task.ID,
			Config: cfg,
			Output: "wrong answer",
			Calls: []costbench.CallRecord{
				{Backend: "anthropic", Model: "claude-opus-4-6", Role: "parent", InputTokens: 1000, OutputTokens: 100},
			},
		}, nil
	}

	customCfg := costbench.ConfigID("custom_experiment")
	report, err := costbench.RunSuite(context.Background(), costbench.SuiteOptions{
		SuiteName: "zero_pass",
		Tasks:     tasks,
		Configs:   []costbench.ConfigID{customCfg},
	}, exec)
	if err != nil {
		t.Fatalf("RunSuite: %v", err)
	}
	if !math.IsInf(report.Configs[customCfg].CostPerPassingTaskUSD, 1) {
		t.Errorf("CostPerPassingTaskUSD = %v, want +Inf", report.Configs[customCfg].CostPerPassingTaskUSD)
	}
	md := report.FormatMarkdown()
	if !strings.Contains(md, "custom_experiment") || !strings.Contains(md, "∞ (0 passed)") {
		t.Errorf("FormatMarkdown missing custom_experiment or ∞:\n%s", md)
	}
	raw, err := report.FormatJSON()
	if err != nil {
		t.Fatalf("FormatJSON with 0 passing tasks failed: %v", err)
	}
	var decoded costbench.Report
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("Unmarshal JSON with 0 passing tasks: %v", err)
	}
	if !math.IsInf(decoded.Configs[customCfg].CostPerPassingTaskUSD, 1) {
		t.Errorf("round-tripped CostPerPassingTaskUSD = %v, want +Inf", decoded.Configs[customCfg].CostPerPassingTaskUSD)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := costbench.RunSuite(ctx, costbench.SuiteOptions{
		Tasks: costbench.DefaultTasks(),
	}, costbench.NewReplayExecutor(costbench.DefaultAnthropicPair())); err == nil {
		t.Error("RunSuite with cancelled context succeeded, want error")
	}
}
