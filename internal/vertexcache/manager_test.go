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

package vertexcache

import (
	"context"
	"errors"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/genai"
)

// fakeCaches is a minimal in-memory CachesClient for exercising the
// state machine without a live Vertex client. Behavior knobs:
//
//   - createErr / updateErr / deleteErr: injected error returns.
//   - ttlOverride: force a specific ExpireTime on Create/Update
//     regardless of the config's TTL — lets tests fabricate a
//     near-expiry cache without waiting the real TTL.
//
// All fields protected by mu because Manager.Init/Refresh fire
// goroutines that hit the fake concurrently.
type fakeCaches struct {
	mu          sync.Mutex
	createErr   error
	updateErr   error
	deleteErr   error
	ttlOverride time.Duration

	createCount atomic.Int32
	updateCount atomic.Int32
	deleteCount atomic.Int32

	lastCreateModel   string
	lastCreateConfig  *genai.CreateCachedContentConfig
	lastUpdateName    string
	lastDeleteName    string
	nextCacheNameOnce string // if set, used for the next Create's returned Name
}

func (f *fakeCaches) Create(_ context.Context, model string, cfg *genai.CreateCachedContentConfig) (*genai.CachedContent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createCount.Add(1)
	f.lastCreateModel = model
	f.lastCreateConfig = cfg
	if f.createErr != nil {
		return nil, f.createErr
	}
	name := f.nextCacheNameOnce
	if name == "" {
		name = "projects/test/locations/us-central1/cachedContents/fake"
	}
	f.nextCacheNameOnce = ""
	expiry := time.Now().Add(cfg.TTL)
	if f.ttlOverride != 0 {
		expiry = time.Now().Add(f.ttlOverride)
	}
	return &genai.CachedContent{Name: name, ExpireTime: expiry}, nil
}

func (f *fakeCaches) Update(_ context.Context, name string, cfg *genai.UpdateCachedContentConfig) (*genai.CachedContent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updateCount.Add(1)
	f.lastUpdateName = name
	if f.updateErr != nil {
		return nil, f.updateErr
	}
	// Real Vertex honors the requested TTL on Update — the fake mirrors
	// that. ttlOverride is intentionally NOT applied here so refresh
	// tests can see the cache "return to health" after a successful
	// Update; applying ttlOverride to both Create and Update would
	// keep the cache stuck in near-expiry forever and mask real
	// serialization bugs.
	return &genai.CachedContent{Name: name, ExpireTime: time.Now().Add(cfg.TTL)}, nil
}

func (f *fakeCaches) Delete(_ context.Context, name string, _ *genai.DeleteCachedContentConfig) (*genai.DeleteCachedContentResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteCount.Add(1)
	f.lastDeleteName = name
	if f.deleteErr != nil {
		return nil, f.deleteErr
	}
	return &genai.DeleteCachedContentResponse{}, nil
}

// discardLogger swallows the operator-facing warning lines so the
// test output stays clean; tests that want to assert on log content
// can plug in their own log.Logger via Options.Logger.
func discardLogger() *log.Logger { return log.New(os.Stderr, "", 0) }

// waitFor polls fn until it returns true or the deadline elapses.
// Backoff is a tight 5ms loop — sufficient for the goroutine hops
// this file exercises without extending unit-test wall time.
func waitFor(t *testing.T, timeout time.Duration, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition never became true within %s", timeout)
}

