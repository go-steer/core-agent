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
	"os"
	"path/filepath"
	"testing"
)

// TestAutoContinueEnabledTristate is the #559 gate on the config layer:
// enabled is a *bool so the wiring can tell "unset" (apply the
// precondition-gated default) from an explicit false (hard opt-out). A
// plain bool could not express that, so this fails against pre-#559 code.
func TestAutoContinueEnabledTristate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		body    string
		wantNil bool
		wantVal bool // only when !wantNil
	}{
		{name: "absent", body: `{}`, wantNil: true},
		{name: "explicit true", body: `{"enabled":true}`, wantNil: false, wantVal: true},
		{name: "explicit false", body: `{"enabled":false}`, wantNil: false, wantVal: false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var ac AutoContinueConfig
			if err := json.Unmarshal([]byte(tc.body), &ac); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if tc.wantNil {
				if ac.Enabled != nil {
					t.Fatalf("want Enabled nil (unset), got %v", *ac.Enabled)
				}
				return
			}
			if ac.Enabled == nil {
				t.Fatal("want Enabled non-nil, got nil")
			}
			if *ac.Enabled != tc.wantVal {
				t.Errorf("want %v, got %v", tc.wantVal, *ac.Enabled)
			}
		})
	}
}

// TestAutoContinueExplicitFalseRoundTrips verifies an explicit opt-out
// survives Load → Save → reload. If enabled round-tripped as a plain
// bool with omitempty, false would be dropped on save and reload as
// nil/unset — silently flipping the operator's opt-out to default-on.
func TestAutoContinueExplicitFalseRoundTrips(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, ConfigFileName)
	body := `{"version":1,"agent":{"auto_continue":{"enabled":false}}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Agent.AutoContinue == nil || cfg.Agent.AutoContinue.Enabled == nil {
		t.Fatalf("explicit false lost on load: %+v", cfg.Agent.AutoContinue)
	}
	if *cfg.Agent.AutoContinue.Enabled {
		t.Fatal("want enabled=false after load")
	}
	if err := Save(path, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}
	reloaded, err := Load(dir)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.Agent.AutoContinue == nil || reloaded.Agent.AutoContinue.Enabled == nil {
		t.Fatalf("explicit false lost on round-trip: %+v", reloaded.Agent.AutoContinue)
	}
	if *reloaded.Agent.AutoContinue.Enabled {
		t.Error("explicit opt-out flipped to true after round-trip")
	}
}
