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
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"google.golang.org/adk/session"

	"github.com/go-steer/core-agent/v2/pkg/agent"
	"github.com/go-steer/core-agent/v2/pkg/attach"
	"github.com/go-steer/core-agent/v2/pkg/eventlog"
	"github.com/go-steer/core-agent/v2/pkg/usage"
)

// tripSessionCeiling builds an adapter over an agent whose session
// spend already exceeds a $10 ceiling, then drives one Run so the
// enforcement pass at the top of Run trips it — the same path a real
// runaway takes.
func tripSessionCeiling(t *testing.T, spentUSD float64) (*Adapter, *agent.Agent) {
	t.Helper()
	tr := usage.NewTracker()
	tr.Append("test", int(spentUSD*1_000_000), 0, usage.Pricing{InputPerMTok: 1})
	a := newEchoAgent(t,
		agent.WithUsageTracker(tr),
		agent.WithCostCeiling(agent.CostCeiling{MaxSessionUSD: 10}),
	)
	for range a.Run(context.Background(), "hello") { //nolint:revive // drain
	}
	if tripped, _ := a.CostCeilingTripped(); !tripped {
		t.Fatalf("setup: ceiling did not trip at $%.2f against $10", spentUSD)
	}
	return New(a), a
}

func TestAttachGuardrails_ReportsArmedAndTrippedState(t *testing.T) {
	t.Parallel()
	ad, _ := tripSessionCeiling(t, 12)

	info := ad.AttachGuardrails()
	if !info.Halted {
		t.Error("Halted = false with a tripped cost ceiling")
	}
	cc := info.CostCeiling
	if cc.MaxSessionUSD != 10 || cc.SessionCostUSD != 12 {
		t.Errorf("cost ceiling = %+v, want $12 of $10", cc)
	}
	if !cc.Tripped || !cc.WouldRetrip {
		t.Errorf("a $12-of-$10 trip must report tripped + would_retrip: %+v", cc)
	}
	if info.Watchdog.Mode != "off" {
		t.Errorf("watchdog mode = %q, want off (none wired)", info.Watchdog.Mode)
	}
}

func TestAttachGuardrails_NilAgentIsSafe(t *testing.T) {
	t.Parallel()
	info := New(nil).AttachGuardrails()
	if info.Halted || info.Watchdog.Mode != "off" {
		t.Errorf("capability-only adapter = %+v, want everything off", info)
	}
}

// The core refusal: clearing a per-session trip with no extra budget
// would be undone by the next turn, so it's refused with
// ErrGuardrailRetrip and the agent is left untouched.
func TestAttachResetGuardrail_RefusesBareResetOfSessionTrip(t *testing.T) {
	t.Parallel()
	ad, a := tripSessionCeiling(t, 12)

	_, err := ad.AttachResetGuardrail(attach.GuardrailResetRequest{})
	if !errors.Is(err, attach.ErrGuardrailRetrip) {
		t.Fatalf("err = %v, want ErrGuardrailRetrip", err)
	}
	if !strings.Contains(err.Error(), "12.0000") || !strings.Contains(err.Error(), "10.0000") {
		t.Errorf("refusal must carry spend + ceiling: %q", err)
	}
	if tripped, _ := a.CostCeilingTripped(); !tripped {
		t.Error("a refused reset must leave the trip in place")
	}
	if a.CostCeilingLimits().MaxSessionUSD != 10 {
		t.Error("a refused reset must not mutate the ceiling")
	}
}

// Too-small a budget is still a refusal — and still mutates nothing,
// so the operator doesn't have to guess how much of their request was
// applied before it failed.
func TestAttachResetGuardrail_InsufficientBudgetIsAtomic(t *testing.T) {
	t.Parallel()
	ad, a := tripSessionCeiling(t, 12)

	_, err := ad.AttachResetGuardrail(attach.GuardrailResetRequest{AdditionalBudgetUSD: 1})
	if !errors.Is(err, attach.ErrGuardrailRetrip) {
		t.Fatalf("err = %v, want ErrGuardrailRetrip ($10 + $1 < $12)", err)
	}
	if a.CostCeilingLimits().MaxSessionUSD != 10 {
		t.Errorf("ceiling = %v, want the untouched $10 — a refused reset is atomic",
			a.CostCeilingLimits().MaxSessionUSD)
	}
}

