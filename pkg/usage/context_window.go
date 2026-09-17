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
	"strings"

	"github.com/go-steer/core-agent/v2/pkg/pricing"
)

// ContextWindowSize returns the model's max input window from a
// hardcoded table, keyed on the most recent turn's model name. Returns
// 0 for unknown models (or when no turn has landed yet) — consumers
// should treat 0 as "unknown; suppress any per-context UI segment and
// skip threshold-based behaviors like compaction." See
// contextWindowSizeFor for the lookup table.
//
// Lifted from cmd/core-agent/coretui_enabled.go where it was first
// implemented as part of the core-tui adapter tier-3+ work
// (commit be8dae5). Agent-level code (compaction trigger,
// micro-subagents) needs the same accessor, so it lives on the
// substrate type rather than the adapter bridge.
func (t *Tracker) ContextWindowSize() int {
	last, ok := t.Last()
	if !ok {
		return 0
	}
	return contextWindowSizeFor(last.Model)
}

// ContextWindowUsed approximates the current context fill as the most
// recent model call's input-token count PLUS an estimate of everything
// appended to history since that call (#975). Each call re-sends the
// full conversation, so the input count is the rolling context size —
// but only as of the last call, and the gap matters.
//
// The gap is the whole bug. The sequence inside one agentic turn is:
// model call N completes and commits its usage, a tool runs and returns
// a payload, model call N+1 is built with that payload included. Nothing
// evaluates context between the second and third steps, so a single
// large tool result — a full pod log, a wide `kubectl get -o json`, an
// MCP response over a big cluster — takes the next request over the
// window while the utilization figure compaction and the UI both read
// still reflects the comfortable state before it arrived. On the GKE
// path large read results are the normal case, not an edge case.
//
// Returns 0 before any turn has landed (matches "unknown" semantics —
// consumers should suppress the segment). Bytes appended before the
// first turn are deliberately not reported as usage on their own: with
// no measured floor to add them to, the estimate alone would be a number
// with no relationship to the window.
func (t *Tracker) ContextWindowUsed() int {
	used, _ := t.ContextWindowUsedEstimated()
	return used
}

// ContextWindowUsedEstimated is ContextWindowUsed plus whether the
// answer includes an estimate, i.e. whether anything has been appended
// to history since the last measured input count.
//
// Exported separately because the assessment's note on this is right: a
// utilization number that can only be stale is worse than one that
// admits it is estimated. A surface rendering a percentage should say
// which of the two it has. Callers that only need the number keep
// calling ContextWindowUsed.
func (t *Tracker) ContextWindowUsedEstimated() (used int, estimated bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.turns) == 0 {
		return 0, false
	}
	measured := t.turns[len(t.turns)-1].InputTokens
	if t.pendingContextBytes <= 0 {
		return measured, false
	}
	return measured + EstimateTokensFromBytes(t.pendingContextBytes), true
}

// AddPendingContextBytes records that n bytes of content have been
// appended to the conversation since the last measured input count.
// Cleared by the next AppendUsage, whose InputTokens already counts
// them. Negative and zero values are ignored.
func (t *Tracker) AddPendingContextBytes(n int) {
	if t == nil || n <= 0 {
		return
	}
	t.mu.Lock()
	t.pendingContextBytes += n
	t.mu.Unlock()
}

// PendingContextBytes reports the unmeasured byte count currently
// folded into ContextWindowUsed. For tests and for an operator surface
// that wants to show the size of the estimate rather than only its
// existence.
func (t *Tracker) PendingContextBytes() int {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.pendingContextBytes
}

// BytesPerTokenEstimate converts appended bytes to tokens for the
// unmeasured part of ContextWindowUsed. Three, not the usual four.
//
// The usual English-prose rule of thumb is ~4 bytes per token, and using
// it here would be wrong in the unsafe direction twice over. First,
// what is being estimated is overwhelmingly not prose: it is JSON, YAML
// and log output, which tokenize far worse than prose because
// punctuation, indentation and long unbroken identifiers each cost a
// token. Second, the asymmetry — underestimating means the threshold
// does not fire and the defect this exists to fix survives, while
// overestimating means compaction fires somewhat early, which costs a
// summarizer call and is immediately visible. Three is a deliberate
// over-count for the payload shapes that actually blow the window, and
// it is only ever applied to the unmeasured tail: the moment the next
// model call lands, a measured number replaces it.
const BytesPerTokenEstimate = 3

// EstimateTokensFromBytes converts a byte count to an estimated token
// count. Exported because the agent-side budget check reports the
// estimate in operator-facing text and should not re-derive the ratio.
func EstimateTokensFromBytes(n int) int {
	if n <= 0 {
		return 0
	}
	return n / BytesPerTokenEstimate
}

