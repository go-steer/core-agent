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

package costbench

import (
	"context"
	"fmt"
)

// ModelPair names the frontier parent model and the small-tier subtask model
// for a benchmark run.
type ModelPair struct {
	Backend       string
	ParentModel   string
	SubtaskModel  string
	WritesCache5m bool // true for Anthropic-style explicit cache creation billing
}

// DefaultAnthropicPair returns the canonical Anthropic frontier + small model pair.
func DefaultAnthropicPair() ModelPair {
	return ModelPair{
		Backend:       "anthropic",
		ParentModel:   "claude-opus-4-6",
		SubtaskModel:  "claude-haiku-4-5",
		WritesCache5m: true,
	}
}

// DefaultGeminiPair returns the canonical Gemini frontier + small model pair.
func DefaultGeminiPair() ModelPair {
	return ModelPair{
		Backend:       "gemini",
		ParentModel:   "gemini-2.5-pro",
		SubtaskModel:  "gemini-2.5-flash",
		WritesCache5m: false,
	}
}

// DefaultTasks returns the reference 5-task multi-turn engineering/operations
// task suite used by the offline replay benchmark and live evaluation harness.
func DefaultTasks() []TaskSpec {
	return []TaskSpec{
		{
			ID:                "gke-crashloop-triage",
			Title:             "Diagnose CrashLoopBackOff in payments-api across 12-turn MCP session",
			RequiredFragments: []string{"OOMKilled", "limits.memory", "512Mi"},
		},
		{
			ID:                "multi-file-race-fix",
			Title:             "Locate and fix data race across 4 package files with grep + read_file",
			RequiredFragments: []string{"sync.Mutex", "tracker.go", "race"},
		},
		{
			ID:                "config-schema-audit",
			Title:             "Cross-check config schema fields against docs and recipecheck",
			RequiredFragments: []string{"permissions.mode", "GatedFeatures", "valid"},
		},
		{
			ID:                "mcp-log-correlation",
			Title:             "Correlate 3 noisy JSON log streams and identify root-cause upstream 503",
			RequiredFragments: []string{"upstream_timeout", "checkout-svc", "503"},
		},
		{
			ID:                "long-horizon-refactor",
			Title:             "15-turn package extraction with intermediate verification steps",
			RequiredFragments: []string{"pkg/attach/wire", "import_boundary_test", "PASS"},
		},
	}
}

// workloadShape describes the per-task turn structure so the deterministic
// replay executor models the exact token mechanics of the four configurations:
//   - single_*: bulky tool outputs (rawToolTokensPerRead) enter the parent
//     conversation context on every tool-heavy turn and are re-sent on every
//     subsequent parent turn.
//   - split_*: each bulky read is digested by a 1-turn subtask on SubtaskModel
//     (consuming rawToolTokensPerRead once on the cheap model) and only the
//     compact digest (digestedTokensPerRead) enters the parent's cumulative
//     history.
//   - *_cached: the stable system+tool prefix (systemPrefixTokens) plus the
//     prior turn's parent history are served from prompt cache on turn >= 2.
type workloadShape struct {
	parentTurns           int
	heavyReads            int
	systemPrefixTokens    int
	userTurnDeltaTokens   int
	rawToolTokensPerRead  int
	digestedTokensPerRead int
	parentOutputTokens    int
	subtaskOutputTokens   int
	output                string
}

