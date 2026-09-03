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
// CachedInputTokens and CacheCreationInputTokens are therefore both
// subsets of InputTokens, not additions to it, and they never overlap
// with each other. Uncached = InputTokens - CachedInputTokens -
// CacheCreationInputTokens.
//
// CacheCreationInputTokens is the write bucket: tokens this turn spent
// establishing a cache entry, billed at a premium (Anthropic charges
// 1.25x base input on the 5-minute TTL, 2x on the 1-hour TTL). Zero for
// providers that don't bill writes separately — Gemini's explicit
// caches charge per-hour storage, not per written token.
type Turn struct {
	Model                    string
	InputTokens              int
	CachedInputTokens        int
	CacheCreationInputTokens int
	OutputTokens             int
	ThoughtsTokens           int
	ToolUseTokens            int
	CostUSD                  float64
	At                       time.Time
	// Unpriced is true when the model had no rate in the pricing
	// catalog, so CostUSD is 0 because the price is unknown rather
	// than because the model is free. Threads through from the
	// Pricing passed to AppendUsage; consumers rendering cost should
	// show "$—" for unpriced turns instead of "$0.00". See #368.
	Unpriced bool
}

// TurnUsage is the per-call token breakdown a provider adapter hands
// to Tracker.AppendUsage. Provider-independent: adapters normalize
// their per-response metadata into this shape (see
// TurnUsageFromGenaiMetadata for the Gemini/Vertex path).
type TurnUsage struct {
	InputTokens              int
	CachedInputTokens        int
	CacheCreationInputTokens int
	// CacheCreation1hInputTokens is the share of
	// CacheCreationInputTokens written at a 1-hour breakpoint TTL
	// rather than the default 5-minute one — a SUBSET of the write
	// bucket, not a fourth input bucket. Anthropic bills it at 2x base
	// input against the 5-minute TTL's 1.25x and reports the split on
	// the response, so a mixed-TTL request has a right answer to bill
	// rather than a guess (#770).
	//
	// Zero for every provider that offers one TTL or none, which
	// prices exactly as it did before.
	CacheCreation1hInputTokens int
	OutputTokens               int
	// ThoughtsTokens is the turn's reasoning tokens, reported by
	// providers that meter them separately (Gemini's
	// thoughtsTokenCount). ADDITIVE to OutputTokens, not a subset of
	// it — the provider's own total is prompt + candidates + thoughts —
	// and billed at the output rate, which CostUSDForTurn does. Zero
	// for providers that fold thinking into their output count, which
	// is why adding it there can't double-charge them.
	ThoughtsTokens int
	ToolUseTokens  int
}

// Clamped returns u with the two cache buckets forced inside
// InputTokens, so the uncached remainder can never go negative.
// Defensive against provider quirks where a cache counter over-reports;
// reads are clamped first and writes get whatever room is left, so a
// contradictory pair can only shrink the premium-rated write bucket —
// an under-estimate, never a phantom charge.
//
// CacheCreation1hInputTokens is clamped last, into the write bucket it
// is a subset of, for the same reason and in the same direction: the
// 1-hour share is the dearer of the two TTLs, so an over-report shrinks
// to what the write bucket can hold rather than billing tokens the turn
// never wrote.
//
// The three top-line counts — InputTokens, OutputTokens,
// ThoughtsTokens — are floored at zero first, so a provider (or a JSON
// sidecar) reporting a negative can neither subtract from the bill nor
// push a monotonic OTel counter backwards. That mattered less when
// nothing but cost read these; ThoughtsTokens is now billed at the
// output rate (#927) and every one of the three is summed into Totals
// and observed on gen_ai.client.token.usage, which is an
// Int64ObservableCounter and rejects a negative observation outright.
// Flooring input before the cache buckets is deliberate: the cache
// clamps below are all relative to InputTokens, so a negative prompt
// count has to become zero before they can mean anything.
//
// Applied by Tracker.AppendUsage and by Pricing.CostUSDForTurn, so
// tracker-backed and tracker-less call sites agree on what a turn cost.
func (u TurnUsage) Clamped() TurnUsage {
	if u.InputTokens < 0 {
		u.InputTokens = 0
	}
	if u.OutputTokens < 0 {
		u.OutputTokens = 0
	}
	if u.ThoughtsTokens < 0 {
		u.ThoughtsTokens = 0
	}
	if u.CachedInputTokens > u.InputTokens {
		u.CachedInputTokens = u.InputTokens
	}
	if u.CachedInputTokens < 0 {
		u.CachedInputTokens = 0
	}
	if room := u.InputTokens - u.CachedInputTokens; u.CacheCreationInputTokens > room {
		u.CacheCreationInputTokens = room
	}
	if u.CacheCreationInputTokens < 0 {
		u.CacheCreationInputTokens = 0
	}
	if u.CacheCreation1hInputTokens > u.CacheCreationInputTokens {
		u.CacheCreation1hInputTokens = u.CacheCreationInputTokens
	}
	if u.CacheCreation1hInputTokens < 0 {
		u.CacheCreation1hInputTokens = 0
	}
	return u
}

