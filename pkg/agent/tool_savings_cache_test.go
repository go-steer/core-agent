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

// #771: the digest-savings sidecar carries the subagent's cache
// buckets, and the session that pays for the digest prices a TURN
// rather than a token count.
//
// Every test here uses claude-haiku-4-5 — a real digest-tier model
// whose builtin rates make the two failure directions visible: cache
// reads are a tenth of the base input rate, cache writes a quarter
// more. Assertions compare against usage.Pricing rather than hard
// dollar figures, so a rate regen moves the expectation with the
// catalog instead of breaking the test.

package agent

import (
	"math"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/usage"
)

// digestModel is priced in the builtin catalog with a cache-read
// discount AND a cache-write premium, which is what lets these tests
// tell "priced the turn" apart from "priced the token count".
const digestModel = "claude-haiku-4-5"

// closeEnough compares dollars. Both sides run the same arithmetic in
// the same order, so this is float noise tolerance, not a fudge.
func closeEnough(a, b float64) bool { return math.Abs(a-b) < 1e-12 }

func cacheSidecar(in, cached, write, out int) map[string]any {
	return map[string]any{
		"path":                                 "llm_fallback",
		"original_tokens_est":                  float64(8000),
		"digest_tokens_est":                    float64(200),
		"subagent_model":                       digestModel,
		"subagent_input_tokens":                float64(in),
		"subagent_cached_input_tokens":         float64(cached),
		"subagent_cache_creation_input_tokens": float64(write),
		"subagent_output_tokens":               float64(out),
	}
}

// TestSavings_ACacheReadingDigestIsNotBilledAtTheUncachedRate is the
// bug. Nine tenths of this subagent's prompt was served from cache at
// a tenth of the price, and the old path re-derived cost from raw
// token counts alone — billing all 10,000 input tokens as fresh.
func TestSavings_ACacheReadingDigestIsNotBilledAtTheUncachedRate(t *testing.T) {
	t.Parallel()
	a := &Agent{tracker: usage.NewTracker()}
	a.observeToolSavings(mkFuncResponseEvent("gke_get_pods",
		cacheSidecar(10_000, 9_000, 0, 100)))

	p := usage.PriceFor(digestModel, nil)
	if p.IsZero() {
		t.Skipf("%s carries no builtin rate, so there is nothing to price against", digestModel)
	}
	want := p.CostUSDForTurn(usage.TurnUsage{
		InputTokens:       10_000,
		CachedInputTokens: 9_000,
		OutputTokens:      100,
	})
	flat := p.CostUSD(10_000, 100)

	got := a.tracker.DigestSavings().AgenticSubagentCostUSD
	if !closeEnough(got, want) {
		t.Errorf("AgenticSubagentCostUSD = %.9f, want %.9f (the cache-aware turn cost)", got, want)
	}
	if !(got < flat) {
		t.Errorf("cost %.9f is not below the all-uncached figure %.9f, so the cache read "+
			"was billed at the fresh-input rate", got, flat)
	}
}

// TestSavings_ACacheWritingDigestIsNotUnderBilled is the same defect
// pointing the other way. A cache write costs MORE than fresh input,
// so re-deriving from token counts undercharges — and an undercharge
// is the worse half: --max-session-cost-usd is the thing that stops a
// runaway, and a ceiling that reads low lets one run further.
func TestSavings_ACacheWritingDigestIsNotUnderBilled(t *testing.T) {
	t.Parallel()
	a := &Agent{tracker: usage.NewTracker()}
	a.observeToolSavings(mkFuncResponseEvent("gke_get_pods",
		cacheSidecar(10_000, 0, 10_000, 100)))

	p := usage.PriceFor(digestModel, nil)
	if p.IsZero() {
		t.Skipf("%s carries no builtin rate", digestModel)
	}
	want := p.CostUSDForTurn(usage.TurnUsage{
		InputTokens:              10_000,
		CacheCreationInputTokens: 10_000,
		OutputTokens:             100,
	})
	flat := p.CostUSD(10_000, 100)

	got := a.tracker.DigestSavings().AgenticSubagentCostUSD
	if !closeEnough(got, want) {
		t.Errorf("AgenticSubagentCostUSD = %.9f, want %.9f", got, want)
	}
	if !(got > flat) {
		t.Errorf("cost %.9f is not above the all-uncached figure %.9f, so the cache-write "+
			"premium went unbilled", got, flat)
	}
}

