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
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"google.golang.org/adk/v2/session"

	"github.com/go-steer/core-agent/v3/pkg/agent"
	"github.com/go-steer/core-agent/v3/pkg/attach"
	"github.com/go-steer/core-agent/v3/pkg/attachadapter"
	"github.com/go-steer/core-agent/v3/pkg/eventlog"
	"github.com/go-steer/core-agent/v3/pkg/statestore/memory"
)

// A tenant whose residency lease is lost is evicted from the registry —
// wake loop cancelled, lease released — and an evicted tenant releases
// its lease so another instance can take the tree at once.
func TestWatchResidency_LossEvictsTenantAndEvictionReleases(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "control.db")
	h, err := eventlog.OpenTranscript(ctx, memory.New(), sqlite.Open(dbPath),
		eventlog.WithResidencyLease(), eventlog.WithLockHeartbeat(10*time.Millisecond, time.Second))
	if err != nil {
		t.Fatalf("OpenTranscript: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })

	newTenant := func(sid string) (*agent.Agent, *atomic.Bool, *attach.SessionRegistry) {
		ag, err := agent.New(stubLLM{}, agent.WithEventLog(h), agent.WithSession("alice", sid))
		if err != nil {
			t.Fatalf("agent.New: %v", err)
		}
		reg := attach.NewSessionRegistry()
		release, err := holdResidency(SessionFactoryDeps{EventlogHandle: h, Registry: reg}, ag)
		if err != nil {
			t.Fatalf("holdResidency: %v", err)
		}
		if !h.HoldsResidency(ag.AppName(), "alice", sid) {
			t.Fatalf("holdResidency did not make %s resident", sid)
		}
		var cancelled atomic.Bool
		evict := evictFunc(func() { cancelled.Store(true) }, nil, release)
		if _, err := reg.RegisterOwnedWithCancel(attachadapter.New(ag), "alice", evict); err != nil {
			t.Fatalf("register: %v", err)
		}
		return ag, &cancelled, reg
	}

	// Loss → eviction.
	ag, cancelled, reg := newTenant("t-lost")
	if err := h.DB.Exec(`UPDATE agent_run_lock SET holder = 'successor' WHERE session_id = 'residency:t-lost'`).Error; err != nil {
		t.Fatalf("steal: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !cancelled.Load() {
		if time.Now().After(deadline) {
			t.Fatal("losing the lease did not evict the tenant")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := reg.Lookup(ctx, ag.AppName(), "t-lost"); err == nil {
		t.Fatal("evicted tenant is still registered")
	}

	// Ordinary eviction → release.
	ag2, cancelled2, reg2 := newTenant("t-idle")
	reg2.Unregister(ag2.AppName(), "alice", "t-idle")
	if !cancelled2.Load() {
		t.Fatal("eviction did not cancel the wake loop")
	}
	if h.HoldsResidency(ag2.AppName(), "alice", "t-idle") {
		t.Fatal("eviction did not release the residency lease")
	}
	// A write still in flight from the evicted tenant is refused, not
	// allowed to take the lease back.
	if _, err := h.Service.Create(ctx, &session.CreateRequest{AppName: ag2.AppName(), UserID: "alice", SessionID: "t-idle:sub:late:1"}); !errors.Is(err, eventlog.ErrNotResident) {
		t.Fatalf("late subagent Create after eviction = %v, want ErrNotResident", err)
	}
	if h.HoldsResidency(ag2.AppName(), "alice", "t-idle") {
		t.Fatal("a late write took the evicted tenant's lease back")
	}

	// A tenant whose session is resident elsewhere cannot be built here.
	other, err := eventlog.OpenTranscript(ctx, memory.New(), sqlite.Open(filepath.Join(filepath.Dir(dbPath), "control.db")), eventlog.WithResidencyLease())
	if err != nil {
		t.Fatalf("second instance: %v", err)
	}
	t.Cleanup(func() { _ = other.Close() })
	agB, err := agent.New(stubLLM{}, agent.WithEventLog(other), agent.WithSession("alice", "t-held"))
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	if _, err := holdResidency(SessionFactoryDeps{EventlogHandle: other}, agB); err != nil {
		t.Fatalf("first holder: %v", err)
	}
	agA, _ := agent.New(stubLLM{}, agent.WithEventLog(h), agent.WithSession("alice", "t-held"))
	if _, err := holdResidency(SessionFactoryDeps{EventlogHandle: h}, agA); !errors.Is(err, eventlog.ErrSessionLocked) {
		t.Fatalf("second holder = %v, want ErrSessionLocked", err)
	}
}

func TestEvictFunc_RunsEveryPart(t *testing.T) {
	t.Parallel()
	var loop, released atomic.Bool
	closed := make(chan struct{})
	evictFunc(func() { loop.Store(true) }, func() { close(closed) }, func() { released.Store(true) })()
	if !loop.Load() || !released.Load() {
		t.Fatalf("loop cancelled=%v, residency released=%v; want both", loop.Load(), released.Load())
	}
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("subagents were not torn down")
	}
	evictFunc(func() {}, nil, func() {})() // no subagents: no panic
}