// UncachedInputTokens is the fresh-input remainder: the prompt minus
// what was served from cache and minus what was written to cache. Never
// negative (the buckets are clamped first).
func (u TurnUsage) UncachedInputTokens() int {
	c := u.Clamped()
	n := c.InputTokens - c.CachedInputTokens - c.CacheCreationInputTokens
	if n < 0 {
		return 0
	}
	return n
}

// UncachedInputTokens is the turn's fresh-input remainder: the prompt
// minus cache reads minus cache writes. Never negative. Wire
// projections use it instead of open-coding the subtraction, which is
// how the cache-write bucket got double-counted into "uncached" before
// #263.
func (t Turn) UncachedInputTokens() int {
	n := t.InputTokens - t.CachedInputTokens - t.CacheCreationInputTokens
	if n < 0 {
		return 0
	}
	return n
}

// Totals aggregates a slice of Turns. Cached / thoughts / tool-use
// mirror the Turn fields so callers projecting Totals into wire
// formats can render every dimension without walking All().
type Totals struct {
	Turns                    int
	InputTokens              int
	CachedInputTokens        int
	CacheCreationInputTokens int
	OutputTokens             int
	ThoughtsTokens           int
	ToolUseTokens            int
	CostUSD                  float64
	// UnpricedTurns counts turns whose model had no catalog rate, so
	// their contribution to CostUSD was 0 for lack of a price rather
	// than because the model is free. When > 0, CostUSD is a lower
	// bound and consumers should flag the total as incomplete (e.g.
	// "$X.YY+" or a "$—" marker) rather than presenting it as exact.
	// See #368.
	UnpricedTurns int
}

// UncachedInputTokens is the session's fresh-input remainder — the
// aggregate counterpart to Turn.UncachedInputTokens.
func (t Totals) UncachedInputTokens() int {
	n := t.InputTokens - t.CachedInputTokens - t.CacheCreationInputTokens
	if n < 0 {
		return 0
	}
	return n
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

	// Digest-savings counters (#223 Phase 4). Cumulative across the
	// session; rendered by /context. Populated via
	// AppendDigestSavings from the MCP wrap's OnResult callback. The
	// tracker stays pricing-lookup-agnostic — callers compute
	// SubagentCostUSD from their own pricing catalog before appending
	// so pkg/usage doesn't need a pricing import here.
	digestSavings DigestSavingsTotals
}

// DigestSavingsRecord is one per-call sample of the MCP digest wrap's
// effect on the parent's context. Aggregated into DigestSavingsTotals
// via Tracker.AppendDigestSavings; callers construct one per Process
// result the wrap hands back.
//
// Path mirrors digest.Method (structural_json / llm_fallback /
// passthrough). Passthrough records still flow through — a call the
// router decided to pass through verbatim IS a data point (told the
// operator "the wrap layer thought this was small enough to skip").
type DigestSavingsRecord struct {
	Path                 string
	ParentTokensSaved    int // max(0, OriginalTokensEst - DigestTokensEst)
	SubagentModel        string
	SubagentInputTokens  int
	SubagentOutputTokens int
	SubagentCostUSD      float64

	// The two cache buckets inside SubagentInputTokens, so a caller
	// charging this record to a session can rebuild the TurnUsage the
	// subagent actually spent instead of a flat uncached one (#771).
	SubagentCachedInputTokens        int
	SubagentCacheCreationInputTokens int

	// SubagentCacheCreation1hInputTokens is the 1-hour-TTL share of
	// the write bucket — a SUBSET of the field above, priced at 2x
	// base input rather than 1.25x (#770).
	SubagentCacheCreation1hInputTokens int

	// SubagentThoughtsTokens is the reasoning bucket, ADDITIVE to
	// SubagentOutputTokens and billed at the output rate (#927). A
	// sidecar from a producer predating that field carries zero, which
	// prices as it always did.
	SubagentThoughtsTokens int
}

