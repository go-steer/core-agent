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

package pricing

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestLookup_BuiltinOnly verifies the zero-config path: empty Options
// yields a catalog that resolves only against the compiled-in builtin
// table. This is the air-gapped / fresh-install baseline.
func TestLookup_BuiltinOnly(t *testing.T) {
	t.Parallel()
	c, err := NewCatalog(Options{})
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	r, ok := c.Lookup("gemini-3.1-pro-preview")
	if !ok {
		t.Fatal("builtin entry not found")
	}
	if r.InputPerMTok == 0 {
		t.Errorf("builtin returned zero rates: %+v", r)
	}
	// Empty catalog (no override / files) returns false for unknown.
	if _, ok := c.Lookup("totally-made-up-model"); ok {
		t.Error("unknown model should report not-found, not fall through silently")
	}
}

// TestLookup_PrefixFallback exercises the longest-prefix rule that
// makes date-suffixed and custom-tools variants Just Work without
// per-variant entries. The bug this guards against: a future
// refactor that drops the prefix loop and returns "$—" for
// "gemini-3.1-pro-preview-customtools" even though the family rate
// is sitting right there.
func TestLookup_PrefixFallback(t *testing.T) {
	t.Parallel()
	c, _ := NewCatalog(Options{})
	for _, name := range []string{
		"gemini-3.1-pro-preview-customtools",
		"gemini-3.1-pro-preview-05-15",
		"gemini-3-pro-2026-03-01",
	} {
		r, ok := c.Lookup(name)
		if !ok || r.InputPerMTok == 0 {
			t.Errorf("prefix fallback missed %q: ok=%v rates=%+v", name, ok, r)
		}
	}
}

// TestLookup_PrecedenceOrder pins the layered precedence: a
// cfg.Model.Pricing override beats a project file beats user manual
// beats user external beats builtin. Each layer carries a unique
// rate so the resolved value identifies which layer won.
func TestLookup_PrecedenceOrder(t *testing.T) {
	t.Parallel()
	c := &Catalog{
		cfgOverride: map[string]Rates{"target": {InputPerMTok: 1}},
		projectFile: map[string]Rates{"target": {InputPerMTok: 2}},
		userManual:  map[string]Rates{"target": {InputPerMTok: 3}},
		userExt:     map[string]Rates{"target": {InputPerMTok: 4}},
		builtin:     map[string]Rates{"target": {InputPerMTok: 5}},
	}
	r, ok := c.Lookup("target")
	if !ok || r.InputPerMTok != 1 {
		t.Errorf("cfg override should win: got %+v ok=%v", r, ok)
	}
	// Drop the highest layer; next layer wins.
	c.cfgOverride = nil
	if r, _ := c.Lookup("target"); r.InputPerMTok != 2 {
		t.Errorf("project file should win after cfg dropped: %+v", r)
	}
	c.projectFile = nil
	if r, _ := c.Lookup("target"); r.InputPerMTok != 3 {
		t.Errorf("user manual should win: %+v", r)
	}
	c.userManual = nil
	if r, _ := c.Lookup("target"); r.InputPerMTok != 4 {
		t.Errorf("user external should win: %+v", r)
	}
	c.userExt = nil
	if r, _ := c.Lookup("target"); r.InputPerMTok != 5 {
		t.Errorf("builtin should be the final fallback: %+v", r)
	}
}

// TestLookup_CaseInsensitive guards against operators who write
// "Gemini-3.1-Pro-Preview" in config.json (provider docs vary on
// case) suddenly seeing $— because of a case mismatch.
func TestLookup_CaseInsensitive(t *testing.T) {
	t.Parallel()
	c, _ := NewCatalog(Options{
		CfgOverride: map[string]ModelRates{
			"My-Custom-Model": {InputPerMTok: 7, OutputPerMTok: 9},
		},
	})
	for _, variant := range []string{"my-custom-model", "MY-CUSTOM-MODEL", "My-Custom-Model"} {
		r, ok := c.Lookup(variant)
		if !ok || r.InputPerMTok != 7 {
			t.Errorf("case variant %q not resolved: ok=%v %+v", variant, ok, r)
		}
	}
}

// TestNewCatalog_LoadsProjectAndUserFiles round-trips a project +
// user file through NewCatalog and verifies both layers land with
// the right precedence.
func TestNewCatalog_LoadsProjectAndUserFiles(t *testing.T) {
	t.Parallel()
	agentsDir := t.TempDir()
	userHome := t.TempDir()

	writeJSON(t, filepath.Join(agentsDir, ProjectFileName), ProjectFile{
		Version: 1,
		Models: map[string]ModelRates{
			"shared-model": {InputPerMTok: 1, OutputPerMTok: 2},
			"project-only": {InputPerMTok: 3, OutputPerMTok: 4},
		},
	})
	writeJSON(t, filepath.Join(userHome, UserFileName), UserFile{
		Version: 1,
		Manual: &ManualSection{Models: map[string]ModelRates{
			// Same key in project + user manual; project should win.
			"shared-model": {InputPerMTok: 100, OutputPerMTok: 200},
			"user-only":    {InputPerMTok: 5, OutputPerMTok: 6},
		}},
	})

	c, err := NewCatalog(Options{AgentsDir: agentsDir, UserHome: userHome})
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	if r, _ := c.Lookup("shared-model"); r.InputPerMTok != 1 {
		t.Errorf("project file should outrank user manual: %+v", r)
	}
	if r, _ := c.Lookup("project-only"); r.InputPerMTok != 3 {
		t.Errorf("project-only entry missing: %+v", r)
	}
	if r, _ := c.Lookup("user-only"); r.InputPerMTok != 5 {
		t.Errorf("user-only entry missing: %+v", r)
	}
}

