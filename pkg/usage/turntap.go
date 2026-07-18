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
	"google.golang.org/adk/session"
)

// TurnTap accumulates per-model-turn usage from a stream of
// *session.Event, applying Gemini's "cumulative UsageMetadata per
// chunk, final on TurnComplete" convention: overwrite last-seen per
// event, commit exactly once on TurnComplete, reset between turns.
//
// Motivation: Gemini's UsageMetadata is cumulative across streaming
// chunks within a single model turn — earlier chunks carry running
// totals, the final chunk carries the per-turn total. Naïve Append-
// on-every-event both inflates the tracker's turn count (one Append
// per chunk) and double-counts tokens (summing cumulative running
// totals). This bug bit us in the core-tui adapter (fixed in #156,
// surfaced in the field as "totals exactly 2x the last turn") and
// #157 extracted the pattern so future adapters get it right by
// default.
//
// Zero-value ready. Not safe for concurrent use — one TurnTap per
// event iterator.
//
// Typical usage (bookkeeping only):
//
//	var tap usage.TurnTap
//	for ev, err := range agent.Run(ctx, prompt) {
//	    tap.Observe(ev)
//	    if u, ok := tap.Commit(ev); ok {
//	        tracker.AppendUsage(model, u, pricing)
//	    }
//	    // ... other per-event work
//	}
//
// TUI-style usage (live per-event running total AND commit):
//
//	tap.Observe(ev)
//	if peek := tap.Peek(); peek.InputTokens > 0 {
//	    stampLiveCounter(peek)  // reflects running total mid-turn
//	}
//	if u, ok := tap.Commit(ev); ok {
//	    turn := tracker.AppendUsage(model, u, pricing)
//	    stampFinalCost(turn.CostUSD)
//	}
type TurnTap struct {
	last TurnUsage
}

// Observe updates the last-seen usage from ev.UsageMetadata. Ignores
// events without UsageMetadata (and nil events); safe to call for
// every event in the iterator.
func (t *TurnTap) Observe(ev *session.Event) {
	if ev == nil || ev.UsageMetadata == nil {
		return
	}
	t.last = TurnUsageFromGenaiMetadata(ev.UsageMetadata)
}

// Commit returns (per-turn totals, true) exactly when ev is a
// TurnComplete carrying non-zero accumulated usage, after resetting
// internal state so the next turn's chunks accumulate cleanly.
// Returns (zero, false) otherwise. Call after Observe.
func (t *TurnTap) Commit(ev *session.Event) (TurnUsage, bool) {
	if ev == nil || !ev.TurnComplete {
		return TurnUsage{}, false
	}
	if t.last.InputTokens == 0 && t.last.OutputTokens == 0 {
		return TurnUsage{}, false
	}
	u := t.last
	t.last = TurnUsage{}
	return u, true
}

// Peek returns the current last-seen usage without touching state.
// Reflects the running cumulative total mid-turn and the final total
// at the instant of TurnComplete (before Commit resets it).
func (t *TurnTap) Peek() TurnUsage { return t.last }
