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
	"strings"
	"testing"
)

// The wait_and_verify ceilings are operator bounds the model cannot
// raise (#648), so a nonsensical value has to fail at load rather than
// silently become "unbounded" at runtime. Zero stays legal — it means
// "use the built-in default" — which is why the check is < 0, not <= 0.
func TestValidate_WaitAndVerifyBounds(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		cfg     WaitAndVerifyConfig
		wantErr bool
	}{
		{"zero means default", WaitAndVerifyConfig{}, false},
		{"explicit ceilings", WaitAndVerifyConfig{MaxTimeoutSeconds: 120, MaxAttempts: 20}, false},
		{"negative timeout", WaitAndVerifyConfig{MaxTimeoutSeconds: -1}, true},
		{"negative attempts", WaitAndVerifyConfig{MaxAttempts: -1}, true},
		{"allow list", WaitAndVerifyConfig{PollAllow: []string{"gke_get_pod"}}, false},
		{"empty allow entry", WaitAndVerifyConfig{PollAllow: []string{"gke_get_pod", ""}}, true},
		{"blank allow entry", WaitAndVerifyConfig{PollAllow: []string{"  "}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := DefaultConfig()
			c.Tools.WaitAndVerify = tc.cfg
			err := c.Validate()
			if tc.wantErr && err == nil {
				t.Errorf("Validate() with %+v: got nil, want error", tc.cfg)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("Validate() with %+v: got %v, want nil", tc.cfg, err)
			}
		})
	}
}

// A blank poll_allow entry is a typo, not an allow-everything wildcard:
// the error has to name the index so the operator can find it.
func TestValidate_WaitAndVerifyEmptyAllowEntryNamesTheIndex(t *testing.T) {
	t.Parallel()
	c := DefaultConfig()
	c.Tools.WaitAndVerify.PollAllow = []string{"gke_get_pod", ""}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "poll_allow[1]") {
		t.Fatalf("Validate() = %v, want an error naming poll_allow[1]", err)
	}
}

// Pin the wire names. These land in a recipe's config.json, so a rename
// silently drops an operator's ceiling back to the built-in default.
func TestWaitAndVerifyConfig_JSONRoundTrip(t *testing.T) {
	t.Parallel()
	const raw = `{
	  "tools": {
	    "wait_and_verify": {
	      "poll_allow": ["gke_get_pod"],
	      "max_timeout_seconds": 120,
	      "max_attempts": 20
	    }
	  }
	}`
	var c Config
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	wv := c.Tools.WaitAndVerify
	if len(wv.PollAllow) != 1 || wv.PollAllow[0] != "gke_get_pod" {
		t.Errorf("poll_allow = %v, want [gke_get_pod]", wv.PollAllow)
	}
	if wv.MaxTimeoutSeconds != 120 || wv.MaxAttempts != 20 {
		t.Errorf("ceilings = %d/%d, want 120/20", wv.MaxTimeoutSeconds, wv.MaxAttempts)
	}

	// Round-trip: what an operator writes must survive a re-marshal, so
	// a config rewritten by the daemon keeps its ceilings.
	out, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var back Config
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatalf("Unmarshal (round trip): %v", err)
	}
	if got := back.Tools.WaitAndVerify; got.MaxTimeoutSeconds != 120 || got.MaxAttempts != 20 || len(got.PollAllow) != 1 {
		t.Errorf("after round trip = %+v, want the original", got)
	}
}
