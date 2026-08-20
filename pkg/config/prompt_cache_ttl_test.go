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

func TestPromptCacheConfig_CacheTTL(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		in   *PromptCacheConfig
		want string
	}{
		{"nil receiver", nil, PromptCacheTTL5m},
		{"unset", &PromptCacheConfig{}, PromptCacheTTL5m},
		{"5m", &PromptCacheConfig{TTL: "5m"}, PromptCacheTTL5m},
		{"1h", &PromptCacheConfig{TTL: "1h"}, PromptCacheTTL1h},
		{"padded", &PromptCacheConfig{TTL: "  1h\n"}, PromptCacheTTL1h},
		{"unknown", &PromptCacheConfig{TTL: "24h"}, PromptCacheTTL5m},
	} {
		if got := tc.in.CacheTTL(); got != tc.want {
			t.Errorf("%s: CacheTTL() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// An unknown TTL has to be a load-time error, not a silent 5m. The two
// values differ by 60% on write cost, so an operator who typed "1hr"
// has a budget expectation the run would quietly fail to meet — and
// they would only find out on the invoice.
func TestValidate_RejectsAnUnknownPromptCacheTTL(t *testing.T) {
	t.Parallel()
	cfgWithTTL := func(ttl string) *Config {
		c := DefaultConfig()
		c.Model.Name = "claude-opus-5"
		c.Model.Provider = ProviderAnthropic
		c.Model.Anthropic = &AnthropicConfig{PromptCache: &PromptCacheConfig{TTL: ttl}}
		return c
	}
	for _, ok := range []string{"", "5m", "1h"} {
		if err := cfgWithTTL(ok).Validate(); err != nil {
			t.Errorf("ttl %q: Validate = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"1hr", "3600s", "60m", "1H", "forever"} {
		err := cfgWithTTL(bad).Validate()
		if err == nil {
			t.Errorf("ttl %q: Validate = nil, want an error", bad)
			continue
		}
		if !strings.Contains(err.Error(), "prompt_cache.ttl") {
			t.Errorf("ttl %q: error %q does not name the offending field", bad, err)
		}
	}

	// An absent block must not trip the check — the overwhelmingly
	// common config has no anthropic section at all.
	c := DefaultConfig()
	c.Model.Name = "gemini-3.5-flash"
	if err := c.Validate(); err != nil {
		t.Errorf("no anthropic block: Validate = %v, want nil", err)
	}
}

// A subagent's own model block reaches the same CacheTTL fallback the
// parent's does, so an unchecked typo there downgrades the delegate
// silently while the identical typo one level up stops the run.
// Subagent turns are not the cheap ones by definition — a declarative
// delegate can be the same model as its parent.
func TestValidate_RejectsAnUnknownPromptCacheTTLOnASubagent(t *testing.T) {
	t.Parallel()
	cfgWithSubagentTTL := func(ttl string) *Config {
		c := DefaultConfig()
		c.Model.Name = "claude-opus-5"
		c.Model.Provider = ProviderAnthropic
		c.Subagents = []SubagentSpec{{
			Name:        "researcher",
			Description: "digs through the docs",
			Model: &ModelConfig{
				Name:      "claude-haiku-4-5",
				Provider:  ProviderAnthropic,
				Anthropic: &AnthropicConfig{PromptCache: &PromptCacheConfig{TTL: ttl}},
			},
		}}
		return c
	}
	for _, ok := range []string{"", "5m", "1h"} {
		if err := cfgWithSubagentTTL(ok).Validate(); err != nil {
			t.Errorf("subagent ttl %q: Validate = %v, want nil", ok, err)
		}
	}
	err := cfgWithSubagentTTL("1hr").Validate()
	if err == nil {
		t.Fatal("subagent ttl \"1hr\": Validate = nil, want an error")
	}
	if !strings.Contains(err.Error(), "subagents[0].model.anthropic.prompt_cache.ttl") {
		t.Errorf("error %q does not point at the subagent block that carries the typo", err)
	}
}

// The operator writes this key by hand; the JSON spelling is the
// contract, and `omitempty` keeps an unset TTL out of a written file.
func TestPromptCacheConfig_JSONShape(t *testing.T) {
	t.Parallel()
	b, err := json.Marshal(&PromptCacheConfig{TTL: "1h"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got, want := string(b), `{"ttl":"1h"}`; got != want {
		t.Errorf("marshal = %s, want %s", got, want)
	}

	var pc PromptCacheConfig
	if err := json.Unmarshal([]byte(`{"enabled":true,"ttl":"1h"}`), &pc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if pc.CacheTTL() != PromptCacheTTL1h || !pc.IsEnabled() {
		t.Errorf("round-trip = %+v, want enabled at 1h", pc)
	}
}

// The per-model override map has to be able to state the 1-hour write
// rate, or an operator correcting a rate for an unknown model fixes the
// 5-minute one and silently leaves the 1-hour one wrong.
func TestPricingConfig_CarriesTheOneHourWriteRate(t *testing.T) {
	t.Parallel()
	var pc PricingConfig
	if err := json.Unmarshal([]byte(`{"cache_creation_1h_input_per_mtok":30}`), &pc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if pc.CacheCreation1hInputPerMTok != 30 {
		t.Errorf("CacheCreation1hInputPerMTok = %v, want 30", pc.CacheCreation1hInputPerMTok)
	}
}
