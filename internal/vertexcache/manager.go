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

// Package vertexcache owns the lifecycle of a single Vertex explicit
// context cache — Create at agent startup, Refresh on TTL pressure,
// Delete on session unregister. Callers read the resolved cache name
// via Name() to plumb it onto GenerateContentConfig.CachedContent
// per turn.
//
// See docs/vertex-context-caching-design.md for the v1 scope
// (system-instruction + tools; not conversation history) and the
// failure-mode contract (every error path degrades to "no cache for
// this call," never breaks the session).
package vertexcache

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"google.golang.org/genai"
)

// CachesClient is the subset of *genai.Client's Caches field that
// the manager depends on. Wrapping it in an interface keeps the
// manager unit-testable without a real Vertex client — the fake in
// manager_test.go implements the same shape.
type CachesClient interface {
	Create(ctx context.Context, model string, config *genai.CreateCachedContentConfig) (*genai.CachedContent, error)
	Update(ctx context.Context, name string, config *genai.UpdateCachedContentConfig) (*genai.CachedContent, error)
	Delete(ctx context.Context, name string, config *genai.DeleteCachedContentConfig) (*genai.DeleteCachedContentResponse, error)
}

// Options configures a Manager. Defaults come from
// docs/vertex-context-caching-design.md — TTL 6h, Refresh threshold
// 30min. Zero values pick the default; callers that want the default
// for both fields can pass Options{}.
type Options struct {
	// TTL is how long each Create/Update requests the cache live for.
	// Vertex caps at 24h; the daemon's default matches the session
	// idle timeout (6h). Zero → 6h.
	TTL time.Duration
	// RefreshThreshold triggers a background Refresh call when Name()
	// is read and time-to-expiry drops below this value. Zero → 30min.
	RefreshThreshold time.Duration
	// DisplayName annotates the cache in Vertex's list view. Optional
	// — omitted from Create when empty.
	DisplayName string
	// Logger receives structured operator-facing warnings when a
	// Caches RPC fails. Zero → log.Default() ("core-agent-vertexcache:"
	// prefix included).
	Logger *log.Logger
}

const (
	defaultTTL              = 6 * time.Hour
	defaultRefreshThreshold = 30 * time.Minute
)

func (o Options) ttl() time.Duration {
	if o.TTL > 0 {
		return o.TTL
	}
	return defaultTTL
}

func (o Options) refreshThreshold() time.Duration {
	if o.RefreshThreshold > 0 {
		return o.RefreshThreshold
	}
	return defaultRefreshThreshold
}

func (o Options) logger() *log.Logger {
	if o.Logger != nil {
		return o.Logger
	}
	return log.Default()
}

// Manager owns one cache handle. Init creates it lazily (called from
// pkg/models/gemini on the first GenerateContent that carries a
// system instruction we can cache); Name returns the resolved name
// or "" if not yet ready; Refresh extends TTL when it's about to
// expire; Delete cleans up on shutdown.
//
// Manager is safe for concurrent use. Name() is called from every
// turn's GenerateContent — the read path is a plain RLock, cheap.
type Manager struct {
	caches CachesClient
	model  string
	opts   Options

	mu          sync.RWMutex
	state       state
	cacheName   string
	expiresAt   time.Time
	initStarted bool
	// refreshing is true while a background Refresh RPC is in-flight,
	// preventing us from firing multiple Refresh goroutines from
	// concurrent Name() reads on the same near-expiry window.
	refreshing bool
}

type state int

const (
	stateStart state = iota
	stateActive
	stateFailed
	stateDeleted
)

// NewManager returns a Manager bound to caches + model. model is the
// same modelID passed to Caches.Create — must match the model the
// referencing GenerateContent call uses (Vertex enforces this).
func NewManager(caches CachesClient, model string, opts Options) *Manager {
	return &Manager{
		caches: caches,
		model:  model,
		opts:   opts,
		state:  stateStart,
	}
}

// Init kicks off cache creation with the given systemInstruction +
// tools. Non-blocking: returns immediately after a state check;
// the actual Create RPC runs on a background goroutine so the
// caller (typically the first GenerateContent call) doesn't wait.
//
// Called at-most-once — subsequent calls with the manager already
// initializing / active are no-ops. This is by design: once we've
// seeded the cache from turn N's config, turn N+1's config should be
// identical (both come from the same agent's system instruction +
// tools slice), so re-Init would be wasted RPCs.
func (m *Manager) Init(ctx context.Context, systemInstruction *genai.Content, tools []*genai.Tool) {
	m.mu.Lock()
	if m.initStarted || m.state == stateDeleted {
		m.mu.Unlock()
		return
	}
	m.initStarted = true
	m.mu.Unlock()

	go m.doInit(ctx, systemInstruction, tools)
}

func (m *Manager) doInit(ctx context.Context, systemInstruction *genai.Content, tools []*genai.Tool) {
	cfg := &genai.CreateCachedContentConfig{
		SystemInstruction: systemInstruction,
		Tools:             tools,
		TTL:               m.opts.ttl(),
	}
	if m.opts.DisplayName != "" {
		cfg.DisplayName = m.opts.DisplayName
	}
	created, err := m.caches.Create(ctx, m.model, cfg)
	if err != nil {
		m.opts.logger().Printf("core-agent-vertexcache: Caches.Create failed (agent will run uncached for its lifetime): %v", err)
		m.mu.Lock()
		m.state = stateFailed
		m.mu.Unlock()
		return
	}
	m.mu.Lock()
	m.state = stateActive
	m.cacheName = created.Name
	m.expiresAt = created.ExpireTime
	m.mu.Unlock()
}

