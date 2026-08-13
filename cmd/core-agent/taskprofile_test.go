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
	"slices"
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/taskclass"
	"github.com/go-steer/core-agent/v2/pkg/tools"
)

func TestResolveProfileDisables(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name           string
		profile        []string
		configDisable  []string
		flagDisable    []string
		enable         []string
		want           []string
		wantErrSnippet string
	}{
		{
			name:    "no task class disables nothing",
			profile: nil,
			want:    nil,
		},
		{
			name:    "profile disable applies",
			profile: []string{"bash"},
			want:    []string{"bash"},
		},
		{
			name:    "enable-tools cancels the profile",
			profile: []string{"bash"},
			enable:  []string{"bash"},
			want:    nil,
		},
		{
			name:    "enable-tools only cancels what it names",
			profile: []string{"bash", "write_file"},
			enable:  []string{"bash"},
			want:    []string{"write_file"},
		},
		{
			name:    "enabling a tool the profile never dropped is a no-op",
			profile: nil,
			enable:  []string{"bash"},
			want:    nil,
		},
		{
			name:          "profile disable composes with config disable",
			profile:       []string{"bash"},
			configDisable: []string{"write_file"},
			want:          []string{"bash"},
		},
		{
			name:           "enable-tools cannot undo tools.disable",
			profile:        []string{"bash"},
			configDisable:  []string{"bash"},
			enable:         []string{"bash"},
			wantErrSnippet: "conflicts with tools.disable",
		},
		{
			name:           "enable-tools cannot undo --disable-tools",
			profile:        []string{"bash"},
			flagDisable:    []string{"bash"},
			enable:         []string{"bash"},
			wantErrSnippet: "conflicts with --disable-tools",
		},
		{
			name:           "unknown enable name fails at startup",
			profile:        []string{"bash"},
			enable:         []string{"bahs"},
			wantErrSnippet: `unknown built-in tool "bahs"`,
		},
		{
			// main.go runs this resolution for every start, not just
			// --task ones, so a typo has to fail even when no profile
			// disabled anything. Otherwise --enable-tools would be
			// validated or ignored depending on an unrelated flag.
			name:           "unknown enable name fails with no task class",
			profile:        nil,
			enable:         []string{"bahs"},
			wantErrSnippet: `unknown built-in tool "bahs"`,
		},
		{
			name:           "conflict is caught with no task class",
			profile:        nil,
			configDisable:  []string{"bash"},
			enable:         []string{"bash"},
			wantErrSnippet: "conflicts with tools.disable",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := resolveProfileDisables(tc.profile, tc.configDisable, tc.flagDisable, tc.enable)
			if tc.wantErrSnippet != "" {
				if err == nil {
					t.Fatalf("want error containing %q, got nil (result %v)", tc.wantErrSnippet, got)
				}
				if !strings.Contains(err.Error(), tc.wantErrSnippet) {
					t.Errorf("error = %q, want it to contain %q", err, tc.wantErrSnippet)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("disables = %v, want %v", got, tc.want)
			}
		})
	}
}

// The conflict errors are the load-bearing half of the precedence
// rule: `tools.disable` is a hardening statement, and a flag that
// quietly undid it would make the config file a claim the runtime
// doesn't keep. Assert the message tells the operator where the other
// half of the conflict lives, since "it errored" is useless without it.
func TestResolveProfileDisables_ConflictErrorNamesTheOtherSource(t *testing.T) {
	t.Parallel()
	_, err := resolveProfileDisables([]string{"bash"}, []string{"bash"}, nil, []string{"bash"})
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "config file") {
		t.Errorf("error doesn't point at the config file: %v", err)
	}
}

