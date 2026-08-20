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
	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/models"
	"github.com/go-steer/core-agent/v2/pkg/models/anthropic"
)

// MaybeWirePromptCache applies the operator's prompt-cache kill switch
// (the CLI's --no-prompt-cache) to an Anthropic provider and returns a
// one-line status plus whether caching survived.
//
// Unlike MaybeWireContextCache there is nothing to construct: Anthropic
// prompt caching is cache_control markers on the ordinary request, not
// a server-side resource. The config gate is already applied by the
// registry constructor, so this helper exists for the one thing the
// registry can't see — a CLI flag that arrives after models.Resolve.
// Call it after Resolve and BEFORE the first provider.Model() call;
// Model() copies the policy into each LLM it builds.
//
// Providers other than Anthropic are left alone and report "" — Gemini
// has no equivalent, and an operator running Gemini shouldn't see a line
// about a knob that doesn't apply to them. The caller decides what to
// print: the daemon announces its own provider either way, while a
// per-subagent provider only announces a deviation from the default.
func MaybeWirePromptCache(provider models.Provider, noPromptCache bool) (status string, enabled bool) {
	return MaybeWirePromptCacheTTL(provider, noPromptCache, "")
}

// MaybeWirePromptCacheTTL is MaybeWirePromptCache plus the breakpoint
// TTL override behind --prompt-cache-ttl: config.PromptCacheTTL5m,
// config.PromptCacheTTL1h, or "" to keep whatever the config gate
// already put on the provider.
//
// The TTL is a launch-time property more often than a repo-level one —
// the same checkout run interactively wants 5m and run from cron wants
// 1h — which is why it gets a flag and not just a config key. The 1-hour
// breakpoint bills writes at 2x base input against 5m's 1.25x, so it
// pays only when consecutive turns are more than five minutes apart
// (#770).
func MaybeWirePromptCacheTTL(provider models.Provider, noPromptCache bool, ttl string) (status string, enabled bool) {
	p, ok := provider.(*anthropic.Provider)
	if !ok {
		return "", false
	}
	if noPromptCache {
		p.SetPromptCache(anthropic.CacheOptions{})
		return "prompt cache: disabled (--no-prompt-cache)", false
	}
	opts := p.PromptCache()
	if !opts.Enabled() {
		// The registry constructor already read the config gate; say so
		// rather than re-deriving it, so the line can't drift from what
		// the provider actually carries.
		return "prompt cache: disabled (cfg.model.anthropic.prompt_cache.enabled=false)", false
	}
	if ttl != "" {
		opts.TTL = ttl
		p.SetPromptCache(opts)
	}
	effective := config.PromptCacheTTL5m
	if opts.TTL == config.PromptCacheTTL1h {
		effective = config.PromptCacheTTL1h
	}
	return "prompt cache: enabled (" + effective + " ttl, system + rolling history breakpoints)", true
}
