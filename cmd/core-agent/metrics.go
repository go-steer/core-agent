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
// daemon's sessions. Constructed before agent.New completes so the
// metrics observer can be registered against a live MeterProvider up
// front; the primary session's identity fields are stamped in via
// SetIdentity once agent.New returns (typically from the
// agent.WithPostConstruct hook), and attach-mode sessions join via
// SetRegistry once the SessionRegistry exists.
type primaryTrackerProvider struct {
	tracker *usage.Tracker

	mu        sync.RWMutex
	agent     *agent.Agent // stamped by SetIdentity; feeds Agents()
	sessionID string
	appName   string
	userID    string
	// registry contributes attach-created sessions (multi-session
	// daemons). The PRIMARY session is registered there too, so
	// Trackers dedups by tracker pointer — the registry entry wins
	// because its identity triple is complete from registration
	// time, whereas the local fields may still be empty in the
	// pre-SetIdentity window.
	registry usage.TrackerProvider
	// agents enumerates registry-backed agents for the
	// agent.AgentSource side (SetAgentRegistry).
	agents registryAgentsFn
}

// SetRegistry installs the attach-registry adapter
// (compose.RegistryTrackerProvider). Safe to call once the registry
// exists; nil is ignored.
func (p *primaryTrackerProvider) SetRegistry(tp usage.TrackerProvider) {
	if tp == nil {
		return
	}
	p.mu.Lock()
	p.registry = tp
	p.mu.Unlock()
}

// SetIdentity stamps the session-identity fields once the agent
// exists. Safe to call from agent.WithPostConstruct. Callable more
// than once; last write wins.
func (p *primaryTrackerProvider) SetIdentity(a *agent.Agent) {
	if a == nil {
		return
	}
	p.mu.Lock()
	p.agent = a
	p.sessionID = a.SessionID()
	p.appName = a.AppName()
	p.userID = a.UserID()
	p.mu.Unlock()
}

// registryAgents is the attach-registry half of Agents(); stamped by
// SetAgentRegistry alongside SetRegistry.
type registryAgentsFn func() []*agent.Agent

// SetAgentRegistry installs the registry-backed agent enumerator
// (compose.RegistryAgents closure) so agent.RegisterMetrics sees
// attach-created sessions too.
func (p *primaryTrackerProvider) SetAgentRegistry(f registryAgentsFn) {
	if f == nil {
		return
	}
	p.mu.Lock()
	p.agents = f
	p.mu.Unlock()
}

// Agents implements agent.AgentSource: the primary agent plus every
// registry-backed agent, deduped by pointer (the primary registers in
// both places).
func (p *primaryTrackerProvider) Agents() []*agent.Agent {
	p.mu.RLock()
	primary := p.agent
	agents := p.agents
	p.mu.RUnlock()

	var out []*agent.Agent
	seen := map[*agent.Agent]bool{}
	seenSIDs := map[string]bool{}
	if agents != nil {
		for _, a := range agents() {
			if a == nil || seen[a] {
				continue
			}
			seen[a] = true
			seenSIDs[a.SessionID()] = true
			out = append(out, a)
		}
	}
	// Session-id dedup mirrors Trackers(): after evict + lazy resume
	// the registry holds a fresh agent for the primary session; the
	// stale p.agent must not shadow its gauges (inbox_pending frozen
	// at the dead agent's value would be actively misleading).
	if primary != nil && !seen[primary] && !seenSIDs[primary.SessionID()] {
		out = append(out, primary)
	}
	return out
}

// Trackers implements usage.TrackerProvider. Merges the primary
// session with the attach registry's sessions, deduplicating by
// *usage.Tracker pointer: the primary agent is ALSO registered in the
// registry, and double-listing its tracker would double-count every
// series in aggregated (labels-off) mode. Registry entries win the
// dedup (complete identity triple). Session-identity may be empty on
// early calls before SetIdentity fires; the tracker has no turns
// recorded in that window so the observer emits nothing
// consequential.
func (p *primaryTrackerProvider) Trackers() []usage.TrackedSession {
	p.mu.RLock()
	primary := usage.TrackedSession{
		Tracker:   p.tracker,
		SessionID: p.sessionID,
		AppName:   p.appName,
		UserID:    p.userID,
	}
	registry := p.registry
	p.mu.RUnlock()

	var out []usage.TrackedSession
	seen := map[*usage.Tracker]bool{}
	seenSIDs := map[string]bool{}
	if registry != nil {
		for _, ts := range registry.Trackers() {
			if ts.Tracker == nil || seen[ts.Tracker] {
				continue
			}
			seen[ts.Tracker] = true
			seenSIDs[ts.SessionID] = true
			out = append(out, ts)
		}
	}
	// Skip the primary on session-id match too, not just tracker
	// pointer: after an idle-evict + lazy resume the registry holds a
	// FRESH tracker for the primary session while p.tracker still
	// points at the dead incarnation — emitting both would let the
	// stale snapshot shadow the live one (identical attrs, last
	// observation wins).
	if primary.Tracker != nil && !seen[primary.Tracker] &&
		(primary.SessionID == "" || !seenSIDs[primary.SessionID]) {
		out = append(out, primary)
	}
	return out
}
