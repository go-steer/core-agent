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

package compose

import (
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/models/anthropic"
)

func newTestAnthropicProvider(t *testing.T) *anthropic.Provider {
	t.Helper()
	p, err := anthropic.New("test-key-not-real")
	if err != nil {
		t.Fatalf("anthropic.New: %v", err)
	}
	return p
}

func TestMaybeWirePromptCache_DefaultsOn(t *testing.T) {
	t.Parallel()
	p := newTestAnthropicProvider(t)
	status, enabled := MaybeWirePromptCache(p, false)

	if got := p.PromptCache(); !got.System || !got.History {
		t.Errorf("PromptCache() = %+v, want both breakpoint families on", got)
	}
	if !enabled || !strings.Contains(status, "enabled") {
		t.Errorf("status = %q, enabled = %v, want an 'enabled' status", status, enabled)
	}
}

// TestMaybeWirePromptCache_KillSwitch is the property an operator
// depends on when a workload's prefix changes every call: --no-prompt-
// cache must leave the provider with no breakpoints at all, so no write
// premium is paid for reads that never come.
func TestMaybeWirePromptCache_KillSwitch(t *testing.T) {
	t.Parallel()
	p := newTestAnthropicProvider(t)
	status, enabled := MaybeWirePromptCache(p, true)

	if got := p.PromptCache(); got.Enabled() {
		t.Errorf("PromptCache() = %+v after --no-prompt-cache, want everything off", got)
	}
	if enabled || !strings.Contains(status, "--no-prompt-cache") {
		t.Errorf("status = %q, enabled = %v, want a status naming the flag", status, enabled)
	}
}

// TestMaybeWirePromptCache_ReportsConfigDisable covers the shape where
// the registry constructor already applied
// cfg.model.anthropic.prompt_cache.enabled=false: the helper must not
// re-enable it, and must tell the operator why it is off.
func TestMaybeWirePromptCache_ReportsConfigDisable(t *testing.T) {
	t.Parallel()
	p := newTestAnthropicProvider(t)
	p.SetPromptCache(anthropic.CacheOptions{})

	status, enabled := MaybeWirePromptCache(p, false)

	if got := p.PromptCache(); got.Enabled() {
		t.Errorf("PromptCache() = %+v, want the config disable preserved", got)
	}
	if enabled || !strings.Contains(status, "prompt_cache.enabled=false") {
		t.Errorf("status = %q, enabled = %v, want a status naming the config field", status, enabled)
	}
}

func TestMaybeWirePromptCache_IgnoresOtherProviders(t *testing.T) {
	t.Parallel()
	// Gemini has no prompt-cache equivalent; an operator running it
	// shouldn't see a startup line about a knob that doesn't apply.
	status, enabled := MaybeWirePromptCache(fakeNonGeminiProvider{}, true)
	if status != "" || enabled {
		t.Errorf("non-Anthropic provider reported (%q, %v), want silence", status, enabled)
	}
}
