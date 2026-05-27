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

//go:build !no_tui

package main

import (
	"testing"

	coretui "github.com/go-steer/core-tui/tui"

	"github.com/go-steer/core-agent/config"
)

func TestUIThemeToCoreTui_Dark(t *testing.T) {
	cfg := &config.Config{UI: config.UIConfig{Theme: config.ThemeDark}}
	if got := uiThemeToCoreTui(cfg); got != coretui.ThemeDark {
		t.Errorf("dark theme: got %q, want %q", got, coretui.ThemeDark)
	}
}

func TestUIThemeToCoreTui_Light(t *testing.T) {
	cfg := &config.Config{UI: config.UIConfig{Theme: config.ThemeLight}}
	if got := uiThemeToCoreTui(cfg); got != coretui.ThemeLight {
		t.Errorf("light theme: got %q, want %q", got, coretui.ThemeLight)
	}
}

func TestUIThemeToCoreTui_Auto(t *testing.T) {
	for _, theme := range []string{"", "auto"} {
		cfg := &config.Config{UI: config.UIConfig{Theme: theme}}
		if got := uiThemeToCoreTui(cfg); got != coretui.ThemeAuto {
			t.Errorf("theme=%q: got %q, want %q (auto)", theme, got, coretui.ThemeAuto)
		}
	}
}

func TestUIThemeToCoreTui_NilCfg(t *testing.T) {
	if got := uiThemeToCoreTui(nil); got != coretui.ThemeAuto {
		t.Errorf("nil cfg: got %q, want %q (auto)", got, coretui.ThemeAuto)
	}
}

func TestUIMouseToCoreTui_NilWhenUnset(t *testing.T) {
	cfg := &config.Config{}
	if got := uiMouseToCoreTui(cfg); got != nil {
		t.Errorf("unset Mouse: got %v, want nil (default on)", *got)
	}
}

func TestUIMouseToCoreTui_PropagatesExplicitFalse(t *testing.T) {
	off := false
	cfg := &config.Config{UI: config.UIConfig{Mouse: &off}}
	got := uiMouseToCoreTui(cfg)
	if got == nil {
		t.Fatal("explicit false: got nil, want *false")
	}
	if *got != false {
		t.Errorf("explicit false: got *%v, want *false", *got)
	}
}

func TestUIMouseToCoreTui_PropagatesExplicitTrue(t *testing.T) {
	on := true
	cfg := &config.Config{UI: config.UIConfig{Mouse: &on}}
	got := uiMouseToCoreTui(cfg)
	if got == nil {
		t.Fatal("explicit true: got nil, want *true")
	}
	if *got != true {
		t.Errorf("explicit true: got *%v, want *true", *got)
	}
}

func TestUIMouseToCoreTui_NilCfg(t *testing.T) {
	if got := uiMouseToCoreTui(nil); got != nil {
		t.Errorf("nil cfg: got non-nil pointer, want nil")
	}
}
