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

package anthropic

import (
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/usage"
)

// ttlOf collects the TTL from every marker the request carries, so a
// test can assert the whole request agrees rather than spot-checking
// one block. A request that mixes TTLs would make "which breakpoint
// expired" depend on marker position.
func ttlOf(p anthropic.MessageNewParams) []anthropic.CacheControlEphemeralTTL {
	var out []anthropic.CacheControlEphemeralTTL
	for _, b := range p.System {
		if b.CacheControl.Type != "" {
			out = append(out, b.CacheControl.TTL)
		}
	}
	for _, m := range p.Messages {
		for _, blk := range m.Content {
			if cc := blk.GetCacheControl(); cc != nil && cc.Type != "" {
				out = append(out, cc.TTL)
			}
		}
	}
	return out
}

func ttlParams(t *testing.T, opts CacheOptions) anthropic.MessageNewParams {
	t.Helper()
	cfg := &genai.GenerateContentConfig{
		SystemInstruction: genai.NewContentFromText("be helpful", genai.RoleUser),
	}
	p, err := buildParams("claude-opus-4-7", longHistory(40), cfg, opts, BuiltinTools{})
	if err != nil {
		t.Fatalf("buildParams: %v", err)
	}
	return p
}

// The request half of #770: asking for the 1-hour breakpoint has to
// reach the wire. Without the TTL on the marker the provider writes a
// 5-minute entry, the operator's config is silently ignored, and the
// cache expires between the turns it was chosen for.
func TestApplyCacheBreakpoints_StampsTheOneHourTTLOnEveryMarker(t *testing.T) {
	t.Parallel()
	got := ttlOf(ttlParams(t, CacheOptions{System: true, History: true, TTL: config.PromptCacheTTL1h}))
	if len(got) < 2 {
		t.Fatalf("markers = %d, want the system marker plus history ones", len(got))
	}
	for i, ttl := range got {
		if ttl != anthropic.CacheControlEphemeralTTLTTL1h {
			t.Errorf("marker %d TTL = %q, want %q", i, ttl, anthropic.CacheControlEphemeralTTLTTL1h)
		}
	}
}

// The default must stay 5m and must stay *unset* on the wire: the API
// documents 5m as the default, and omitzero keeps the request byte-
// identical to every pre-#770 one. A request whose bytes changed would
// miss every cache entry written by the previous build.
func TestApplyCacheBreakpoints_DefaultLeavesTheTTLUnset(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		opts CacheOptions
	}{
		{"explicit 5m", CacheOptions{System: true, History: true, TTL: config.PromptCacheTTL5m}},
		{"zero TTL", CacheOptions{System: true, History: true}},
		{"unrecognized value", CacheOptions{System: true, History: true, TTL: "1hr"}},
	} {
		got := ttlOf(ttlParams(t, tc.opts))
		if len(got) < 2 {
			t.Fatalf("%s: markers = %d, want several", tc.name, len(got))
		}
		for i, ttl := range got {
			if ttl != "" {
				t.Errorf("%s: marker %d TTL = %q, want it omitted (the API default is 5m)", tc.name, i, ttl)
			}
		}
	}
}

// reapplyCacheBreakpoints clears and re-marks a request that grew after
// buildParams returned (the pause_turn continuation). The TTL has to
// survive that round trip or a long-running turn silently downgrades.
func TestReapplyCacheBreakpoints_KeepsTheOneHourTTL(t *testing.T) {
	t.Parallel()
	opts := CacheOptions{System: true, History: true, TTL: config.PromptCacheTTL1h}
	p := ttlParams(t, opts)
	if got := reapplyCacheBreakpoints(&p, opts); got == 0 {
		t.Fatal("reapply placed no markers")
	}
	for i, ttl := range ttlOf(p) {
		if ttl != anthropic.CacheControlEphemeralTTLTTL1h {
			t.Errorf("marker %d TTL = %q after reapply, want %q", i, ttl, anthropic.CacheControlEphemeralTTLTTL1h)
		}
	}
}

