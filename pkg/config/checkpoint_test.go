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

package config

import (
	"encoding/json"
	"testing"
)

// Pin the checkpoint.mode validation accept set (#905). Pre-fix this
// test fails to compile — there was no config field at all, so the only
// lever was --no-checkpoint on the command line, and a recipe that
// wanted the operator-only posture could not express it anywhere its
// content image travels.
func TestValidate_CheckpointMode(t *testing.T) {
	t.Parallel()
	cases := []struct {
		mode    string
		wantErr bool
	}{
		{"", false},
		{CheckpointModeModel, false},
		{CheckpointModeOperator, false},
		{CheckpointModeOff, false},
		{"operater", true}, // the typo an operator actually makes
		{"none", true},     // plausible synonym for off — must not silently pass
		{"true", true},     // migrating from a boolean flag
		{"OPERATOR", true}, // case-sensitive, matching safety.watchdog
		{"off ", true},
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			t.Parallel()
			c := DefaultConfig()
			c.Checkpoint.Mode = tc.mode
			err := c.Validate()
			if tc.wantErr && err == nil {
				t.Errorf("Validate() with checkpoint.mode=%q: got nil, want error", tc.mode)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("Validate() with checkpoint.mode=%q: got %v, want nil", tc.mode, err)
			}
		})
	}
}

// The field must actually round-trip from a config file — a struct tag
// typo would make `{"checkpoint":{"mode":"operator"}}` parse to the
// zero value, which resolves to "model" and hands the recipe back the
// exact tool it was trying to withhold.
func TestCheckpointMode_UnmarshalsFromJSON(t *testing.T) {
	t.Parallel()
	var c Config
	if err := json.Unmarshal([]byte(`{"checkpoint":{"mode":"operator"}}`), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if c.Checkpoint.Mode != CheckpointModeOperator {
		t.Errorf("checkpoint.mode: got %q, want %q", c.Checkpoint.Mode, CheckpointModeOperator)
	}
}

// Checkpointing and compaction are separate mechanisms, and conflating
// them is what made --no-checkpoint feel like it disabled context
// reduction. Nothing in the config should couple them: turning
// checkpointing off must leave compaction settings untouched and valid.
func TestCheckpointModeOff_LeavesCompactionAlone(t *testing.T) {
	t.Parallel()
	threshold := 0.5
	c := DefaultConfig()
	c.Checkpoint.Mode = CheckpointModeOff
	c.Compaction.Threshold = &threshold
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate(): %v", err)
	}
	if c.Compaction.Threshold == nil || *c.Compaction.Threshold != threshold {
		t.Errorf("compaction.threshold = %v, want %v preserved alongside checkpoint.mode=off", c.Compaction.Threshold, threshold)
	}
}
