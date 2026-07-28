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

package attach

import (
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/go-steer/core-agent/v2/pkg/auth"
)

// Per-caller rate limiting for the COST-BEARING attach endpoints
// (#463). The synchronous operator endpoints that construct an agent
// or drive unbounded model work — the five slash ops, POST /sessions,
// and POST .../pricing/refresh — previously ran as fast as an
// authenticated caller could POST, so any single credential could
// drive cost-DoS. A token bucket per resolved caller identity bounds
// the damage while leaving reads, SSE streams, /inject, /wake, and
// permission responses untouched.

const (
	// defaultCostPerMinute / defaultCostBurst apply when
	// CostRateLimit is the zero value. 10/min with a burst of 5 is
	// generous for a human operator (slash ops take 5-30s each) and
	// still caps a runaway script at ~10 model-bearing calls/min.
	defaultCostPerMinute = 10
	defaultCostBurst     = 5

	// costLimiterPruneThreshold is the bucket-map size above which
	// Allow lazily sweeps idle buckets. Keeps the map bounded when a
	// proxy churns through many asserted identities.
	costLimiterPruneThreshold = 1024

	// costLimiterIdleAfter is how long a bucket must sit unused
	// before the lazy prune may evict it. An evicted identity simply
	// starts over with a full burst — strictly more permissive, so
	// eviction is never a correctness problem.
	costLimiterIdleAfter = time.Hour

	// anonymousRateIdentity buckets requests whose resolved Caller
	// has an empty Identity (e.g., a zero DefaultCaller). All such
	// requests share one bucket by design — an empty identity is
	// nobody in particular, so it must not mint fresh buckets.
	anonymousRateIdentity = "anonymous"
)

// CostRateLimit configures the per-caller token bucket the attach
// server applies to the COST-BEARING endpoints only: the slash
// dispatchers (compact, done, btw, subagent, replan), POST /sessions
// (constructs a fresh agent), and POST .../pricing/refresh (network
// fetch + catalog rebuild). Read endpoints, /events streams, /inject,
// /wake, and permission responses are never limited.
//
// The zero value enables the limiter with defaults (PerMinute 10,
// Burst 5). Set Disabled to switch enforcement off entirely.
type CostRateLimit struct {
	// PerMinute is the sustained refill rate in requests per minute
	// per caller identity. <= 0 means the default (10).
	PerMinute int
	// Burst is the bucket capacity — how many requests a caller can
	// fire back-to-back before the sustained rate applies. <= 0
	// means the default (5).
	Burst int
	// Disabled turns the limiter off. No 429s are ever produced.
	Disabled bool
}

// costBucket is one caller's token-bucket state.
type costBucket struct {
	tokens     float64
	lastRefill time.Time
	lastSeen   time.Time
}

// costRateLimiter is a token-bucket-per-identity limiter. Buckets
// refill continuously at perMinute/60 tokens per second up to burst.
// The clock is injectable (now) so tests are deterministic — never
// sleep-based.
type costRateLimiter struct {
	perMinute float64
	burst     float64
	now       func() time.Time

	mu      sync.Mutex
	buckets map[string]*costBucket
}

// newCostRateLimiter builds the limiter from config. Returns nil when
// cfg.Disabled — a nil limiter means "no enforcement" throughout.
func newCostRateLimiter(cfg CostRateLimit) *costRateLimiter {
	if cfg.Disabled {
		return nil
	}
	perMinute := cfg.PerMinute
	if perMinute <= 0 {
		perMinute = defaultCostPerMinute
	}
	burst := cfg.Burst
	if burst <= 0 {
		burst = defaultCostBurst
	}
	return &costRateLimiter{
		perMinute: float64(perMinute),
		burst:     float64(burst),
		now:       time.Now,
		buckets:   make(map[string]*costBucket),
	}
}

// Allow tries to consume one token from identity's bucket. Returns
// (true, 0) when the request may proceed, or (false, wait) where wait
// is how long until the bucket next holds a full token.
func (l *costRateLimiter) Allow(identity string) (bool, time.Duration) {
	if identity == "" {
		identity = anonymousRateIdentity
	}
	now := l.now()
	rate := l.perMinute / 60.0 // tokens per second

	l.mu.Lock()
	defer l.mu.Unlock()
	l.pruneLocked(now)

	b, ok := l.buckets[identity]
	if !ok {
		b = &costBucket{tokens: l.burst, lastRefill: now}
		l.buckets[identity] = b
	} else if elapsed := now.Sub(b.lastRefill).Seconds(); elapsed > 0 {
		// A stalled (or backwards) clock simply doesn't refill —
		// the elapsed > 0 guard keeps the bucket from going weird.
		b.tokens = math.Min(l.burst, b.tokens+elapsed*rate)
		b.lastRefill = now
	}
	b.lastSeen = now

	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	waitSec := (1 - b.tokens) / rate
	return false, time.Duration(waitSec * float64(time.Second))
}

// pruneLocked lazily sweeps buckets idle longer than
// costLimiterIdleAfter, but only once the map has grown past
// costLimiterPruneThreshold — small deployments never pay the sweep.
// Caller must hold l.mu.
func (l *costRateLimiter) pruneLocked(now time.Time) {
	if len(l.buckets) <= costLimiterPruneThreshold {
		return
	}
	cutoff := now.Add(-costLimiterIdleAfter)
	for id, b := range l.buckets {
		if b.lastSeen.Before(cutoff) {
			delete(l.buckets, id)
		}
	}
}

// allowCost runs the cost limiter for the request's middleware-
// resolved Caller (never raw headers or IPs — see #385 for why only
// the server's own verdict is trustworthy). Returns true when the
// request may proceed; otherwise writes the 429 response — with a
// Retry-After header and a JSON body — and returns false.
func (h *handlers) allowCost(w http.ResponseWriter, r *http.Request) bool {
	if h.costLimit == nil {
		return true
	}
	c, _ := auth.CallerFromContext(r.Context())
	ok, wait := h.costLimit.Allow(c.Identity)
	if ok {
		return true
	}
	secs := int(math.Ceil(wait.Seconds()))
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	writeJSON(w, http.StatusTooManyRequests, map[string]any{
		"error":               "rate limited",
		"retry_after_seconds": secs,
	})
	return false
}

// The session-scoped cost-limited endpoints register through
// handlers.routeSessionLimited (handlers.go), which runs allowCost
// BEFORE entry lookup — a Lookup miss lazily resumes the session,
// and that construction cost is precisely what the limiter bounds
// (#484). POST /sessions calls allowCost directly in register.