// Name returns the resolved cache name if the manager is active,
// or "" if init hasn't landed yet / failed / been deleted. Callers
// wire the return value into GenerateContentConfig.CachedContent —
// an empty value means the request runs uncached, which is always
// safe.
//
// Name also opportunistically triggers a background Refresh if the
// active cache is inside its refresh window. The refresh runs
// asynchronously; the current call still returns the (about-to-expire
// but still-valid) cache name.
func (m *Manager) Name(ctx context.Context) string {
	m.mu.RLock()
	if m.state != stateActive {
		m.mu.RUnlock()
		return ""
	}
	name := m.cacheName
	needsRefresh := !m.expiresAt.IsZero() &&
		time.Until(m.expiresAt) < m.opts.refreshThreshold() &&
		!m.refreshing
	m.mu.RUnlock()

	if needsRefresh {
		m.mu.Lock()
		// Re-check under the write lock — another reader may have
		// already scheduled the refresh between our RUnlock and Lock.
		if m.state == stateActive && !m.refreshing &&
			time.Until(m.expiresAt) < m.opts.refreshThreshold() {
			m.refreshing = true
			go m.doRefresh(ctx, name)
		}
		m.mu.Unlock()
	}
	return name
}

func (m *Manager) doRefresh(ctx context.Context, name string) {
	defer func() {
		m.mu.Lock()
		m.refreshing = false
		m.mu.Unlock()
	}()
	updated, err := m.caches.Update(ctx, name, &genai.UpdateCachedContentConfig{
		TTL: m.opts.ttl(),
	})
	if err != nil {
		m.opts.logger().Printf("core-agent-vertexcache: Caches.Update failed (cache will expire; agent falls back to uncached): %v", err)
		// Don't flip to stateFailed on refresh error — the cache is
		// still valid until it expires. Let Name() return the name
		// until Vertex actually reaps it, then degradation is
		// automatic on the next cache-not-found response from
		// GenerateContent (handled by the caller's retry-once path).
		return
	}
	m.mu.Lock()
	m.expiresAt = updated.ExpireTime
	m.mu.Unlock()
}

// MarkEvicted resets the manager to its pre-Init state after learning
// (via a GenerateContent NOT_FOUND on the cache reference) that Vertex
// has reaped our cache server-side. Callers who see the eviction — the
// pkg/models/gemini wrapper's retry-once path — invoke this so:
//
//  1. Subsequent Name() calls return "" (the request goes uncached).
//  2. The next non-cached turn can call Init again and get a fresh
//     cache handle instead of the agent running uncached for the rest
//     of its lifetime.
//
// Distinct from stateFailed (which stays sticky — a Create failure at
// Init time signals a persistent problem worth surfacing). Eviction is
// a normal end-of-TTL event, especially on long-lived daemons whose
// cache outlives a single session.
//
// Idempotent + safe against races with in-flight Refresh: a refresh
// that lands after MarkEvicted just sees state != active and no-ops.
func (m *Manager) MarkEvicted(reason string) {
	m.mu.Lock()
	// Only meaningful when we currently think we hold a cache.
	// stateFailed / stateDeleted / stateStart-with-init-in-flight all
	// have nothing to invalidate.
	if m.state != stateActive {
		m.mu.Unlock()
		return
	}
	name := m.cacheName
	m.state = stateStart
	m.cacheName = ""
	m.expiresAt = time.Time{}
	m.initStarted = false
	m.mu.Unlock()
	m.opts.logger().Printf("core-agent-vertexcache: cache %s marked evicted (%s); next turn will attempt fresh Init", name, reason)
}

// Delete releases the Vertex cache. Best-effort — failures are
// logged but don't propagate. Callers should invoke this on session
// unregister / daemon shutdown; skipping the call is fine (Vertex
// reaps expired caches) but leaves the cache resource around until
// TTL elapses.
//
// Idempotent: safe to call multiple times, and safe to call before
// Init has landed (skips the RPC if there's no cache to delete).
func (m *Manager) Delete(ctx context.Context) {
	m.mu.Lock()
	if m.state != stateActive || m.cacheName == "" {
		m.state = stateDeleted
		m.mu.Unlock()
		return
	}
	name := m.cacheName
	m.state = stateDeleted
	m.cacheName = ""
	m.expiresAt = time.Time{}
	m.mu.Unlock()

	if _, err := m.caches.Delete(ctx, name, nil); err != nil {
		m.opts.logger().Printf("core-agent-vertexcache: Caches.Delete failed (cache %s will expire naturally): %v", name, err)
	}
}

// Status is a snapshot of the manager's state for diagnostic use
// (e.g. an /internal/cache-status endpoint or a startup log line
// once init settles). Read-only; each call takes a fresh RLock.
type Status struct {
	Active    bool
	Failed    bool
	CacheName string
	ExpiresAt time.Time
	ExpiresIn time.Duration
}

// Snapshot returns the current manager state. Cheap — RLock-only.
func (m *Manager) Snapshot() Status {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s := Status{
		Active:    m.state == stateActive,
		Failed:    m.state == stateFailed,
		CacheName: m.cacheName,
		ExpiresAt: m.expiresAt,
	}
	if !m.expiresAt.IsZero() {
		s.ExpiresIn = time.Until(m.expiresAt)
	}
	return s
}

// String is a short human-readable form used by the startup log
// line ("context cache: active (cache_id=..., ttl=5h59m)").
func (s Status) String() string {
	switch {
	case s.Active:
		return fmt.Sprintf("active (ttl=%s)", s.ExpiresIn.Truncate(time.Second))
	case s.Failed:
		return "failed (agent runs uncached)"
	default:
		return "initializing"
	}
}
