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
	"context"
	"fmt"
	"time"

	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/internal/vertexcache"
	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/models"
	"github.com/go-steer/core-agent/v2/pkg/models/gemini"
)

// ContextCacheHandle is the exported face of the wired Vertex
// context-cache manager. The manager itself lives in
// internal/vertexcache (its construction couples to the genai SDK's
// Caches client); everything a HOST needs after wiring — tearing the
// remote cache resource down at shutdown — is this one method, so
// promoting the whole manager would widen the stability surface for
// no consumer benefit (#489: the previous *vertexcache.Manager
// return type was un-nameable outside the module, making the
// function's result usable only via := inference).
type ContextCacheHandle interface {
	// Delete tears down the remote cache resource. Call (typically
	// deferred) at daemon shutdown; safe on a manager whose cache
	// was never created.
	Delete(ctx context.Context)
}

// MaybeWireContextCache builds the Vertex context-cache manager and
// installs its hooks on the provider when the following are all true:
//
//  1. The provider is *gemini.Provider (concrete type — cache
//     hooks live on that struct).
//  2. Backend is Vertex (cfg.Model.Provider == "vertex").
//  3. Caching is enabled in config (default ON; explicit
//     enabled=false in cfg.Model.Vertex.ContextCache disables).
//  4. The noContextCache kill switch (the CLI's --no-context-cache)
//     was NOT set.
//
// Returns the handle on success (caller wires deferred Delete) or
// nil when caching was skipped for any reason. Never fails hard: if
// constructing the sibling genai.Client fails, the helper logs and
// returns nil — the agent still starts, just without caching.
//
// Contract note: every skip path returns a LITERAL nil (a nil
// interface), never a nil *Manager boxed into the interface — the
// caller's `handle != nil` guard is load-bearing because the
// manager's Delete is not nil-receiver-safe. Keep it that way when
// adding skip paths.
func MaybeWireContextCache(
	ctx context.Context,
	provider models.Provider,
	cfg *config.Config,
	noContextCache bool,
	send func(string),
) ContextCacheHandle {
	if noContextCache {
		send("context cache: disabled (--no-context-cache)")
		return nil
	}
	if cfg == nil || cfg.Model.Provider != config.ProviderVertex {
		// Silent: caching is Vertex-only and irrelevant to other
		// providers; no need to spam the log with a "not applicable"
		// line for every Anthropic / echo session.
		return nil
	}
	// cfg.Model.Vertex may be nil in the common auto-detection path:
	// operators typically set `"provider": "vertex"` and rely on
	// GOOGLE_CLOUD_PROJECT / GOOGLE_CLOUD_LOCATION env vars rather
	// than duplicating them in config.json. Missing block means
	// "no per-project override" — caching defaults to ON, not off.
	var cc *config.ContextCacheConfig
	if cfg.Model.Vertex != nil {
		cc = cfg.Model.Vertex.ContextCache
	}
	if !cc.IsEnabled() {
		send("context cache: disabled (cfg.model.vertex.context_cache.enabled=false)")
		return nil
	}
	gemProvider, ok := provider.(*gemini.Provider)
	if !ok {
		// Also silent — a non-Gemini Provider under a "vertex"
		// config would be an internal misconfiguration, not
		// something a normal operator would see.
		return nil
	}
	clientCfg := gemProvider.ClientConfig()
	if clientCfg == nil {
		send("context cache: skipped (provider has no ClientConfig)")
		return nil
	}
	client, err := genai.NewClient(ctx, clientCfg)
	if err != nil {
		send(fmt.Sprintf("context cache: skipped (genai.NewClient failed: %v)", err))
		return nil
	}
	// Parse TTL/Refresh from the config strings. Fall back to
	// vertexcache's own defaults (via zero-value Options) on
	// parse errors — better to run with defaults than fail startup
	// over an operator typo in a duration string.
	var opts vertexcache.Options
	opts.DisplayName = fmt.Sprintf("core-agent-%s", cfg.Model.Name)
	if cc != nil {
		if cc.TTL != "" {
			if d, err := time.ParseDuration(cc.TTL); err == nil {
				opts.TTL = d
			} else {
				send(fmt.Sprintf("context cache: bad TTL %q — using default: %v", cc.TTL, err))
			}
		}
		if cc.Refresh != "" {
			if d, err := time.ParseDuration(cc.Refresh); err == nil {
				opts.RefreshThreshold = d
			} else {
				send(fmt.Sprintf("context cache: bad Refresh %q — using default: %v", cc.Refresh, err))
			}
		}
	}
	manager := vertexcache.NewManager(client.Caches, cfg.Model.Name, opts)
	gemProvider.SetContextCache(manager.Init, manager.Name)
	// Wire the eviction-recovery hook so a Vertex-side TTL expiry
	// (the common case on long-lived daemons whose cache outlives a
	// single session) triggers uncached retry + fresh Init on the
	// next turn instead of a hard turn error.
	gemProvider.SetContextCacheInvalidate(manager.MarkEvicted)

	// Startup log — mirrors the "agentic subtasks:" line pattern so
	// operators see cache state at the same glance.
	ttl := opts.TTL
	if ttl == 0 {
		ttl = 6 * time.Hour // mirror vertexcache defaultTTL for the log line
	}
	send(fmt.Sprintf("context cache: enabled (ttl=%s, model=%s)", ttl, cfg.Model.Name))
	return manager
}
