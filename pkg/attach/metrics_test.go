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
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// poolWithBroadcaster hand-assembles a pool + one broadcaster with n
// live subscribers, bypassing the eventlog plumbing the pump needs —
// these tests exercise counting, not delivery.
func poolWithBroadcaster(entry *Entry, n int) (*broadcasterPool, *broadcaster) {
	p := &broadcasterPool{bcasts: map[tripleKey]*broadcaster{}}
	b := &broadcaster{
		entry:   entry,
		subs:    map[*subscriber]struct{}{},
		closing: make(chan struct{}),
		drops:   &p.drops,
	}
	for range n {
		b.subs[&subscriber{ch: make(chan Frame, 1)}] = struct{}{}
	}
	p.bcasts[tripleKey{App: entry.AppName, User: entry.UserID, SID: entry.SessionID}] = b
	return p, b
}

// TestBroadcaster_DropIncrementsPoolCounter pins both drop sites: a
// subscriber whose buffer is full gets detached AND counted on the
// pool-lifetime counter.
func TestBroadcaster_DropIncrementsPoolCounter(t *testing.T) {
	t.Parallel()
	entry := &Entry{AppName: "app", UserID: "u", SessionID: "s1"}
	p, b := poolWithBroadcaster(entry, 0)

	full := &subscriber{ch: make(chan Frame)} // unbuffered → always full
	b.subs[full] = struct{}{}
	b.mu.Lock()
	if ok := b.send(full, Frame{Seq: 1}); ok {
		t.Fatal("send should have dropped the full subscriber")
	}
	b.mu.Unlock()
	if got := p.drops.Load(); got != 1 {
		t.Fatalf("drops after send = %d, want 1", got)
	}

	typedFull := &subscriber{ch: make(chan Frame)}
	b.subs[typedFull] = struct{}{}
	b.mu.Lock()
	if ok := b.sendTyped(typedFull, Frame{Type: "status-update"}); ok {
		t.Fatal("sendTyped should have dropped the full subscriber")
	}
	b.mu.Unlock()
	if got := p.drops.Load(); got != 2 {
		t.Fatalf("drops after sendTyped = %d, want 2", got)
	}
}

// TestAttachRegisterMetrics_Observes pins the observer shape over a
// hand-assembled server: sessions gauge from the registry,
// per-session subscriber gauge, cumulative drops, and NO peers series
// when the server has no PeerRegistry.
func TestAttachRegisterMetrics_Observes(t *testing.T) {
	t.Parallel()
	entry := &Entry{AppName: "app", UserID: "u", SessionID: "s1"}
	p, _ := poolWithBroadcaster(entry, 2)
	p.drops.Add(3)
	srv := &Server{opts: Options{Registry: NewSessionRegistry()}, pool: p}

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	if _, err := RegisterMetrics(mp, srv); err != nil {
		t.Fatalf("RegisterMetrics: %v", err)
	}
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	byName := map[string]metricdata.Metrics{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			byName[m.Name] = m
		}
	}

	sessions := byName[MetricAttachSessionsActive].Data.(metricdata.Gauge[int64])
	if len(sessions.DataPoints) != 1 || sessions.DataPoints[0].Value != 0 {
		t.Errorf("sessions.active = %+v, want single 0 point (empty registry)", sessions.DataPoints)
	}

	subs := byName[MetricAttachSubscribers].Data.(metricdata.Gauge[int64])
	if len(subs.DataPoints) != 1 || subs.DataPoints[0].Value != 2 {
		t.Fatalf("subscribers = %+v, want single point of 2", subs.DataPoints)
	}
	if sid, ok := subs.DataPoints[0].Attributes.Value(attribute.Key(AttrMetricSessionID)); !ok || sid.AsString() != "s1" {
		t.Errorf("subscribers session.id = %v, want s1", sid)
	}

	drops := byName[MetricAttachSubscriberDrop].Data.(metricdata.Sum[int64])
	if len(drops.DataPoints) != 1 || drops.DataPoints[0].Value != 3 {
		t.Errorf("drops = %+v, want single point of 3", drops.DataPoints)
	}

	if _, present := byName[MetricAttachPeersActive]; present {
		t.Error("peers.active present without a PeerRegistry")
	}
}
