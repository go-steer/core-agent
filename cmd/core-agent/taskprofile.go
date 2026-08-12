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
	planFirst    bool
	planFirstSet bool
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

// planFirstInputs is everything resolvePlanFirst needs. Split out for
// the same reason guardrailInputs is: the precedence chain is worth a
// table test, not an inline conditional nobody exercises.
type planFirstInputs struct {
	// Flag is --plan-first and FlagSet reports whether the operator
	// typed it. Both values are meaningful when set.
	Flag    bool
	FlagSet bool
	// Config is permissions.require_plan_artifact. A bool can't tell
	// "false" from "absent", so only true is load-bearing here — the
	// profile is what fills the gap.
	Config bool
	// Profile is the task class's RequirePlanArtifact.
	Profile bool
	// CanRecordPlan reports that record_plan will actually be
	// registered for this run — there is a .agents/ directory to
	// persist plans into, the built-in suite is on, and nothing
	// disabled the tool.
	//
	// It gates the profile's opinion because plan-first without
	// record_plan is a deadlock, not a stricter posture: every
	// mutating call is denied for the life of the session and nothing
	// can clear the flag (/replan only revokes a plan, it can't grant
	// one). An explicit flag or config value is still honored — the
	// operator said it out loud — but the caller warns.
	CanRecordPlan bool
}

// Sources reported by resolvePlanFirst, so the startup line can say
// which input won without re-deriving the chain.
const (
	sourcePlanFlag       = "--plan-first flag"
	sourcePlanConfig     = "permissions.require_plan_artifact config"
	sourcePlanProfile    = "task profile"
	sourcePlanNoRecorder = "task profile (suppressed: record_plan won't register)"
	sourcePlanUnset      = "unset"
)

// resolvePlanFirst decides whether plan-first gating is on.
//
// Precedence: --plan-first (either value) > require_plan_artifact:
// true in config > the task profile > off. The profile can only turn
// the gate on; an operator who wrote `true` in config is never
// overruled by a class default, and one who wants it off passes
// --plan-first=false.
func resolvePlanFirst(in planFirstInputs) (on bool, source string) {
	switch {
	case in.FlagSet:
		return in.Flag, sourcePlanFlag
	case in.Config:
		return true, sourcePlanConfig
	case in.Profile && in.CanRecordPlan:
		return true, sourcePlanProfile
	case in.Profile:
		return false, sourcePlanNoRecorder
	default:
		return false, sourcePlanUnset
	}
}