var defaultShapes = map[string]workloadShape{
	"gke-crashloop-triage": {
		parentTurns:           12,
		heavyReads:            6,
		systemPrefixTokens:    14000,
		userTurnDeltaTokens:   600,
		rawToolTokensPerRead:  12000,
		digestedTokensPerRead: 900,
		parentOutputTokens:    320,
		subtaskOutputTokens:   180,
		output:                "Root cause: container terminated with OOMKilled because limits.memory is set to 512Mi.",
	},
	"multi-file-race-fix": {
		parentTurns:           10,
		heavyReads:            5,
		systemPrefixTokens:    14000,
		userTurnDeltaTokens:   550,
		rawToolTokensPerRead:  9500,
		digestedTokensPerRead: 800,
		parentOutputTokens:    380,
		subtaskOutputTokens:   160,
		output:                "Fixed the data race in tracker.go by guarding the map mutation with sync.Mutex.",
	},
	"config-schema-audit": {
		parentTurns:           8,
		heavyReads:            4,
		systemPrefixTokens:    14000,
		userTurnDeltaTokens:   500,
		rawToolTokensPerRead:  11000,
		digestedTokensPerRead: 850,
		parentOutputTokens:    290,
		subtaskOutputTokens:   150,
		output:                "Verified permissions.mode and GatedFeatures table are valid and in sync.",
	},
	"mcp-log-correlation": {
		parentTurns:           14,
		heavyReads:            8,
		systemPrefixTokens:    14000,
		userTurnDeltaTokens:   650,
		rawToolTokensPerRead:  15000,
		digestedTokensPerRead: 1100,
		parentOutputTokens:    340,
		subtaskOutputTokens:   200,
		output:                "Correlated 503 responses on checkout-svc to upstream_timeout during pod drain.",
	},
	"long-horizon-refactor": {
		parentTurns:           15,
		heavyReads:            7,
		systemPrefixTokens:    14000,
		userTurnDeltaTokens:   700,
		rawToolTokensPerRead:  10000,
		digestedTokensPerRead: 850,
		parentOutputTokens:    410,
		subtaskOutputTokens:   170,
		output:                "Extracted pkg/attach/wire and verified import_boundary_test PASS.",
	},
}

// NewReplayExecutor returns a deterministic TaskExecutor that replays the
// multi-turn token traces for DefaultTasks() using pair's parent and subtask
// models. Used by CI unit tests and as the reference baseline until live
// multi-turn traces are recorded.
func NewReplayExecutor(pair ModelPair) TaskExecutor {
	return func(ctx context.Context, task TaskSpec, cfg ConfigID) (TaskTrace, error) {
		if err := ctx.Err(); err != nil {
			return TaskTrace{}, err
		}
		shape, ok := defaultShapes[task.ID]
		if !ok {
			return TaskTrace{}, fmt.Errorf("unknown benchmark task %q", task.ID)
		}
		isSplit := cfg == ConfigSplitUncached || cfg == ConfigSplitCached
		isCached := cfg == ConfigSingleCached || cfg == ConfigSplitCached

		var calls []CallRecord
		historyTokens := shape.systemPrefixTokens
		prevCachedPrefix := 0

		for turn := 0; turn < shape.parentTurns; turn++ {
			turnAddition := shape.userTurnDeltaTokens
			if turn < shape.heavyReads {
				if isSplit {
					// Subtask digests the bulky tool payload on the small model
					// in an isolated 1-turn call (not cached across turns).
					subPrompt := 1200 + shape.rawToolTokensPerRead
					calls = append(calls, CallRecord{
						Backend:      pair.Backend,
						Model:        pair.SubtaskModel,
						Role:         "subtask",
						InputTokens:  subPrompt,
						OutputTokens: shape.subtaskOutputTokens,
					})
					turnAddition += shape.digestedTokensPerRead
				} else {
					turnAddition += shape.rawToolTokensPerRead
				}
			}

			totalPrompt := historyTokens + turnAddition
			call := CallRecord{
				Backend:      pair.Backend,
				Model:        pair.ParentModel,
				Role:         "parent",
				InputTokens:  totalPrompt,
				OutputTokens: shape.parentOutputTokens,
			}
			if isCached {
				if turn == 0 {
					if pair.WritesCache5m {
						call.CacheCreationInputTokens = totalPrompt
					}
				} else {
					call.CachedInputTokens = prevCachedPrefix
					if pair.WritesCache5m {
						call.CacheCreationInputTokens = totalPrompt - prevCachedPrefix
					}
				}
				prevCachedPrefix = totalPrompt
			}

			calls = append(calls, call)
			historyTokens = totalPrompt + shape.parentOutputTokens
		}

		return TaskTrace{
			TaskID: task.ID,
			Config: cfg,
			Output: shape.output,
			Calls:  calls,
		}, nil
	}
}
