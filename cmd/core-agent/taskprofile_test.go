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

func TestResolvePlanFirst(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		in         planFirstInputs
		wantOn     bool
		wantSource string
	}{
		{
			name:       "nothing set",
			in:         planFirstInputs{CanRecordPlan: true},
			wantOn:     false,
			wantSource: sourcePlanUnset,
		},
		{
			name:       "profile turns it on",
			in:         planFirstInputs{Profile: true, CanRecordPlan: true},
			wantOn:     true,
			wantSource: sourcePlanProfile,
		},
		{
			name:       "config turns it on without a task class",
			in:         planFirstInputs{Config: true, CanRecordPlan: true},
			wantOn:     true,
			wantSource: sourcePlanConfig,
		},
		{
			name:       "explicit false beats the profile",
			in:         planFirstInputs{Flag: false, FlagSet: true, Profile: true, CanRecordPlan: true},
			wantOn:     false,
			wantSource: sourcePlanFlag,
		},
		{
			name:       "explicit false beats the config",
			in:         planFirstInputs{Flag: false, FlagSet: true, Config: true, CanRecordPlan: true},
			wantOn:     false,
			wantSource: sourcePlanFlag,
		},
		{
			name:       "explicit true with no task class or config",
			in:         planFirstInputs{Flag: true, FlagSet: true, CanRecordPlan: true},
			wantOn:     true,
			wantSource: sourcePlanFlag,
		},
		{
			// The deadlock case: record_plan doesn't register without
			// a .agents/ directory, and /replan only revokes, so a
			// profile-driven gate here would deny every mutating call
			// for the life of the session with no way to clear it.
			name:       "profile is suppressed when record_plan cannot register",
			in:         planFirstInputs{Profile: true, CanRecordPlan: false},
			wantOn:     false,
			wantSource: sourcePlanNoRecorder,
		},
		{
			// Explicit operator intent is still honored — main.go
			// warns loudly rather than second-guessing it.
			name:       "explicit true is honored even without record_plan",
			in:         planFirstInputs{Flag: true, FlagSet: true, CanRecordPlan: false},
			wantOn:     true,
			wantSource: sourcePlanFlag,
		},
		{
			name:       "config true is honored even without record_plan",
			in:         planFirstInputs{Config: true, CanRecordPlan: false},
			wantOn:     true,
			wantSource: sourcePlanConfig,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			on, source := resolvePlanFirst(tc.in)
			if on != tc.wantOn || source != tc.wantSource {
				t.Errorf("resolvePlanFirst = (%v, %q), want (%v, %q)", on, source, tc.wantOn, tc.wantSource)
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
