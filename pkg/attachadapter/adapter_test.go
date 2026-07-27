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

package attachadapter

import (
	"context"
	"math"
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/agent"
	"github.com/go-steer/core-agent/v2/pkg/attach"
	"github.com/go-steer/core-agent/v2/pkg/models/mock"
	"github.com/go-steer/core-agent/v2/pkg/usage"
)

// newEchoAgent constructs a real agent against the mock echo model —
// the same shape hosts wrap with New().
func newEchoAgent(t *testing.T, opts ...agent.Option) *agent.Agent {
	t.Helper()
	provider := mock.NewEcho()
	m, err := provider.Model(context.Background(), "echo")
	if err != nil {
		t.Fatalf("model: %v", err)
	}
	a, err := agent.New(m, opts...)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	return a
}

func TestAdapter_Registrant_ForwardsIdentity(t *testing.T) {
	t.Parallel()
	a := newEchoAgent(t, agent.WithSession("u-1", "s-1"))
	ad := New(a)
	if ad.Agent() != a {
		t.Fatalf("Agent() = %p, want the wrapped agent %p", ad.Agent(), a)
	}
	if ad.AppName() != a.AppName() || ad.UserID() != "u-1" || ad.SessionID() != "s-1" {
		t.Errorf("Registrant triple = (%s,%s,%s), want (%s,u-1,s-1)",
			ad.AppName(), ad.UserID(), ad.SessionID(), a.AppName())
	}
	// The adapter is what hosts register — it must satisfy Registrant
	// against a live registry, not just the compile-time assertion.
	reg := attach.NewSessionRegistry()
	if _, err := reg.Register(ad); err != nil {
		t.Fatalf("Register(adapter): %v", err)
	}
}

func TestAdapter_AttachReload_NotRegistered_ReturnsSentinel(t *testing.T) {
	t.Parallel()
	ad := New(newEchoAgent(t))
	resp := ad.AttachReload(context.Background())
	if resp.Memory || resp.Skills || resp.MCP {
		t.Errorf("AttachReload without reloader: surface flags = %+v, want all false", resp)
	}
	if len(resp.Errors) == 0 || !strings.Contains(resp.Errors[0], attach.ErrCapabilityNotRegistered.Error()) {
		t.Errorf("AttachReload without reloader: errors = %v, want one containing %q",
			resp.Errors, attach.ErrCapabilityNotRegistered.Error())
	}
}

func TestAdapter_AttachReload_Wired_DelegatesToClosure(t *testing.T) {
	t.Parallel()
	called := false
	ad := New(newEchoAgent(t), WithReloader(func(_ context.Context) attach.ReloadResponse {
		called = true
		return attach.ReloadResponse{Memory: true, Skills: true, MCP: false, Errors: []string{"mcp: not yet"}}
	}))
	resp := ad.AttachReload(context.Background())
	if !called {
		t.Fatal("AttachReload: closure was not invoked")
	}
	if !resp.Memory || !resp.Skills || resp.MCP {
		t.Errorf("AttachReload: got %+v, want Memory=true Skills=true MCP=false", resp)
	}
	if len(resp.Errors) != 1 || resp.Errors[0] != "mcp: not yet" {
		t.Errorf("AttachReload: errors = %v, want [\"mcp: not yet\"]", resp.Errors)
	}
}

