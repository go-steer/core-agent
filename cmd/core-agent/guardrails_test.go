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

package main

import (
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v3/pkg/agent"
	"github.com/go-steer/core-agent/v3/pkg/agent/background"
	"github.com/go-steer/core-agent/v3/pkg/compose"
	"github.com/go-steer/core-agent/v3/pkg/config"
	"github.com/go-steer/core-agent/v3/pkg/pricing"
	"github.com/go-steer/core-agent/v3/pkg/usage"
)

func f64(v float64) *float64 { return &v }

// TestResolveGuardrails_DefaultPosturePerMode is the #642 acceptance
// test: with no flags and no config, an unattended run must come up
// with an ACTIVE backstop (enforce watchdog + a session cost ceiling),
// and an interactive run must be byte-for-byte the pre-#642 posture
// (warn watchdog, no ceiling).
//
// Pre-fix this fails on both unattended rows: the flag default was the
// literal string "warn" and the session ceiling defaulted to 0, so an
// unattended daemon booted with observe-and-log and unbounded spend.
func TestResolveGuardrails_DefaultPosturePerMode(t *testing.T) {
	t.Parallel()

	t.Run("unattended", func(t *testing.T) {
		t.Parallel()
		got, err := resolveGuardrails(guardrailInputs{Unattended: true})
		if err != nil {
			t.Fatalf("resolveGuardrails: %v", err)
		}
		if got.Watchdog != config.WatchdogEnforce {
			t.Errorf("watchdog: got %q, want %q — an unattended daemon has nobody reading warn-mode alerts", got.Watchdog, config.WatchdogEnforce)
		}
		if got.SessionCostUSD != DefaultUnattendedSessionCostUSD {
			t.Errorf("session ceiling: got %v, want %v", got.SessionCostUSD, DefaultUnattendedSessionCostUSD)
		}
		if got.WatchdogSource != sourceUnattendedDefault {
			t.Errorf("watchdog source: got %q, want %q", got.WatchdogSource, sourceUnattendedDefault)
		}
	})

	t.Run("interactive", func(t *testing.T) {
		t.Parallel()
		got, err := resolveGuardrails(guardrailInputs{Unattended: false})
		if err != nil {
			t.Fatalf("resolveGuardrails: %v", err)
		}
		if got.Watchdog != config.WatchdogWarn {
			t.Errorf("watchdog: got %q, want %q — interactive defaults must not change", got.Watchdog, config.WatchdogWarn)
		}
		if got.SessionCostUSD != 0 {
			t.Errorf("session ceiling: got %v, want 0 (disabled) — interactive defaults must not change", got.SessionCostUSD)
		}
	})
}