// The response half: Anthropic reports which TTL produced the writes,
// so there is a right answer to bill rather than a guess about what the
// request asked for.
func TestCacheCreationMetadata_CarriesTheOneHourShare(t *testing.T) {
	t.Parallel()
	u := anthropic.Usage{CacheCreationInputTokens: 1000}
	u.CacheCreation.Ephemeral1hInputTokens = 400
	u.CacheCreation.Ephemeral5mInputTokens = 600

	m := cacheCreationMetadata(u)
	if got := m[cacheCreationTokensMetadataKey]; got != int64(1000) {
		t.Errorf("total write bucket = %v, want 1000", got)
	}
	if got := m[cacheCreation1hTokensMetadataKey]; got != int64(400) {
		t.Errorf("1h share = %v, want 400", got)
	}

	// Round-trip through the reader that consumes it, since the two
	// sides spell the keys independently.
	turn := usage.TurnUsageFromMetadata(&genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 5000}, m)
	if turn.CacheCreation1hInputTokens != 400 {
		t.Errorf("usage read back %d, want 400", turn.CacheCreation1hInputTokens)
	}
}

// An all-5m turn — the default and overwhelmingly common case — should
// not stamp a zero. The absent key already means "no 1h writes", and
// the sidecar rides on every event.
func TestCacheCreationMetadata_OmitsTheOneHourKeyWhenUnused(t *testing.T) {
	t.Parallel()
	u := anthropic.Usage{CacheCreationInputTokens: 1000}
	u.CacheCreation.Ephemeral5mInputTokens = 1000
	m := cacheCreationMetadata(u)
	if _, ok := m[cacheCreation1hTokensMetadataKey]; ok {
		t.Error("a 5m-only turn stamped the 1h key")
	}
	if len(m) != 1 {
		t.Errorf("metadata = %v, want just the total", m)
	}
}

// The pause_turn continuation loop issues several requests and folds
// their usage into one turn. Missing the per-TTL fold would leave a
// continuation's 1-hour writes priced at the 5-minute rate.
func TestAddUsage_FoldsThePerTTLSplit(t *testing.T) {
	t.Parallel()
	var dst anthropic.Usage
	for range 3 {
		src := anthropic.Usage{CacheCreationInputTokens: 100}
		src.CacheCreation.Ephemeral1hInputTokens = 70
		src.CacheCreation.Ephemeral5mInputTokens = 30
		addUsage(&dst, src)
	}
	if dst.CacheCreationInputTokens != 300 {
		t.Errorf("total = %d, want 300", dst.CacheCreationInputTokens)
	}
	if dst.CacheCreation.Ephemeral1hInputTokens != 210 {
		t.Errorf("1h = %d, want 210", dst.CacheCreation.Ephemeral1hInputTokens)
	}
	if dst.CacheCreation.Ephemeral5mInputTokens != 90 {
		t.Errorf("5m = %d, want 90", dst.CacheCreation.Ephemeral5mInputTokens)
	}
}

func TestCacheOptionsFromConfig_TTL(t *testing.T) {
	t.Parallel()
	on := true
	for _, tc := range []struct {
		name string
		pc   *config.PromptCacheConfig
		want string
	}{
		{"no block", nil, config.PromptCacheTTL5m},
		{"empty ttl", &config.PromptCacheConfig{}, config.PromptCacheTTL5m},
		{"explicit 5m", &config.PromptCacheConfig{TTL: "5m"}, config.PromptCacheTTL5m},
		{"1h", &config.PromptCacheConfig{TTL: "1h"}, config.PromptCacheTTL1h},
		{"1h with enabled", &config.PromptCacheConfig{Enabled: &on, TTL: "1h"}, config.PromptCacheTTL1h},
		{"whitespace tolerated", &config.PromptCacheConfig{TTL: " 1h "}, config.PromptCacheTTL1h},
		// Validate rejects this before it can reach here; the fallback
		// exists so a library caller bypassing Validate gets the
		// cheaper TTL rather than an unpredictable one.
		{"garbage falls back", &config.PromptCacheConfig{TTL: "nope"}, config.PromptCacheTTL5m},
	} {
		cfg := withAnthropic(&config.AnthropicConfig{PromptCache: tc.pc})
		if got := cacheOptionsFromConfig(cfg).TTL; got != tc.want {
			t.Errorf("%s: TTL = %q, want %q", tc.name, got, tc.want)
		}
	}
}
