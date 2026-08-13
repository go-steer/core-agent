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

func cfgWithCallPeer(cp CallPeerConfig) *Config {
	c := DefaultConfig()
	c.Tools.CallPeer = cp
	return c
}

func TestValidateCallPeer_Accepts(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cp   CallPeerConfig
	}{
		{"zero value", CallPeerConfig{}},
		{"enabled with defaults", CallPeerConfig{Enabled: true}},
		{"renamed", CallPeerConfig{Enabled: true, Name: "ask_operator"}},
		{"underscored and digits", CallPeerConfig{Enabled: true, Name: "call_peer_v2"}},
		{"tuned", CallPeerConfig{Enabled: true, TimeoutSeconds: 900, MaxResponseBytes: 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := cfgWithCallPeer(tc.cp).Validate(); err != nil {
				t.Errorf("Validate() = %v, want nil", err)
			}
		})
	}
}

func TestValidateCallPeer_Rejects(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cp   CallPeerConfig
		want string
	}{
		{"hyphenated name", CallPeerConfig{Name: "call-peer"}, "call-peer"},
		{"dotted name", CallPeerConfig{Name: "fleet.call"}, "fleet.call"},
		{"leading digit", CallPeerConfig{Name: "2call"}, "2call"},
		{"name too long", CallPeerConfig{Name: strings.Repeat("a", 65)}, "must be"},
		{"negative timeout", CallPeerConfig{TimeoutSeconds: -1}, "must not be negative"},
		{"timeout over ceiling", CallPeerConfig{TimeoutSeconds: 901}, "ceiling"},
		{"negative cap", CallPeerConfig{MaxResponseBytes: -1}, "must not be negative"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := cfgWithCallPeer(tc.cp).Validate()
			if err == nil {
				t.Fatalf("Validate() = nil, want an error mentioning %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Validate() = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// A bad knob under enabled=false still fails now rather than the day
// someone flips the switch.
func TestValidateCallPeer_ValidatesEvenWhenDisabled(t *testing.T) {
	t.Parallel()
	if err := cfgWithCallPeer(CallPeerConfig{Enabled: false, TimeoutSeconds: 99999}).Validate(); err == nil {
		t.Error("Validate() = nil; a disabled block with an out-of-range timeout should still fail")
	}
}

func TestCallPeerConfig_JSONRoundTrip(t *testing.T) {
	t.Parallel()
	raw := `{"version":1,"model":{"name":"m"},"tools":{"call_peer":{"enabled":true,"name":"ask_operator","token_env":"PEER_TOKEN","timeout_seconds":60,"max_response_bytes":2048}}}`
	var c Config
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	cp := c.Tools.CallPeer
	if !cp.Enabled || cp.Name != "ask_operator" || cp.TokenEnv != "PEER_TOKEN" ||
		cp.TimeoutSeconds != 60 || cp.MaxResponseBytes != 2048 {
		t.Errorf("call_peer = %+v, want every field parsed from the on-disk shape", cp)
	}
	// Re-marshaling keeps every field an operator set (Save round-trips
	// through this path).
	out, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(out), `"call_peer":{"enabled":true,"name":"ask_operator"`) {
		t.Errorf("re-marshaled config lost call_peer fields: %s", out)
	}
}