// TestManager_InitHappyPath is the load-bearing state-machine test:
// Init fires the Create RPC on a goroutine, Name() returns "" until
// it lands, then the resolved cache name. Covers the async-init
// contract the design doc calls out as "non-blocking, turn 1 must
// not wait for cache creation."
func TestManager_InitHappyPath(t *testing.T) {
	t.Parallel()
	fake := &fakeCaches{nextCacheNameOnce: "projects/p/locations/l/cachedContents/abc"}
	m := NewManager(fake, "gemini-2.5-flash", Options{TTL: time.Hour})

	// Name before Init → "".
	if got := m.Name(context.Background()); got != "" {
		t.Errorf("Name before Init = %q, want empty", got)
	}

	m.Init(context.Background(), &genai.Content{Parts: []*genai.Part{{Text: "sys"}}}, nil)
	waitFor(t, time.Second, func() bool { return m.Snapshot().Active })

	// Post-init, Name resolves.
	if got := m.Name(context.Background()); got != "projects/p/locations/l/cachedContents/abc" {
		t.Errorf("Name after Init = %q, want the created cache name", got)
	}
	if fake.createCount.Load() != 1 {
		t.Errorf("Create called %d times, want 1", fake.createCount.Load())
	}
	if fake.lastCreateModel != "gemini-2.5-flash" {
		t.Errorf("Create model = %q, want gemini-2.5-flash", fake.lastCreateModel)
	}
	// System instruction from Init was threaded into the Create call.
	if fake.lastCreateConfig == nil ||
		fake.lastCreateConfig.SystemInstruction == nil ||
		len(fake.lastCreateConfig.SystemInstruction.Parts) == 0 ||
		fake.lastCreateConfig.SystemInstruction.Parts[0].Text != "sys" {
		t.Errorf("Create system instruction not threaded through: %+v", fake.lastCreateConfig)
	}
}

// TestManager_InitError degrades cleanly: Create returns an error,
// state goes to Failed, Name returns "" forever, and callers can
// still use the manager (no panics, no unbounded retries).
func TestManager_InitError(t *testing.T) {
	t.Parallel()
	fake := &fakeCaches{createErr: errors.New("boom")}
	m := NewManager(fake, "gemini-2.5-flash", Options{Logger: discardLogger()})

	m.Init(context.Background(), &genai.Content{Parts: []*genai.Part{{Text: "sys"}}}, nil)
	waitFor(t, time.Second, func() bool { return m.Snapshot().Failed })

	if got := m.Name(context.Background()); got != "" {
		t.Errorf("Name after failed Init = %q, want empty (degrade to uncached)", got)
	}
	// Design contract: Init is at-most-once; re-Init doesn't retry
	// after a hard failure (the operator log line is the signal).
	m.Init(context.Background(), &genai.Content{Parts: []*genai.Part{{Text: "sys"}}}, nil)
	// Give any spurious background goroutine a chance to run before
	// we assert.
	time.Sleep(20 * time.Millisecond)
	if fake.createCount.Load() != 1 {
		t.Errorf("Create called %d times after failed retry, want 1 (at-most-once)", fake.createCount.Load())
	}
}

// TestManager_InitAtMostOnce guards the design contract: concurrent
// Init calls only fire one Create RPC. Matters because
// builtinsLLM.GenerateContent may run from many goroutines and each
// may call Init on the first turn's ambient conditions.
func TestManager_InitAtMostOnce(t *testing.T) {
	t.Parallel()
	fake := &fakeCaches{}
	m := NewManager(fake, "gemini-2.5-flash", Options{})
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.Init(context.Background(), &genai.Content{Parts: []*genai.Part{{Text: "sys"}}}, nil)
		}()
	}
	wg.Wait()
	waitFor(t, time.Second, func() bool { return m.Snapshot().Active })
	if fake.createCount.Load() != 1 {
		t.Errorf("Create called %d times under concurrent Init, want 1", fake.createCount.Load())
	}
}

