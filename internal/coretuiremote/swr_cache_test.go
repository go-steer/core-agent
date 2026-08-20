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

package coretuiremote

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-steer/core-agent/v2/internal/attachclient"
	"github.com/go-steer/core-agent/v2/pkg/attach"
)

// swrServer serves GET /sessions/{sid}/status and /usage, counts the
// hits per endpoint, and can be flipped to fail so the cold-fetch path
// is testable. The roster equivalent lives in subagent_roster_test.go.
type swrServer struct {
	*httptest.Server

	statusHits atomic.Int64
	usageHits  atomic.Int64

	mu   sync.Mutex
	fail bool
}

func startSWRServer(t *testing.T) *swrServer {
	t.Helper()
	ss := &swrServer{}
	failing := func() bool {
		ss.mu.Lock()
		defer ss.mu.Unlock()
		return ss.fail
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /sessions/{sid}/status", func(w http.ResponseWriter, _ *http.Request) {
		ss.statusHits.Add(1)
		if failing() {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(attach.StatusInfo{State: "idle", ModelName: "claude-opus-5"})
	})
	mux.HandleFunc("GET /sessions/{sid}/usage", func(w http.ResponseWriter, _ *http.Request) {
		ss.usageHits.Add(1)
		if failing() {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(attach.UsageInfo{
			Overall: attach.UsageTotals{InputTokens: 1200, OutputTokens: 340, Turns: 7, CostUSD: 0.42},
		})
	})
	ss.Server = httptest.NewServer(mux)
	t.Cleanup(ss.Close)
	return ss
}

func (ss *swrServer) setFail(fail bool) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	ss.fail = fail
}

func newSWRAdapter(t *testing.T, ss *swrServer) *Adapter {
	t.Helper()
	parsed, err := attachclient.ParseURL(ss.URL + "/sessions/s1")
	if err != nil {
		t.Fatalf("parseURL: %v", err)
	}
	return New(attachclient.New(parsed, "", 0), "/sessions/s1")
}

// TestStatus_ColdErrorIsNotCachedAsData — attach while the daemon is
// mid-restart and the first GET /status fails. Pre-#781 the error path
// bumped lastFetch while leaving cached zero, so "we asked and failed"
// was indistinguishable from "the answer is empty" and the placeholder
// header ("—" / "(model not set)") was pinned for the whole 2s TTL even
// after the daemon came back. Recovery must track the cold-retry
// interval instead.
func TestStatus_ColdErrorIsNotCachedAsData(t *testing.T) {
	t.Parallel()
	ss := startSWRServer(t)
	ss.setFail(true)
	a := newSWRAdapter(t, ss)

	if got := a.Status(); got.ModelName != "" {
		t.Fatalf("cold error: got %+v, want the zero Status (nothing known yet)", got)
	}
	a.status.mu.Lock()
	haveGood := a.status.haveGood
	a.status.mu.Unlock()
	if haveGood {
		t.Fatal("a failed cold fetch was recorded as a good status snapshot")
	}

	ss.setFail(false)
	waitForRecovery(t, statusCacheTTL, func() bool { return a.Status().ModelName == "claude-opus-5" },
		"the status header is serving the cold-path error as data until the %v TTL lapses")
}

// TestUsage_ColdErrorIsNotCachedAsData — the same defect on the usage
// footer, where the placeholder is a row of zeros. Cosmetic like the
// status header, and fixed for the same reason: the shape is what the
// next cached capability copies.
func TestUsage_ColdErrorIsNotCachedAsData(t *testing.T) {
	t.Parallel()
	ss := startSWRServer(t)
	ss.setFail(true)
	a := newSWRAdapter(t, ss)

	if got := a.SessionTurns(); got != 0 {
		t.Fatalf("cold error: got %d turns, want 0 (nothing known yet)", got)
	}
	a.usage.mu.Lock()
	haveGood := a.usage.haveGood
	a.usage.mu.Unlock()
	if haveGood {
		t.Fatal("a failed cold fetch was recorded as a good usage snapshot")
	}

	ss.setFail(false)
	waitForRecovery(t, usageCacheTTL, func() bool { return a.SessionTurns() == 7 },
		"the usage footer is serving the cold-path error as data until the %v TTL lapses")
}

// waitForRecovery polls until the surface reports real data, on a budget
// deliberately shorter than the steady-state TTL: passing at ~ttl is the
// regression this is here to catch, not a pass.
func waitForRecovery(t *testing.T, ttl time.Duration, recovered func() bool, msg string) {
	t.Helper()
	budget := ttl / 2
	if budget < 4*coldRetryTTL {
		t.Fatalf("TTL %v leaves no room between the %v cold-retry interval and a meaningful budget", ttl, coldRetryTTL)
	}
	start := time.Now()
	for time.Since(start) < budget {
		if recovered() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no recovery %v after the daemon came back; "+msg, budget, ttl)
}

// TestSWRCaches_WarmReadsStayCached — the other half of the bargain.
// coretui pulls StatusReporter on every render and the UsageTracker up
// to 8× per render, so once a fetch has succeeded the short cold-retry
// interval must stop applying: a cache that re-asked every 250ms
// forever would trade a cosmetic staleness bug for a traffic one.
func TestSWRCaches_WarmReadsStayCached(t *testing.T) {
	t.Parallel()
	ss := startSWRServer(t)
	a := newSWRAdapter(t, ss)

	if got := a.Status(); got.ModelName != "claude-opus-5" {
		t.Fatalf("cold fetch: got %+v, want the real status", got)
	}
	if got := a.SessionTurns(); got != 7 {
		t.Fatalf("cold fetch: got %d turns, want 7", got)
	}

	// Render cadence for well over the cold-retry interval but well
	// inside the 2s steady-state TTL.
	deadline := time.Now().Add(4 * coldRetryTTL)
	for time.Now().Before(deadline) {
		_ = a.Status()
		_ = a.SessionTurns()
		time.Sleep(10 * time.Millisecond)
	}

	if n := ss.statusHits.Load(); n != 1 {
		t.Fatalf("GET /status hit %d times inside one TTL; want 1 (cold fetch only)", n)
	}
	if n := ss.usageHits.Load(); n != 1 {
		t.Fatalf("GET /usage hit %d times inside one TTL; want 1 (cold fetch only)", n)
	}
}

// TestSWRState_ColdIntervalNeverExceedsTheTTL pins the clamp in
// swrState.next. Every caller today passes a TTL far longer than
// coldRetryTTL, so the guard is unreachable through the three
// accessors — but "cold state retries sooner" is the invariant, and a
// future cache with a sub-250ms TTL must not have its retries slowed
// down by the very mechanism that exists to speed them up.
func TestSWRState_ColdIntervalNeverExceedsTheTTL(t *testing.T) {
	t.Parallel()
	var s swrState
	tiny := coldRetryTTL / 10
	s.lastFetch = time.Now().Add(-2 * tiny)

	if got := s.next(tiny); got != swrRefresh {
		t.Fatalf("cold state, %v past a %v TTL: got action %d, want swrRefresh", 2*tiny, tiny, got)
	}
	if !s.refreshing {
		t.Fatal("swrRefresh did not claim the in-flight guard")
	}
}
