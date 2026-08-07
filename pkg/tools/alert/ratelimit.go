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

package alert

import (
	"fmt"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/go-steer/core-agent/v2/pkg/config"
)

// rateLimiter is a per-target token bucket guarding against pathological
// alert loops (an agent stuck re-escalating). It is NOT an operational
// cadence control — distinct targets have independent buckets, so the
// agent can fire slack-oncall immediately after pagerduty-critical.
//
// In-memory only (design OQ7): a daemon restart resets every bucket.
// Buckets are created lazily on first use so an unfired target costs
// nothing.
type rateLimiter struct {
	enabled bool
	limit   rate.Limit
	burst   int
	now     func() time.Time

	mu      sync.Mutex
	buckets map[string]*rate.Limiter
}

// newRateLimiter builds a limiter from the alerts.rate_limit_per_target
// spec. An empty spec yields an always-allow limiter (no limit). now may
// be nil (defaults to time.Now); tests inject a fake clock so rate-limit
// behavior is deterministic. The spec is normally pre-validated by
// config.validateAlerts, so a parse error here is a defense-in-depth
// backstop for hand-constructed configs.
func newRateLimiter(spec string, now func() time.Time) (*rateLimiter, error) {
	if now == nil {
		now = time.Now
	}
	rl := &rateLimiter{now: now, buckets: make(map[string]*rate.Limiter)}
	if spec == "" {
		return rl, nil // no limit
	}
	count, window, err := config.ParseAlertRateLimit(spec)
	if err != nil {
		return nil, fmt.Errorf("alert: rate_limit_per_target: %w", err)
	}
	rl.enabled = true
	// "count per window" → refill rate count/window, burst=count so a
	// fresh bucket can fire the full allowance before throttling.
	rl.limit = rate.Limit(float64(count) / window.Seconds())
	rl.burst = count
	return rl, nil
}

// allow reports whether target may fire now, consuming one token if so.
func (rl *rateLimiter) allow(target string) bool {
	if !rl.enabled {
		return true
	}
	rl.mu.Lock()
	lim, ok := rl.buckets[target]
	if !ok {
		lim = rate.NewLimiter(rl.limit, rl.burst)
		rl.buckets[target] = lim
	}
	rl.mu.Unlock()
	// AllowN takes an explicit "now" so an injected clock makes tests
	// deterministic; production passes the real wall clock.
	return lim.AllowN(rl.now(), 1)
}
