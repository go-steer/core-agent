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
	"fmt"
	"time"

	"google.golang.org/genai"

	"github.com/go-steer/core-agent/internal/vertexcache"
	"github.com/go-steer/core-agent/pkg/config"
	"github.com/go-steer/core-agent/pkg/models"
	"github.com/go-steer/core-agent/pkg/models/gemini"
)

// maybeWireContextCache builds a vertexcache.Manager and installs
// its hooks on the provider when the following are all true:
//
//  1. The provider is *gemini.Provider (concrete type — cache
//     hooks live on that struct).
//  2. Backend is Vertex (cfg.Model.Provider == "vertex").
//  3. Caching is enabled in config (default ON; explicit
//     enabled=false in cfg.Model.Vertex.ContextCache disables).
//  4. The --no-context-cache CLI kill switch was NOT set.
//
// Returns the manager on success (caller wires deferred Delete)
// or nil when caching was skipped for any reason. Never fails
// hard: if constructing the sibling genai.Client fails, the
// helper logs and returns nil — the agent still starts, just
// without caching.
func maybeWireContextCache(
	ctx context.Context,
	provider models.Provider,
	cfg *config.Config,
	noContextCache bool,
	send func(string),
) *vertexcache.Manager {
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
	if cfg.Model.Vertex == nil || !cfg.Model.Vertex.ContextCache.IsEnabled() {
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
	if cc := cfg.Model.Vertex.ContextCache; cc != nil {
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

	// Startup log — mirrors the "agentic subtasks:" line pattern so
	// operators see cache state at the same glance.
	ttl := opts.TTL
	if ttl == 0 {
		ttl = 6 * time.Hour // mirror vertexcache defaultTTL for the log line
	}
	send(fmt.Sprintf("context cache: enabled (ttl=%s, model=%s)", ttl, cfg.Model.Name))
	return manager
}
