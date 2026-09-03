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
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/config"
)

func TestResolveCheckpoint(t *testing.T) {
	tests := []struct {
		name       string
		in         checkpointInputs
		wantMode   string
		wantSource string
	}{
		{
			name:       "unset everywhere defaults to model",
			in:         checkpointInputs{},
			wantMode:   config.CheckpointModeModel,
			wantSource: sourceCheckpointModeDefault,
		},
		{
			name:       "config alone decides",
			in:         checkpointInputs{ModeConfig: config.CheckpointModeOperator},
			wantMode:   config.CheckpointModeOperator,
			wantSource: sourceCheckpointConfig,
		},
		{
			name:       "flag beats config",
			in:         checkpointInputs{ModeFlag: config.CheckpointModeModel, ModeConfig: config.CheckpointModeOff},
			wantMode:   config.CheckpointModeModel,
			wantSource: sourceCheckpointFlag,
		},
		{
			name:       "deprecated no-checkpoint maps to off",
			in:         checkpointInputs{NoCheckpointFlag: true, NoCheckpointFlagSet: true},
			wantMode:   config.CheckpointModeOff,
			wantSource: sourceNoCheckpointFlag,
		},
		{
			// The whole point of the mode: a recipe ships
			// checkpoint.mode=operator and a stale deploy manifest
			// still passes --no-checkpoint. The flag is the more
			// specific statement, so it wins — but it is louder than
			// the recipe author intended, which is what the
			// deprecation notice at the call site is for.
			name:       "no-checkpoint beats config",
			in:         checkpointInputs{NoCheckpointFlag: true, NoCheckpointFlagSet: true, ModeConfig: config.CheckpointModeOperator},
			wantMode:   config.CheckpointModeOff,
			wantSource: sourceNoCheckpointFlag,
		},
		{
			// --no-checkpoint=false is how a script overrides a
			// wrapper that passed the flag; it must not be read as a
			// vote for any particular mode.
			name:       "explicit no-checkpoint=false falls through to config",
			in:         checkpointInputs{NoCheckpointFlag: false, NoCheckpointFlagSet: true, ModeConfig: config.CheckpointModeOperator},
			wantMode:   config.CheckpointModeOperator,
			wantSource: sourceCheckpointConfig,
		},
		{
			name:       "redundant off plus no-checkpoint is not a contradiction",
			in:         checkpointInputs{ModeFlag: config.CheckpointModeOff, NoCheckpointFlag: true, NoCheckpointFlagSet: true},
			wantMode:   config.CheckpointModeOff,
			wantSource: sourceCheckpointFlag,
		},
		{
			name:       "case and whitespace are normalized",
			in:         checkpointInputs{ModeFlag: "  OPERATOR "},
			wantMode:   config.CheckpointModeOperator,
			wantSource: sourceCheckpointFlag,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveCheckpoint(tt.in)
			if err != nil {
				t.Fatalf("resolveCheckpoint(%+v) = error %v, want none", tt.in, err)
			}
			if got.Mode != tt.wantMode {
				t.Errorf("Mode = %q, want %q", got.Mode, tt.wantMode)
			}
			if got.Source != tt.wantSource {
				t.Errorf("Source = %q, want %q", got.Source, tt.wantSource)
			}
		})
	}
}

func TestResolveCheckpoint_Errors(t *testing.T) {
	tests := []struct {
		name    string
		in      checkpointInputs
		wantSub string
	}{
		{
			name:    "unknown flag mode",
			in:      checkpointInputs{ModeFlag: "sometimes"},
			wantSub: `invalid --checkpoint mode "sometimes"`,
		},
		{
			name:    "unknown config mode",
			in:      checkpointInputs{ModeConfig: "sometimes"},
			wantSub: `invalid checkpoint.mode "sometimes"`,
		},
		{
			// Guessing which one the operator meant is how a posture
			// silently changes; make them say it once.
			name:    "model plus no-checkpoint contradicts",
			in:      checkpointInputs{ModeFlag: config.CheckpointModeModel, NoCheckpointFlag: true, NoCheckpointFlagSet: true},
			wantSub: "contradicts --no-checkpoint",
		},
		{
			name:    "operator plus no-checkpoint contradicts",
			in:      checkpointInputs{ModeFlag: config.CheckpointModeOperator, NoCheckpointFlag: true, NoCheckpointFlagSet: true},
			wantSub: "contradicts --no-checkpoint",
		},
		{
			// A misspelled mode alongside --no-checkpoint has two
			// problems, and the typo is the one that has to be fixed
			// either way. Reporting the contradiction first sends the
			// operator to drop a flag, rerun, and only then learn the
			// mode was never valid.
			name:    "typo wins over contradiction",
			in:      checkpointInputs{ModeFlag: "operater", NoCheckpointFlag: true, NoCheckpointFlagSet: true},
			wantSub: `invalid --checkpoint mode "operater"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := resolveCheckpoint(tt.in)
			if err == nil {
				t.Fatalf("resolveCheckpoint(%+v) = nil error, want one", tt.in)
			}
			if !strings.Contains(err.Error(), tt.wantSub) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tt.wantSub)
			}
		})
	}
}

// TestCheckpointBootLine covers the operator-facing half: every mode
// gets a line (a posture that only announces itself when weakened is
// one an operator has to infer), the source is named, and the "off"
// line says out loud that compaction is a different mechanism — the
// misconception that turning checkpointing off also stops context
// reduction is what made --no-checkpoint feel unusable.
func TestCheckpointBootLine(t *testing.T) {
	for _, mode := range []string{config.CheckpointModeModel, config.CheckpointModeOperator, config.CheckpointModeOff} {
		line := checkpointBootLine(checkpointResolution{Mode: mode, Source: sourceCheckpointConfig})
		if !strings.HasPrefix(line, "checkpoint: "+mode+" ") {
			t.Errorf("mode %q: line = %q, want it to lead with the mode", mode, line)
		}
		if !strings.Contains(line, sourceCheckpointConfig) {
			t.Errorf("mode %q: line = %q, want it to name the source", mode, line)
		}
	}

	off := checkpointBootLine(checkpointResolution{Mode: config.CheckpointModeOff, Source: sourceNoCheckpointFlag})
	if !strings.Contains(off, "compaction") {
		t.Errorf("off line = %q, want it to say compaction is unaffected", off)
	}

	operator := checkpointBootLine(checkpointResolution{Mode: config.CheckpointModeOperator, Source: sourceCheckpointConfig})
	if !strings.Contains(operator, "/done") || !strings.Contains(operator, "mark_task_done") {
		t.Errorf("operator line = %q, want it to name both what survives and what is withheld", operator)
	}
}

// TestCheckpointDeprecationNoticeNamesWhatItRemoves guards the reason
// this deprecation exists. --no-checkpoint's original help text
// promised only that the model would stop self-signalling completion;
// it also removes /done and the post-turn heuristic, and an operator
// who reads the notice should not have to discover that the way we
// did.
func TestCheckpointDeprecationNoticeNamesWhatItRemoves(t *testing.T) {
	for _, want := range []string{"/done", "heuristic", "mark_task_done", "--checkpoint=operator"} {
		if !strings.Contains(checkpointDeprecationNotice, want) {
			t.Errorf("deprecation notice does not mention %q: %q", want, checkpointDeprecationNotice)
		}
	}
}
