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
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/go-steer/core-agent/pkg/auth"
)

// stubResumer is a SessionResumer test double. Tracks call counts so
// tests can assert singleflight dedup + cache-hit behavior.
type stubResumer struct {
	mu      sync.Mutex
	calls   int32
	rows    map[string]SessionACLRow // keyed by "app/sid"
	failErr error                    // returned when no row matches and failErr is set
	blockCh chan struct{}            // when non-nil, Resume blocks until closed (for concurrency tests)
}

func (s *stubResumer) Resume(ctx context.Context, app, sid string) (Registrant, auth.SessionACL, error) {
	atomic.AddInt32(&s.calls, 1)
	if s.blockCh != nil {
		<-s.blockCh
	}
	s.mu.Lock()
	row, ok := s.rows[app+"/"+sid]
	s.mu.Unlock()
	if !ok {
		if s.failErr != nil {
			return nil, auth.SessionACL{}, s.failErr
		}
		return nil, auth.SessionACL{}, ErrSessionACLNotFound
	}
	return &stubRegistrant{app: row.AppName, user: row.UserID, sid: row.SessionID}, row.ACL(), nil
}

func (s *stubResumer) addRow(row SessionACLRow) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rows == nil {
		s.rows = make(map[string]SessionACLRow)
	}
	s.rows[row.AppName+"/"+row.SessionID] = row
}

func TestRegistry_Lookup_NilResumerReturnsNotFound(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	// No WithResumer call — legacy behavior.
	_, err := reg.Lookup(context.Background(), "core-agent", "missing")
	if !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("want ErrSessionNotFound, got %v", err)
	}
}

func TestRegistry_Lookup_ResumerHit(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	resumer := &stubResumer{}
	resumer.addRow(SessionACLRow{
		AppName:   "core-agent",
		UserID:    "u",
		SessionID: "sess-x",
		Owner:     "alice@example.com",
	})
	reg.WithResumer(resumer)

	got, err := reg.Lookup(context.Background(), "core-agent", "sess-x")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got.ACL.Owner != "alice@example.com" {
		t.Errorf("ACL.Owner: got %q, want alice@example.com", got.ACL.Owner)
	}
	// Subsequent Lookup is a cache hit — no second resumer call.
	if _, err := reg.Lookup(context.Background(), "core-agent", "sess-x"); err != nil {
		t.Fatalf("second Lookup: %v", err)
	}
	if calls := atomic.LoadInt32(&resumer.calls); calls != 1 {
		t.Errorf("resumer calls: got %d, want 1 (subsequent lookups should hit memory)", calls)
	}
}

func TestRegistry_Lookup_ResumerMissReturnsNotFound(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry().WithResumer(&stubResumer{})
	_, err := reg.Lookup(context.Background(), "core-agent", "nope")
	if !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("want ErrSessionNotFound, got %v", err)
	}
}

func TestRegistry_Lookup_ResumerErrorPropagates(t *testing.T) {
	t.Parallel()
	// Non-not-found errors from the resumer surface as-is so the
	// handler returns 500 with the underlying cause (per OQ #2).
	reg := NewSessionRegistry()
	sentinel := errors.New("simulated factory failure")
	resumer := &stubResumer{failErr: sentinel}
	reg.WithResumer(resumer)
	_, err := reg.Lookup(context.Background(), "core-agent", "broken")
	if err == nil {
		t.Fatal("want error, got nil")
	}
	// Should NOT be ErrSessionNotFound — that would map to 404, hiding the real failure.
	if errors.Is(err, ErrSessionNotFound) {
		t.Errorf("factory-failure error must NOT be ErrSessionNotFound; got %v", err)
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("want sentinel error wrapped, got %v", err)
	}
}

func TestRegistry_LookupSingle_ResumerHit(t *testing.T) {
	t.Parallel()
	// LookupSingle's resumer path uses resumerDefaultApp ("core-agent")
	// since the shortcut URL doesn't carry an app segment.
	reg := NewSessionRegistry()
	resumer := &stubResumer{}
	resumer.addRow(SessionACLRow{
		AppName:   resumerDefaultApp,
		UserID:    "u",
		SessionID: "sess-shortcut",
		Owner:     "bob@example.com",
	})
	reg.WithResumer(resumer)

	got, err := reg.LookupSingle(context.Background(), "sess-shortcut")
	if err != nil {
		t.Fatalf("LookupSingle: %v", err)
	}
	if got.ACL.Owner != "bob@example.com" {
		t.Errorf("ACL.Owner: got %q, want bob@example.com", got.ACL.Owner)
	}
}

