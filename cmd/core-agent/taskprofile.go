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
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/tools"
)

// toolProfileOpts bundles the two CLI knobs that exist so a task
// profile's tool and plan-first opinions stay overridable (#160).
// Bundled rather than passed loose because the plan-first value is
// meaningless without the "operator actually typed it" bit — false is
// the flag's default AND its most important explicit value.
type toolProfileOpts struct {
	// enableTools is --enable-tools: a comma-separated list of
	// built-ins to add back after a task profile dropped them. It
	// cancels the profile only; see resolveProfileDisables.
	enableTools string
	// planFirst is --plan-first, qualified by planFirstSet.
	// --plan-first=false is how an operator opts out of a profile
	// that turns the gate on, so it must be distinguishable from the
	// flag sitting at its default.
	//
	// Deprecated alongside permissions.require_plan_artifact: it can
	// only say off/required. --plan-mode is the three-way spelling.
	planFirst    bool
	planFirstSet bool
	// planMode is --plan-mode: off | advisory | required. Empty means
	// the operator didn't pass it.
	planMode string
}

// splitList parses a comma-separated CLI list, dropping empties and
// surrounding whitespace. Shared by --enable-tools and the callers
// that need the same shape as --disable-tools.
func splitList(s string) []string {
	var out []string
	for _, name := range strings.Split(s, ",") {
		if name = strings.TrimSpace(name); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// resolveProfileDisables returns the built-in tools a task profile
// should switch off, after subtracting anything --enable-tools asked
// to keep.
//
// Precedence, and the reason for it: a task profile is a default, and
// defaults lose to explicit operator input in both directions. So
// --enable-tools cancels the *profile's* disable and nothing else. It
// deliberately cannot resurrect a tool the operator turned off in
// tools.disable or --disable-tools — those are hardening statements,
// and a flag that quietly undid them would make `tools.disable` a
// claim the runtime doesn't keep. Asking for that combination is an
// error rather than a silent win for either side, because both
// readings ("I want it off" / "I want it back") are plausible and
// guessing wrong is a security-shaped mistake.
//
// Naming a tool the profile never disabled is not an error: it is a
// no-op, so `--task=chat --enable-tools=bash` (chat never dropped it)
// behaves the way an operator scripting across classes would expect.
// Unknown names ARE an error, so typos fail at startup rather than
// silently leaving a tool registered.
func resolveProfileDisables(profileDisable, configDisable, flagDisable []string, enable []string) ([]string, error) {
	known := tools.BuiltinToolNames()
	for _, name := range enable {
		if !slices.Contains(known, name) {
			sorted := append([]string{}, known...)
			sort.Strings(sorted)
			return nil, fmt.Errorf("--enable-tools: unknown built-in tool %q (want one of %s)", name, strings.Join(sorted, ", "))
		}
		if slices.Contains(configDisable, name) {
			return nil, fmt.Errorf("--enable-tools=%s conflicts with tools.disable in the config file; remove it there if you want the tool back", name)
		}
		if slices.Contains(flagDisable, name) {
			return nil, fmt.Errorf("--enable-tools=%s conflicts with --disable-tools=%s", name, name)
		}
	}
	var out []string
	for _, name := range profileDisable {
		if slices.Contains(enable, name) {
			continue
		}
		out = append(out, name)
	}
	return out, nil
}

// planModeInputs is everything resolvePlanMode needs. Split out for
// the same reason guardrailInputs is: the precedence chain is worth a
// table test, not an inline conditional nobody exercises.
type planModeInputs struct {
	// FlagMode is --plan-mode verbatim; "" means the operator didn't
	// pass it. Already validated against the three constants by the
	// caller, so an unknown value never reaches here.
	FlagMode string
	// Flag is the deprecated --plan-first and FlagSet reports whether
	// the operator typed it. Both values are meaningful when set:
	// true == required, false == off.
	Flag    bool
	FlagSet bool
	// ConfigSet, ConfigResolved and ConfigSpelling come from
	// PermissionsConfig.PlanModeSet / ResolvedPlanMode /
	// PlanModeSpelling — the config half of the chain, already folded
	// by the one type that owns both spellings. Deliberately not the
	// two raw fields: a second place that knows plan_mode outranks
	// require_plan_artifact is a second place that can get it wrong.
	// ConfigSet is what distinguishes "the operator wrote off" from
	// "the operator said nothing", which ConfigResolved alone cannot.
	ConfigSet      bool
	ConfigResolved string
	ConfigSpelling string
	// Profile is the task class's RequirePlanArtifact.
	Profile bool
	// CanRecordPlan reports that record_plan will actually be
	// registered for this run — there is a .agents/ directory to
	// persist plans into, the built-in suite is on, and nothing
	// disabled the tool.
	//
	// It gates the profile's opinion because required-without-
	// record_plan is a deadlock, not a stricter posture: every
	// mutating call is denied for the life of the session and nothing
	// can clear the flag (/replan only revokes a plan, it can't grant
	// one). An explicit flag or config value is still honored — the
	// operator said it out loud — but the caller warns.
	CanRecordPlan bool
}

// Sources reported by resolvePlanMode, so the startup line can say
// which input won without re-deriving the chain.
const (
	sourcePlanModeFlag   = "--plan-mode flag"
	sourcePlanFlag       = "--plan-first flag"
	sourcePlanConfigMode = "permissions.plan_mode config"
	sourcePlanConfig     = "permissions.require_plan_artifact config"
	sourcePlanProfile    = "task profile"
	sourcePlanNoRecorder = "task profile (suppressed: record_plan won't register)"
	sourcePlanUnset      = "unset"
)

// validatePlanModeFlag rejects an unknown --plan-mode value. The flag
// path never reaches config.Validate, and resolvePlanMode takes the
// flag's word for it, so without this a typo (`--plan-mode=advisery`)
// would sail through and land in cfg.Permissions.PlanMode as a value
// ResolvedPlanMode reads as "off" — a silently disabled gate, which is
// the failure mode this whole area exists to remove. Empty is fine: it
// means the operator didn't pass the flag.
func validatePlanModeFlag(v string) error {
	switch v {
	case "", config.PlanModeOff, config.PlanModeAdvisory, config.PlanModeRequired:
		return nil
	default:
		return fmt.Errorf("--plan-mode: unknown value %q (want one of %q, %q, %q)",
			v, config.PlanModeOff, config.PlanModeAdvisory, config.PlanModeRequired)
	}
}

// resolvePlanMode decides the effective plan mode (#215).
//
// Precedence: --plan-mode > --plan-first (either value) >
// permissions.plan_mode > require_plan_artifact: true > the task
// profile > off. Both deprecated spellings sit directly under their
// replacement so a config that carries the old field keeps behaving
// the way it did, and either CLI flag still overrides config.
//
// The profile can only reach "required"; an operator who wrote a mode
// in config is never overruled by a class default, and one who wants
// the gate off passes --plan-mode=off (or the old --plan-first=false).
func resolvePlanMode(in planModeInputs) (mode string, source string) {
	switch {
	case in.FlagMode != "":
		return in.FlagMode, sourcePlanModeFlag
	case in.FlagSet:
		if in.Flag {
			return config.PlanModeRequired, sourcePlanFlag
		}
		return config.PlanModeOff, sourcePlanFlag
	case in.ConfigSet:
		// Derived from the spelling rather than matched against it: a
		// literal comparison here would silently mislabel the source
		// if PlanModeSpelling ever grew a value. The two
		// sourcePlanConfig* constants are what this produces today.
		return in.ConfigResolved, "permissions." + in.ConfigSpelling + " config"
	case in.Profile && in.CanRecordPlan:
		return config.PlanModeRequired, sourcePlanProfile
	case in.Profile:
		// Downgrade to advisory rather than off would be worse than
		// useless: without record_plan there is no artifact either.
		return config.PlanModeOff, sourcePlanNoRecorder
	default:
		return config.PlanModeOff, sourcePlanUnset
	}
}
