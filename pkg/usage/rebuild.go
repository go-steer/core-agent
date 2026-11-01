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

package usage

import (
	"context"
	"fmt"
	"iter"
	"strings"

	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

const (
	// SkipParentUsageMetadataKey marks a subtask event whose spend is
	// attributed to a different session via a tool-response savings
	// sidecar (e.g. the primary agent's MCP digest LLM fallback, #717,
	// #1359) so a session-tree rebuild skips the subtask's own
	// UsageMetadata and does not double-count or mis-bill it.
	SkipParentUsageMetadataKey = "skip_parent_usage"

	// ModelVersionMetadataKey is the CustomMetadata key under which
	// eventlog and BranchInjectingService persist LLMResponse.ModelVersion
	// across ADK database serialization (#1359).
	ModelVersionMetadataKey = "model_version"

	// BillingBackendMetadataKey is the CustomMetadata key under which
	// BranchInjectingService persists a subagent's billing backend
	// across ADK database serialization (#1359).
	BillingBackendMetadataKey = "billing_backend"

	// toolSavingsResponseKey is the FunctionResponse map key carrying
	// MCP digest savings sidecars (#1359).
	toolSavingsResponseKey = "savings"
)

// RebuildTrackerFromEvents replays a persisted-event stream into t,
// reconstructing the per-turn totals via the same TurnTap.Observe +
// TurnTap.Commit pattern the live turn loop uses. Called on the
// session-resume path (pkg/compose/multi_session.go's ReproduceAgent
// when origin=="resumed") so the newly-minted per-session tracker
// carries the historical totals — including delegated and background
// subagent turns across the session tree (#1359) — instead of starting
// at zero.
//
// Motivation: PR #275 correctly isolated per-session usage.Trackers to
// stop cross-session contamination, but every session-resume path
// (SessionResumer / registry eviction miss / daemon restart) then
// began at zero — the eventlog was intact, but the tracker was fresh.
// The visible bug: /stats and the TUI's status-bar aggregate showed
// "0 in / 0 out / $0.00" for sessions with real historical work.
// Per-turn footers kept working because they replay from live SSE
// events, not from the tracker.
//
// Best-effort semantics:
//
//   - Events missing UsageMetadata: skipped by TurnTap.Observe (safe).
//   - Events with CustomMetadata["skip_parent_usage"] == true: skipped
//     so SkipParentUsage subtasks (such as the shared MCP digest LLM
//     fallback) are not billed to the host session on a session-tree
//     rebuild; the calling session's tool-response "savings" sidecar is
//     replayed instead (#1359).
//   - Events missing ModelVersion: fall back to
//     CustomMetadata["model_version"] (persisted by eventlog /
//     BranchInjectingService across ADK database round-trips) and then
//     to defaultModel (#1359).
//   - Subtask branches ("sub.<name>"): replayed via AppendSideUsage so
//     Tracker.Last() remains the conversation's own turn (#1191, #1359).
//   - pricingFor returning zero: tracker records tokens but $0.00
//     cost for that model. Downstream cost totals reflect this;
//     operators re-appending after a pricing refresh land the
//     correct cost on the next real turn.
//   - Context cancellation: early return with ctx.Err().
//   - Iterator errors: early return with the error (caller decides
//     whether to fail-open or fail-loud).
//
// Caller MUST invoke this BEFORE wiring tracker.SetOnAppend (which
// happens later inside agent.New's option evaluation), otherwise
// each rebuild AppendUsage would fire the OnAppend callback and
// broadcast N synthetic usage-update SSE events. The
// compose.ReproduceAgent call site respects this ordering by
// construction.
func RebuildTrackerFromEvents(
	ctx context.Context,
	t *Tracker,
	events iter.Seq2[*session.Event, error],
	defaultModel string,
	pricingFor func(model string) Pricing,
) error {
	if t == nil {
		return nil
	}
	if pricingFor == nil {
		pricingFor = func(string) Pricing { return Pricing{} }
	}
	// Persisted events differ from live-stream events: the ADK
	// eventlog serialization strips TurnComplete / Partial /
	// FinishReason (all zero-valued in rehydrated events). So the
	// live TurnTap.Commit pattern that keys off TurnComplete can't
	// fire on rebuild.
	//
	// Fortunately, Vertex + ADK only emit UsageMetadata on the
	// FINAL response chunk per model call, and ADK persists the
	// terminal chunk (not intermediates), so every event we see
	// carrying UsageMetadata represents exactly one committed
	// model response — one turn's worth of usage. AppendUsage per
	// UsageMetadata-bearing event reconstructs the tracker's
	// cumulative totals without needing the TurnComplete signal
	// that persistence dropped.
	for ev, err := range events {
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if ev == nil {
			continue
		}
		if skipParentUsageFrom(ev.CustomMetadata) {
			continue
		}
		if model, u, ok := sideUsageFrom(ev.CustomMetadata); ok {
			u.EventID = ev.ID
			t.AppendSideUsage(model, u, resolveRebuildPricing(ev, model, pricingFor))
			continue
		}
		replayToolSavings(t, ev, pricingFor)
		if ev.UsageMetadata == nil {
			continue
		}
		u := TurnUsageFromMetadata(ev.UsageMetadata, ev.CustomMetadata)
		if u.InputTokens == 0 && u.OutputTokens == 0 {
			// Vertex occasionally emits a UsageMetadata block with
			// zero token counts on error paths. Skip — appending
			// would inflate the turn count without adding real cost.
			continue
		}
		// The replayed turn keeps its event's identity, so a spend
		// ledger seeded from this replay keys each row on the same
		// event ID the live TurnTap would have (#1433).
		u.EventID = ev.ID
		modelName := resolveRebuildModel(ev, defaultModel)
		price := resolveRebuildPricing(ev, modelName, pricingFor)
		if isSubtaskBranch(ev.Branch) {
			t.AppendSideUsage(modelName, u, price)
		} else {
			t.AppendUsage(modelName, u, price)
		}
	}
	return nil
}

func skipParentUsageFrom(meta map[string]any) bool {
	if len(meta) == 0 {
		return false
	}
	skip, _ := meta[SkipParentUsageMetadataKey].(bool)
	return skip
}

func resolveRebuildModel(ev *session.Event, defaultModel string) string {
	if ev != nil {
		if ev.ModelVersion != "" {
			return ev.ModelVersion
		}
		if mv, ok := ev.CustomMetadata[ModelVersionMetadataKey].(string); ok && mv != "" {
			return mv
		}
	}
	return defaultModel
}

func resolveRebuildPricing(ev *session.Event, modelName string, pricingFor func(string) Pricing) Pricing {
	price := pricingFor(modelName)
	if ev == nil || len(ev.CustomMetadata) == 0 {
		return price
	}
	backend, _ := ev.CustomMetadata[BillingBackendMetadataKey].(string)
	if backend != "" && (price.Unpriced || (price.Backend != "" && price.Backend != backend)) {
		if pOn := PriceOn(backend, modelName); !pOn.Unpriced {
			return pOn
		}
	}
	return price
}

func isSubtaskBranch(branch string) bool {
	return strings.HasPrefix(branch, "sub.") || strings.Contains(branch, ".sub.")
}

// replayToolSavings restores the calling session's token and dollar
// spend from MCP digest "savings" sidecars via AppendSideUsage (#1359).
// It deliberately does NOT call AppendDigestSavings: in
// WithoutSessionLabels mode, aggregatedObserver.retiredDigest retains
// evicted sessions' digest-cost baseline across lazy resume
// (pkg/usage/metrics.go), so replaying into DigestSavings here would
// double-count core_agent.digest.subagent.cost_usd.
func replayToolSavings(t *Tracker, ev *session.Event, pricingFor func(string) Pricing) {
	if t == nil || ev == nil || ev.Content == nil || ev.Partial {
		return
	}
	for i, p := range ev.Content.Parts {
		rec, ok := extractSavingsRecordFromPart(p, pricingFor)
		if !ok {
			continue
		}
		if rec.SubagentModel != "" && (rec.SubagentInputTokens > 0 || rec.SubagentOutputTokens > 0) {
			u := rec.SubagentTurn()
			// One event can carry several sidecars, so the part index
			// keeps each one's ledger key distinct (#1433).
			if ev.ID != "" {
				u.EventID = fmt.Sprintf("%s:savings:%d", ev.ID, i)
			}
			t.AppendSideUsage(rec.SubagentModel, u, resolveRebuildPricing(ev, rec.SubagentModel, pricingFor))
		}
	}
}

func extractSavingsRecordFromPart(p *genai.Part, pricingFor func(string) Pricing) (DigestSavingsRecord, bool) {
	if p == nil || p.FunctionResponse == nil {
		return DigestSavingsRecord{}, false
	}
	raw, ok := p.FunctionResponse.Response[toolSavingsResponseKey].(map[string]any)
	if !ok {
		return DigestSavingsRecord{}, false
	}
	path, _ := raw["path"].(string)
	if path == "" {
		return DigestSavingsRecord{}, false
	}
	origTokens := intField(raw["original_tokens_est"])
	digestTokens := intField(raw["digest_tokens_est"])
	subModel, _ := raw["subagent_model"].(string)
	rec := DigestSavingsRecord{
		Path:                               path,
		ParentTokensSaved:                  origTokens - digestTokens,
		SubagentModel:                      subModel,
		SubagentInputTokens:                intField(raw["subagent_input_tokens"]),
		SubagentOutputTokens:               intField(raw["subagent_output_tokens"]),
		SubagentCachedInputTokens:          intField(raw["subagent_cached_input_tokens"]),
		SubagentCacheCreationInputTokens:   intField(raw["subagent_cache_creation_input_tokens"]),
		SubagentCacheCreation1hInputTokens: intField(raw["subagent_cache_creation_1h_input_tokens"]),
		SubagentThoughtsTokens:             intField(raw["subagent_thoughts_tokens"]),
	}
	if subModel != "" {
		rec.SubagentCostUSD = pricingFor(subModel).CostUSDForTurn(rec.SubagentTurn())
	}
	return rec, true
}
