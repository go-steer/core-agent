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

// Pin the safety.watchdog validation accept set (#660). Pre-fix this
// test fails to compile — there was no config field at all, so a
// recipe declaring its own runaway backstop was silently unenforceable
// and a typo'd value was silently ignored.
func TestValidate_SafetyWatchdog(t *testing.T) {
	t.Parallel()
	cases := []struct {
		mode    string
		wantErr bool
	}{
		{"", false},
		{WatchdogOff, false},
		{WatchdogWarn, false},
		{WatchdogEnforce, false},
		{"enfroce", true}, // the typo an operator actually makes
		{"prompt", true},  // designed-but-deferred mode — must error today
		{"ENFORCE", true}, // case-sensitive guard, matching small_tier_parent
		{"enforce ", true},
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			t.Parallel()
			c := DefaultConfig()
			c.Safety.Watchdog = tc.mode
			err := c.Validate()
			if tc.wantErr && err == nil {
				t.Errorf("Validate() with watchdog=%q: got nil, want error", tc.mode)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("Validate() with watchdog=%q: got %v, want nil", tc.mode, err)
			}
		})
	}
}

// The field must actually round-trip from a config file — a struct tag
// typo would make `{"safety":{"watchdog":"enforce"}}` parse to the
// zero value and hand the recipe a silent warn-mode agent.
func TestSafetyWatchdog_UnmarshalsFromJSON(t *testing.T) {
	t.Parallel()
	var c Config
	if err := json.Unmarshal([]byte(`{"safety":{"watchdog":"enforce"}}`), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if c.Safety.Watchdog != WatchdogEnforce {
		t.Errorf("safety.watchdog: got %q, want %q", c.Safety.Watchdog, WatchdogEnforce)
	}
}

// Canonical-constant sanity. These strings are what operators type
// in their config and what the CLI flag accepts; a silent rename
// would break every existing config file in the wild.
func TestSmallTierParentConstants_AreStable(t *testing.T) {
	t.Parallel()
	// A slice, not a map: SmallTierParentWarn and WatchdogWarn are both
	// "warn", and duplicate constant keys don't compile.
	cases := []struct{ got, want string }{
		{SmallTierParentWarn, "warn"},
		{SmallTierParentRefuse, "refuse"},
		{SmallTierParentAllow, "allow"},
		{WatchdogOff, "off"},
		{WatchdogWarn, "warn"},
		{WatchdogEnforce, "enforce"},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("constant drift: got %q, want %q", tc.got, tc.want)
		}
	}
}
