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

package agent

import (
	"github.com/go-steer/core-agent/pkg/usage"
)

// recordInternalLLMUsage rolls one internal-LLM call's token usage
// into the agent's tracker so /stats, /context, and the
// --max-turn-cost-usd / --max-session-cost-usd ceilings (see
// cost_ceiling.go and #145) see it. "Internal" here means an
// agent-driven LLM call that doesn't go through Agent.Run — today
// that's runSummarizer (compaction + checkpoint) and AskSideQuestion
// (the /btw flow). The original gap was reported as #61.
//
// Both call sites use a.model directly (one TurnComplete per call —
// no tool loop), so the caller's pattern is "capture last UsageMetadata
// during the stream, call this helper after the loop." Mirroring the
// shape from subtask.go's Run loop. The model-name is always a.model.Name()
// because both internal callers use the agent's primary model — no
// per-call model override path exists.
//
// No-ops when the tracker isn't wired (tests / hand-constructed
// agents) or when both token counts are zero (some providers report
// usage only on the final response and may leave it empty on
// failure paths).
func (a *Agent) recordInternalLLMUsage(promptTokens, completionTokens int) {
	if a == nil || a.tracker == nil {
		return
	}
	if promptTokens <= 0 && completionTokens <= 0 {
		return
	}
	if a.model == nil {
		return
	}
	modelName := a.model.Name()
	a.tracker.Append(modelName, promptTokens, completionTokens, usage.PriceFor(modelName, nil))
}