// TestSavings_TheSessionLedgerCarriesTheCacheBuckets. The savings
// block is a display surface; Totals() is what maybeEnforceCostCeiling
// reads. The turn appended there has to be the turn that ran, buckets
// and all — a session whose ledger says "10,000 fresh input tokens"
// prices its own ceiling wrong no matter what the sidecar knew.
func TestSavings_TheSessionLedgerCarriesTheCacheBuckets(t *testing.T) {
	t.Parallel()
	a := &Agent{tracker: usage.NewTracker()}
	a.observeToolSavings(mkFuncResponseEvent("gke_get_pods",
		cacheSidecar(10_000, 9_000, 500, 100)))

	totals := a.tracker.Totals()
	if totals.Turns != 1 {
		t.Fatalf("Totals().Turns = %d, want 1", totals.Turns)
	}
	if totals.CachedInputTokens != 9_000 {
		t.Errorf("Totals().CachedInputTokens = %d, want 9000", totals.CachedInputTokens)
	}
	if totals.CacheCreationInputTokens != 500 {
		t.Errorf("Totals().CacheCreationInputTokens = %d, want 500", totals.CacheCreationInputTokens)
	}
	if got, want := totals.UncachedInputTokens(), 500; got != want {
		t.Errorf("Totals().UncachedInputTokens() = %d, want %d", got, want)
	}

	p := usage.PriceFor(digestModel, nil)
	if p.IsZero() {
		t.Skipf("%s carries no builtin rate", digestModel)
	}
	want := p.CostUSDForTurn(usage.TurnUsage{
		InputTokens:              10_000,
		CachedInputTokens:        9_000,
		CacheCreationInputTokens: 500,
		OutputTokens:             100,
	})
	if !closeEnough(totals.CostUSD, want) {
		t.Errorf("Totals().CostUSD = %.9f, want %.9f — the ceiling reads this number",
			totals.CostUSD, want)
	}
}

// TestSavings_ASidecarWithNoCacheKeysPricesExactlyAsBefore. Structural
// callers, older daemons replaying a recording, and any provider
// without caching all produce a sidecar with no bucket keys. Absent
// and zero have to price identically — a turn with no cache activity
// genuinely has none, and this is the path every shipped in-tree
// caller takes today (#714 suppresses caching below three turns).
func TestSavings_ASidecarWithNoCacheKeysPricesExactlyAsBefore(t *testing.T) {
	t.Parallel()
	a := &Agent{tracker: usage.NewTracker()}
	a.observeToolSavings(mkFuncResponseEvent("gke_get_pods", map[string]any{
		"path":                   "llm_fallback",
		"original_tokens_est":    float64(8000),
		"digest_tokens_est":      float64(200),
		"subagent_model":         digestModel,
		"subagent_input_tokens":  float64(400),
		"subagent_output_tokens": float64(80),
	}))

	p := usage.PriceFor(digestModel, nil)
	if p.IsZero() {
		t.Skipf("%s carries no builtin rate", digestModel)
	}
	got := a.tracker.DigestSavings().AgenticSubagentCostUSD
	if want := p.CostUSD(400, 80); !closeEnough(got, want) {
		t.Errorf("a bucket-less sidecar priced %.9f, want the plain uncached %.9f", got, want)
	}
}

// TestSavings_ContradictoryBucketsCannotInventANegativeRemainder. The
// sidecar crosses JSON from a producer this process does not control,
// so the buckets are untrusted input. Clamping is what keeps a
// provider quirk (or a hand-edited recording) from pricing a negative
// number of fresh tokens; it can only shrink the premium-rated write
// bucket, which under-estimates rather than inventing a charge.
func TestSavings_ContradictoryBucketsCannotInventANegativeRemainder(t *testing.T) {
	t.Parallel()
	rec := usage.DigestSavingsRecord{
		SubagentInputTokens:              1_000,
		SubagentCachedInputTokens:        5_000, // more than the whole prompt
		SubagentCacheCreationInputTokens: 5_000,
		SubagentOutputTokens:             10,
	}
	turn := rec.SubagentTurn()
	if turn.CachedInputTokens != 1_000 {
		t.Errorf("CachedInputTokens = %d, want it clamped to the 1000-token prompt",
			turn.CachedInputTokens)
	}
	if turn.CacheCreationInputTokens != 0 {
		t.Errorf("CacheCreationInputTokens = %d, want 0 — no room left after the read bucket",
			turn.CacheCreationInputTokens)
	}
	if got := turn.UncachedInputTokens(); got != 0 {
		t.Errorf("UncachedInputTokens() = %d, want 0", got)
	}
}