// TestNewCatalog_MissingFilesAreOK is the common-case path: no
// project or user pricing file. NewCatalog returns a builtin-only
// catalog without error.
func TestNewCatalog_MissingFilesAreOK(t *testing.T) {
	t.Parallel()
	c, err := NewCatalog(Options{
		AgentsDir: t.TempDir(), // empty dir, no pricing.json
		UserHome:  t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewCatalog with missing files should not error: %v", err)
	}
	if r, ok := c.Lookup("gemini-3.1-pro-preview"); !ok || r.InputPerMTok == 0 {
		t.Errorf("builtin layer missing after empty file load")
	}
}

// TestSaveAndReloadUserFile verifies the atomic write round-trips,
// preserving manual + external sections. Critical for PR B's
// refresh-without-clobbering-manual semantics.
func TestSaveAndReloadUserFile(t *testing.T) {
	t.Parallel()
	userHome := t.TempDir()
	uf := &UserFile{
		Version: 1,
		Manual: &ManualSection{Models: map[string]ModelRates{
			"my-model": {InputPerMTok: 1, OutputPerMTok: 2},
		}},
	}
	if err := SaveUserFile(userHome, uf); err != nil {
		t.Fatalf("SaveUserFile: %v", err)
	}
	loaded, err := LoadUserFile(userHome)
	if err != nil {
		t.Fatalf("LoadUserFile: %v", err)
	}
	if loaded.Manual == nil || loaded.Manual.Models["my-model"].InputPerMTok != 1 {
		t.Errorf("manual section round-trip failed: %+v", loaded)
	}
}

// TestModelRates_CachedInputPerMTokRoundTrip guards the on-disk field
// name so an operator's pricing.json cache-rate override survives the
// load path and reaches Rates.CachedInputPerMTok.
func TestModelRates_CachedInputPerMTokRoundTrip(t *testing.T) {
	t.Parallel()
	userHome := t.TempDir()
	if err := SaveUserFile(userHome, &UserFile{
		Version: 1,
		Manual: &ManualSection{Models: map[string]ModelRates{
			"my-model": {InputPerMTok: 2, CachedInputPerMTok: 0.5, OutputPerMTok: 4},
		}},
	}); err != nil {
		t.Fatalf("SaveUserFile: %v", err)
	}
	c, err := NewCatalog(Options{UserHome: userHome})
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	r, ok := c.Lookup("my-model")
	if !ok {
		t.Fatal("lookup missing")
	}
	if r.CachedInputPerMTok != 0.5 {
		t.Errorf("CachedInputPerMTok = %v, want 0.5", r.CachedInputPerMTok)
	}
}

// TestRates_CostUSD spot-checks the cost arithmetic. The exit-time
// summary + per-message footer both depend on this being correct
// for non-trivial token counts.
func TestRates_CostUSD(t *testing.T) {
	t.Parallel()
	r := Rates{InputPerMTok: 1.25, OutputPerMTok: 5.0}
	// 1M input + 200K output = $1.25 + $1.00 = $2.25
	got := r.CostUSD(1_000_000, 200_000)
	want := 2.25
	if got != want {
		t.Errorf("CostUSD(1M, 200K) = %v, want %v", got, want)
	}
	if !(Rates{}).IsZero() {
		t.Error("zero Rates should report IsZero")
	}
	if r.IsZero() {
		t.Error("non-zero Rates should not report IsZero")
	}
	// Cache rate alone doesn't make a row "priced" — a row that only
	// carries a cache-read rate is still unpriced for base billing.
	if !(Rates{CachedInputPerMTok: 0.5}).IsZero() {
		t.Error("cache-only Rates should still report IsZero")
	}
}

// TestRates_CostUSDWithCache pins the cache-hit rate application and
// the "no discount known" fallback so operators never see cached input
// silently drop to zero cost.
func TestRates_CostUSDWithCache(t *testing.T) {
	t.Parallel()
	r := Rates{InputPerMTok: 1.25, CachedInputPerMTok: 0.3125, OutputPerMTok: 5.0}
	// 800k uncached at $1.25/M + 200k cached at $0.3125/M + 100k out at $5/M.
	got := r.CostUSDWithCache(800_000, 200_000, 100_000)
	want := 0.8*1.25 + 0.2*0.3125 + 0.1*5.0
	if got != want {
		t.Errorf("CostUSDWithCache = %v, want %v", got, want)
	}
	// Fallback: cached rate absent → cache hits bill at input rate.
	r2 := Rates{InputPerMTok: 1.0, OutputPerMTok: 2.0}
	got2 := r2.CostUSDWithCache(500_000, 500_000, 100_000)
	want2 := 0.5*1.0 + 0.5*1.0 + 0.1*2.0
	if got2 != want2 {
		t.Errorf("CostUSDWithCache (fallback) = %v, want %v", got2, want2)
	}
}

// TestBuiltin_GeminiHasCachedRate guards against dropping the cached
// rate when the builtin table gets regenerated. Every entry must carry
// a positive CachedInputPerMTok — issue #222's operator-facing cache
// savings depend on it.
func TestBuiltin_GeminiHasCachedRate(t *testing.T) {
	t.Parallel()
	for name, r := range builtin {
		if r.CachedInputPerMTok <= 0 {
			t.Errorf("builtin %q has zero CachedInputPerMTok — cache savings won't render", name)
		}
		if r.CachedInputPerMTok >= r.InputPerMTok {
			t.Errorf("builtin %q cached rate (%v) >= input rate (%v) — should be a discount",
				name, r.CachedInputPerMTok, r.InputPerMTok)
		}
	}
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil { //nolint:gosec // test-only
		t.Fatalf("write %s: %v", path, err)
	}
}