func TestResolvePlanMode(t *testing.T) {
	t.Parallel()

	// cfgMode / cfgLegacy are fed through the real config.PermissionsConfig
	// accessors rather than hand-built planModeInputs fields, so this
	// table exercises the same fold main.go uses. A test that hardcoded
	// the triple could pass with the two spellings disagreeing.
	cases := []struct {
		name       string
		in         planModeInputs
		cfgMode    string
		cfgLegacy  bool
		wantMode   string
		wantSource string
	}{
		{
			name:       "nothing set",
			in:         planModeInputs{CanRecordPlan: true},
			wantMode:   config.PlanModeOff,
			wantSource: sourcePlanUnset,
		},
		{
			name:       "profile turns it on",
			in:         planModeInputs{Profile: true, CanRecordPlan: true},
			wantMode:   config.PlanModeRequired,
			wantSource: sourcePlanProfile,
		},
		{
			name:       "deprecated config bool turns it on without a task class",
			in:         planModeInputs{CanRecordPlan: true},
			cfgLegacy:  true,
			wantMode:   config.PlanModeRequired,
			wantSource: sourcePlanConfig,
		},
		{
			name:       "explicit --plan-first=false beats the profile",
			in:         planModeInputs{Flag: false, FlagSet: true, Profile: true, CanRecordPlan: true},
			wantMode:   config.PlanModeOff,
			wantSource: sourcePlanFlag,
		},
		{
			name:       "explicit --plan-first=false beats the config",
			in:         planModeInputs{Flag: false, FlagSet: true, CanRecordPlan: true},
			cfgLegacy:  true,
			wantMode:   config.PlanModeOff,
			wantSource: sourcePlanFlag,
		},
		{
			name:       "explicit --plan-first with no task class or config",
			in:         planModeInputs{Flag: true, FlagSet: true, CanRecordPlan: true},
			wantMode:   config.PlanModeRequired,
			wantSource: sourcePlanFlag,
		},
		{
			// The deadlock case: record_plan doesn't register without
			// a .agents/ directory, and /replan only revokes, so a
			// profile-driven gate here would deny every mutating call
			// for the life of the session with no way to clear it.
			name:       "profile is suppressed when record_plan cannot register",
			in:         planModeInputs{Profile: true, CanRecordPlan: false},
			wantMode:   config.PlanModeOff,
			wantSource: sourcePlanNoRecorder,
		},
		{
			// Explicit operator intent is still honored — main.go
			// warns loudly rather than second-guessing it.
			name:       "explicit --plan-first is honored even without record_plan",
			in:         planModeInputs{Flag: true, FlagSet: true, CanRecordPlan: false},
			wantMode:   config.PlanModeRequired,
			wantSource: sourcePlanFlag,
		},
		{
			name:       "deprecated config bool is honored even without record_plan",
			in:         planModeInputs{CanRecordPlan: false},
			cfgLegacy:  true,
			wantMode:   config.PlanModeRequired,
			wantSource: sourcePlanConfig,
		},

		// --- #215: the three-way mode ---
		{
			name:       "config advisory with no flags",
			in:         planModeInputs{CanRecordPlan: true},
			cfgMode:    config.PlanModeAdvisory,
			wantMode:   config.PlanModeAdvisory,
			wantSource: sourcePlanConfigMode,
		},
		{
			// The whole point of the issue: a recipe that wants the
			// audit artifact must be able to keep it while overriding a
			// task profile that would otherwise arm the gate.
			name:       "config advisory beats the task profile",
			in:         planModeInputs{Profile: true, CanRecordPlan: true},
			cfgMode:    config.PlanModeAdvisory,
			wantMode:   config.PlanModeAdvisory,
			wantSource: sourcePlanConfigMode,
		},
		{
			name:       "config off beats the task profile",
			in:         planModeInputs{Profile: true, CanRecordPlan: true},
			cfgMode:    config.PlanModeOff,
			wantMode:   config.PlanModeOff,
			wantSource: sourcePlanConfigMode,
		},
		{
			name:       "--plan-mode beats config",
			in:         planModeInputs{FlagMode: config.PlanModeAdvisory, CanRecordPlan: true},
			cfgMode:    config.PlanModeRequired,
			wantMode:   config.PlanModeAdvisory,
			wantSource: sourcePlanModeFlag,
		},
		{
			name:       "--plan-mode beats the deprecated config bool",
			in:         planModeInputs{FlagMode: config.PlanModeAdvisory, CanRecordPlan: true},
			cfgLegacy:  true,
			wantMode:   config.PlanModeAdvisory,
			wantSource: sourcePlanModeFlag,
		},
		{
			// Both flags passed: the expressive one wins, so a script
			// migrating to --plan-mode can leave the old flag in place
			// during the transition without silently losing advisory.
			name:       "--plan-mode beats --plan-first",
			in:         planModeInputs{FlagMode: config.PlanModeAdvisory, Flag: true, FlagSet: true, CanRecordPlan: true},
			wantMode:   config.PlanModeAdvisory,
			wantSource: sourcePlanModeFlag,
		},
		{
			name:       "--plan-mode=off beats everything",
			in:         planModeInputs{FlagMode: config.PlanModeOff, Flag: true, FlagSet: true, Profile: true, CanRecordPlan: true},
			cfgMode:    config.PlanModeRequired,
			cfgLegacy:  true,
			wantMode:   config.PlanModeOff,
			wantSource: sourcePlanModeFlag,
		},
		{
			// Advisory can't deadlock — nothing is ever denied — so it
			// is honored rather than suppressed. main.go warns that the
			// artifact won't be written.
			name:       "advisory is honored even without record_plan",
			in:         planModeInputs{CanRecordPlan: false},
			cfgMode:    config.PlanModeAdvisory,
			wantMode:   config.PlanModeAdvisory,
			wantSource: sourcePlanConfigMode,
		},
		{
			// The deprecated bool sits UNDER plan_mode, so a config
			// carrying both required-via-bool and advisory-via-mode
			// resolves advisory. (Validate rejects the one genuinely
			// contradictory pair, plan_mode=off + bool true.)
			name:       "plan_mode outranks the deprecated bool",
			in:         planModeInputs{CanRecordPlan: true},
			cfgMode:    config.PlanModeAdvisory,
			cfgLegacy:  true,
			wantMode:   config.PlanModeAdvisory,
			wantSource: sourcePlanConfigMode,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			perms := config.PermissionsConfig{PlanMode: tc.cfgMode, RequirePlanArtifact: tc.cfgLegacy} //nolint:staticcheck // SA1019: the deprecated spelling is half of what's under test
			in := tc.in
			in.ConfigSet = perms.PlanModeSet()
			in.ConfigResolved = perms.ResolvedPlanMode()
			in.ConfigSpelling = perms.PlanModeSpelling()
			mode, source := resolvePlanMode(in)
			if mode != tc.wantMode || source != tc.wantSource {
				t.Errorf("resolvePlanMode = (%q, %q), want (%q, %q)", mode, source, tc.wantMode, tc.wantSource)
			}
		})
	}
}

