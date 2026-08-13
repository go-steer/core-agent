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
	// honorCtx makes Create return ctx.Err() when the passed context
	// is already cancelled — mimics a real transport so the #370
	// detachment tests can prove doInit no longer runs on the
	// spawning request's context.
	honorCtx bool

	// updateStarted / updateRelease, when non-nil, make Update signal
	// entry and then block until released — lets tests hold a refresh
	// RPC in flight while racing MarkEvicted / Delete against it.
	// Set before the manager is constructed; never mutated after.
	updateStarted chan struct{}
	updateRelease chan struct{}

	createCount atomic.Int32
	updateCount atomic.Int32
	deleteCount atomic.Int32

	lastCreateModel   string
	lastCreateConfig  *genai.CreateCachedContentConfig
	lastUpdateName    string
	lastDeleteName    string
	nextCacheNameOnce string // if set, used for the next Create's returned Name
}

func (f *fakeCaches) Create(ctx context.Context, model string, cfg *genai.CreateCachedContentConfig) (*genai.CachedContent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createCount.Add(1)
	if f.honorCtx {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
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
	if f.updateStarted != nil {
		f.updateStarted <- struct{}{}
	}
	if f.updateRelease != nil {
		<-f.updateRelease
	}
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

// testWait is the waitFor deadline for every asynchronous assertion
// in this file. Generous on purpose (#499): the conditions are
// goroutine-scheduling hops that complete in microseconds when the
// scheduler is idle, but the full-suite -race CI run showed 1s was
// tight enough to flake under load ("condition never became true
// within 1s", 2026-07-29 on PR #498's run). A long deadline costs
// nothing on the passing path — waitFor returns the moment the
// condition holds — and only slows genuinely broken runs.
const testWait = 10 * time.Second

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
	waitFor(t, testWait, func() bool { return m.Snapshot().Active })

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
// Name returns "" (uncached, no panic), and callers can keep using the
// manager. Since #707 the failure is retried on a bounded schedule
// rather than being permanent, so the assertion here is that nothing
// retries *within the backoff* — the give-up path is
// TestInit_RealErrorGivesUpAfterRetryBudget.
func TestManager_InitError(t *testing.T) {
	t.Parallel()
	fake := &fakeCaches{createErr: errors.New("boom")}
	m := NewManager(fake, "gemini-2.5-flash", Options{Logger: discardLogger()})

	m.Init(context.Background(), &genai.Content{Parts: []*genai.Part{{Text: "sys"}}}, nil)
	waitFor(t, testWait, func() bool { return fake.createCount.Load() == 1 })

	if got := m.Name(context.Background()); got != "" {
		t.Errorf("Name after failed Init = %q, want empty (degrade to uncached)", got)
	}
	// Re-Init inside the backoff window must not fire a second RPC.
	m.Init(context.Background(), &genai.Content{Parts: []*genai.Part{{Text: "sys"}}}, nil)
	// Give any spurious background goroutine a chance to run before
	// we assert.
	time.Sleep(20 * time.Millisecond)
	if fake.createCount.Load() != 1 {
		t.Errorf("Create called %d times inside the backoff, want 1", fake.createCount.Load())
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
	waitFor(t, testWait, func() bool { return m.Snapshot().Active })
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
	waitFor(t, testWait, func() bool { return m.Snapshot().Active })

	// First Name call reads the cache + schedules a Refresh goroutine.
	_ = m.Name(context.Background())
	waitFor(t, testWait, func() bool { return fake.updateCount.Load() >= 1 })

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
	waitFor(t, testWait, func() bool { return m.Snapshot().Active })

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
	waitFor(t, testWait, func() bool { return m.Name(context.Background()) != "" })
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
	waitFor(t, testWait, func() bool { return m.Name(context.Background()) == "projects/p/l/l/cc/second" })
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
	waitFor(t, testWait, func() bool {
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

// TestInit_DetachedFromRequestContext is the #370 regression gate:
// Init runs on the FIRST TURN's request context, and pre-fix the
// Create RPC died with context.Canceled when that turn finished
// first — flipping the manager to sticky stateFailed and silently
// disabling caching for the daemon's life. doInit now runs on a
// detached (WithoutCancel) context, so a dead parent must not stop
// the cache from activating.
func TestInit_DetachedFromRequestContext(t *testing.T) {
	t.Parallel()
	f := &fakeCaches{honorCtx: true}
	m := NewManager(f, "gemini-3.1-pro", Options{Logger: discardLogger()})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the spawning turn is already gone
	m.Init(ctx, &genai.Content{Parts: []*genai.Part{{Text: "sys"}}}, nil)

	waitFor(t, testWait, func() bool { return m.Snapshot().Active })
	if got := f.createCount.Load(); got != 1 {
		t.Errorf("createCount = %d, want 1", got)
	}
}

// TestInit_TransientCancelRetriesInsteadOfStickyFail pins the error
// classification half of #370: a Create that dies with
// context.Canceled / DeadlineExceeded (RPC timeout, transport blip)
// resets the manager to its pre-Init state so a later turn retries —
// only non-cancellation failures stay sticky.
func TestInit_TransientCancelRetriesInsteadOfStickyFail(t *testing.T) {
	t.Parallel()
	for _, transient := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(transient.Error(), func(t *testing.T) {
			t.Parallel()
			f := &fakeCaches{createErr: transient}
			m := NewManager(f, "gemini-3.1-pro", Options{Logger: discardLogger()})
			sys := &genai.Content{Parts: []*genai.Part{{Text: "sys"}}}

			m.Init(context.Background(), sys, nil)
			// The failed attempt must NOT go sticky-failed, and must
			// re-open the Init gate.
			waitFor(t, testWait, func() bool { return f.createCount.Load() == 1 })
			waitFor(t, testWait, func() bool {
				s := m.Snapshot()
				return !s.Active && !s.Failed
			})

			// Blip over: later turns retry and activate. Poll Init
			// rather than calling it once — a single call can land in
			// the window between Create returning context.Canceled and
			// doInit resetting the gate (state stays stateStart through
			// the transient path, so the waits above can pass with
			// initStarted still true), get swallowed as an "already
			// initializing" no-op, and leave nobody to retry. That is
			// the real shape of the #499 flake: a lost retry, not a
			// slow one — no waitFor deadline can save a condition that
			// can never become true (#534). Polling mirrors the product
			// contract: every real turn calls Init again.
			f.mu.Lock()
			f.createErr = nil
			f.mu.Unlock()
			waitFor(t, testWait, func() bool {
				m.Init(context.Background(), sys, nil)
				return m.Snapshot().Active
			})
			if got := f.createCount.Load(); got != 2 {
				t.Errorf("createCount = %d, want 2 (one failed, one retried)", got)
			}
		})
	}
}

// fakeClock is a manually advanced clock for the #707 retry-backoff
// tests, so they assert on the schedule instead of sleeping through it.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// setCreateErr flips the injected Create error mid-test, so a retry can
// find the API healthy again.
func (f *fakeCaches) setCreateErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createErr = err
}

// newRetryManager builds a Manager on a fake clock with a two-entry
// backoff schedule (so three Create attempts total).
func newRetryManager(f *fakeCaches) (*Manager, *fakeClock) {
	clk := &fakeClock{t: time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)}
	m := NewManager(f, "gemini-3.1-pro", Options{Logger: discardLogger()})
	m.now = clk.now
	m.retryBackoff = []time.Duration{15 * time.Second, 30 * time.Second}
	return m, clk
}

// TestInit_PermissionDeniedRecoversOnRetry is the #707 regression gate.
// A live GKE daemon came up before its Workload-Identity binding had
// propagated, Caches.Create returned 403, and the pre-fix sticky
// stateFailed meant the session paid full input price for all 365K of
// its billed input tokens — for the daemon's entire life, long after
// IAM had settled. A later Init past the backoff must retry and
// succeed.
//
// Pre-fix this fails at the final Snapshot: state is stateFailed and no
// second Create is ever issued.
func TestInit_PermissionDeniedRecoversOnRetry(t *testing.T) {
	t.Parallel()
	f := &fakeCaches{createErr: errors.New("rpc error: code = PermissionDenied desc = 403")}
	m, clk := newRetryManager(f)
	sys := &genai.Content{Parts: []*genai.Part{{Text: "sys"}}}

	m.Init(context.Background(), sys, nil)
	waitFor(t, testWait, func() bool { return f.createCount.Load() == 1 })
	if snap := m.Snapshot(); snap.Failed {
		t.Fatal("Snapshot().Failed = true after one 403; want a pending retry, not a lifetime sentence")
	}

	// Still inside the first backoff: a busy daemon's next turn must
	// not re-issue the doomed RPC.
	m.Init(context.Background(), sys, nil)
	time.Sleep(20 * time.Millisecond)
	if got := f.createCount.Load(); got != 1 {
		t.Fatalf("createCount = %d during backoff, want 1 (backoff not honored)", got)
	}

	// IAM propagates; the next turn past the backoff picks it up.
	f.setCreateErr(nil)
	clk.advance(16 * time.Second)
	m.Init(context.Background(), sys, nil)
	waitFor(t, testWait, func() bool { return m.Snapshot().Active })

	if got := f.createCount.Load(); got != 2 {
		t.Errorf("createCount = %d, want 2", got)
	}
	m.mu.Lock()
	failures := m.initFailures
	m.mu.Unlock()
	if failures != 0 {
		t.Errorf("initFailures = %d after a successful retry, want 0 (history must reset)", failures)
	}
}

// TestInit_RealErrorGivesUpAfterRetryBudget pins the property the old
// sticky failure existed to protect: a genuinely misconfigured project
// is not hammered forever. Retries are bounded by the backoff schedule,
// and the attempt after the last one is permanent.
func TestInit_RealErrorGivesUpAfterRetryBudget(t *testing.T) {
	t.Parallel()
	f := &fakeCaches{createErr: errors.New("permission denied")}
	m, clk := newRetryManager(f) // 2 retries => 3 attempts
	sys := &genai.Content{Parts: []*genai.Part{{Text: "sys"}}}

	for i, wait := range []time.Duration{0, 16 * time.Second, 31 * time.Second} {
		clk.advance(wait)
		m.Init(context.Background(), sys, nil)
		want := int32(i + 1)
		waitFor(t, testWait, func() bool { return f.createCount.Load() == want })
	}
	waitFor(t, testWait, func() bool { return m.Snapshot().Failed })

	// Budget spent: no further attempt, however long we wait.
	clk.advance(time.Hour)
	m.Init(context.Background(), sys, nil)
	time.Sleep(20 * time.Millisecond)
	if got := f.createCount.Load(); got != 3 {
		t.Errorf("createCount = %d, want 3 (a spent retry budget must stay spent)", got)
	}
}

// TestManager_RefreshLandingAfterEvictionIsDropped pins the doRefresh
// success-path guard: when MarkEvicted lands while the Update RPC is
// in flight, the refreshed ExpireTime must be dropped — applying it
// would stamp the already-evicted (or a subsequently re-created)
// cache identity with a stale expiry (#372).
func TestManager_RefreshLandingAfterEvictionIsDropped(t *testing.T) {
	t.Parallel()
	fake := &fakeCaches{
		// Near-expiry Create so the first Name() schedules a refresh.
		ttlOverride:   500 * time.Millisecond,
		updateStarted: make(chan struct{}, 1),
		updateRelease: make(chan struct{}),
	}
	m := NewManager(fake, "gemini-2.5-flash", Options{
		TTL:              time.Hour,
		RefreshThreshold: 30 * time.Minute,
		Logger:           discardLogger(),
	})
	m.Init(context.Background(), &genai.Content{Parts: []*genai.Part{{Text: "sys"}}}, nil)
	waitFor(t, testWait, func() bool { return m.Snapshot().Active })

	// Schedule the refresh, then hold its Update RPC in flight.
	_ = m.Name(context.Background())
	select {
	case <-fake.updateStarted:
	case <-time.After(testWait):
		t.Fatal("refresh Update RPC never started")
	}

	// Eviction lands mid-RPC: state resets to pre-Init, expiry zeroed.
	m.MarkEvicted("simulated NOT_FOUND")

	// Let the in-flight refresh complete and settle.
	close(fake.updateRelease)
	waitFor(t, testWait, func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return !m.refreshing
	})

	snap := m.Snapshot()
	if snap.Active {
		t.Errorf("Active = true after eviction; the landed refresh must not resurrect the cache")
	}
	if snap.CacheName != "" {
		t.Errorf("CacheName = %q, want empty after eviction", snap.CacheName)
	}
	if !snap.ExpiresAt.IsZero() {
		t.Errorf("ExpiresAt = %v, want zero (stale refresh result must be dropped)", snap.ExpiresAt)
	}
}
