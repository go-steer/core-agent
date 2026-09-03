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
	"testing"

	"google.golang.org/genai"
)

// thinkingPricing is a Gemini-shaped rate card: thoughts have no rate
// of their own anywhere in the industry, so a bucket landing on the
// wrong side of the input/output split shows up as a 4x error.
var thinkingPricing = Pricing{
	InputPerMTok:       1.25,
	CachedInputPerMTok: 0.3125,
	OutputPerMTok:      5.00,
}

// The reason the term exists: Gemini reports thoughts as a bucket
// ADDITIVE to candidates, and bills them at the output rate. The
// fixture is one real turn's metadata — 12455 + 85 + 570 = 13110,
// totalTokenCount agreeing is what proves the bucket is not a subset of
// either neighbour — so a regression that folds thoughts back into
// "reported but not billed" fails here rather than in a monthly invoice.
func TestCostUSDForTurn_PricesThoughtsAtTheOutputRate(t *testing.T) {
	t.Parallel()
	u := TurnUsageFromGenaiMetadata(&genai.GenerateContentResponseUsageMetadata{
		PromptTokenCount:     12_455,
		CandidatesTokenCount: 85,
		ThoughtsTokenCount:   570,
		TotalTokenCount:      13_110,
	})
	if u.InputTokens+u.OutputTokens+u.ThoughtsTokens != 13_110 {
		t.Fatalf("fixture doesn't add up: %+v", u)
	}

	got := thinkingPricing.CostUSDForTurn(u)
	want := (12_455/1e6)*1.25 + ((85+570)/1e6)*5.00
	if !nearUSD(got, want) {
		t.Errorf("turn cost = %v, want %v (85 candidate + 570 thought tokens, both at the output rate)", got, want)
	}

	// The pre-fix number, spelled out: thoughts dropped on the floor.
	// Asserting the gap and not just the total keeps the test honest if
	// someone "fixes" want to match a broken implementation.
	unpriced := (12_455/1e6)*1.25 + (85/1e6)*5.00
	if got <= unpriced {
		t.Errorf("turn cost %v is no higher than the thoughts-unpriced %v; the thinking bucket is still free", got, unpriced)
	}
}

// The undercount is not a rounding error on a thinking model. Measured
// on a real agentic turn: 6,449 thought tokens against 1,180 candidate
// tokens — 85% of the billable output — and the same figure feeds
// --max-session-cost-usd, so the ceiling was letting a run spend
// several times its budget before tripping.
func TestCostUSDForTurn_ThoughtsDominateAgenticOutput(t *testing.T) {
	t.Parallel()
	u := TurnUsage{InputTokens: 100_000, OutputTokens: 1_180, ThoughtsTokens: 6_449}

	withThoughts := thinkingPricing.CostUSDForTurn(u)
	noThoughts := thinkingPricing.CostUSDForTurn(TurnUsage{InputTokens: 100_000, OutputTokens: 1_180})

	outputBill := withThoughts - noThoughts
	if !nearUSD(outputBill, (6_449/1e6)*5.00) {
		t.Errorf("thoughts contributed %v, want %v", outputBill, (6_449/1e6)*5.00)
	}
	if withThoughts <= noThoughts {
		t.Fatalf("thinking turn (%v) priced no higher than the same turn without thoughts (%v)", withThoughts, noThoughts)
	}
}

// Anthropic bills thinking inside output_tokens, which
// pkg/models/anthropic maps to CandidatesTokenCount while leaving
// ThoughtsTokenCount unset. Adding the bucket therefore adds zero on
// that path — this pins that the fix is provider-safe rather than a
// double-charge for every Claude turn.
func TestCostUSDForTurn_AnthropicShapeIsUnchanged(t *testing.T) {
	t.Parallel()
	// The shape usageMetadata produces: all three input buckets folded
	// into the prompt count, thinking already inside the candidates
	// count, no thoughts bucket at all.
	anthropicShape := TurnUsageFromGenaiMetadata(&genai.GenerateContentResponseUsageMetadata{
		PromptTokenCount:        25_000,
		CachedContentTokenCount: 20_000,
		CandidatesTokenCount:    3_000, // text + thinking, as Anthropic bills it
	})
	if anthropicShape.ThoughtsTokens != 0 {
		t.Fatalf("ThoughtsTokens = %d; the Anthropic producer never sets it, so this fixture is wrong", anthropicShape.ThoughtsTokens)
	}
	got := thinkingPricing.CostUSDForTurn(anthropicShape)
	want := thinkingPricing.CostUSDWithCache(5_000, 20_000, 3_000)
	if !nearUSD(got, want) {
		t.Errorf("turn cost = %v, want %v — the thoughts term must be inert when the provider folds thinking into output", got, want)
	}
}

