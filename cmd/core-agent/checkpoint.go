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
	"strings"

	"github.com/go-steer/core-agent/v2/pkg/config"
)

// checkpointOpts bundles the task-boundary-checkpoint CLI flags so
// run()'s signature carries one parameter instead of three, and so the
// "operator actually passed --no-checkpoint" bit travels with the value
// it qualifies.
type checkpointOpts struct {
	// mode is --checkpoint; empty == unset.
	mode string
	// noCheckpoint is the deprecated --no-checkpoint, qualified by
	// noCheckpointSet. It is an alias for --checkpoint=off, and
	// presence rather than value is what matters: --no-checkpoint=false
	// is how a script overrides a wrapper that passed the flag.
	noCheckpoint    bool
	noCheckpointSet bool
}

// checkpointInputs is everything resolveCheckpoint needs to decide the
// run's checkpoint posture. Split out from run()'s parameter list so
// the resolution is a pure function with a table test rather than an
// inline switch nobody can exercise — same shape as guardrailInputs.
type checkpointInputs struct {
	// ModeFlag is --checkpoint. Empty == operator left it unset.
	ModeFlag string
	// NoCheckpointFlag is --no-checkpoint, and NoCheckpointFlagSet
	// reports whether the operator actually passed it. The pair
	// matters because an explicit --no-checkpoint=false must be
	// distinguishable from the flag sitting at its default.
	NoCheckpointFlag    bool
	NoCheckpointFlagSet bool
	// ModeConfig is checkpoint.mode. Empty == unset.
	ModeConfig string
}

// checkpointResolution is the decided posture plus the reason it came
// out that way, so the startup summary can tell an operator "the
// default did this" from "my config did this" without making them
// re-derive the precedence chain.
type checkpointResolution struct {
	// Mode is one of config.CheckpointModeModel /
	// CheckpointModeOperator / CheckpointModeOff. Never empty.
	Mode   string
	Source string
}

// Sources reported in checkpointResolution, kept as constants so the
// tests assert on the same strings the startup summary prints.
const (
	sourceCheckpointFlag        = "--checkpoint flag"
	sourceNoCheckpointFlag      = "--no-checkpoint flag"
	sourceCheckpointConfig      = "checkpoint.mode config"
	sourceCheckpointModeDefault = "default"

	// checkpointDeprecationNotice is printed once when a run still
	// reaches for --no-checkpoint. It names what the flag actually does
	// (its old help text promised only that the model would stop
	// self-signalling) and the narrower replacement.
	checkpointDeprecationNotice = "--no-checkpoint is deprecated: it removes /done and the post-turn heuristic as well as the model's mark_task_done tool. Use --checkpoint=off for that, or --checkpoint=operator to keep /done while withholding the model's trigger."

	checkpointContradictionHint = "pass one or the other"
)

// resolveCheckpoint decides which parties may declare a task boundary.
//
// Precedence: --checkpoint > --no-checkpoint > checkpoint.mode >
// "model", mirroring --watchdog / safety.watchdog (#660).
//
// --no-checkpoint participates only when it is TRUE. An explicit
// --no-checkpoint=false is read as "as if unset" and falls through to
// checkpoint.mode, which differs from --plan-first (resolvePlanMode in
// taskprofile.go), where an explicit false beats config. The asymmetry
// is deliberate: --no-checkpoint's only remaining job is to be
// overridable by a wrapper script that appends --no-checkpoint=false to
// an inherited command line, and Go's flag package already collapses a
// repeated flag to its last value. A caller who wants checkpointing
// back in spite of config says --checkpoint=model, which is the
// spelling that survives the deprecation.
//
// The two flags overlap by design — --no-checkpoint predates the mode
// and stays as an alias for "off" so existing manifests keep working.
// Passing both with different intents is a migration left half-done,
// and guessing which one the operator meant is how a posture silently
// changes, so that combination is an error rather than a precedence
// rule (same call as permissions.plan_mode vs require_plan_artifact).
//
// Returns an error for an unrecognized mode from either source.
// config.Validate already rejects a bad config value, but a library
// caller can hand-build a Config, so this re-checks rather than
// silently falling through to the default.
func resolveCheckpoint(in checkpointInputs) (checkpointResolution, error) {
	res := checkpointResolution{}

	norm := func(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
	valid := func(s string) bool {
		switch s {
		case config.CheckpointModeModel, config.CheckpointModeOperator, config.CheckpointModeOff:
			return true
		}
		return false
	}
	modes := strings.Join([]string{config.CheckpointModeModel, config.CheckpointModeOperator, config.CheckpointModeOff}, "|")

	flagMode, cfgMode := norm(in.ModeFlag), norm(in.ModeConfig)
	// Only a --no-checkpoint that actually asks for "off" can
	// contradict --checkpoint; --no-checkpoint=false is an override of
	// an outer wrapper, not a second opinion.
	disabling := in.NoCheckpointFlagSet && in.NoCheckpointFlag

	// Validity before contradiction, deliberately. `--checkpoint=operater
	// --no-checkpoint` has two problems, and the typo is the one the
	// operator has to fix either way — reporting the contradiction first
	// sends them to drop a flag, rerun, and only then learn the mode was
	// misspelled.
	if flagMode != "" && !valid(flagMode) {
		return res, fmt.Errorf("invalid --checkpoint mode %q (want %s)", in.ModeFlag, modes)
	}
	if flagMode != "" && disabling && flagMode != config.CheckpointModeOff {
		return res, fmt.Errorf("--checkpoint=%s contradicts --no-checkpoint (which means --checkpoint=%s); %s",
			flagMode, config.CheckpointModeOff, checkpointContradictionHint)
	}

	switch {
	case flagMode != "":
		res.Mode, res.Source = flagMode, sourceCheckpointFlag
	case disabling:
		res.Mode, res.Source = config.CheckpointModeOff, sourceNoCheckpointFlag
	case cfgMode != "":
		if !valid(cfgMode) {
			return res, fmt.Errorf("invalid checkpoint.mode %q (want %s)", in.ModeConfig, modes)
		}
		res.Mode, res.Source = cfgMode, sourceCheckpointConfig
	default:
		res.Mode, res.Source = config.CheckpointModeModel, sourceCheckpointModeDefault
	}

	return res, nil
}

// checkpointBootLine is the one-line posture summary printed at
// startup, alongside the watchdog and bash-search-gate lines. Printed
// in every mode including the default: "which levers are actually
// armed" is the question those lines exist to answer, and a posture
// that only announces itself when weakened is one an operator has to
// infer.
func checkpointBootLine(res checkpointResolution) string {
	switch res.Mode {
	case config.CheckpointModeOff:
		return fmt.Sprintf("checkpoint: off [%s] (no task-boundary slicing; /done reports no checkpointer. Automatic context compaction is unaffected)", res.Source)
	case config.CheckpointModeOperator:
		return fmt.Sprintf("checkpoint: operator [%s] (/done + the heuristic stay; the model-facing mark_task_done tool is not registered, so the model cannot declare its own task boundary)", res.Source)
	default:
		return fmt.Sprintf("checkpoint: model [%s] (mark_task_done + /done + the heuristic. --checkpoint=operator withholds the model's trigger on a long-lived service)", res.Source)
	}
}