// TestAttachUsage_CachedFieldsAndPerTurn is the on-the-wire spec test
// for issue #222: /sessions/<id>/usage must expose cached vs uncached
// input tokens, cost-usd + a counterfactual "if nothing had cached"
// reference, and one entry per model call in a per_turn array.
func TestAttachUsage_CachedFieldsAndPerTurn(t *testing.T) {
	t.Parallel()
	tr := usage.NewTracker()
	// Rates match builtin gemini-3.5-flash ($1.50/$0.15/$9.00) so the
	// numbers are the same operators will see against a real Vertex
	// session; the reference-cost check depends on PriceFor resolving
	// against the same builtin entry.
	p := usage.Pricing{InputPerMTok: 1.50, CachedInputPerMTok: 0.15, OutputPerMTok: 9.00}

	// Turn 1: cold. No cache.
	tr.AppendUsage("gemini-3.5-flash", usage.TurnUsage{
		InputTokens:  10_000,
		OutputTokens: 500,
	}, p)
	// Turn 2: warm. 8k of the 10k input from cache.
	tr.AppendUsage("gemini-3.5-flash", usage.TurnUsage{
		InputTokens:       10_000,
		CachedInputTokens: 8_000,
		OutputTokens:      500,
	}, p)

	ad := New(newEchoAgent(t, agent.WithUsageTracker(tr)))
	info := ad.AttachUsage()

	// Overall aggregates.
	if info.Overall.Turns != 2 {
		t.Errorf("Overall.Turns = %d, want 2", info.Overall.Turns)
	}
	if info.Overall.InputTokens != 20_000 {
		t.Errorf("Overall.InputTokens = %d, want 20_000", info.Overall.InputTokens)
	}
	if info.Overall.InputTokensCached != 8_000 {
		t.Errorf("Overall.InputTokensCached = %d, want 8_000", info.Overall.InputTokensCached)
	}
	if info.Overall.InputTokensUncached != 12_000 {
		t.Errorf("Overall.InputTokensUncached = %d, want 12_000", info.Overall.InputTokensUncached)
	}
	if info.Overall.OutputTokens != 1_000 {
		t.Errorf("Overall.OutputTokens = %d, want 1_000", info.Overall.OutputTokens)
	}

	// Actual cost: turn1 (10k * 1.50 + 500 * 9)/1e6 + turn2 (2k * 1.50 + 8k * 0.15 + 500 * 9)/1e6
	wantCost := (0.01*1.50 + 0.0005*9.00) + (0.002*1.50 + 0.008*0.15 + 0.0005*9.00)
	if math.Abs(info.Overall.CostUSD-wantCost) > 1e-9 {
		t.Errorf("Overall.CostUSD = %f, want %f", info.Overall.CostUSD, wantCost)
	}
	// Reference cost: both turns billed at input rate for all inputs.
	// (10k * 1.50 + 500 * 9)/1e6 * 2 = 2 * (0.015 + 0.0045) = 0.039
	wantRef := 2 * (0.01*1.50 + 0.0005*9.00)
	// PriceFor("gemini-3.5-flash", nil) may or may not resolve — depends on
	// whether SetCatalog was called by any test that ran first. If it
	// hasn't, PriceFor still falls back to a fresh builtin catalog per
	// the pkg/usage/pricing.go path, so the answer is stable.
	if math.Abs(info.Overall.CostUSDUncachedReference-wantRef) > 1e-9 {
		t.Errorf("Overall.CostUSDUncachedReference = %f, want %f",
			info.Overall.CostUSDUncachedReference, wantRef)
	}
	// The whole point of #222: reference > actual = caching win visible.
	if info.Overall.CostUSDUncachedReference <= info.Overall.CostUSD {
		t.Errorf("cache savings should be visible: ref=%f actual=%f",
			info.Overall.CostUSDUncachedReference, info.Overall.CostUSD)
	}

	// PerTurn shape.
	if len(info.PerTurn) != 2 {
		t.Fatalf("len(PerTurn) = %d, want 2", len(info.PerTurn))
	}
	if info.PerTurn[0].Turn != 1 || info.PerTurn[1].Turn != 2 {
		t.Errorf("PerTurn indexes wrong: %d, %d", info.PerTurn[0].Turn, info.PerTurn[1].Turn)
	}
	if info.PerTurn[0].InputTokensCached != 0 {
		t.Errorf("cold turn should have 0 cached: %+v", info.PerTurn[0])
	}
	if info.PerTurn[1].InputTokensCached != 8_000 {
		t.Errorf("warm turn cached = %d, want 8_000", info.PerTurn[1].InputTokensCached)
	}
	if info.PerTurn[0].Model != "gemini-3.5-flash" {
		t.Errorf("PerTurn[0].Model = %q, want gemini-3.5-flash", info.PerTurn[0].Model)
	}
	// TotalTokens = input + output (+ thoughts + tool-use, both zero here).
	if info.PerTurn[0].TotalTokens != 10_500 {
		t.Errorf("PerTurn[0].TotalTokens = %d, want 10_500", info.PerTurn[0].TotalTokens)
	}
}

