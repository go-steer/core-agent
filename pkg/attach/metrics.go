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
	"context"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Metric export for the attach subsystem (#338 Phase 3). Observables
// over registry / broadcaster-pool state; the broadcaster and pool
// stay unexported (#390 API shrink) — the two Server accessors below
// are the only new public surface.

const (
	MetricAttachSessionsActive = "core_agent.attach.sessions.active"
	MetricAttachSubscribers    = "core_agent.attach.subscribers"
	MetricAttachSubscriberDrop = "core_agent.attach.subscriber_drops"
	MetricAttachPeersActive    = "core_agent.attach.peers.active"

	// AttrMetricSessionID mirrors pkg/usage's session.id key (kept
	// local; attach must not import usage).
	AttrMetricSessionID = "session.id"
)

// SubscriberStat is one live-broadcaster snapshot for
// Server.SubscriberStats.
type SubscriberStat struct {
	AppName   string
	UserID    string
	SessionID string
	// Subscribers is the current SSE subscriber count on this
	// session's broadcaster.
	Subscribers int
}

// SubscriberStats snapshots the per-session SSE subscriber counts.
// Sessions without a live broadcaster (nobody attached since the last
// detach — broadcasters tear down with their last subscriber) don't
// appear.
func (s *Server) SubscriberStats() []SubscriberStat {
	if s == nil || s.pool == nil {
		return nil
	}
	return s.pool.subscriberStats()
}

// SubscriberDrops reports the cumulative count of SSE subscribers
// dropped for falling behind (channel buffer full) across the
// server's lifetime.
func (s *Server) SubscriberDrops() int64 {
	if s == nil || s.pool == nil {
		return 0
	}
	return s.pool.drops.Load()
}

func (p *broadcasterPool) subscriberStats() []SubscriberStat {
	p.mu.Lock()
	bs := make([]*broadcaster, 0, len(p.bcasts))
	for _, b := range p.bcasts {
		bs = append(bs, b)
	}
	p.mu.Unlock()

	// Per-broadcaster locks taken AFTER the pool lock is released —
	// no path holds b.mu and then takes p.mu, but keeping the scopes
	// disjoint means never having to prove it.
	out := make([]SubscriberStat, 0, len(bs))
	for _, b := range bs {
		b.mu.Lock()
		n := len(b.subs)
		b.mu.Unlock()
		if n == 0 {
			continue
		}
		out = append(out, SubscriberStat{
			AppName:     b.entry.AppName,
			UserID:      b.entry.UserID,
			SessionID:   b.entry.SessionID,
			Subscribers: n,
		})
	}
	return out
}

// RegisterMetrics wires the attach observers against mp. Call once
// after NewServer with the process-global MeterProvider. The peer
// gauge is skipped entirely when the server has no PeerRegistry
// (non-hub daemons).
func RegisterMetrics(mp metric.MeterProvider, srv *Server) (metric.Registration, error) {
	if mp == nil {
		return nil, fmt.Errorf("attach.RegisterMetrics: nil MeterProvider")
	}
	if srv == nil {
		return nil, fmt.Errorf("attach.RegisterMetrics: nil Server")
	}
	meter := mp.Meter("github.com/go-steer/core-agent/v2/pkg/attach")

	sessions, err := meter.Int64ObservableGauge(
		MetricAttachSessionsActive,
		metric.WithDescription("Sessions currently registered on the attach listener."),
		metric.WithUnit("{session}"),
	)
	if err != nil {
		return nil, fmt.Errorf("attach: sessions instrument: %w", err)
	}
	subscribers, err := meter.Int64ObservableGauge(
		MetricAttachSubscribers,
		metric.WithDescription("Live SSE subscribers per session."),
		metric.WithUnit("{subscriber}"),
	)
	if err != nil {
		return nil, fmt.Errorf("attach: subscribers instrument: %w", err)
	}
	drops, err := meter.Int64ObservableCounter(
		MetricAttachSubscriberDrop,
		metric.WithDescription("SSE subscribers dropped for falling behind (channel buffer full)."),
		metric.WithUnit("{drop}"),
	)
	if err != nil {
		return nil, fmt.Errorf("attach: drops instrument: %w", err)
	}
	peers, err := meter.Int64ObservableGauge(
		MetricAttachPeersActive,
		metric.WithDescription("Peers currently registered on this hub."),
		metric.WithUnit("{peer}"),
	)
	if err != nil {
		return nil, fmt.Errorf("attach: peers instrument: %w", err)
	}

	callback := func(_ context.Context, o metric.Observer) error {
		if reg := srv.opts.Registry; reg != nil {
			o.ObserveInt64(sessions, int64(reg.Len()))
		}
		for _, st := range srv.SubscriberStats() {
			o.ObserveInt64(subscribers, int64(st.Subscribers),
				metric.WithAttributes(attribute.String(AttrMetricSessionID, st.SessionID)))
		}
		o.ObserveInt64(drops, srv.SubscriberDrops())
		if pr := srv.opts.PeerRegistry; pr != nil {
			o.ObserveInt64(peers, int64(pr.Len()))
		}
		return nil
	}
	return meter.RegisterCallback(callback, sessions, subscribers, drops, peers)
}
