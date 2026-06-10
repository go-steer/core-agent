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

import "testing"

// Pin the safety.small_tier_parent validation accept set + the
// canonical constants. Default ("") accepts; explicit warn/refuse/
// allow accept; anything else is rejected. The CLI flag's accept set
// (cmd/core-agent/main.go) must stay in sync — if a fourth mode is
// added (e.g. "prompt"), both sides need bumping.
func TestValidate_SafetySmallTierParent(t *testing.T) {
	t.Parallel()
	cases := []struct {
		mode    string
		wantErr bool
	}{
		{"", false},
		{SmallTierParentWarn, false},
		{SmallTierParentRefuse, false},
		{SmallTierParentAllow, false},
		{"prompt", true}, // future mode — must error today
		{"WARN", true},   // case-sensitive guard against typos
		{"refuse ", true},
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			t.Parallel()
			c := DefaultConfig()
			c.Safety.SmallTierParent = tc.mode
			err := c.Validate()
			if tc.wantErr && err == nil {
				t.Errorf("Validate() with small_tier_parent=%q: got nil, want error", tc.mode)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("Validate() with small_tier_parent=%q: got %v, want nil", tc.mode, err)
			}
		})
	}
}

// Canonical-constant sanity. These strings are what operators type
// in their config and what the CLI flag accepts; a silent rename
// would break every existing config file in the wild.
func TestSmallTierParentConstants_AreStable(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		SmallTierParentWarn:   "warn",
		SmallTierParentRefuse: "refuse",
		SmallTierParentAllow:  "allow",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("constant drift: got %q, want %q", got, want)
		}
	}
}
