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

// Package usage tracks token + cost accounting for the agent loop.
//
// Every model call returns a UsageMetadata block with input and output
// token counts; a Tracker accumulates these across a session. Pricing
// numbers come from a built-in table that callers may override per
// model via .agents/config.json (model.pricing).
package usage

import (
	"sync"
	"time"
)

// Turn captures one model call's resource use. Times are wall clock so
// summary lines can include session duration without a monotonic ref.
//
// InputTokens is the total effective prompt size — for Gemini this
// matches PromptTokenCount, which already includes any cache-hit tokens
// (google.golang.org/genai types.go: "the total effective prompt size
// meaning this includes the number of tokens in the cached content").
// CachedInputTokens is therefore a subset of InputTokens, not an
// addition to it. Uncached = InputTokens - CachedInputTokens.
type Turn struct {
	Model             string
	InputTokens       int
	CachedInputTokens int
	OutputTokens      int
	ThoughtsTokens    int
	ToolUseTokens     int
	CostUSD           float64
	At                time.Time
}

// TurnUsage is the per-call token breakdown a provider adapter hands
// to Tracker.AppendUsage. Provider-independent: adapters normalize
// their per-response metadata into this shape (see
// TurnUsageFromGenaiMetadata for the Gemini/Vertex path).
type TurnUsage struct {
	InputTokens       int
	CachedInputTokens int
	OutputTokens      int
	ThoughtsTokens    int
	ToolUseTokens     int
}

// Totals aggregates a slice of Turns. Cached / thoughts / tool-use
// mirror the Turn fields so callers projecting Totals into wire
// formats can render every dimension without walking All().
type Totals struct {
	Turns             int
	InputTokens       int
	CachedInputTokens int
	OutputTokens      int
	ThoughtsTokens    int
	ToolUseTokens     int
	CostUSD           float64
}

// Tracker accumulates per-turn usage for one session.
//
// Thread-safe: the agent goroutine (or run loop) calls Append; readers
// access via Last/Totals/All.
type Tracker struct {
	mu        sync.Mutex
	turns     []Turn
	startedAt time.Time
	onAppend  func() // optional; fired after each Append, under no lock
}

// NewTracker returns a tracker with its session-start time set to now.
func NewTracker() *Tracker { return &Tracker{startedAt: time.Now()} }

// SetOnAppend registers a callback that fires after every Append call.
// The callback runs after the lock is released, so it can safely call
// Totals(), TotalsByModel(), or any other Tracker accessor without
// risking a re-entrant deadlock.
//
// Used by the attach layer to push usage-update events on the SSE
// stream as turn cost lands — each Append represents a turn whose
// cumulative impact should reach connected operators.
//
// Pass nil to unregister. Safe to set multiple times (last wins);
// callers wiring this from the broadcaster do so on first subscriber
// and clear it on last detach.
func (t *Tracker) SetOnAppend(f func()) {
	t.mu.Lock()
	t.onAppend = f
	t.mu.Unlock()
}

// Append records one turn's usage with input/output only. Cost is
// computed via the supplied Pricing; pass a zero Pricing to skip cost
// tracking. If SetOnAppend has been called with a non-nil callback,
// the callback fires after the new turn is durable in the tracker and
// the lock has been released.
//
// Callers that have a full per-turn breakdown (cache hits, thoughts,
// tool-use) should use AppendUsage instead so the extra dimensions
// flow through to Totals + wire formats.
func (t *Tracker) Append(model string, inputTokens, outputTokens int, p Pricing) Turn {
	return t.AppendUsage(model, TurnUsage{
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
	}, p)
}

// AppendUsage records one turn's usage with the full per-field
// breakdown. CachedInputTokens > InputTokens is clamped to InputTokens
// (defensive against occasional provider quirks where the cached
// counter over-reports; the input/uncached math must stay non-negative
// downstream). Cost applies CostUSDWithCache when any cache hits are
// present so the cached-vs-uncached rate split is reflected in the
// stored Turn.
func (t *Tracker) AppendUsage(model string, u TurnUsage, p Pricing) Turn {
	if u.CachedInputTokens > u.InputTokens {
		u.CachedInputTokens = u.InputTokens
	}
	uncached := u.InputTokens - u.CachedInputTokens
	cost := p.CostUSDWithCache(uncached, u.CachedInputTokens, u.OutputTokens)
	turn := Turn{
		Model:             model,
		InputTokens:       u.InputTokens,
		CachedInputTokens: u.CachedInputTokens,
		OutputTokens:      u.OutputTokens,
		ThoughtsTokens:    u.ThoughtsTokens,
		ToolUseTokens:     u.ToolUseTokens,
		CostUSD:           cost,
		At:                time.Now(),
	}
	t.mu.Lock()
	t.turns = append(t.turns, turn)
	cb := t.onAppend
	t.mu.Unlock()
	if cb != nil {
		cb()
	}
	return turn
}

// Last returns the most recently appended turn, or zero if none yet.
func (t *Tracker) Last() (Turn, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.turns) == 0 {
		return Turn{}, false
	}
	return t.turns[len(t.turns)-1], true
}

// Totals returns the cumulative usage across all turns.
func (t *Tracker) Totals() Totals {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := Totals{Turns: len(t.turns)}
	for _, x := range t.turns {
		out.InputTokens += x.InputTokens
		out.CachedInputTokens += x.CachedInputTokens
		out.OutputTokens += x.OutputTokens
		out.ThoughtsTokens += x.ThoughtsTokens
		out.ToolUseTokens += x.ToolUseTokens
		out.CostUSD += x.CostUSD
	}
	return out
}

// TotalsByModel groups the session's turns by model name and
// returns the per-model totals. Useful for surfaces that want to
// break down "$X.YY total" into "$A.BB parent model + $C.DD
// subtask model" so the cost-efficiency win of routing subtasks
// to a cheaper model is directly visible. Empty map when no
// turns recorded.
func (t *Tracker) TotalsByModel() map[string]Totals {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.turns) == 0 {
		return map[string]Totals{}
	}
	out := make(map[string]Totals)
	for _, x := range t.turns {
		cur := out[x.Model]
		cur.Turns++
		cur.InputTokens += x.InputTokens
		cur.CachedInputTokens += x.CachedInputTokens
		cur.OutputTokens += x.OutputTokens
		cur.ThoughtsTokens += x.ThoughtsTokens
		cur.ToolUseTokens += x.ToolUseTokens
		cur.CostUSD += x.CostUSD
		out[x.Model] = cur
	}
	return out
}

// All returns a copy of every recorded turn.
func (t *Tracker) All() []Turn {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]Turn, len(t.turns))
	copy(out, t.turns)
	return out
}

// Duration reports wall-clock time since NewTracker was called.
func (t *Tracker) Duration() time.Duration { return time.Since(t.startedAt) }
