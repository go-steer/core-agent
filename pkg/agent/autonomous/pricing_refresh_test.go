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

package autonomous

import (
	"context"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/pricing"
	"github.com/go-steer/core-agent/v2/pkg/usage"
)

// #930. An autonomous loop is the longest-lived billing path in the
// tree — it runs for as long as events keep arriving — so the Pricing
// handed to WithTracker at configuration time is the most stale copy a
// /pricing refresh leaves behind.
//
// These tests install a process-global catalog and so are not parallel.

func installRate(t *testing.T, model string, in, out float64) {
	t.Helper()
	c, err := pricing.NewCatalog(pricing.Options{
		CfgOverride: map[string]pricing.ModelRates{
			model: {InputPerMTok: in, OutputPerMTok: out},
		},
	})
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	usage.SetCatalog(c)
}

// Pre-fix this bills at the configured 2/8 rate: $0.0018 instead of
// $0.0060.
func TestRunAutonomous_BillsTheRefreshedRateNotTheConfiguredOne(t *testing.T) {
	usage.SetCatalog(nil)
	t.Cleanup(func() { usage.SetCatalog(nil) })
	// stubLLM.Name() is "stub"; that is the key the driver bills under.
	installRate(t, "stub", 20, 20)

	llm := &stubLLM{scenarios: []scenarioFn{
		textTurn("a", 200, 100),
		doneCallTurn("done"),
		// The follow-up call the driver makes after the done tool runs.
		textTurn("ok", 10, 5),
	}}
	tracker := usage.NewTracker()
	stale := usage.Pricing{InputPerMTok: 2.0, OutputPerMTok: 8.0}

	res, err := Run(context.Background(), buildAgent(llm, "refresh"), "go",
		WithTracker(tracker, stale))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// (200+10) in + (100+5) out at $20/MTok both ways.
	wantIn, wantOut := 210, 105
	want := float64(wantIn+wantOut) * 20.0 / 1_000_000
	if got := tracker.Totals().CostUSD; !nearUSD(got, want) {
		t.Errorf("tracker billed $%.6f, want $%.6f at the refreshed rate (the configured 2/8 rate bills $%.6f)",
			got, want, stale.CostUSD(wantIn, wantOut))
	}
	// RunResult rolls up from the same per-turn record, so it has to
	// move with the ledger — a host reading only RunResult must not see
	// a different number from the one the cost ceiling enforces on.
	if !nearUSD(res.CostUSD, want) {
		t.Errorf("RunResult.CostUSD = $%.6f, want $%.6f", res.CostUSD, want)
	}
}

// The tracker-less arm prices into RunResult directly. A host that
// deliberately wired NO pricing must not start being billed just
// because a catalog happens to be installed in-process — IsZero is the
// host's "cost accounting off" signal and is still asked of the
// configured value, not of the refreshed one.
func TestRunAutonomous_NoPricingConfiguredStaysFree(t *testing.T) {
	usage.SetCatalog(nil)
	t.Cleanup(func() { usage.SetCatalog(nil) })
	installRate(t, "stub", 20, 20)

	llm := &stubLLM{scenarios: []scenarioFn{
		textTurn("a", 200, 100),
		doneCallTurn("done"),
		// The follow-up call the driver makes after the done tool runs.
		textTurn("ok", 10, 5),
	}}
	res, err := Run(context.Background(), buildAgent(llm, "nopricing"), "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.CostUSD != 0 {
		t.Errorf("CostUSD = %v, want 0 — an installed catalog must not switch on cost accounting a host never asked for", res.CostUSD)
	}
}

func nearUSD(got, want float64) bool {
	d := got - want
	if d < 0 {
		d = -d
	}
	return d < 1e-9
}
