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

func TestUIConfig_MouseEnabled_DefaultsTrue(t *testing.T) {
	t.Parallel()
	var u UIConfig
	if !u.MouseEnabled() {
		t.Errorf("MouseEnabled() = false, want true when Mouse is nil (default)")
	}
}

func TestUIConfig_MouseEnabled_ExplicitOverride(t *testing.T) {
	t.Parallel()
	f := false
	u := UIConfig{Mouse: &f}
	if u.MouseEnabled() {
		t.Errorf("MouseEnabled() = true, want false when Mouse = &false")
	}
	tr := true
	u = UIConfig{Mouse: &tr}
	if !u.MouseEnabled() {
		t.Errorf("MouseEnabled() = false, want true when Mouse = &true")
	}
}

func TestDefaultConfig_UI(t *testing.T) {
	t.Parallel()
	c := DefaultConfig()
	if c.UI.Theme != ThemeAuto {
		t.Errorf("DefaultConfig().UI.Theme = %q, want %q", c.UI.Theme, ThemeAuto)
	}
	if !c.UI.MouseEnabled() {
		t.Errorf("DefaultConfig().UI mouse should default to enabled")
	}
}

func TestValidate_UITheme(t *testing.T) {
	t.Parallel()
	cases := []struct {
		theme   string
		wantErr bool
	}{
		{"", false},
		{ThemeAuto, false},
		{ThemeDark, false},
		{ThemeLight, false},
		{"midnight", true},
		{"DARK", true}, // case-sensitive — guard against typos
	}
	for _, tc := range cases {
		t.Run(tc.theme, func(t *testing.T) {
			t.Parallel()
			c := DefaultConfig()
			c.UI.Theme = tc.theme
			err := c.Validate()
			if tc.wantErr && err == nil {
				t.Errorf("Validate() with theme=%q: got nil, want error", tc.theme)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("Validate() with theme=%q: got %v, want nil", tc.theme, err)
			}
		})
	}
}

func TestUIConfig_JSONRoundTrip(t *testing.T) {
	t.Parallel()
	in := `{
        "version": 1,
        "model": {"name": "test"},
        "ui": {"theme": "dark", "mouse": false}
    }`
	var c Config
	if err := json.Unmarshal([]byte(in), &c); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if c.UI.Theme != "dark" {
		t.Errorf("Theme = %q, want dark", c.UI.Theme)
	}
	if c.UI.MouseEnabled() {
		t.Errorf("MouseEnabled() = true, want false (config disabled it)")
	}
	out, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(out), `"theme":"dark"`) {
		t.Errorf("round-tripped JSON missing theme: %s", out)
	}
	if !strings.Contains(string(out), `"mouse":false`) {
		t.Errorf("round-tripped JSON missing mouse: %s", out)
	}
}
