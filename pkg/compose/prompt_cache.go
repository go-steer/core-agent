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
	p, ok := provider.(*anthropic.Provider)
	if !ok {
		return "", false
	}
	if noPromptCache {
		p.SetPromptCache(anthropic.CacheOptions{})
		return "prompt cache: disabled (--no-prompt-cache)", false
	}
	if !p.PromptCache().Enabled() {
		// The registry constructor already read the config gate; say so
		// rather than re-deriving it, so the line can't drift from what
		// the provider actually carries.
		return "prompt cache: disabled (cfg.model.anthropic.prompt_cache.enabled=false)", false
	}
	return "prompt cache: enabled (5m ttl, system + rolling history breakpoints)", true
}