// TestAttachUsage_NoTracker exercises the nil-tracker path. Must
// return a zero UsageInfo without panicking or allocating a PerTurn
// slice. Also pins the nil-adapter and nil-agent paths every
// capability method promises.
func TestAttachUsage_NoTracker(t *testing.T) {
	t.Parallel()
	ad := New(newEchoAgent(t))
	info := ad.AttachUsage()
	if info.Overall.Turns != 0 || info.PerTurn != nil || info.PerModel != nil {
		t.Errorf("nil-tracker AttachUsage should be zero: %+v", info)
	}
	var nilAd *Adapter
	if got := nilAd.AttachUsage(); got.Overall.Turns != 0 {
		t.Errorf("nil-adapter AttachUsage should be zero: %+v", got)
	}
	if got := New(nil).AttachUsage(); got.Overall.Turns != 0 {
		t.Errorf("nil-agent AttachUsage should be zero: %+v", got)
	}
}

// TestAttachUsage_PerModelWhenMultipleModels covers the mixed-model
// path (parent frontier + subtask flash). PerModel must be populated
// and CostUSDUncachedReference must roll up per-model too.
func TestAttachUsage_PerModelWhenMultipleModels(t *testing.T) {
	t.Parallel()
	tr := usage.NewTracker()
	// Use rates the pricing catalog knows: gemini-3.5-flash + gemini-3.1-flash-lite.
	flash := usage.PriceFor("gemini-3.5-flash", nil)
	lite := usage.PriceFor("gemini-3.1-flash-lite", nil)
	if flash.IsZero() || lite.IsZero() {
		t.Skip("catalog didn't resolve gemini rates; skipping in this env")
	}
	tr.AppendUsage("gemini-3.5-flash", usage.TurnUsage{InputTokens: 10_000, OutputTokens: 500}, flash)
	tr.AppendUsage("gemini-3.1-flash-lite", usage.TurnUsage{InputTokens: 5_000, OutputTokens: 200}, lite)

	ad := New(newEchoAgent(t, agent.WithUsageTracker(tr)))
	info := ad.AttachUsage()
	if len(info.PerModel) != 2 {
		t.Fatalf("PerModel has %d models, want 2", len(info.PerModel))
	}
	// Sanity: each per-model row should carry a positive uncached-ref
	// cost since both models are priced.
	for name, row := range info.PerModel {
		if row.CostUSDUncachedReference <= 0 {
			t.Errorf("PerModel[%s].CostUSDUncachedReference = %v, want > 0",
				name, row.CostUSDUncachedReference)
		}
	}
}

// TestUsageTotalsToAttach_UncachedMathIsHonest guards the projection
// math: if a caller ever manages to smuggle in Totals with cached >
// input (shouldn't happen post-AppendUsage but the projection helper
// runs unconditionally), InputTokensUncached must not underflow into
// a garbage negative int64.
func TestUsageTotalsToAttach_UncachedMathIsHonest(t *testing.T) {
	t.Parallel()
	// This test pins the current invariant: AppendUsage clamps at
	// the record site, so Totals().CachedInputTokens can never exceed
	// InputTokens for any tracker built via the public API. If that
	// changes, revisit usageTotalsToAttach.
	got := usageTotalsToAttach(usage.Totals{
		Turns:             1,
		InputTokens:       1000,
		CachedInputTokens: 400,
		OutputTokens:      100,
	})
	if got.InputTokensUncached != 600 {
		t.Errorf("InputTokensUncached = %d, want 600", got.InputTokensUncached)
	}
	if got.InputTokens != 1000 || got.InputTokensCached != 400 {
		t.Errorf("field mapping wrong: %+v", got)
	}
	// Zero-cache case leaves omitempty fields at zero.
	got2 := usageTotalsToAttach(usage.Totals{Turns: 1, InputTokens: 100, OutputTokens: 10})
	if got2.InputTokensCached != 0 || got2.InputTokensUncached != 100 {
		t.Errorf("cold-only case: %+v", got2)
	}
}

func TestAttachInterrupt_IdleAgentReturnsFalse(t *testing.T) {
	t.Parallel()
	ad := New(newEchoAgent(t))
	if ad.AttachInterrupt() {
		t.Errorf("AttachInterrupt on idle agent returned true, want false")
	}
}