// TestManager_RefreshTriggersOnLowTTL exercises the ACTIVE → REFRESH
// transition: when Name() reads a cache whose remaining TTL is
// under the refresh threshold, a background Update fires. Post-
// refresh, subsequent Name() calls see the pushed-out expiry and
// do NOT re-refresh — the whole point of the refresh-threshold
// serialization is to keep the Vertex Update RPC rate at ~2×/24h.
func TestManager_RefreshTriggersOnLowTTL(t *testing.T) {
	t.Parallel()
	// ttlOverride on Create makes the initial cache look near-expiry
	// (500ms remaining). Update is unaffected: it honors cfg.TTL so
	// the refresh returns a healthy 1h expiry.
	fake := &fakeCaches{ttlOverride: 500 * time.Millisecond}
	m := NewManager(fake, "gemini-2.5-flash", Options{
		TTL:              time.Hour,
		RefreshThreshold: 30 * time.Minute, // 500ms remaining << 30min → refresh
	})
	m.Init(context.Background(), &genai.Content{Parts: []*genai.Part{{Text: "sys"}}}, nil)
	waitFor(t, time.Second, func() bool { return m.Snapshot().Active })

	// First Name call reads the cache + schedules a Refresh goroutine.
	_ = m.Name(context.Background())
	waitFor(t, time.Second, func() bool { return fake.updateCount.Load() >= 1 })

	// After Refresh completes, the cache has a full 1h remaining —
	// well outside the 30min refresh window. A burst of Name() calls
	// must NOT re-refresh.
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = m.Name(context.Background())
		}()
	}
	wg.Wait()
	if got := fake.updateCount.Load(); got != 1 {
		t.Errorf("update called %d times, want exactly 1 (post-refresh Name burst must not re-refresh)", got)
	}
}

// TestManager_DeleteHappyPath verifies the ACTIVE → DELETE transition:
// after Delete, Name returns "" forever and subsequent Init is a no-op.
func TestManager_DeleteHappyPath(t *testing.T) {
	t.Parallel()
	fake := &fakeCaches{}
	m := NewManager(fake, "gemini-2.5-flash", Options{})
	m.Init(context.Background(), &genai.Content{Parts: []*genai.Part{{Text: "sys"}}}, nil)
	waitFor(t, time.Second, func() bool { return m.Snapshot().Active })

	m.Delete(context.Background())
	if fake.deleteCount.Load() != 1 {
		t.Errorf("Delete called %d times, want 1", fake.deleteCount.Load())
	}
	if got := m.Name(context.Background()); got != "" {
		t.Errorf("Name after Delete = %q, want empty", got)
	}
	// Re-init after delete is a no-op — the manager is single-use.
	m.Init(context.Background(), &genai.Content{Parts: []*genai.Part{{Text: "sys"}}}, nil)
	time.Sleep(20 * time.Millisecond)
	if fake.createCount.Load() != 1 {
		t.Errorf("post-Delete Init spuriously re-created: %d Create calls", fake.createCount.Load())
	}
}

// TestManager_DeleteBeforeInit tolerates the pathological ordering
// (Delete before Init lands): no RPC, no panic, subsequent Init
// remains a no-op.
func TestManager_DeleteBeforeInit(t *testing.T) {
	t.Parallel()
	fake := &fakeCaches{}
	m := NewManager(fake, "gemini-2.5-flash", Options{})
	m.Delete(context.Background()) // before Init
	if fake.deleteCount.Load() != 0 {
		t.Errorf("Delete called Vertex despite no active cache: %d", fake.deleteCount.Load())
	}
	m.Init(context.Background(), &genai.Content{Parts: []*genai.Part{{Text: "sys"}}}, nil)
	time.Sleep(20 * time.Millisecond)
	if fake.createCount.Load() != 0 {
		t.Errorf("Init post-Delete fired Create: %d", fake.createCount.Load())
	}
}