func TestAttachResetGuardrail_BudgetClearsTheTrip(t *testing.T) {
	t.Parallel()
	ad, a := tripSessionCeiling(t, 12)

	resp, err := ad.AttachResetGuardrail(attach.GuardrailResetRequest{AdditionalBudgetUSD: 5})
	if err != nil {
		t.Fatalf("AttachResetGuardrail: %v", err)
	}
	if len(resp.Reset) != 1 || resp.Reset[0] != attach.GuardrailCostCeiling {
		t.Errorf("Reset = %v, want [cost_ceiling]", resp.Reset)
	}
	if resp.BudgetAddedUSD != 5 {
		t.Errorf("BudgetAddedUSD = %v, want 5", resp.BudgetAddedUSD)
	}
	if tripped, _ := a.CostCeilingTripped(); tripped {
		t.Error("trip flag should be clear")
	}
	if resp.Guardrails.CostCeiling.MaxSessionUSD != 15 {
		t.Errorf("echoed ceiling = %v, want 15", resp.Guardrails.CostCeiling.MaxSessionUSD)
	}
	// Raising the bar must not rewrite what the session actually
	// spent — /usage and the eventlog keep counting the same dollars.
	if resp.Guardrails.CostCeiling.SessionCostUSD != 12 {
		t.Errorf("spend = %v, want the untouched $12", resp.Guardrails.CostCeiling.SessionCostUSD)
	}
	// And the next turn must not immediately re-trip.
	for range a.Run(context.Background(), "again") { //nolint:revive // drain
	}
	if tripped, reason := a.CostCeilingTripped(); tripped {
		t.Errorf("re-tripped after a budgeted reset: %s", reason)
	}
}

func TestAttachResetGuardrail_UntrippedIsANoOpSuccess(t *testing.T) {
	t.Parallel()
	ad := New(newEchoAgent(t))
	resp, err := ad.AttachResetGuardrail(attach.GuardrailResetRequest{})
	if err != nil {
		t.Fatalf("a defensive reset should succeed: %v", err)
	}
	if len(resp.Reset) != 0 {
		t.Errorf("Reset = %v, want empty", resp.Reset)
	}
	if !strings.Contains(resp.Message, "nothing to reset") {
		t.Errorf("Message = %q, want it to say nothing was tripped", resp.Message)
	}
}

// Scoping: /guardrail reset watchdog must not touch the cost ceiling.
// An operator clearing one backstop hasn't consented to clearing the
// other, and the retrip guard must not fire for an out-of-scope
// ceiling either — that would make a watchdog reset impossible on any
// session that had also blown its budget.
func TestAttachResetGuardrail_WatchdogScopeLeavesTheCeilingAlone(t *testing.T) {
	t.Parallel()
	ad, a := tripSessionCeiling(t, 12)

	resp, err := ad.AttachResetGuardrail(attach.GuardrailResetRequest{Guardrail: attach.GuardrailWatchdog})
	if err != nil {
		t.Fatalf("watchdog-scoped reset must not be blocked by a cost trip: %v", err)
	}
	if len(resp.Reset) != 0 {
		t.Errorf("Reset = %v, want empty (no watchdog was tripped)", resp.Reset)
	}
	if tripped, _ := a.CostCeilingTripped(); !tripped {
		t.Error("a watchdog-scoped reset cleared the cost ceiling too")
	}
	// "nothing to reset" while the session stays halted is exactly
	// the message that makes operators think the command is broken.
	if !strings.Contains(resp.Message, "still halted by cost_ceiling") {
		t.Errorf("Message = %q, want it to say the session is still halted", resp.Message)
	}
}

