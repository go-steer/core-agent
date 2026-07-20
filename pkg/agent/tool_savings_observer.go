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

// Per-agent observer for digest-wrap savings on tool responses.
//
// Background (2026-07-18): the original savings-accumulator wiring
// (PR #290) plumbed `DigestOptions.OnResult` in main.go to increment
// the *process-level* `usage.Tracker`. That works in single-session
// mode where the daemon has one tracker. In multi-session mode each
// on-demand session gets its own `usage.Tracker` (`multi_session.go`),
// and `agent.ContextStats().DigestSavings` reads the *session-scoped*
// tracker — which never sees the OnResult callbacks, because those
// still fire against the process-level one captured in the closure
// at mcp.Build time.
//
// Fix (option B, chosen 2026-07-18 in a demo debug session): read the
// savings data from the tool-result event itself, on the agent's
// event-tapping goroutine. The `savings` sidecar already rides on
// `FunctionResponse.Response` (that's how core-tui renders the
// per-tool chip), so no new plumbing is required at the digest layer.
// Each session's agent observes its own tool results, extracts the
// sidecar, computes any subagent cost against the layered pricing
// catalog, and appends to the *session's* tracker. Multi-session and
// single-session both behave identically.

package agent

import (
	"google.golang.org/adk/session"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/pkg/usage"
)

// toolSavingsResponseKey is the well-known sidecar key core-agent's
// MCP digest wrap stamps on every wrapped tool response. Duplicated
// (not imported) from `pkg/mcp` to keep `pkg/agent` from taking a
// dependency on `pkg/mcp` — the sidecar is a wire contract, not an
// API. Matches `pkg/mcp/digest_wrap.go`'s outbound map key AND the
// `toolSavingsResponseKey` used by core-tui's `tool_savings.go`
// consumer.
const toolSavingsResponseKey = "savings"

// observeToolSavings walks a session.Event for FunctionResponse parts
// carrying a digest `savings` sidecar and appends each one to the
// agent's usage.Tracker. Cheap and additive — no-op when the event
// isn't a tool result, or when the response has no sidecar (single-
// tool built-ins that skip the wrap layer, MCP servers with
// `agentic_never: true`).
//
// Cost math for the agentic path resolves subagent pricing via the
// process-installed catalog (`usage.PriceFor`). Nil `cfg` is
// intentional: the layered catalog was installed at startup with
// `usage.SetCatalog`, and the per-Agent value has no config handle.
// PriceFor's nil-cfg path consults the global catalog, which is what
// we want here.
//
// Called from the per-turn event-tap in Agent.Run (alongside
// watchdog + onEvent observers) so multi-session, single-session, and
// autonomous k8s-event-injected sessions all populate their local
// tracker uniformly. Replaces the per-process
// `DigestOptions.OnResult` wiring from PR #290 which only worked in
// single-session mode.
func (a *Agent) observeToolSavings(ev *session.Event) {
	if a == nil || a.tracker == nil || ev == nil || ev.Content == nil {
		return
	}
	for _, p := range ev.Content.Parts {
		rec, ok := extractSavingsRecord(p)
		if !ok {
			continue
		}
		a.tracker.AppendDigestSavings(rec)
	}
}

// extractSavingsRecord pulls a DigestSavingsRecord out of one Content
// Part. Returns (_, false) when the part isn't a function response,
// has no response map, or has no `savings` sidecar — the common cases
// on non-wrapped tools + built-ins.
//
// SubagentCostUSD resolves at extraction time against the layered
// pricing catalog. Zero on structural / passthrough paths where
// SubagentModel is empty (which is the shipped default until the
// operator opts in via `--mcp-agentic-wrap-llm`).
//
// Type tolerance mirrors core-tui's `resolveToolSavings` — numeric
// values arrive as float64 through JSON, int / int64 in-process.
// Both accepted; unknown shapes are silently dropped so a malformed
// sidecar can't crash the tap goroutine.
func extractSavingsRecord(p *genai.Part) (usage.DigestSavingsRecord, bool) {
	if p == nil || p.FunctionResponse == nil {
		return usage.DigestSavingsRecord{}, false
	}
	raw, ok := p.FunctionResponse.Response[toolSavingsResponseKey].(map[string]any)
	if !ok {
		return usage.DigestSavingsRecord{}, false
	}
	path, _ := raw["path"].(string)
	if path == "" {
		return usage.DigestSavingsRecord{}, false
	}
	origTokens := savingsIntField(raw, "original_tokens_est")
	digestTokens := savingsIntField(raw, "digest_tokens_est")
	subModel, _ := raw["subagent_model"].(string)
	subIn := savingsIntField(raw, "subagent_input_tokens")
	subOut := savingsIntField(raw, "subagent_output_tokens")

	var subCost float64
	if subModel != "" {
		p := usage.PriceFor(subModel, nil)
		subCost = p.CostUSD(subIn, subOut)
	}
	return usage.DigestSavingsRecord{
		Path:                 path,
		ParentTokensSaved:    origTokens - digestTokens,
		SubagentModel:        subModel,
		SubagentInputTokens:  subIn,
		SubagentOutputTokens: subOut,
		SubagentCostUSD:      subCost,
	}, true
}

// savingsIntField pulls an integer out of the sidecar map. Handles
// the three shapes numeric values arrive as: int (in-process), int64
// (in-process, some producers), float64 (JSON-decoded). Returns 0 on
// unknown / missing / wrong type — safe default for token counts.
func savingsIntField(m map[string]any, key string) int {
	switch v := m[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	}
	return 0
}
