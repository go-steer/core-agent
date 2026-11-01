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

// Originally derived from go-steer/mast.

package eventlog

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
)

func TestAckStore_RoundTripAndUpdateAcrossReopen(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "acks.db")

	h, err := Open(ctx, sqlite.Open(dbPath))
	if err != nil {
		t.Fatalf("Open(1): %v", err)
	}

	t0 := time.Date(2026, 4, 15, 10, 0, 0, 0, time.UTC)
	rec := AckRecord{
		Workload:  "cluster-watch",
		Subject:   "ConditionFalse/Deployment/payments/api/Available",
		By:        "alice@example.com",
		ProxyBy:   "sa:switchboard",
		Reason:    "investigating pod rollout",
		At:        t0,
		Forwarded: false,
	}
	if err := h.RecordAck(ctx, "app", rec); err != nil {
		t.Fatalf("RecordAck(1): %v", err)
	}

	// Update Forwarded=true on the same (workload, subject).
	rec.Forwarded = true
	rec.At = t0.Add(5 * time.Second)
	if err := h.RecordAck(ctx, "app", rec); err != nil {
		t.Fatalf("RecordAck(2): %v", err)
	}

	if err := h.Close(); err != nil {
		t.Fatalf("Close(1): %v", err)
	}

	// Re-open the SQLite DB to prove durability across process restarts.
	h2, err := Open(ctx, sqlite.Open(dbPath))
	if err != nil {
		t.Fatalf("Open(2): %v", err)
	}
	defer func() { _ = h2.Close() }()

	got, err := h2.Ack(ctx, "app", rec.Workload, rec.Subject)
	if err != nil {
		t.Fatalf("Ack(): %v", err)
	}
	if got == nil {
		t.Fatalf("Ack() = nil, want record")
	}
	if got.By != "alice@example.com" || got.ProxyBy != "sa:switchboard" || !got.Forwarded || !got.At.Equal(rec.At) {
		t.Errorf("Ack() = %+v, want %+v", *got, rec)
	}

	miss, err := h2.Ack(ctx, "app", rec.Workload, "nonexistent")
	if err != nil || miss != nil {
		t.Errorf("Ack(nonexistent) = (%+v, %v), want (nil, nil)", miss, err)
	}
}

func TestAckStore_KeyCollisionResistanceAndZeroDecisionRecords(t *testing.T) {
	ctx := context.Background()
	h, cleanup := openTestHandle(t)
	defer cleanup()

	// ("a:b", "c") vs ("a", "b:c") must not collide under ackKey.
	r1 := AckRecord{Workload: "a:b", Subject: "c", By: "u1", At: time.Unix(100, 0).UTC()}
	r2 := AckRecord{Workload: "a", Subject: "b:c", By: "u2", At: time.Unix(200, 0).UTC()}
	if err := h.RecordAck(ctx, "app", r1); err != nil {
		t.Fatalf("RecordAck(r1): %v", err)
	}
	if err := h.RecordAck(ctx, "app", r2); err != nil {
		t.Fatalf("RecordAck(r2): %v", err)
	}

	g1, err := h.Ack(ctx, "app", "a:b", "c")
	if err != nil || g1 == nil || g1.By != "u1" {
		t.Fatalf("Ack(a:b, c) = (%+v, %v), want By=u1", g1, err)
	}
	g2, err := h.Ack(ctx, "app", "a", "b:c")
	if err != nil || g2 == nil || g2.By != "u2" {
		t.Fatalf("Ack(a, b:c) = (%+v, %v), want By=u2", g2, err)
	}

	list, err := h.Acks(ctx, "app", "")
	if err != nil || len(list) != 2 {
		t.Fatalf("Acks(all) = (%+v, %v), want 2 records", list, err)
	}

	// Recording acks must NEVER write a DecisionRecord row.
	decisions, err := h.Decisions.ListDecisions(ctx, ackOpsSessionID)
	if err != nil {
		t.Fatalf("Decisions.ListDecisions(): %v", err)
	}
	if len(decisions) != 0 {
		t.Errorf("Decisions.ListDecisions() after RecordAck = %+v, want 0 decision records", decisions)
	}
}