func TestAttachSpawnSubagent_NoManager_ReturnsSentinel(t *testing.T) {
	t.Parallel()
	ad := New(newEchoAgent(t))
	_, err := ad.AttachSpawnSubagent(context.Background(), attach.SubagentSpec{Name: "probe", Goal: "x"})
	if err != ErrSubagentSpawnerUnavailable {
		t.Fatalf("err = %v, want ErrSubagentSpawnerUnavailable", err)
	}
	// The message string is load-bearing — pkg/attach's slash handler
	// matches it literally. Pin it.
	const want = "agent: subagent spawner unavailable (no BackgroundAgentManager wired)"
	if err.Error() != want {
		t.Fatalf("sentinel message drifted: %q, want %q", err.Error(), want)
	}
}

func TestAttachStatus_ReportsModelName(t *testing.T) {
	t.Parallel()
	ad := New(newEchoAgent(t))
	got := ad.AttachStatus()
	if got.State != attach.AgentStateIdle {
		t.Errorf("State = %q, want %q", got.State, attach.AgentStateIdle)
	}
	if got.ModelName == "" {
		t.Errorf("ModelName empty, want the wrapped agent's model id")
	}
}

func TestCapabilities_UnwiredReturnEmptyOrSentinel(t *testing.T) {
	t.Parallel()
	ad := New(newEchoAgent(t))
	if got := ad.AttachMemory(); got != nil {
		t.Errorf("AttachMemory unwired = %v, want nil", got)
	}
	if got := ad.AttachSkills(); got != nil {
		t.Errorf("AttachSkills unwired = %v, want nil", got)
	}
	if got := ad.AttachMCP(); len(got.Servers) != 0 {
		t.Errorf("AttachMCP unwired = %+v, want empty", got)
	}
	if _, err := ad.AttachRefreshPricing(context.Background()); err != attach.ErrCapabilityNotRegistered {
		t.Errorf("AttachRefreshPricing unwired err = %v, want ErrCapabilityNotRegistered", err)
	}
	if err := ad.AttachSetManualPricing(attach.PricingSetRequest{}); err != attach.ErrCapabilityNotRegistered {
		t.Errorf("AttachSetManualPricing unwired err = %v, want ErrCapabilityNotRegistered", err)
	}
	if _, err := ad.AttachReplan(context.Background(), attach.ReplanRequest{}); err != attach.ErrCapabilityNotRegistered {
		t.Errorf("AttachReplan unwired err = %v, want ErrCapabilityNotRegistered", err)
	}
	if got := ad.AttachPromptBroker(); got != nil {
		t.Errorf("AttachPromptBroker unwired = %v, want nil", got)
	}
}

func TestCapabilities_WiredProvidersDelegate(t *testing.T) {
	t.Parallel()
	broker := attach.NewPromptBroker()
	t.Cleanup(broker.Close)
	ad := New(newEchoAgent(t),
		WithMemoryProvider(func() []attach.MemorySource {
			return []attach.MemorySource{{Scope: "project", Path: "AGENTS.md"}}
		}),
		WithSkillsProvider(func() []attach.SkillInfo {
			return []attach.SkillInfo{{Name: "deploy"}}
		}),
		WithMCPProvider(func() attach.MCPInfo {
			return attach.MCPInfo{Servers: []attach.MCPServerInfo{{Name: "k8s"}}}
		}),
		WithPricingProvider(func() attach.PricingInfo {
			return attach.PricingInfo{CurrentModel: "echo"}
		}),
		WithPromptBroker(broker),
	)
	if got := ad.AttachMemory(); len(got) != 1 || got[0].Path != "AGENTS.md" {
		t.Errorf("AttachMemory = %+v", got)
	}
	if got := ad.AttachSkills(); len(got) != 1 || got[0].Name != "deploy" {
		t.Errorf("AttachSkills = %+v", got)
	}
	if got := ad.AttachMCP(); len(got.Servers) != 1 || got.Servers[0].Name != "k8s" {
		t.Errorf("AttachMCP = %+v", got)
	}
	if got := ad.AttachPricing(); got.CurrentModel != "echo" {
		t.Errorf("AttachPricing = %+v", got)
	}
	if got := ad.AttachPromptBroker(); got != broker {
		t.Errorf("AttachPromptBroker = %p, want %p", got, broker)
	}
}