// TestResolveGuardrails_WatchdogPrecedence pins the #660 chain:
// --watchdog beats safety.watchdog beats the mode default, matching
// --small-tier-parent / safety.small_tier_parent.
func TestResolveGuardrails_WatchdogPrecedence(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		in         guardrailInputs
		wantMode   string
		wantSource string
	}{
		{
			name:       "config alone: a recipe ships its own backstop",
			in:         guardrailInputs{WatchdogConfig: config.WatchdogEnforce},
			wantMode:   config.WatchdogEnforce,
			wantSource: sourceConfig,
		},
		{
			name:       "flag beats config",
			in:         guardrailInputs{WatchdogFlag: config.WatchdogOff, WatchdogConfig: config.WatchdogEnforce},
			wantMode:   config.WatchdogOff,
			wantSource: sourceFlag,
		},
		{
			name:       "config beats the unattended default",
			in:         guardrailInputs{WatchdogConfig: config.WatchdogWarn, Unattended: true},
			wantMode:   config.WatchdogWarn,
			wantSource: sourceConfig,
		},
		{
			name:       "flag beats the unattended default",
			in:         guardrailInputs{WatchdogFlag: config.WatchdogOff, Unattended: true},
			wantMode:   config.WatchdogOff,
			wantSource: sourceFlag,
		},
		{
			name:       "case and whitespace are tolerated",
			in:         guardrailInputs{WatchdogFlag: "  ENFORCE "},
			wantMode:   config.WatchdogEnforce,
			wantSource: sourceFlag,
		},
		{
			name:       "feedback is accepted from the flag",
			in:         guardrailInputs{WatchdogFlag: config.WatchdogFeedback},
			wantMode:   config.WatchdogFeedback,
			wantSource: sourceFlag,
		},
		{
			// A recipe that wants self-correction without a halt has to
			// be able to say so in the file — #660's whole point.
			name:       "feedback is accepted from config",
			in:         guardrailInputs{WatchdogConfig: config.WatchdogFeedback},
			wantMode:   config.WatchdogFeedback,
			wantSource: sourceConfig,
		},
		{
			// Feedback is weaker than the unattended default, and an
			// operator asking for it explicitly gets it. The startup
			// summary is what tells them what they gave up.
			name:       "explicit feedback beats the unattended default",
			in:         guardrailInputs{WatchdogFlag: config.WatchdogFeedback, Unattended: true},
			wantMode:   config.WatchdogFeedback,
			wantSource: sourceFlag,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := resolveGuardrails(tc.in)
			if err != nil {
				t.Fatalf("resolveGuardrails: %v", err)
			}
			if got.Watchdog != tc.wantMode {
				t.Errorf("watchdog: got %q, want %q", got.Watchdog, tc.wantMode)
			}
			if got.WatchdogSource != tc.wantSource {
				t.Errorf("source: got %q, want %q", got.WatchdogSource, tc.wantSource)
			}
		})
	}
}

// TestResolveGuardrails_InvalidWatchdogValue keeps a typo loud rather
// than silently degrading to a default — the same contract
// safety.small_tier_parent and tools.disable already hold.
func TestResolveGuardrails_InvalidWatchdogValue(t *testing.T) {
	t.Parallel()

	if _, err := resolveGuardrails(guardrailInputs{WatchdogFlag: "enfroce"}); err == nil {
		t.Error("--watchdog=enfroce: got nil error, want a config error")
	}
	if _, err := resolveGuardrails(guardrailInputs{WatchdogConfig: "enfroce"}); err == nil {
		t.Error("safety.watchdog=enfroce: got nil error, want a config error")
	}
	// The error has to list every accepted mode. An operator who typo'd
	// "feedbck" and reads a message offering only off|warn|enforce
	// concludes the mode doesn't exist.
	_, err := resolveGuardrails(guardrailInputs{WatchdogFlag: "feedbck"})
	if err == nil {
		t.Fatal("--watchdog=feedbck: got nil error, want a config error")
	}
	for _, mode := range []string{config.WatchdogOff, config.WatchdogWarn, config.WatchdogFeedback, config.WatchdogEnforce} {
		if !strings.Contains(err.Error(), mode) {
			t.Errorf("error message omits the %q mode: %v", mode, err)
		}
	}
}

// TestResolveGuardrails_SessionCostPrecedence covers the documented
// opt-out. An explicit 0 from either the flag or config must beat the
// unattended default, or an operator running a deliberately unbounded
// autonomous job has no way to say so.
func TestResolveGuardrails_SessionCostPrecedence(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		in         guardrailInputs
		want       float64
		wantSource string
	}{
		{
			name:       "explicit flag 0 opts an unattended run out",
			in:         guardrailInputs{SessionCostFlag: 0, SessionCostFlagSet: true, Unattended: true},
			want:       0,
			wantSource: sourceCostFlag,
		},
		{
			name:       "config 0 opts an unattended run out",
			in:         guardrailInputs{SessionCostConfig: f64(0), Unattended: true},
			want:       0,
			wantSource: sourceCostConfig,
		},
		{
			name:       "flag beats config",
			in:         guardrailInputs{SessionCostFlag: 3, SessionCostFlagSet: true, SessionCostConfig: f64(99), Unattended: true},
			want:       3,
			wantSource: sourceCostFlag,
		},
		{
			name:       "config beats the unattended default",
			in:         guardrailInputs{SessionCostConfig: f64(2.5), Unattended: true},
			want:       2.5,
			wantSource: sourceCostConfig,
		},
		{
			name:       "config applies to interactive runs too",
			in:         guardrailInputs{SessionCostConfig: f64(2.5)},
			want:       2.5,
			wantSource: sourceCostConfig,
		},
		{
			name:       "an unset flag does not mask the config value",
			in:         guardrailInputs{SessionCostFlag: 0, SessionCostFlagSet: false, SessionCostConfig: f64(7)},
			want:       7,
			wantSource: sourceCostConfig,
		},
		{
			name:       "a negative ceiling normalizes to disabled",
			in:         guardrailInputs{SessionCostFlag: -1, SessionCostFlagSet: true},
			want:       0,
			wantSource: sourceCostFlag,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := resolveGuardrails(tc.in)
			if err != nil {
				t.Fatalf("resolveGuardrails: %v", err)
			}
			if got.SessionCostUSD != tc.want {
				t.Errorf("session ceiling: got %v, want %v", got.SessionCostUSD, tc.want)
			}
			if got.SessionCostSource != tc.wantSource {
				t.Errorf("source: got %q, want %q", got.SessionCostSource, tc.wantSource)
			}
		})
	}
}

