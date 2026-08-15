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
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/models/anthropic"
)

// anthropicSubagentCfg builds a parent config plus a subagent spec that
// pins its OWN Anthropic model — the shape that makes the subagent
// resolve a fresh provider instead of reusing the parent's.
func anthropicSubagentCfg(t *testing.T, parentPromptCache, subPromptCache *config.PromptCacheConfig) (*config.Config, config.SubagentSpec) {
	t.Helper()
	t.Setenv(anthropic.EnvAPIKey, "test-key-not-real")

	cfg := &config.Config{}
	cfg.Model.Provider = config.ProviderAnthropic
	cfg.Model.Name = "claude-opus-4-7"
	cfg.Model.Anthropic = &config.AnthropicConfig{PromptCache: parentPromptCache}

	spec := config.SubagentSpec{
		Name: "helper",
		Model: &config.ModelConfig{
			Provider:  config.ProviderAnthropic,
			Name:      "claude-haiku-4-5",
			Anthropic: &config.AnthropicConfig{PromptCache: subPromptCache},
		},
	}
	if subPromptCache == nil {
		spec.Model.Anthropic = nil
	}
	return cfg, spec
}

func promptCacheOf(t *testing.T, cfg *config.Config, spec config.SubagentSpec, noPromptCache bool) anthropic.CacheOptions {
	t.Helper()
	p, _, err := resolveSubagentProvider(cfg, nil, spec, noPromptCache, func(string) {})
	if err != nil {
		t.Fatalf("resolveSubagentProvider: %v", err)
	}
	ap, ok := p.(*anthropic.Provider)
	if !ok {
		t.Fatalf("provider = %T, want *anthropic.Provider", p)
	}
	return ap.PromptCache()
}

// TestResolveSubagentProvider_KillSwitchReachesOwnModel is the gap the
// flag would otherwise have: a subagent with its own model resolves a
// provider straight out of the registry, which never sees CLI flags. If
// the switch stopped here, "--no-prompt-cache" would silently mean "off
// for the parent, on for every subagent".
//
// Scope: this pins resolveSubagentProvider's own behaviour, not the hop
// that carries the parsed flag into subagentDeps — main.go's wiring has
// no test seam in this repo.
func TestResolveSubagentProvider_KillSwitchReachesOwnModel(t *testing.T) {
	cfg, spec := anthropicSubagentCfg(t, nil, nil)
	if got := promptCacheOf(t, cfg, spec, true); got.Enabled() {
		t.Errorf("subagent PromptCache = %+v under --no-prompt-cache, want everything off", got)
	}
}

// TestResolveSubagentProvider_InheritsParentConfigDisable: overwriting
// cfg.Model with the subagent's own block also drops the parent's
// prompt_cache setting, since it hangs off model.anthropic. A
// project-wide disable has to survive that copy.
func TestResolveSubagentProvider_InheritsParentConfigDisable(t *testing.T) {
	off := false
	cfg, spec := anthropicSubagentCfg(t, &config.PromptCacheConfig{Enabled: &off}, nil)
	if got := promptCacheOf(t, cfg, spec, false); got.Enabled() {
		t.Errorf("subagent PromptCache = %+v, want the parent's disable inherited", got)
	}
}

// A subagent that states its own preference wins over the parent's —
// that is what writing the block per-subagent is for.
func TestResolveSubagentProvider_OwnSettingOverridesParent(t *testing.T) {
	off, on := false, true
	cfg, spec := anthropicSubagentCfg(t,
		&config.PromptCacheConfig{Enabled: &off},
		&config.PromptCacheConfig{Enabled: &on},
	)
	if got := promptCacheOf(t, cfg, spec, false); !got.Enabled() {
		t.Errorf("subagent PromptCache = %+v, want its own enable to win", got)
	}
}

func TestResolveSubagentProvider_DefaultsOn(t *testing.T) {
	cfg, spec := anthropicSubagentCfg(t, nil, nil)
	if got := promptCacheOf(t, cfg, spec, false); !got.Enabled() {
		t.Errorf("subagent PromptCache = %+v, want the default-on policy", got)
	}
}

// TestInheritPromptCache_DoesNotMutateParent guards the shallow-copy
// hazard: subCfg shares the parent's *AnthropicConfig pointer, so
// filling the field in place would rewrite the real config for every
// later reader.
func TestInheritPromptCache_DoesNotMutateParent(t *testing.T) {
	t.Parallel()
	off := false
	parent := &config.AnthropicConfig{
		APIKey:      "parent-key",
		PromptCache: &config.PromptCacheConfig{Enabled: &off},
	}
	sub := &config.AnthropicConfig{APIKey: "sub-key"}

	got := inheritPromptCache(parent, sub)
	if got.PromptCache != parent.PromptCache {
		t.Error("subagent did not inherit the parent's prompt_cache")
	}
	if got.APIKey != "sub-key" {
		t.Errorf("APIKey = %q, want the subagent's own", got.APIKey)
	}
	if sub.PromptCache != nil {
		t.Error("inheritPromptCache mutated the subagent's config block")
	}
	if parent.APIKey != "parent-key" {
		t.Error("inheritPromptCache mutated the parent's config block")
	}
	if got := inheritPromptCache(nil, sub); got != sub {
		t.Error("a nil parent should leave the subagent block untouched")
	}
	if got := inheritPromptCache(&config.AnthropicConfig{}, nil); got != nil {
		t.Errorf("a parent with no prompt_cache should leave a nil subagent block nil, got %+v", got)
	}
}