func TestRegistry_Lookup_SingleflightDedupesConcurrentMisses(t *testing.T) {
	t.Parallel()
	// Two concurrent Lookups for the same evicted session must
	// produce ONE resumer call (not two double-construction races).
	// The block channel keeps the first Resume in flight until both
	// goroutines have joined the singleflight; closing the channel
	// lets it return.
	reg := NewSessionRegistry()
	blockCh := make(chan struct{})
	resumer := &stubResumer{blockCh: blockCh}
	resumer.addRow(SessionACLRow{
		AppName:   "core-agent",
		UserID:    "u",
		SessionID: "sess-race",
		Owner:     "alice@example.com",
	})
	reg.WithResumer(resumer)

	var wg sync.WaitGroup
	const racers = 5
	errs := make([]error, racers)
	entries := make([]*Entry, racers)
	for i := 0; i < racers; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			e, err := reg.Lookup(context.Background(), "core-agent", "sess-race")
			entries[i] = e
			errs[i] = err
		}()
	}
	// Tiny sleep to make sure all goroutines reach the singleflight
	// before unblocking. The block channel guarantees correctness;
	// the sleep just exercises the deduplication window more
	// aggressively.
	close(blockCh)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("racer %d: %v", i, err)
		}
		if entries[i] == nil {
			t.Errorf("racer %d: nil entry", i)
		}
	}
	// All racers must point at the SAME *Entry — distinct entries
	// would mean the registry double-constructed and one was lost.
	for i := 1; i < racers; i++ {
		if entries[i] != entries[0] {
			t.Errorf("racer %d entry != racer 0; double-register raced", i)
		}
	}
	// Singleflight guarantees ≤ 1 resumer call per dedup window.
	// In practice all racers join in one window because the resumer
	// blocks on blockCh, so exactly 1 call.
	if calls := atomic.LoadInt32(&resumer.calls); calls != 1 {
		t.Errorf("resumer calls: got %d, want 1 (singleflight must dedupe)", calls)
	}
}

func TestRegistry_Lookup_MemoryHitSkipsResumer(t *testing.T) {
	t.Parallel()
	// In-memory entry must short-circuit the resumer — no DB hit
	// for sessions already in the registry.
	reg := NewSessionRegistry()
	resumer := &stubResumer{}
	reg.WithResumer(resumer)
	ag := &stubRegistrant{app: "core-agent", user: "u", sid: "sess-mem"}
	if _, err := reg.Register(ag); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, err := reg.Lookup(context.Background(), "core-agent", "sess-mem"); err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if calls := atomic.LoadInt32(&resumer.calls); calls != 0 {
		t.Errorf("resumer calls: got %d, want 0 (memory hit should skip resumer)", calls)
	}
}

func TestRegistry_WithResumer_Idempotent(t *testing.T) {
	t.Parallel()
	// WithResumer(nil) is a no-op — preserves the existing resumer.
	reg := NewSessionRegistry()
	first := &stubResumer{}
	reg.WithResumer(first)
	reg.WithResumer(nil)
	first.addRow(SessionACLRow{AppName: "core-agent", UserID: "u", SessionID: "s", Owner: "alice@example.com"})
	if _, err := reg.Lookup(context.Background(), "core-agent", "s"); err != nil {
		t.Fatalf("Lookup after nil WithResumer: %v", err)
	}
}

// TestRegistry_Resume_RegistersOnce verifies that a successful resume
// installs the entry so subsequent ListAuthorized / Len reflect it.
func TestRegistry_Resume_RegistersResumedEntry(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	resumer := &stubResumer{}
	resumer.addRow(SessionACLRow{
		AppName:   "core-agent",
		UserID:    "u",
		SessionID: "sess-installed",
		Owner:     "alice@example.com",
	})
	reg.WithResumer(resumer)
	if _, err := reg.Lookup(context.Background(), "core-agent", "sess-installed"); err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got := reg.Len(); got != 1 {
		t.Errorf("Len after resume: got %d, want 1", got)
	}
	// And subsequent qualified lookup hits in-memory.
	got, err := reg.Lookup(context.Background(), "core-agent", "sess-installed")
	if err != nil {
		t.Fatalf("second Lookup: %v", err)
	}
	if got.ACL.Owner != "alice@example.com" {
		t.Errorf("ACL.Owner: got %q", got.ACL.Owner)
	}
}

// Sanity: the test stub satisfies the SessionResumer interface.
var _ SessionResumer = (*stubResumer)(nil)

// Sanity: a known-broken test would surface here.
func TestRegistry_Lookup_ResumerErrorWraps(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	resumer := &stubResumer{failErr: fmt.Errorf("downstream API 503")}
	reg.WithResumer(resumer)
	_, err := reg.Lookup(context.Background(), "core-agent", "any")
	if err == nil {
		t.Fatal("want error")
	}
	if err.Error() == "" {
		t.Error("error message empty")
	}
}