// TestWatchdogFlagDefaultIsEmpty guards the sentinel the whole #660
// precedence chain rests on. The flag used to default to the literal
// "warn", which made safety.watchdog unreachable — config could never
// win because the flag was never "unset". If someone restores a
// non-empty default, this fails.
func TestWatchdogFlagDefaultIsEmpty(t *testing.T) {
	t.Parallel()

	fs := flag.NewFlagSet("core-agent-test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	mode := fs.String("watchdog", "", "behavioral watchdog mode")
	if err := fs.Parse([]string{}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if *mode != "" {
		t.Fatalf("--watchdog default: want empty sentinel, got %q", *mode)
	}

	// And the empty sentinel must still land on warn for an
	// interactive run — the no-flag default behaves as before.
	got, err := resolveGuardrails(guardrailInputs{WatchdogFlag: *mode})
	if err != nil {
		t.Fatalf("resolveGuardrails: %v", err)
	}
	if got.Watchdog != config.WatchdogWarn {
		t.Errorf("empty sentinel, interactive: got %q, want %q", got.Watchdog, config.WatchdogWarn)
	}
}

// TestFlagWasSet covers the presence-vs-value distinction that makes
// "--max-session-cost-usd=0 means no ceiling" expressible.
func TestFlagWasSet(t *testing.T) {
	t.Parallel()

	build := func(args []string) *flag.FlagSet {
		fs := flag.NewFlagSet("core-agent-test", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		fs.Float64("max-session-cost-usd", 0, "session ceiling")
		if err := fs.Parse(args); err != nil {
			t.Fatalf("parse %v: %v", args, err)
		}
		return fs
	}

	if flagWasSet(build(nil), "max-session-cost-usd") {
		t.Error("unset flag: got true, want false")
	}
	// The value equals the default here — only presence distinguishes
	// this case from the one above.
	if !flagWasSet(build([]string{"--max-session-cost-usd=0"}), "max-session-cost-usd") {
		t.Error("explicit =0: got false, want true")
	}
	if !flagWasSet(build([]string{"--max-session-cost-usd=5"}), "max-session-cost-usd") {
		t.Error("explicit =5: got false, want true")
	}
	if flagWasSet(build([]string{"--max-session-cost-usd=5"}), "no-such-flag") {
		t.Error("unknown name: got true, want false")
	}
}

// unpricedTestModel is priced by no layer on any backend; pricedTestModel
// is in the built-in table.
const (
	unpricedTestModel = "house/model"
	pricedTestModel   = "gemini-2.5-flash"
)

func TestCeilingPricing_Check(t *testing.T) {
	if !usage.PriceOn("", unpricedTestModel).Unpriced || usage.PriceOn(config.ProviderGemini, pricedTestModel).Unpriced {
		t.Fatal("fixture models no longer isolate priced from unpriced")
	}
	turn := ceilingPricing{ceiling: agent.CostCeiling{MaxTurnUSD: 1}, sessionSource: sourceCostUnset}
	sessionFlag := ceilingPricing{ceiling: agent.CostCeiling{MaxSessionUSD: 5}, sessionSource: sourceCostFlag}
	unattended := ceilingPricing{ceiling: agent.CostCeiling{MaxSessionUSD: 10}, sessionSource: sourceCostUnattendedDefault}
	none := ceilingPricing{sessionSource: sourceCostUnset}

	for _, backend := range []string{"vllm", config.ProviderGemini} {
		// Every provider now, not only profiles: a Gemini, Claude,
		// OpenAI or xAI model can be unpriced too (a typo, or a model
		// newer than the catalog).
		if _, err := turn.check("model", unpricedTestModel, backend, 0); err == nil || !strings.Contains(err.Error(), "models[].rates") || !strings.Contains(err.Error(), "drop the ceiling") {
			t.Errorf("%s: operator turn ceiling over an unpriced model: err = %v, want a refusal naming the remedies", backend, err)
		}
		if _, err := sessionFlag.check("model", unpricedTestModel, backend, 0); err == nil {
			t.Errorf("%s: operator session ceiling over an unpriced model was accepted", backend)
		}
		warn, err := unattended.check("model", unpricedTestModel, backend, 0)
		if err != nil || !strings.Contains(warn, "NOT protecting") {
			t.Errorf("%s: unattended default: warn = %q, err = %v; want a warning and no refusal", backend, warn, err)
		}
	}
	if warn, err := turn.check("model", pricedTestModel, config.ProviderGemini, 0); warn != "" || err != nil {
		t.Errorf("priced model: %q, %v; want nothing", warn, err)
	}
	if warn, err := none.check("model", unpricedTestModel, "vllm", 0); warn != "" || err != nil {
		t.Errorf("no ceiling at all: %q, %v; want nothing", warn, err)
	}
	// A subagent's own budget is always somebody's decision, and it
	// cannot hold on an unpriced model: refused with or without a ceiling.
	for _, p := range []ceilingPricing{none, unattended, turn} {
		if _, err := p.check(`subagent "x": model`, unpricedTestModel, "vllm", 0.25); err == nil || !strings.Contains(err.Error(), "max_cost_usd") {
			t.Errorf("budget on an unpriced model under %+v: err = %v, want a refusal naming the budget", p.ceiling, err)
		}
	}
	// A budget on a priced model is fine.
	if warn, err := turn.check("model", pricedTestModel, config.ProviderGemini, 0.25); warn != "" || err != nil {
		t.Errorf("priced model with a budget: %q, %v; want nothing", warn, err)
	}
}

func TestCeilingPricing_StartupReachesSubagentsAndTheSmallTier(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Model.Provider = config.ProviderGemini
	cfg.Model.Name = pricedTestModel
	templates := []background.SubagentTemplate{
		{Name: "ok", ModelID: pricedTestModel, Backend: config.ProviderGemini},
		{Name: "selfhosted", ModelID: unpricedTestModel, Backend: "vllm"},
		{Name: "budgeted", ModelID: unpricedTestModel, Backend: "vllm", Budgets: background.Budgets{MaxCost: 0.5}},
	}
	ms := startupCeilingModels(cfg, templates, "acme-small-unpriced")

	turn := ceilingPricing{ceiling: agent.CostCeiling{MaxTurnUSD: 1}, sessionSource: sourceCostUnset}
	_, err := turn.checkStartup(ms)
	if err == nil {
		t.Fatal("unpriced subagents and small tier under an operator ceiling were accepted")
	}
	for _, want := range []string{`subagent "selfhosted"`, `subagent "budgeted"`, "small-tier", "max_cost_usd"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("startup refusal does not name %q (all refusals are reported at once):\n%v", want, err)
		}
	}
	if strings.Contains(err.Error(), `subagent "ok"`) {
		t.Errorf("a priced subagent was refused:\n%v", err)
	}

	// Under only the unattended default, the ceiling refusals become
	// warnings, but the declared budget still refuses.
	unattended := ceilingPricing{ceiling: agent.CostCeiling{MaxSessionUSD: 10}, sessionSource: sourceCostUnattendedDefault}
	warns, err := unattended.checkStartup(ms)
	if err == nil || !strings.Contains(err.Error(), `subagent "budgeted"`) || strings.Contains(err.Error(), `subagent "selfhosted"`) {
		t.Errorf("unattended default: err = %v, want only the budgeted subagent refused", err)
	}
	if len(warns) != 2 {
		t.Errorf("unattended default warnings = %d (%q), want 2: the self-hosted subagent and the small tier", len(warns), warns)
	}
}

// TestCeilingPricing_ZeroRatesEscapeHatchIsModelPricing pins the remedy
// both halves of the unpriced-model policy name, through the real
// config loader. A self-hosted model nobody pays per token for is
// priced at $0 by zero rates in model.pricing. Zero rates in a profile's
// models[].rates are not a way out: core-models rejects them at load,
// so a message that suggested them sent the operator to a config that
// does not start.
func TestCeilingPricing_ZeroRatesEscapeHatchIsModelPricing(t *testing.T) {
	t.Parallel()
	load := func(t *testing.T, body string) (*config.Config, error) {
		t.Helper()
		agents := filepath.Join(t.TempDir(), ".agents")
		if err := os.MkdirAll(agents, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(agents, config.ConfigFileName), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return config.Load(agents)
	}
	profile := func(rates string) string {
		return `{"name":"house-vllm","extends":"vllm","base_url":"http://10.0.0.2:8000/v1",` +
			`"models":[{"id":"` + unpricedTestModel + `"` + rates + `}]}`
	}

	t.Run("model.pricing zero rates are priced", func(t *testing.T) {
		t.Parallel()
		cfg, err := load(t, `{"version":1,"model":{"name":"`+unpricedTestModel+`","provider":"house-vllm",`+
			`"pricing":{"`+unpricedTestModel+`":{"input_per_mtok":0,"output_per_mtok":0}}},"providers":[`+profile("")+`]}`)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		cat, err := pricing.NewCatalog(pricing.Options{
			CfgOverride: compose.CfgToCatalogOverride(cfg.Model.Pricing),
			Declared:    usage.DeclaredRates(cfg),
		})
		if err != nil {
			t.Fatalf("NewCatalog: %v", err)
		}
		if _, _, found := cat.LookupForWithSource(usage.BackendFor(cfg), unpricedTestModel); !found {
			t.Error("zero rates in model.pricing read as unpriced; the remedy the ceiling messages name would not work")
		}
	})

	t.Run("profile zero rates are rejected at load", func(t *testing.T) {
		t.Parallel()
		_, err := load(t, `{"version":1,"model":{"name":"`+unpricedTestModel+`","provider":"house-vllm"},"providers":[`+
			profile(`,"rates":{"input_per_mtok":0,"output_per_mtok":0}`)+`]}`)
		if err == nil || !strings.Contains(err.Error(), "leave rates out instead") {
			t.Fatalf("Load with all-zero profile rates = %v, want core-models' rejection", err)
		}
	})

	if msg := unpricedCeilingWhy("model", unpricedTestModel, "vllm"); !strings.Contains(msg, "model.pricing") || strings.Contains(msg, "zero rates price") {
		t.Errorf("the remedy must point a $0 model at model.pricing, not at zero profile rates:\n%s", msg)
	}
}

func TestCeilingPricing_DoorWarnsOrRefuses(t *testing.T) {
	turn := ceilingPricing{ceiling: agent.CostCeiling{MaxTurnUSD: 1}, sessionSource: sourceCostUnset}
	if err := turn.door("model", unpricedTestModel, "vllm"); err == nil {
		t.Error("/model to an unpriced model under an operator ceiling was allowed")
	}
	unattended := ceilingPricing{ceiling: agent.CostCeiling{MaxSessionUSD: 10}, sessionSource: sourceCostUnattendedDefault}
	if err := unattended.door("model", unpricedTestModel, "vllm"); err != nil {
		t.Errorf("under only the unattended default a door must warn, not refuse: %v", err)
	}
}
