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

//go:build !no_tui

package main

import (
	"context"
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/agent"
	"github.com/go-steer/core-agent/v2/pkg/attachadapter"
	"github.com/go-steer/core-agent/v2/pkg/usage"
)

// guardrailAdapter builds the in-process TUI adapter over an agent
// whose per-session ceiling has already tripped, so /guardrail has
// something real to report and reset.
func guardrailAdapter(t *testing.T, spentUSD float64) (*coreAgentAdapter, *agent.Agent) {
	t.Helper()
	tr := usage.NewTracker()
	if spentUSD > 0 {
		tr.Append("test", int(spentUSD*1_000_000), 0, usage.Pricing{InputPerMTok: 1})
	}
	inner, err := agent.New(&cumulativeUsageLLM{finalIn: 10, finalOut: 5},
		agent.WithName("test"),
		agent.WithUsageTracker(tr),
		agent.WithCostCeiling(agent.CostCeiling{MaxSessionUSD: 10}),
	)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	if spentUSD >= 10 {
		for range inner.Run(context.Background(), "hi") { //nolint:revive // drain
		}
		if tripped, _ := inner.CostCeilingTripped(); !tripped {
			t.Fatalf("setup: ceiling did not trip at $%.2f", spentUSD)
		}
	}
	return &coreAgentAdapter{inner: inner, attachAd: attachadapter.New(inner)}, inner
}

// The command must be in the palette unconditionally — an operator
// looking for the recovery command shouldn't have to already know
// which backstop is armed.
func TestSlashGuardrail_IsRegistered(t *testing.T) {
	a, _ := guardrailAdapter(t, 0)
	var found bool
	for _, c := range a.SlashCommands() {
		if c.Name == "guardrail" {
			found = true
			if len(c.Aliases) == 0 || c.Aliases[0] != "guardrails" {
				t.Errorf("aliases = %v, want the plural form", c.Aliases)
			}
		}
	}
	if !found {
		t.Fatal("/guardrail missing from SlashCommands()")
	}
}

func TestSlashGuardrail_BareShowsState(t *testing.T) {
	a, _ := guardrailAdapter(t, 12)
	res, err := a.InvokeSlash(context.Background(), "guardrail", "")
	if err != nil {
		t.Fatalf("InvokeSlash: %v", err)
	}
	if !strings.Contains(res.SystemMessage, "HALTED") {
		t.Errorf("status output missing the halt banner:\n%s", res.SystemMessage)
	}
	if !strings.Contains(res.SystemMessage, "/guardrail reset +") {
		t.Errorf("a would-retrip status must ask for budget:\n%s", res.SystemMessage)
	}
}

func TestSlashGuardrail_ResetRefusesWithoutBudget(t *testing.T) {
	a, inner := guardrailAdapter(t, 12)
	res, err := a.InvokeSlash(context.Background(), "guardrail", "reset")
	if err != nil {
		t.Fatalf("InvokeSlash: %v", err)
	}
	if !strings.Contains(res.SystemMessage, "refused") {
		t.Errorf("bare reset of a session trip must be refused:\n%s", res.SystemMessage)
	}
	if tripped, _ := inner.CostCeilingTripped(); !tripped {
		t.Error("a refused reset must leave the trip in place")
	}
}

func TestSlashGuardrail_ResetWithBudget(t *testing.T) {
	a, inner := guardrailAdapter(t, 12)
	res, err := a.InvokeSlash(context.Background(), "guardrails", "reset cost_ceiling +5")
	if err != nil {
		t.Fatalf("InvokeSlash: %v", err)
	}
	if tripped, _ := inner.CostCeilingTripped(); tripped {
		t.Fatalf("ceiling still tripped after a budgeted reset:\n%s", res.SystemMessage)
	}
	if inner.CostCeilingLimits().MaxSessionUSD != 15 {
		t.Errorf("ceiling = %v, want 15", inner.CostCeilingLimits().MaxSessionUSD)
	}
	if !strings.Contains(res.SystemMessage, "raised by $5.00") {
		t.Errorf("output should confirm the raise:\n%s", res.SystemMessage)
	}
}

func TestSlashGuardrail_RejectsGarbageArgs(t *testing.T) {
	a, _ := guardrailAdapter(t, 0)
	for _, args := range []string{"clear", "reset everything", "reset +abc", "reset +0"} {
		res, err := a.InvokeSlash(context.Background(), "guardrail", args)
		if err != nil {
			t.Fatalf("InvokeSlash(%q): %v", args, err)
		}
		if !strings.Contains(res.SystemMessage, "unknown") && !strings.Contains(res.SystemMessage, "not a positive") {
			t.Errorf("args %q accepted silently:\n%s", args, res.SystemMessage)
		}
	}
}
