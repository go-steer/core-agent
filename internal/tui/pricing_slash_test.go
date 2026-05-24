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

package tui

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
)

func TestHandlePricingRefresh_CallsHook(t *testing.T) {
	t.Parallel()
	m := newSlashTestModel(t)
	var called atomic.Int32
	m.refreshPricing = func(_ context.Context) (string, error) {
		called.Add(1)
		return "Refresh: updated 247 models from upstream", nil
	}

	m.handlePricingCommand("refresh")

	if called.Load() != 1 {
		t.Errorf("refreshPricing called %d times, want 1", called.Load())
	}
	out := lastSystemMessage(t, m)
	if !strings.Contains(out, "updated 247 models") {
		t.Errorf("system message missing refresh summary: %q", out)
	}
}

func TestHandlePricingRefresh_NoHookErrors(t *testing.T) {
	t.Parallel()
	m := newSlashTestModel(t)
	// refreshPricing intentionally nil.

	m.handlePricingCommand("refresh")

	out := lastSystemMessage(t, m)
	if !strings.Contains(out, "not available") {
		t.Errorf("expected 'not available' message when hook nil; got %q", out)
	}
}

func TestHandlePricingSet_ParsesAndCallsHook(t *testing.T) {
	t.Parallel()
	m := newSlashTestModel(t)
	var gotModel string
	var gotIn, gotOut float64
	m.setPricing = func(model string, in, out float64) (string, error) {
		gotModel, gotIn, gotOut = model, in, out
		return "Set ok", nil
	}

	m.handlePricingCommand("set gemini-3.5-flash 0.075 0.30")

	if gotModel != "gemini-3.5-flash" {
		t.Errorf("model = %q, want gemini-3.5-flash", gotModel)
	}
	if gotIn != 0.075 || gotOut != 0.30 {
		t.Errorf("rates = (%v, %v), want (0.075, 0.30)", gotIn, gotOut)
	}
}

func TestHandlePricingSet_RejectsBadArity(t *testing.T) {
	t.Parallel()
	m := newSlashTestModel(t)
	m.setPricing = func(_ string, _, _ float64) (string, error) {
		t.Error("setPricing should not have been called for bad arity")
		return "", nil
	}

	m.handlePricingCommand("set onlyone")

	out := lastSystemMessage(t, m)
	if !strings.Contains(out, "Usage:") {
		t.Errorf("expected usage message; got %q", out)
	}
}

func TestHandlePricingSet_RejectsNonNumericRates(t *testing.T) {
	t.Parallel()
	m := newSlashTestModel(t)
	m.setPricing = func(_ string, _, _ float64) (string, error) {
		t.Error("setPricing should not have been called for non-numeric rates")
		return "", nil
	}

	m.handlePricingCommand("set claude-opus-4-7 fifteen seventyfive")

	out := lastSystemMessage(t, m)
	if !strings.Contains(out, "must be numbers") {
		t.Errorf("expected numeric-validation message; got %q", out)
	}
}

func TestHandlePricingSet_RejectsNegativeRates(t *testing.T) {
	t.Parallel()
	m := newSlashTestModel(t)
	m.setPricing = func(_ string, _, _ float64) (string, error) {
		t.Error("setPricing should not have been called for negative rates")
		return "", nil
	}

	m.handlePricingCommand("set claude-opus-4-7 -1 5")

	out := lastSystemMessage(t, m)
	if !strings.Contains(out, "non-negative") {
		t.Errorf("expected non-negative message; got %q", out)
	}
}

func TestHandlePricingCommand_BareUsage(t *testing.T) {
	t.Parallel()
	m := newSlashTestModel(t)

	m.handlePricingCommand("")

	out := lastSystemMessage(t, m)
	if !strings.Contains(out, "/pricing refresh") || !strings.Contains(out, "/pricing set") {
		t.Errorf("bare /pricing should print usage for both subcommands; got %q", out)
	}
}

func TestHandlePricingCommand_UnknownSubcommand(t *testing.T) {
	t.Parallel()
	m := newSlashTestModel(t)

	m.handlePricingCommand("nonsense")

	out := lastSystemMessage(t, m)
	if !strings.Contains(out, "Unknown") {
		t.Errorf("unknown subcommand should report Unknown; got %q", out)
	}
}

func TestParseSlash_PricingRoutes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		input string
		want  SlashAction
		args  string
	}{
		{"/pricing", SlashPricing, ""},
		{"/pricing refresh", SlashPricing, "refresh"},
		{"/pricing set gemini-3.5-flash 0.075 0.30", SlashPricing, "set gemini-3.5-flash 0.075 0.30"},
	}
	for _, tc := range cases {
		action, _, args, isSlash := ParseSlash(tc.input)
		if !isSlash || action != tc.want || args != tc.args {
			t.Errorf("ParseSlash(%q) = (%v, %q, %v), want (%v, %q, true)",
				tc.input, action, args, isSlash, tc.want, tc.args)
		}
	}
}
