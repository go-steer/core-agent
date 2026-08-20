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

package anthropic

import (
	"github.com/anthropics/anthropic-sdk-go"
)

// Anthropic prompt caching is a byte-exact PREFIX match over the
// rendered request, in the order tools → system → messages. A
// cache_control marker declares "the prefix ending here is worth
// keeping"; the next request that carries the identical prefix reads it
// at ~0.1x the input rate instead of paying full price again. Writing
// an entry costs 1.25x, so the break-even is two requests.
//
// core-agent's agentic loop is the ideal shape for this: the ADK runner
// replays the whole history on every model call, so request N+1's
// prefix is request N's entire prompt. Without breakpoints the
// transcript is re-billed at full rate on every turn, which is the
// quadratic input-cost curve #714 measured.

const (
	// maxCacheBreakpoints is Anthropic's hard per-request limit. A
	// fifth marker is a 400, not a silent drop.
	maxCacheBreakpoints = 4

	// historyBreakpointStride is how many unmarked content blocks may
	// sit between two rolling breakpoints — so consecutive markers land
	// 16 blocks apart.
	//
	// A breakpoint searches backward at most 20 content blocks for an
	// existing cache entry. One agentic step can append more than 20
	// blocks (a fan-out of parallel tool calls plus their results), and
	// when it does, a single marker at the very end can no longer see
	// the previous turn's entry: the read silently misses and the whole
	// prefix is re-written at 1.25x — worse than not caching at all. A
	// 16-block spacing keeps every marker inside the next one's lookback
	// window with room to spare, so the chain back to the previous
	// turn's entry holds.
	//
	// Extra markers are close to free: cache writes are billed for the
	// tokens after the longest entry that already exists, so N markers
	// over one turn's new blocks cost the same as one marker at the
	// end. What they buy is that the chain doesn't break.
	historyBreakpointStride = 15
)

// applyCacheBreakpoints stamps the request's cache_control markers
// according to opts and returns how many it placed.
//
// Order matters: the system marker is placed first because it sits
// earliest in the rendered prefix and is the one that pays off across
// *sessions*, not just across turns — it covers the tool schemas and
// the persona, which are identical for every session on the same
// build. History markers then take whatever is left of the budget.
//
// Every marker carries opts.TTL — 5 minutes unless the operator asked
// for the 1-hour breakpoint. The two are billed differently (1.25x vs
// 2x base input) and the response reports the split under
// usage.cache_creation, which stream.go forwards so the cost meter can
// price each share at its own rate (#770).
func applyCacheBreakpoints(params *anthropic.MessageNewParams, opts CacheOptions) int {
	placed := 0
	if opts.System && len(params.System) > 0 {
		params.System[len(params.System)-1].CacheControl = opts.cacheControl()
		placed++
	}
	if opts.History {
		placed += markHistoryBreakpoints(params.Messages, maxCacheBreakpoints-placed, opts.cacheControl())
	}
	return placed
}

// reapplyCacheBreakpoints re-places the markers on a request that grew
// after buildParams returned, and returns how many it placed.
//
// The pause_turn continuation appends the paused assistant turn — which
// carries the server-side tool blocks, easily thousands of tokens — to a
// request that has already been marked. Those blocks land after every
// existing marker, so without this the continuation re-sends them at
// full rate and the tail marker drifts backward out of the next
// request's 20-block lookback.
//
// Clearing first is not optional: applyCacheBreakpoints only adds, so a
// second pass over an already-marked request would stamp up to four MORE
// markers and the API would reject the request with a 400.
func reapplyCacheBreakpoints(params *anthropic.MessageNewParams, opts CacheOptions) int {
	clearCacheBreakpoints(params)
	return applyCacheBreakpoints(params, opts)
}

// clearCacheBreakpoints strips every marker from the request. The zero
// CacheControlEphemeralParam serializes as absent, which is what
// "unmarked" means on the wire.
func clearCacheBreakpoints(params *anthropic.MessageNewParams) {
	for i := range params.System {
		params.System[i].CacheControl = anthropic.CacheControlEphemeralParam{}
	}
	for _, msg := range params.Messages {
		for _, block := range msg.Content {
			if cc := block.GetCacheControl(); cc != nil {
				*cc = anthropic.CacheControlEphemeralParam{}
			}
		}
	}
}

// markHistoryBreakpoints walks the conversation backward, marking the
// last cacheable block and then one every historyBreakpointStride
// blocks, until budget is exhausted or the history runs out. Returns
// the number of markers placed.
//
// Walking from the END is what makes the scheme roll: the marker
// positions move forward with the conversation, so each request writes
// an entry covering everything up to now, and the next request reads
// it. Marking from the front instead would pin the cache at a fixed
// early position and re-bill the growing tail forever — the exact
// failure #714 reported against the Vertex context cache, whose cached
// prefix stayed frozen at 15,192 tokens while input grew to 58K.
//
// Blocks that can't carry cache_control (thinking blocks — the API has
// no field for it there) are skipped as marker sites but still counted
// toward the stride, because the 20-block lookback counts every content
// block regardless of type.
func markHistoryBreakpoints(msgs []anthropic.MessageParam, budget int, cc anthropic.CacheControlEphemeralParam) int {
	if budget <= 0 {
		return 0
	}
	placed, sinceMark := 0, 0
	want := true
	for i := len(msgs) - 1; i >= 0; i-- {
		blocks := msgs[i].Content
		for j := len(blocks) - 1; j >= 0; j-- {
			if want && setCacheControl(blocks[j], cc) {
				placed++
				if placed == budget {
					return placed
				}
				want, sinceMark = false, 0
				continue
			}
			sinceMark++
			if sinceMark >= historyBreakpointStride {
				want = true
			}
		}
	}
	return placed
}

// setCacheControl stamps an ephemeral marker on one content block,
// reporting false for block types the API doesn't accept one on.
//
// Safe to call with a copy of the union: every variant is a pointer, so
// GetCacheControl hands back a pointer into the shared underlying
// struct rather than into the copy.
func setCacheControl(block anthropic.ContentBlockParamUnion, marker anthropic.CacheControlEphemeralParam) bool {
	cc := block.GetCacheControl()
	if cc == nil {
		return false
	}
	*cc = marker
	return true
}