// SubagentTurn rebuilds the [TurnUsage] the digest subagent spent, for
// callers that need to price or append it. Clamped, so a sidecar
// carrying contradictory buckets can only shrink the premium-rated
// write bucket rather than invent a negative uncached remainder.
func (r DigestSavingsRecord) SubagentTurn() TurnUsage {
	return TurnUsage{
		InputTokens:                r.SubagentInputTokens,
		CachedInputTokens:          r.SubagentCachedInputTokens,
		CacheCreationInputTokens:   r.SubagentCacheCreationInputTokens,
		CacheCreation1hInputTokens: r.SubagentCacheCreation1hInputTokens,
		OutputTokens:               r.SubagentOutputTokens,
		ThoughtsTokens:             r.SubagentThoughtsTokens,
	}.Clamped()
}

// DigestSavingsTotals is the cumulative session view rendered by
// /context and (when wired) OTel session-close attributes. Structural
// and agentic-path counts are broken out because their cost math
// differs (agentic pays a subagent bill, structural doesn't).
type DigestSavingsTotals struct {
	StructuralCalls          int
	StructuralTokensSaved    int
	AgenticCalls             int
	AgenticTokensSaved       int // parent-side tokens saved BEFORE subagent offset
	AgenticSubagentInTokens  int
	AgenticSubagentOutTokens int
	AgenticSubagentCostUSD   float64
	PassthroughCalls         int
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
// breakdown. Cost goes through CostUSDForTurn so all three input
// buckets — uncached, cache-read, cache-write — are billed at their own
// rates in the stored Turn, and thinking tokens at the output rate.
//
// The cache buckets are clamped into InputTokens — see
// TurnUsage.Clamped for why and in what order.
func (t *Tracker) AppendUsage(model string, u TurnUsage, p Pricing) Turn {
	u = u.Clamped()
	cost := p.CostUSDForTurn(u)
	turn := Turn{
		Model:                    model,
		InputTokens:              u.InputTokens,
		CachedInputTokens:        u.CachedInputTokens,
		CacheCreationInputTokens: u.CacheCreationInputTokens,
		OutputTokens:             u.OutputTokens,
		ThoughtsTokens:           u.ThoughtsTokens,
		ToolUseTokens:            u.ToolUseTokens,
		CostUSD:                  cost,
		At:                       time.Now(),
		Unpriced:                 p.Unpriced,
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
		out.CacheCreationInputTokens += x.CacheCreationInputTokens
		out.OutputTokens += x.OutputTokens
		out.ThoughtsTokens += x.ThoughtsTokens
		out.ToolUseTokens += x.ToolUseTokens
		out.CostUSD += x.CostUSD
		if x.Unpriced {
			out.UnpricedTurns++
		}
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
		cur.CacheCreationInputTokens += x.CacheCreationInputTokens
		cur.OutputTokens += x.OutputTokens
		cur.ThoughtsTokens += x.ThoughtsTokens
		cur.ToolUseTokens += x.ToolUseTokens
		cur.CostUSD += x.CostUSD
		if x.Unpriced {
			cur.UnpricedTurns++
		}
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

// AppendDigestSavings accumulates one MCP digest-wrap result into the
// session's cumulative counters. Negative ParentTokensSaved is
// clamped to zero — a "digest" longer than the original happens
// occasionally on the passthrough path when the wrap adds a
// truncation marker, and we don't want that to subtract from savings
// totals.
func (t *Tracker) AppendDigestSavings(rec DigestSavingsRecord) {
	saved := rec.ParentTokensSaved
	if saved < 0 {
		saved = 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	switch rec.Path {
	case "structural_json":
		t.digestSavings.StructuralCalls++
		t.digestSavings.StructuralTokensSaved += saved
	case "llm_fallback":
		t.digestSavings.AgenticCalls++
		t.digestSavings.AgenticTokensSaved += saved
		t.digestSavings.AgenticSubagentInTokens += rec.SubagentInputTokens
		t.digestSavings.AgenticSubagentOutTokens += rec.SubagentOutputTokens
		// Clamp like ParentTokensSaved above: a buggy (or hostile)
		// MCP savings sidecar reporting negative token counts must
		// not drag the cumulative spend down — the cost meter built
		// on this accumulator is a monotonic counter.
		if rec.SubagentCostUSD > 0 {
			t.digestSavings.AgenticSubagentCostUSD += rec.SubagentCostUSD
		}
	case "passthrough":
		t.digestSavings.PassthroughCalls++
	}
}

// DigestSavings returns the session-cumulative snapshot of the
// digest-wrap's effect. Safe to call from any goroutine.
func (t *Tracker) DigestSavings() DigestSavingsTotals {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.digestSavings
}