// Budget only means something for the cost ceiling. Silently dropping
// it on a watchdog-scoped reset would let an operator believe they'd
// bought runway they hadn't.
func TestAttachResetGuardrail_RejectsBudgetOnWatchdogScope(t *testing.T) {
	t.Parallel()
	ad, a := tripSessionCeiling(t, 12)

	_, err := ad.AttachResetGuardrail(attach.GuardrailResetRequest{
		Guardrail:           attach.GuardrailWatchdog,
		AdditionalBudgetUSD: 5,
	})
	if err == nil {
		t.Fatal("want an error for budget on a watchdog-scoped reset")
	}
	if errors.Is(err, attach.ErrGuardrailRetrip) {
		t.Errorf("wrong error class (that maps to 409, not 400): %v", err)
	}
	if a.CostCeilingLimits().MaxSessionUSD != 10 {
		t.Error("the rejected budget was applied anyway")
	}
}

// cost_ceiling in the capabilities frame was hardcoded false before
// #666; it must now track whether a bound is actually armed.
func TestAttachCapabilities_ReportsGuardrails(t *testing.T) {
	t.Parallel()
	rep := New(newEchoAgent(t)).AttachCapabilities()
	if !rep.Guardrails {
		t.Error("Guardrails = false for a live agent")
	}
	if rep.CostCeiling {
		t.Error("CostCeiling = true with no ceiling configured")
	}

	armed := New(newEchoAgent(t, agent.WithCostCeiling(agent.CostCeiling{MaxTurnUSD: 1}))).AttachCapabilities()
	if !armed.CostCeiling {
		t.Error("CostCeiling = false with a $1 per-turn ceiling armed")
	}
}

// The reset surface is the only writer of the durable reset row (#643 /
// #331): both operator paths — the TUI's /guardrail and POST
// /guardrails/reset — go through AttachResetGuardrail, so persisting
// here is what keeps the two from drifting. Fails on pre-#643 code:
// nothing wrote the row, so the halt came back on the next restart.
func TestAttachResetGuardrail_PersistsOneRow(t *testing.T) {
	t.Parallel()
	h, err := eventlog.Open(context.Background(),
		sqlite.Open(filepath.Join(t.TempDir(), "session.db")))
	if err != nil {
		t.Fatalf("eventlog.Open: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })

	tr := usage.NewTracker()
	tr.Append("test", 12_000_000, 0, usage.Pricing{InputPerMTok: 1}) // $12 of $10
	a := newEchoAgent(t,
		agent.WithEventLog(h),
		agent.WithSession("u", "s-reset-row"),
		agent.WithUsageTracker(tr),
		agent.WithCostCeiling(agent.CostCeiling{MaxSessionUSD: 10}),
	)
	if _, err := h.Service.Create(context.Background(), &session.CreateRequest{
		AppName: a.AppName(), UserID: "u", SessionID: "s-reset-row",
	}); err != nil {
		t.Fatalf("session Create: %v", err)
	}
	for range a.Run(context.Background(), "hello") { //nolint:revive // drain
	}
	if tripped, _ := a.CostCeilingTripped(); !tripped {
		t.Fatal("setup: ceiling did not trip")
	}

	resp, err := New(a).AttachResetGuardrail(attach.GuardrailResetRequest{
		Guardrail:           attach.GuardrailCostCeiling,
		AdditionalBudgetUSD: 5,
		Caller:              "alice@example.com",
	})
	if err != nil {
		t.Fatalf("AttachResetGuardrail: %v", err)
	}
	if len(resp.Reset) != 1 || resp.BudgetAddedUSD != 5 {
		t.Fatalf("reset response = %+v, want the ceiling cleared and $5 added", resp)
	}

	getResp, err := h.Service.Get(context.Background(), &session.GetRequest{
		AppName: a.AppName(), UserID: "u", SessionID: "s-reset-row",
	})
	if err != nil {
		t.Fatalf("session Get: %v", err)
	}
	var rows int
	for ev := range getResp.Session.Events().All() {
		if ev.Author != attach.GuardrailResetEventAuthor {
			continue
		}
		rows++
		if ev.CustomMetadata["caller"] != "alice@example.com" {
			t.Errorf("caller = %v, want the authenticated identity", ev.CustomMetadata["caller"])
		}
		if ev.CustomMetadata["budget_added_usd"] != 5.0 {
			t.Errorf("budget_added_usd = %v, want 5", ev.CustomMetadata["budget_added_usd"])
		}
	}
	if rows != 1 {
		t.Errorf("durable reset rows = %d, want exactly 1 per operator action", rows)
	}
}