// TestManager_MarkEvicted_ResetsForFreshInit pins the eviction-recovery
// contract: after MarkEvicted, Name() returns "" (so the caller runs
// uncached this turn) AND a subsequent Init call fires a fresh Create
// (so future turns re-benefit from caching). Otherwise the daemon runs
// uncached forever after a TTL eviction, defeating the point of #221.
func TestManager_MarkEvicted_ResetsForFreshInit(t *testing.T) {
	t.Parallel()
	fake := &fakeCaches{nextCacheNameOnce: "projects/p/l/l/cc/first"}
	m := NewManager(fake, "gemini-2.5-flash", Options{TTL: time.Hour})

	sys := &genai.Content{Parts: []*genai.Part{{Text: "sys"}}}
	m.Init(context.Background(), sys, nil)
	waitFor(t, time.Second, func() bool { return m.Name(context.Background()) != "" })
	if fake.createCount.Load() != 1 {
		t.Fatalf("expected 1 Create call, got %d", fake.createCount.Load())
	}

	// Simulate the wrapper detecting NOT_FOUND on the cache reference.
	m.MarkEvicted("Vertex 404")

	if got := m.Name(context.Background()); got != "" {
		t.Errorf("Name after MarkEvicted = %q, want empty (so caller runs uncached)", got)
	}

	// Next Init should fire a fresh Create — this is the load-bearing
	// half of eviction recovery.
	fake.nextCacheNameOnce = "projects/p/l/l/cc/second"
	m.Init(context.Background(), sys, nil)
	waitFor(t, time.Second, func() bool { return m.Name(context.Background()) == "projects/p/l/l/cc/second" })
	if fake.createCount.Load() != 2 {
		t.Errorf("expected 2 Create calls after eviction + re-init, got %d", fake.createCount.Load())
	}
}

// TestManager_MarkEvicted_NoOpBeforeActive pins that eviction on a
// pre-active or failed manager doesn't reset a fresh init that hasn't
// landed, and doesn't accidentally unblock a stateFailed manager
// (which represents a persistent problem, not a TTL eviction).
func TestManager_MarkEvicted_NoOpBeforeActive(t *testing.T) {
	t.Parallel()
	fake := &fakeCaches{createErr: errors.New("boom")}
	m := NewManager(fake, "gemini-2.5-flash", Options{TTL: time.Hour})

	// stateStart — MarkEvicted should no-op.
	m.MarkEvicted("nothing to evict")

	// Drive Init to stateFailed.
	m.Init(context.Background(), &genai.Content{Parts: []*genai.Part{{Text: "sys"}}}, nil)
	waitFor(t, time.Second, func() bool {
		return fake.createCount.Load() >= 1
	})
	// Give the goroutine one more scheduler tick to write the state.
	time.Sleep(20 * time.Millisecond)
	// MarkEvicted on a failed manager must NOT reset it to stateStart —
	// stateFailed is a "persistent problem, stay uncached" signal that
	// eviction recovery shouldn't paper over.
	m.MarkEvicted("shouldn't matter")
	m.Init(context.Background(), &genai.Content{Parts: []*genai.Part{{Text: "sys"}}}, nil)
	// Give any incorrectly-scheduled Create a chance to fire.
	time.Sleep(20 * time.Millisecond)
	if fake.createCount.Load() != 1 {
		t.Errorf("Init after eviction on stateFailed re-fired Create: count=%d, want 1", fake.createCount.Load())
	}
}

// TestStatus_String pins the human-readable form used by the daemon's
// startup log line. Deliberately covers all three states because
// operators grep for these strings.
func TestStatus_String(t *testing.T) {
	t.Parallel()
	cases := []struct {
		s        Status
		contains string
	}{
		{Status{Active: true, ExpiresIn: 6 * time.Hour}, "active"},
		{Status{Failed: true}, "failed"},
		{Status{}, "initializing"},
	}
	for _, tc := range cases {
		got := tc.s.String()
		if !contains(got, tc.contains) {
			t.Errorf("Status{%+v}.String() = %q, want to contain %q", tc.s, got, tc.contains)
		}
	}
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) &&
		(haystack == needle || len(needle) < len(haystack) && findSubstr(haystack, needle))
}

func findSubstr(h, n string) bool {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return true
		}
	}
	return false
}
