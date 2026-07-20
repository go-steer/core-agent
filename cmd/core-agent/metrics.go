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

// Metrics wiring for the daemon entrypoint. Kept in its own file so
// main.go doesn't grow another 50-line block of telemetry setup.
// The design doc is docs/metrics-design.md.

import (
	"sync"

	"github.com/go-steer/core-agent/v2/pkg/agent"
	"github.com/go-steer/core-agent/v2/pkg/usage"
)

// metricsOpts bundles the --metrics-* CLI flags. Kept separate from
// attachOpts even though both flags eventually shape HTTP surfaces —
// metrics can be enabled without attach (via OTLP push, or via
// standalone Prometheus scrape), so the two aren't coupled.
type metricsOpts struct {
	// Addr overrides cfg.OTEL.Metrics.PrometheusAddr when non-empty.
	// Ignored when the metrics exporter isn't in prometheus / both
	// mode.
	Addr string
}

// primaryTrackerProvider is a lazy usage.TrackerProvider for the
// daemon's primary session. Constructed before agent.New completes
// so the metrics observer can be registered against a live
// MeterProvider up front; the session-identity fields are stamped
// in via SetIdentity once agent.New returns (typically from the
// agent.WithPostConstruct hook).
//
// Multi-session daemons (per-request sessions spawned via
// buildSessionFactory) are NOT covered by this provider — each
// on-demand session has its own tracker and would need registry-based
// iteration. That's a follow-up slice; the primary-session case
// covers the common single-daemon deployment.
type primaryTrackerProvider struct {
	tracker *usage.Tracker

	mu        sync.RWMutex
	sessionID string
	appName   string
	userID    string
}

// SetIdentity stamps the session-identity fields once the agent
// exists. Safe to call from agent.WithPostConstruct. Callable more
// than once; last write wins.
func (p *primaryTrackerProvider) SetIdentity(a *agent.Agent) {
	if a == nil {
		return
	}
	p.mu.Lock()
	p.sessionID = a.SessionID()
	p.appName = a.AppName()
	p.userID = a.UserID()
	p.mu.Unlock()
}

// Trackers implements usage.TrackerProvider. Returns an empty slice
// when the tracker is nil (metrics disabled path) or a single-element
// slice with the current identity snapshot. Session-identity may be
// empty on early calls before SetIdentity fires; the tracker has no
// turns recorded in that window so the observer emits nothing
// consequential.
func (p *primaryTrackerProvider) Trackers() []usage.TrackedSession {
	if p.tracker == nil {
		return nil
	}
	p.mu.RLock()
	ts := usage.TrackedSession{
		Tracker:   p.tracker,
		SessionID: p.sessionID,
		AppName:   p.appName,
		UserID:    p.userID,
	}
	p.mu.RUnlock()
	return []usage.TrackedSession{ts}
}
