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
	"fmt"
	"slices"
	"strings"

	"github.com/go-steer/core-models/profile"
)

// Provider profiles (docs/model-support-design.md, core-models phase
// L3'): every model.provider beyond core-agent's own six names.
//
// A profile comes from one of two places. core-models ships built-ins
// (vertex-maas, vllm, sglang, ollama, openai-compatible), selectable
// as they are or extended; an operator declares the rest under
// `providers` in .agents/config.json, in core-models' schema. The
// six names below stay core-agent's: a profile cannot take one, so
// `--provider anthropic` means the same thing whatever a config
// declares.

// coreProviders are the provider names core-agent resolves itself, in
// the order error messages list them.
var coreProviders = []string{ProviderGemini, ProviderVertex, ProviderAnthropic, ProviderAnthropicVertex, ProviderEcho, ProviderScripted}

// IsCoreProvider reports whether name is one of core-agent's own
// providers rather than a profile.
func IsCoreProvider(name string) bool { return slices.Contains(coreProviders, name) }

// KnownProvider reports whether name selects a provider: empty
// (auto-detect), one of core-agent's own, a core-models built-in
// profile, or a profile this config declares.
func (c *Config) KnownProvider(name string) bool {
	if name == "" || IsCoreProvider(name) {
		return true
	}
	_, ok := c.Profile(name)
	return ok
}

// IsProfileProvider reports whether name selects a provider profile.
func (c *Config) IsProfileProvider(name string) bool {
	return name != "" && !IsCoreProvider(name) && c.KnownProvider(name)
}

// Profile returns the profile name selects, declared ones first, with
// any `extends` left unexpanded (core-models expands at open).
func (c *Config) Profile(name string) (profile.Profile, bool) {
	if IsCoreProvider(name) {
		return profile.Profile{}, false
	}
	p, err := profile.Find(name, c.Providers)
	if err != nil {
		return profile.Profile{}, false
	}
	return p, true
}

// ProfileNames lists every selectable profile: core-models' built-ins,
// then the declared ones, sorted and without duplicates.
func (c *Config) ProfileNames() []string {
	names := profile.BuiltinNames()
	for _, p := range c.Providers {
		names = append(names, p.Name)
	}
	slices.Sort(names)
	return slices.Compact(names)
}

func (c *Config) providerChoices() string {
	return strings.Join(coreProviders, ", ") + ", or a provider profile (" + strings.Join(c.ProfileNames(), ", ") + ")"
}

// validateProviders checks the declared profiles' shape. Shape only: a
// profile is checked against the environment (variables set,
// credentials found) when it is opened, so a profile nobody selects
// never fails a run for want of its API key.
func (c *Config) validateProviders() error {
	seen := map[string]bool{}
	for i, p := range c.Providers {
		if p.Name == "" {
			return fmt.Errorf("config: providers[%d] has no name", i)
		}
		if IsCoreProvider(p.Name) {
			return fmt.Errorf("config: providers[%d]: %q is one of core-agent's own providers; give the profile another name", i, p.Name)
		}
		if seen[p.Name] {
			return fmt.Errorf("config: providers[%d]: profile %q is declared twice", i, p.Name)
		}
		seen[p.Name] = true
		expanded, err := profile.Expand(p)
		if err == nil {
			err = expanded.Validate()
		}
		if err != nil {
			return fmt.Errorf("config: providers[%d] (%s): %w", i, p.Name, err)
		}
	}
	return nil
}

// ProfileRates collects the per-model rates the declared profiles
// carry, keyed by model id, for pkg/pricing's lowest-precedence layer:
// the operator's price for a model no published catalog covers, such
// as a self-hosted one. The first profile to declare a model wins.
func (c *Config) ProfileRates() PricingMap {
	var out PricingMap
	for _, p := range c.Providers {
		for _, m := range p.Models {
			if m.Rates == nil {
				continue
			}
			if out == nil {
				out = PricingMap{}
			}
			if _, dup := out[m.ID]; dup {
				continue
			}
			out[m.ID] = PricingConfig{
				InputPerMTok:              m.Rates.InputPerMTok,
				CachedInputPerMTok:        m.Rates.CachedInputPerMTok,
				CacheCreationInputPerMTok: m.Rates.CacheWritePerMTok,
				OutputPerMTok:             m.Rates.OutputPerMTok,
			}
		}
	}
	return out
}