// A provider (or a JSON sidecar) reporting a negative count must not be
// able to subtract from the bill. Owned by TurnUsage.Clamped rather
// than by the pricing expression, because cost was never the only
// reader: the same numbers are summed into Totals, rendered by /usage,
// and observed on gen_ai.client.token.usage, which is a MONOTONIC
// counter — a guard local to the price would have kept the negative out
// of the invoice and left it everywhere else.
func TestCostUSDForTurn_NegativeCountsCannotDiscountATurn(t *testing.T) {
	t.Parallel()
	honest := thinkingPricing.CostUSDForTurn(TurnUsage{InputTokens: 1_000, OutputTokens: 500})
	for name, u := range map[string]TurnUsage{
		"thoughts": {InputTokens: 1_000, OutputTokens: 500, ThoughtsTokens: -400},
		"output":   {InputTokens: 1_000, OutputTokens: -500},
		"input":    {InputTokens: -1_000, OutputTokens: 500},
	} {
		if got := thinkingPricing.CostUSDForTurn(u); got < 0 {
			t.Errorf("%s: turn cost = %v, want no less than zero", name, got)
		}
	}
	got := thinkingPricing.CostUSDForTurn(TurnUsage{InputTokens: 1_000, OutputTokens: 500, ThoughtsTokens: -400})
	if !nearUSD(got, honest) {
		t.Errorf("turn cost with negative thoughts = %v, want %v (unchanged)", got, honest)
	}
}

// The clamp is on the struct and not on the price, so the ledger and
// the metrics series see the floored numbers too. Pinned separately
// from the cost assertion above because they are different consumers:
// AppendUsage stores what Clamped returns, Totals sums the stored
// fields, and pkg/usage/metrics feeds them to an
// Int64ObservableCounter that rejects a negative observation outright.
func TestTracker_NegativeCountsNeverReachTheLedger(t *testing.T) {
	t.Parallel()
	tr := NewTracker()
	tr.AppendUsage("gemini-3.1-pro", TurnUsage{
		InputTokens:    1_000,
		OutputTokens:   -500,
		ThoughtsTokens: -400,
	}, thinkingPricing)

	tot := tr.Totals()
	if tot.OutputTokens < 0 || tot.ThoughtsTokens < 0 || tot.InputTokens < 0 {
		t.Errorf("rollup = %d in / %d out / %d thoughts, want none of them negative",
			tot.InputTokens, tot.OutputTokens, tot.ThoughtsTokens)
	}
	if tot.CostUSD < 0 {
		t.Errorf("session cost = %v, want no less than zero: a negative buys headroom under --max-session-cost-usd", tot.CostUSD)
	}
}

// The tracker charges the same as the tracker-less path — the whole
// point of routing both through CostUSDForTurn — and it still REPORTS
// the two buckets separately. Billing thoughts as output must not
// quietly move them into Totals.OutputTokens, which /usage and the
// gen_ai.token.type=thoughts metric series read.
func TestTracker_ThoughtsAreBilledAsOutputButReportedApart(t *testing.T) {
	t.Parallel()
	u := TurnUsage{InputTokens: 12_455, OutputTokens: 85, ThoughtsTokens: 570}

	tr := NewTracker()
	turn := tr.AppendUsage("gemini-3.1-pro", u, thinkingPricing)
	if !nearUSD(turn.CostUSD, thinkingPricing.CostUSDForTurn(u)) {
		t.Errorf("tracker charged %v, CostUSDForTurn says %v; the two must agree", turn.CostUSD, thinkingPricing.CostUSDForTurn(u))
	}
	// Agreement with CostUSDForTurn is necessary and not sufficient: on
	// the pre-fix code both sides drop thoughts and agree exactly. So the
	// absolute figure is pinned too, or this test is green against the
	// bug it was written for.
	if want := (12_455/1e6)*1.25 + ((85+570)/1e6)*5.00; !nearUSD(turn.CostUSD, want) {
		t.Errorf("tracker charged %v, want %v with the thinking bucket billed as output", turn.CostUSD, want)
	}

	tot := tr.Totals()
	if tot.OutputTokens != 85 || tot.ThoughtsTokens != 570 {
		t.Errorf("rollup = %d output / %d thoughts, want 85 / 570 kept in their own buckets", tot.OutputTokens, tot.ThoughtsTokens)
	}
}