// ContextWindowSizeFor returns the max input window for model, or 0
// when it isn't known.
//
// Two tiers, in order:
//
//  1. pricing.BuiltinContextWindow — LiteLLM's max_input_tokens for
//     every model we ship a rate for, generated by
//     dev/regen-builtin-pricing. Exact match on the lowercased id.
//  2. The substring table below, for ids LiteLLM never publishes:
//     long-context "-1m" suffixes, Vertex publication names, and
//     anything an operator pins that upstream hasn't catalogued.
//
// The generated tier exists because the substring table was wrong and
// nobody noticed: it claimed gemini-2.5-pro held 2,000,000 input
// tokens — Gemini 1.5 Pro's number — against a real 1,048,576 cap.
// Mid-tier compaction fires at 0.65 of the window, so it was scheduled
// for ~1.3M tokens on a session that the provider would have hard-
// failed first. Generated numbers can't rot that way.
//
// Exported as a package-level function so callers that have a model
// name in hand (without going through the Tracker) can resolve it
// directly. The Tracker methods above are the common path.
func ContextWindowSizeFor(model string) int { return contextWindowSizeFor(model) }

func contextWindowSizeFor(model string) int {
	// Case-insensitive like modeltier.Classify and pricing.Lookup —
	// the three tables resolve the same operator-typed ids, and this
	// one returning the 0 sentinel on "GEMINI-3.5-FLASH" would
	// silently disable threshold-based compaction for the session.
	model = strings.ToLower(model)

	// Generated table first: it is the authority for anything in the
	// pricing catalog. The fallback below stays deliberately
	// coarse-grained (whole families, not exact ids), so an exact hit
	// here is always the better answer.
	if n, ok := pricing.BuiltinContextWindow(model); ok {
		return n
	}

	switch {
	case containsAny(model, "gemini-3.1-pro", "gemini-3-pro"):
		return 1_000_000
	case containsAny(model, "gemini-3.8-flash", "gemini-3.7-flash", "gemini-3.6-flash", "gemini-3.5-flash", "gemini-3-flash", "gemini-3.1-flash"):
		return 1_000_000
	case containsAny(model, "gemini-2.5-pro"):
		// 1,048,576, not the 2,000,000 this said until #774. Two
		// million was Gemini 1.5 Pro's window and got carried forward
		// by hand; 2.5 Pro caps at 2^20 input tokens. The old number
		// pushed the mid-tier compaction trigger (0.65) out to ~1.3M,
		// i.e. past a limit the provider hard-fails on, so a long
		// session died on a context-length error instead of compacting.
		return 1_048_576
	case containsAny(model, "gemini-2.5-flash", "gemini-2.0-flash"):
		return 1_000_000
	case containsAny(model,
		"claude-opus-4-6", "claude-opus-4-7", "claude-opus-4-8",
		"claude-sonnet-4-6"):
		// Current-gen Claude 4.6+ Opus/Sonnet ship a 1M context window
		// by default — the "-1m" suffix predates the window becoming
		// standard, so these no longer need it. Must stay ABOVE the
		// generic "claude-*-4" case below, which those IDs also match.
		return 1_000_000
	case containsAny(model, "claude-fable-5", "claude-mythos", "claude-opus-5", "claude-sonnet-5"):
		// Claude 5 family (incl. the Mythos-class tier, which LiteLLM
		// publishes as both claude-fable-5 and claude-mythos-*): 1M
		// context. Without an explicit case these fall to the
		// conservative unknown-Claude 200K below.
		return 1_000_000
	case containsAny(model, "claude-opus-4", "claude-sonnet-4", "claude-haiku-4"):
		// Earlier Claude 4.x, reached only by ids the catalog does not
		// publish. Opus 4.0/4.1/4.5 and Haiku 4.x really are 200K; the
		// Sonnet 4/4.5 line went to 1M upstream on 2026-09-17, and
		// every spelling LiteLLM publishes for it answers from the
		// generated tier above — so a Sonnet id that gets this far is
		// one nobody has published a window for, and 200K is the
		// deliberate answer to that rather than a claim about the
		// model: under-estimating compacts early, over-estimating dies
		// on a context-length error. The "-1m" suffix is still honored
		// when an operator spells one out, even though upstream no
		// longer carries any such key.
		if containsAny(model, "-1m") {
			return 1_000_000
		}
		return 200_000
	case containsAny(model, "claude"):
		// Unknown Claude model: default conservatively to 200K so
		// compaction fires early rather than overshooting a real
		// window that turns out smaller than assumed.
		return 200_000
	}
	return 0
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