// --plan-first=false is the documented escape from a profile that
// turns the gate on, which only works if main.go can tell an explicit
// false from the flag's default. Guard the flagWasSet plumbing the
// same way the --max-session-cost-usd=0 case is guarded.
func TestPlanFirstFlag_ExplicitFalseIsDistinguishable(t *testing.T) {
	t.Parallel()
	build := func(args []string) *flag.FlagSet {
		fs := flag.NewFlagSet("core-agent-test", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		fs.Bool("plan-first", false, "")
		if err := fs.Parse(args); err != nil {
			t.Fatalf("parse: %v", err)
		}
		return fs
	}
	if flagWasSet(build(nil), "plan-first") {
		t.Error("unset --plan-first reported as set")
	}
	if !flagWasSet(build([]string{"--plan-first=false"}), "plan-first") {
		t.Error("explicit --plan-first=false must be distinguishable from the default")
	}
	if !flagWasSet(build([]string{"--plan-first"}), "plan-first") {
		t.Error("--plan-first reported as unset")
	}
}

// A --plan-mode typo must be a startup error, not a silently-off gate.
// resolvePlanMode passes the flag value straight through, so the only
// thing standing between `--plan-mode=advisery` and a config field
// ResolvedPlanMode reads as "off" is this check. The parity half keeps
// the flag and config.Validate accepting the same set — the two
// validators drifting is how a value works in a config file and fails
// on the command line, or worse, the reverse.
func TestValidatePlanModeFlag(t *testing.T) {
	t.Parallel()
	cases := []struct {
		v       string
		wantErr bool
	}{
		{"", false}, // not passed
		{config.PlanModeOff, false},
		{config.PlanModeAdvisory, false},
		{config.PlanModeRequired, false},
		{"advisery", true},
		{"Advisory", true},
		{"true", true},
		{"on", true},
	}
	for _, tc := range cases {
		t.Run("value="+tc.v, func(t *testing.T) {
			t.Parallel()
			err := validatePlanModeFlag(tc.v)
			if tc.wantErr != (err != nil) {
				t.Fatalf("validatePlanModeFlag(%q) = %v, wantErr %v", tc.v, err, tc.wantErr)
			}
			// Parity: config.Validate must agree about this value.
			cfg := config.DefaultConfig()
			cfg.Permissions.PlanMode = tc.v
			cfgErr := cfg.Validate()
			if (err != nil) != (cfgErr != nil) {
				t.Errorf("plan_mode=%q: flag err=%v but config.Validate err=%v — the two validators disagree", tc.v, err, cfgErr)
			}
		})
	}
}

// Every name a profile disables has to be one BuiltinTools.Disable
// accepts — a typo in the table would otherwise abort startup for
// whichever operator picked that class first.
func TestProfileDisableNames_AreRealBuiltins(t *testing.T) {
	t.Parallel()
	known := tools.BuiltinToolNames()
	for _, class := range taskclass.Classes() {
		profile, ok := taskclass.Resolve(class)
		if !ok {
			t.Fatalf("Resolve(%q) failed", class)
		}
		for _, name := range profile.DisableTools {
			if !slices.Contains(known, name) {
				t.Errorf("task class %q disables %q, which is not a built-in tool", class, name)
			}
			b := tools.Default()
			if err := b.Disable(name); err != nil {
				t.Errorf("task class %q: Disable(%q): %v", class, name, err)
			}
		}
	}
}

// A class that requires a plan but keeps the tools that plan-first
// gates is coherent; a class that requires a plan and can't record one
// is not. record_plan is registered by tools.Build, not by the
// profile, so the only way a profile could break the escape hatch is
// by disabling it — check that no class does.
func TestProfilesDontDisableTheirOwnEscapeHatch(t *testing.T) {
	t.Parallel()
	for _, class := range taskclass.Classes() {
		profile, _ := taskclass.Resolve(class)
		if !profile.RequirePlanArtifact {
			continue
		}
		if slices.Contains(profile.DisableTools, "record_plan") {
			t.Errorf("task class %q requires a plan artifact but disables record_plan", class)
		}
	}
}
