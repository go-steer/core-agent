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
	"iter"

	"google.golang.org/adk/session"
)

// RebuildTrackerFromEvents replays a persisted-event stream into t,
// reconstructing the per-turn totals via the same TurnTap.Observe +
// TurnTap.Commit pattern the live turn loop uses. Called on the
// session-resume path (cmd/core-agent/multi_session.go's reproduceAgent
// when origin=="resumed") so the newly-minted per-session tracker
// carries the historical totals instead of starting at zero.
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
//   - Events missing ModelVersion: fall back to defaultModel. In
//     practice the session's primary model is stable across its
//     lifetime, so this is fine.
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
// broadcast N synthetic usage-update SSE events. The reproduceAgent
// call site respects this ordering by construction.
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
		if ev == nil || ev.UsageMetadata == nil {
			continue
		}
		u := TurnUsageFromGenaiMetadata(ev.UsageMetadata)
		if u.InputTokens == 0 && u.OutputTokens == 0 {
			// Vertex occasionally emits a UsageMetadata block with
			// zero token counts on error paths. Skip — appending
			// would inflate the turn count without adding real cost.
			continue
		}
		modelName := ev.ModelVersion
		if modelName == "" {
			modelName = defaultModel
		}
		t.AppendUsage(modelName, u, pricingFor(modelName))
	}
	return nil
}
