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

package models

import "context"

// noPromptCacheKey is the context key carrying the one-shot opt-out.
type noPromptCacheKey struct{}

// WithoutPromptCache marks ctx as a one-shot request: a call whose
// prompt prefix is not expected to recur, so a provider that would
// normally write a prompt-cache entry should skip it.
//
// Prompt caching trades a write premium now (Anthropic bills 1.25x on
// the tokens it stores) against cheap reads later (~0.1x). The trade
// pays off from the second request that carries the same prefix. Some
// calls are structurally never that second request — core-agent's
// summarizer and checkpointer send their own system instruction and no
// tools, the /btw side question sends neither, and a tight-budget
// subtask runs in a session whose ID never recurs. Their prefixes
// diverge from the agentic loop's at the first block, so the byte-exact
// match can't hit even where the history is identical, and writing an
// entry is a pure 25% surcharge on a call that is already expensive
// because it re-sends the whole conversation.
//
// This is a hint, not a guarantee: providers without prompt caching
// (Gemini today) ignore it, and it can only turn caching OFF — a
// provider whose caching is disabled by config or CLI stays disabled.
// It lives here rather than on a provider-specific option so callers in
// pkg/agent can set it without importing a backend.
func WithoutPromptCache(ctx context.Context) context.Context {
	return context.WithValue(ctx, noPromptCacheKey{}, true)
}

// PromptCacheSuppressed reports whether WithoutPromptCache was applied
// to ctx. Backends consult it per request, since one model.LLM serves
// both the agentic loop and the one-shot side calls.
func PromptCacheSuppressed(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	suppressed, _ := ctx.Value(noPromptCacheKey{}).(bool)
	return suppressed
}
