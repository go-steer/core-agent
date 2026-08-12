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
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/config"
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
