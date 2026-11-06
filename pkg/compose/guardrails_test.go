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

package compose

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/core-agent/v3/pkg/agent"
	"github.com/go-steer/core-agent/v3/pkg/agent/background"
	"github.com/go-steer/core-agent/v3/pkg/config"
)

func TestResolveGuardrailsFromConfig_UnattendedAndInteractiveDefaults(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultConfig()

	unattended, err := ResolveGuardrailsFromConfig(cfg, true, true)
	if err != nil {
		t.Fatalf("ResolveGuardrailsFromConfig(unattended): %v", err)
	}
	if unattended.Watchdog != config.WatchdogEnforce || unattended.WatchdogSource != WatchdogSourceUnattendedDefault {
		t.Errorf("unattended Watchdog = (%q, %q), want (%q, %q)", unattended.Watchdog, unattended.WatchdogSource, config.WatchdogEnforce, WatchdogSourceUnattendedDefault)
	}
	if unattended.SessionCostUSD != DefaultUnattendedSessionCostUSD || unattended.SessionCostSource != SessionCostSourceUnattendedDefault {
		t.Errorf("unattended SessionCost = (%v, %q), want (%v, %q)", unattended.SessionCostUSD, unattended.SessionCostSource, DefaultUnattendedSessionCostUSD, SessionCostSourceUnattendedDefault)
	}
	if unattended.ApprovalTimeout != DefaultUnattendedApprovalTimeout || unattended.ApprovalTimeoutSource != ApprovalTimeoutSourceUnattendedDefault {
		t.Errorf("unattended ApprovalTimeout = (%v, %q), want (%v, %q)", unattended.ApprovalTimeout, unattended.ApprovalTimeoutSource, DefaultUnattendedApprovalTimeout, ApprovalTimeoutSourceUnattendedDefault)
	}

	interactive, err := ResolveGuardrailsFromConfig(cfg, false, true)
	if err != nil {
		t.Fatalf("ResolveGuardrailsFromConfig(interactive): %v", err)
	}
	if interactive.Watchdog != config.WatchdogWarn || interactive.SessionCostUSD != 0 || interactive.ApprovalTimeout != 0 {
		t.Errorf("interactive defaults = %+v, want warn/0/0", interactive)
	}
}

func TestAppendCostAndWatchdogOpts(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultConfig()
	cfg.Agent.MaxWallclockSeconds = 60
	cfg.Agent.MaxSteps = 10
	cfg.Agent.FinalReport = true

	guard, err := ResolveGuardrailsFromConfig(cfg, true, false)
	if err != nil {
		t.Fatalf("ResolveGuardrailsFromConfig: %v", err)
	}
	var notices []string
	send := func(s string) { notices = append(notices, s) }
	var opts []agent.Option
	opts = AppendCostAndWallclockOpts(opts, cfg, agent.CostCeiling{MaxTurnUSD: 1.5, MaxSessionUSD: guard.SessionCostUSD}, guard.SessionCostSource, send)
	opts = AppendWatchdogOpts(opts, guard, send)
	if len(opts) < 6 {
		t.Errorf("len(opts) = %d, want >= 6", len(opts))
	}
	joined := strings.Join(notices, "\n")
	if !strings.Contains(joined, "cost ceiling:") || !strings.Contains(joined, "watchdog: enforce mode") {
		t.Errorf("notices missing cost ceiling or watchdog line:\n%s", joined)
	}
}

func TestCeilingPricing_UnpricedModelPolicy(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultConfig()
	cfg.Model.Provider = config.ProviderGemini
	cfg.Model.Name = "unpriced-custom-model-xyz"

	models := StartupCeilingModels(cfg, []background.SubagentTemplate{
		{Name: "worker", ModelID: "unpriced-sub-xyz", Backend: "gemini", Budgets: background.Budgets{MaxCost: 2.0}},
	}, "unpriced-small-xyz")
	if len(models) != 3 {
		t.Fatalf("len(StartupCeilingModels) = %d, want 3", len(models))
	}

	// Unattended default ceiling warns on main model, refuses explicit subagent budget.
	unattended := CeilingPricing{
		Ceiling:       agent.CostCeiling{MaxSessionUSD: DefaultUnattendedSessionCostUSD},
		SessionSource: SessionCostSourceUnattendedDefault,
	}
	if warn, err := unattended.Check("model", "unpriced-custom-model-xyz", "gemini", 0); err != nil || !strings.Contains(warn, "WARNING:") {
		t.Errorf("unattended Check(main) = (%q, %v), want warning and nil err", warn, err)
	}
	if _, err := unattended.CheckStartup(models); err == nil {
		t.Error("CheckStartup with explicit subagent budget on unpriced model should fail")
	}

	// Operator-set ceiling refuses unpriced main model.
	opSet := CeilingPricing{
		Ceiling:       agent.CostCeiling{MaxSessionUSD: 5.0},
		SessionSource: SessionCostSourceConfig,
	}
	if _, err := opSet.Check("model", "unpriced-custom-model-xyz", "gemini", 0); err == nil {
		t.Error("operator-set ceiling on unpriced model should fail")
	}

	// SpawnDoor warns once for non-explicit budget on unpriced model.
	var warned WarnOnce
	none := CeilingPricing{SessionSource: SessionCostSourceUnset}
	warns, err := none.SpawnDoor(background.SpawnPricing{ModelID: "unpriced-spawn-xyz", Backend: "gemini", MaxCost: 1.0}, &warned)
	if err != nil || len(warns) != 1 {
		t.Errorf("first SpawnDoor = (%v, %v), want 1 warning and nil err", warns, err)
	}
	warns2, err := none.SpawnDoor(background.SpawnPricing{ModelID: "unpriced-spawn-xyz", Backend: "gemini", MaxCost: 1.0}, &warned)
	if err != nil || len(warns2) != 0 {
		t.Errorf("second SpawnDoor = (%v, %v), want 0 warnings (warned once)", warns2, err)
	}
	// Passing nil *WarnOnce still warns rather than panicking or suppressing.
	warnsNil, err := none.SpawnDoor(background.SpawnPricing{ModelID: "unpriced-spawn-xyz", Backend: "gemini", MaxCost: 1.0}, nil)
	if err != nil || len(warnsNil) != 1 {
		t.Errorf("nil-WarnOnce SpawnDoor = (%v, %v), want 1 warning and nil err", warnsNil, err)
	}
	// StartupCeilingModels handles nil cfg without panicking.
	if got := StartupCeilingModels(nil, nil, "small-m"); len(got) != 2 {
		t.Errorf("StartupCeilingModels(nil, nil, small-m) len = %d, want 2", len(got))
	}
}

func TestResolveAutoContinue(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	res, err := ResolveAutoContinue(nil, true, false, true, &buf)
	if err != nil {
		t.Fatalf("ResolveAutoContinue: %v", err)
	}
	if !res.Enabled || res.Freshness != time.Hour || !res.Retry || res.RetryInterval != 5*time.Minute {
		t.Errorf("ResolveAutoContinue defaults = %+v, want enabled/1h/retry/5m", res)
	}
	if !strings.Contains(buf.String(), "auto_continue on by default") {
		t.Errorf("stderr = %q, want default notice", buf.String())
	}
}
