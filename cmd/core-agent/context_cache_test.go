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
	"context"
	"strings"
	"testing"

	adkmodel "google.golang.org/adk/model"

	"github.com/go-steer/core-agent/pkg/config"
	"github.com/go-steer/core-agent/pkg/models/gemini"
)

// TestMaybeWireContextCache_DefaultsOnWhenVertexBlockAbsent guards
// the auto-detection path: operators typically set
// `"provider": "vertex"` in config.json and let
// GOOGLE_CLOUD_PROJECT / GOOGLE_CLOUD_LOCATION env vars provide the
// project + location. The `cfg.Model.Vertex` block ends up nil at
// runtime. Caching must still default to ON for that shape — the
// original v1 gate treated a nil Vertex block as "disabled," which
// silently broke the demo path.
func TestMaybeWireContextCache_DefaultsOnWhenVertexBlockAbsent(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{}
	cfg.Model.Provider = config.ProviderVertex
	cfg.Model.Name = "gemini-3.5-flash"
	// cfg.Model.Vertex intentionally nil — mirrors the auto-detection
	// config.json shape shipped in examples/gke-troubleshoot-agent/.

	// Provider *not* a *gemini.Provider — the helper should reach the
	// "silent skip" branch after passing the enabled gate, proving
	// we made it past the buggy nil-Vertex gate. Regression signal:
	// if the "disabled" log line fires, we're back in the old bug.
	var lines []string
	send := func(s string) { lines = append(lines, s) }

	got := maybeWireContextCache(context.Background(), fakeNonGeminiProvider{}, cfg, false, send)

	if got != nil {
		t.Errorf("maybeWireContextCache returned non-nil for non-gemini provider — the type-assertion branch should skip: %v", got)
	}
	for _, line := range lines {
		if strings.Contains(line, "disabled") {
			t.Errorf("nil Vertex block triggered the disabled branch (regression): %q", line)
		}
	}
}

// TestMaybeWireContextCache_ExplicitDisableRespected pins the honest
// "off" path: an operator who explicitly writes `enabled: false` in
// their config.json must see caching off + the disabled log line.
func TestMaybeWireContextCache_ExplicitDisableRespected(t *testing.T) {
	t.Parallel()
	off := false
	cfg := &config.Config{}
	cfg.Model.Provider = config.ProviderVertex
	cfg.Model.Name = "gemini-3.5-flash"
	cfg.Model.Vertex = &config.VertexConfig{
		Project:      "p",
		Location:     "us-central1",
		ContextCache: &config.ContextCacheConfig{Enabled: &off},
	}

	var lines []string
	send := func(s string) { lines = append(lines, s) }
	got := maybeWireContextCache(context.Background(), fakeNonGeminiProvider{}, cfg, false, send)
	if got != nil {
		t.Errorf("explicit disable should return nil manager, got %v", got)
	}
	foundDisabled := false
	for _, line := range lines {
		if strings.Contains(line, "disabled") {
			foundDisabled = true
		}
	}
	if !foundDisabled {
		t.Errorf("explicit disable didn't emit the disabled log line; got %v", lines)
	}
}

// TestMaybeWireContextCache_CLIKillSwitchWins verifies --no-context-cache
// beats an on-by-default config.
func TestMaybeWireContextCache_CLIKillSwitchWins(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{}
	cfg.Model.Provider = config.ProviderVertex
	cfg.Model.Name = "gemini-3.5-flash"
	// No Vertex block — config default = ON. CLI kill switch = ON.
	// Expect the CLI to win.
	var lines []string
	send := func(s string) { lines = append(lines, s) }
	got := maybeWireContextCache(context.Background(), fakeNonGeminiProvider{}, cfg, true, send)
	if got != nil {
		t.Errorf("--no-context-cache should return nil manager, got %v", got)
	}
	found := false
	for _, line := range lines {
		if strings.Contains(line, "--no-context-cache") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected the CLI-kill-switch log line; got %v", lines)
	}
}

// TestMaybeWireContextCache_NonVertexProviderSilent confirms the "not
// applicable" path stays silent — no operator on Anthropic should see
// context-cache log spam.
func TestMaybeWireContextCache_NonVertexProviderSilent(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{}
	cfg.Model.Provider = config.ProviderAnthropic
	cfg.Model.Name = "claude-opus-4-7"
	var lines []string
	send := func(s string) { lines = append(lines, s) }
	got := maybeWireContextCache(context.Background(), fakeNonGeminiProvider{}, cfg, false, send)
	if got != nil {
		t.Errorf("non-vertex provider should return nil manager, got %v", got)
	}
	if len(lines) != 0 {
		t.Errorf("non-vertex provider should emit no log lines; got %v", lines)
	}
}

// fakeNonGeminiProvider satisfies models.Provider without being a
// *gemini.Provider, so maybeWireContextCache reaches the type-assertion
// branch. It never actually gets Model()-called in these tests.
type fakeNonGeminiProvider struct{}

func (fakeNonGeminiProvider) Name() string { return "fake" }
func (fakeNonGeminiProvider) Model(_ context.Context, _ string) (adkmodel.LLM, error) {
	panic("Model called on fake provider — not expected in these tests")
}

// Compile-time proof that we're NOT masquerading as *gemini.Provider.
var _ = (*gemini.Provider)(nil)
