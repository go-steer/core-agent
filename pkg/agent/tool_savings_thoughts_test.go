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

// #927, sidecar half. Billing thinking tokens at the output rate fixes
// the parent's own turns; the digest subagent reaches the session's
// ledger by a different route entirely — a JSON sidecar on the tool
// result, re-priced on the far side — and that route carried no
// thoughts bucket. Post-#717 it is the ONLY route by which a digest's
// spend lands on the calling session's books and under its cost
// ceiling, so a missing bucket there is not a display bug.

package agent

import (
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/usage"
)

// recordFromSidecar runs a sidecar map through the same extraction the
// observer uses, so these tests exercise the JSON hop rather than a
// hand-built record.
func recordFromSidecar(t *testing.T, sv map[string]any) usage.DigestSavingsRecord {
	t.Helper()
	ev := mkFuncResponseEvent("gke_get_pods", sv)
	rec, ok := extractSavingsRecord(ev.Content.Parts[0])
	if !ok {
		t.Fatal("sidecar not parsed")
	}
	return rec
}

// thinkingSidecar is a digest subagent that reasoned far more than it
// answered — the ordinary shape on a small-tier thinking model asked to
// compress a wall of prose.
func thinkingSidecar(in, out, thoughts int) map[string]any {
	return map[string]any{
		"path":                     "llm_fallback",
		"original_tokens_est":      float64(8000),
		"digest_tokens_est":        float64(200),
		"subagent_model":           digestModel,
		"subagent_input_tokens":    float64(in),
		"subagent_output_tokens":   float64(out),
		"subagent_thoughts_tokens": float64(thoughts),
	}
}

// The bug: RunSubtask priced the subagent's thinking correctly, that
// figure did not cross the sidecar (by design — the far side re-prices
// so historical digests re-price when rates change), and the far side
// had no field to re-price it from. Two numbers for one turn, and the
// ledger got the low one.
func TestSavings_AThinkingDigestIsNotBilledForItsVisibleHalfOnly(t *testing.T) {
	t.Parallel()
	rec := recordFromSidecar(t, thinkingSidecar(4_000, 120, 5_000))
	if rec.SubagentThoughtsTokens != 5_000 {
		t.Fatalf("SubagentThoughtsTokens = %d, want 5000 read off the sidecar", rec.SubagentThoughtsTokens)
	}

	p := usage.PriceFor(digestModel, nil)
	if p.IsZero() {
		t.Skipf("%s carries no builtin rate, so there is nothing to price against", digestModel)
	}
	// The whole turn: thinking billed at the output rate on top of the
	// candidates it is additive to.
	want := p.CostUSDForTurn(usage.TurnUsage{InputTokens: 4_000, OutputTokens: 5_120})
	if !closeEnough(rec.SubagentCostUSD, want) {
		t.Errorf("charged %v, want %v", rec.SubagentCostUSD, want)
	}
	// And the failure spelled out, so a regression that drops the bucket
	// again fails here rather than in an invoice: 5,000 of the 5,120
	// output tokens were the thinking.
	visibleOnly := p.CostUSDForTurn(usage.TurnUsage{InputTokens: 4_000, OutputTokens: 120})
	if rec.SubagentCostUSD <= visibleOnly {
		t.Errorf("charged %v, no more than the thoughts-free %v — the reasoning is still free", rec.SubagentCostUSD, visibleOnly)
	}
}

// A sidecar written by a daemon that predates the field carries no
// thoughts key, and must price exactly as it always did rather than
// blowing up or inventing a bucket. savingsIntField returns 0 for
// missing, which is the same thing a genuinely thought-free turn
// reports — correctly, because those two cost the same.
func TestSavings_ASidecarWithoutTheThoughtsKeyPricesAsBefore(t *testing.T) {
	t.Parallel()
	old := thinkingSidecar(4_000, 120, 0)
	delete(old, "subagent_thoughts_tokens")

	rec := recordFromSidecar(t, old)
	if rec.SubagentThoughtsTokens != 0 {
		t.Errorf("SubagentThoughtsTokens = %d, want 0 for a producer that never wrote the key", rec.SubagentThoughtsTokens)
	}
	want := usage.PriceFor(digestModel, nil).CostUSDForTurn(usage.TurnUsage{InputTokens: 4_000, OutputTokens: 120})
	if !closeEnough(rec.SubagentCostUSD, want) {
		t.Errorf("charged %v, want the unchanged %v", rec.SubagentCostUSD, want)
	}
}

// A hostile or buggy sidecar cannot buy headroom under the session's
// cost ceiling by reporting negative reasoning. Clamped owns this — the
// point of the test is that SubagentTurn routes through it.
func TestSavings_NegativeThoughtsInTheSidecarCannotDiscountTheDigest(t *testing.T) {
	t.Parallel()
	rec := usage.DigestSavingsRecord{
		SubagentInputTokens:    4_000,
		SubagentOutputTokens:   120,
		SubagentThoughtsTokens: -9_000,
	}
	if got := rec.SubagentTurn().ThoughtsTokens; got != 0 {
		t.Errorf("SubagentTurn().ThoughtsTokens = %d, want 0", got)
	}
	p := usage.PriceFor(digestModel, nil)
	honest := p.CostUSDForTurn(usage.TurnUsage{InputTokens: 4_000, OutputTokens: 120})
	if got := p.CostUSDForTurn(rec.SubagentTurn()); !closeEnough(got, honest) {
		t.Errorf("charged %v, want the undiscounted %v", got, honest)
	}
}