// TestSavings_TheCacheBucketsReachTheRecordAtAll. The three tests
// above would all still pass if the extractor silently dropped the
// keys and the model happened to be unpriced. This one asks the
// extractor directly.
func TestSavings_TheCacheBucketsReachTheRecordAtAll(t *testing.T) {
	t.Parallel()
	ev := mkFuncResponseEvent("gke_get_pods", cacheSidecar(10_000, 9_000, 500, 100))
	rec, ok := extractSavingsRecord(ev.Content.Parts[0])
	if !ok {
		t.Fatal("extractSavingsRecord found no sidecar")
	}
	if rec.SubagentCachedInputTokens != 9_000 {
		t.Errorf("SubagentCachedInputTokens = %d, want 9000", rec.SubagentCachedInputTokens)
	}
	if rec.SubagentCacheCreationInputTokens != 500 {
		t.Errorf("SubagentCacheCreationInputTokens = %d, want 500",
			rec.SubagentCacheCreationInputTokens)
	}
}

// #770 extends the same sidecar with the 1-hour write share. A daemon
// configured for the 1-hour breakpoint writes at twice the base input
// rate rather than 1.25x, so a digest whose writes were all 1h and is
// billed at the 5m rate undercharges the paying session by 37.5% of
// its write bucket — the exact defect class #771 just closed, pointing
// the same way.
func TestSavings_AOneHourDigestIsNotBilledAtTheFiveMinuteRate(t *testing.T) {
	t.Parallel()
	sidecar := cacheSidecar(10_000, 0, 10_000, 100)
	sidecar["subagent_cache_creation_1h_input_tokens"] = float64(10_000)

	a := &Agent{tracker: usage.NewTracker()}
	a.observeToolSavings(mkFuncResponseEvent("gke_get_pods", sidecar))

	p := usage.PriceFor(digestModel, nil)
	if p.IsZero() || p.CacheCreation1hInputPerMTok == 0 {
		t.Skipf("%s carries no builtin 1h write rate", digestModel)
	}
	want := p.CostUSDForTurn(usage.TurnUsage{
		InputTokens:                10_000,
		CacheCreationInputTokens:   10_000,
		CacheCreation1hInputTokens: 10_000,
		OutputTokens:               100,
	})
	at5m := p.CostUSDForTurn(usage.TurnUsage{
		InputTokens:              10_000,
		CacheCreationInputTokens: 10_000,
		OutputTokens:             100,
	})

	got := a.tracker.DigestSavings().AgenticSubagentCostUSD
	if !closeEnough(got, want) {
		t.Errorf("AgenticSubagentCostUSD = %.9f, want %.9f (the 1h write rate)", got, want)
	}
	if !(got > at5m) {
		t.Errorf("cost %.9f is not above the all-5m figure %.9f, so the 1h premium went unbilled",
			got, at5m)
	}
	if totals := a.tracker.Totals(); !closeEnough(totals.CostUSD, want) {
		t.Errorf("Totals().CostUSD = %.9f, want %.9f — the ceiling reads this number",
			totals.CostUSD, want)
	}
}

// The extractor is the only thing standing between the sidecar key and
// the record; the test above would still pass if it dropped the key on
// an unpriced model.
func TestSavings_TheOneHourShareReachesTheRecord(t *testing.T) {
	t.Parallel()
	sidecar := cacheSidecar(10_000, 0, 500, 100)
	sidecar["subagent_cache_creation_1h_input_tokens"] = float64(300)

	rec, ok := extractSavingsRecord(mkFuncResponseEvent("gke_get_pods", sidecar).Content.Parts[0])
	if !ok {
		t.Fatal("extractSavingsRecord found no sidecar")
	}
	if rec.SubagentCacheCreation1hInputTokens != 300 {
		t.Errorf("SubagentCacheCreation1hInputTokens = %d, want 300",
			rec.SubagentCacheCreation1hInputTokens)
	}
	if rec.SubagentCacheCreationInputTokens != 500 {
		t.Errorf("the total write bucket read back as %d, want 500 — the 1h count is a "+
			"subset of it, not a fourth bucket", rec.SubagentCacheCreationInputTokens)
	}
}
