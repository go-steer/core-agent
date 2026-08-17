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

// rosterServer serves GET /sessions/{sid}/agents, counts the hits, and
// can be flipped to fail so the last-known-good path is testable.
type rosterServer struct {
	*httptest.Server

	hits atomic.Int64

	mu     sync.Mutex
	agents []attach.AgentInfo
	fail   bool
}

func startRosterServer(t *testing.T) *rosterServer {
	t.Helper()
	rs := &rosterServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /sessions/{sid}/agents", func(w http.ResponseWriter, _ *http.Request) {
		rs.hits.Add(1)
		rs.mu.Lock()
		fail, agents := rs.fail, rs.agents
		rs.mu.Unlock()
		if fail {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"agents": agents})
	})
	rs.Server = httptest.NewServer(mux)
	t.Cleanup(rs.Close)
	return rs
}

func (rs *rosterServer) setAgents(agents []attach.AgentInfo) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.agents = agents
}

func (rs *rosterServer) setFail(fail bool) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.fail = fail
}

func newRosterAdapter(t *testing.T, rs *rosterServer) *Adapter {
	t.Helper()
	parsed, err := attachclient.ParseURL(rs.URL + "/sessions/s1")
	if err != nil {
		t.Fatalf("parseURL: %v", err)
	}
	return New(attachclient.New(parsed, "", 0), "/sessions/s1")
}

// TestSubagents_CachesAcrossHostSnapshotPolls — core-tui v0.20.0 pulls
// SubagentLister from hostSnapshot once a second, so an uncached
// Subagents() would be one HTTP round-trip per second forever. Every
// call inside the TTL must be served from cache.
func TestSubagents_CachesAcrossHostSnapshotPolls(t *testing.T) {
	t.Parallel()
	rs := startRosterServer(t)
	rs.setAgents([]attach.AgentInfo{{Name: "watcher", Status: "running"}})
	a := newRosterAdapter(t, rs)

	// Ten polls, the cadence core-tui's 1 Hz snapshot would produce
	// over ten seconds, all inside one TTL.
	for i := 0; i < 10; i++ {
		got := a.Subagents()
		if len(got) != 1 || got[0].Name != "watcher" {
			t.Fatalf("poll %d: got %+v, want one 'watcher' row", i, got)
		}
	}
	if n := rs.hits.Load(); n != 1 {
		t.Fatalf("GET /agents hit %d times across 10 polls; want 1 (cold fetch only)", n)
	}
}

// TestSubagents_TransientErrorKeepsLastKnownGood — a nil return from a
// WIRED SubagentLister reads as "none running" in core-tui's sidebar,
// so a single dropped request used to make the TUI assert there were
// no subagents while subagents were running. On a once-a-second
// surface that is a visible flicker; the cache must hold the last good
// roster instead.
func TestSubagents_TransientErrorKeepsLastKnownGood(t *testing.T) {
	t.Parallel()
	rs := startRosterServer(t)
	rs.setAgents([]attach.AgentInfo{{Name: "watcher", Status: "running"}})
	a := newRosterAdapter(t, rs)

	if got := a.Subagents(); len(got) != 1 {
		t.Fatalf("cold fetch: got %+v, want one row", got)
	}

	// Daemon starts failing, and the TTL lapses so the next call kicks
	// a (doomed) background refresh.
	rs.setFail(true)
	a.subagents.mu.Lock()
	a.subagents.lastFetch = time.Now().Add(-2 * subagentsCacheTTL)
	a.subagents.mu.Unlock()

	if got := a.Subagents(); len(got) != 1 || got[0].Name != "watcher" {
		t.Fatalf("stale read: got %+v, want the last-known-good row", got)
	}

	// Let the background refresh land and confirm it did NOT blank the
	// roster on the error.
	waitForRefreshDone(t, a)
	if got := a.Subagents(); len(got) != 1 || got[0].Name != "watcher" {
		t.Fatalf("after failed refresh: got %+v, want the last-known-good row", got)
	}
}

// TestSubagents_RefreshAdoptsNewRoster — the flip side: once the TTL
// lapses and the fetch succeeds, the new roster replaces the old one.
func TestSubagents_RefreshAdoptsNewRoster(t *testing.T) {
	t.Parallel()
	rs := startRosterServer(t)
	rs.setAgents([]attach.AgentInfo{{Name: "watcher", Status: "running"}})
	a := newRosterAdapter(t, rs)

	if got := a.Subagents(); len(got) != 1 {
		t.Fatalf("cold fetch: got %+v, want one row", got)
	}

	rs.setAgents([]attach.AgentInfo{
		{Name: "watcher", Status: "completed"},
		{Name: "auditor", Status: "running"},
	})
	a.subagents.mu.Lock()
	a.subagents.lastFetch = time.Now().Add(-2 * subagentsCacheTTL)
	a.subagents.mu.Unlock()

	_ = a.Subagents() // kicks the background refresh, serves the stale value
	waitForRefreshDone(t, a)

	got := a.Subagents()
	if len(got) != 2 || got[0].Status != "completed" || got[1].Name != "auditor" {
		t.Fatalf("after refresh: got %+v, want the two-row roster", got)
	}
}

// TestSubagents_ColdErrorIsNotCachedAsData — the failure mode a naive
// TTL cache makes WORSE than no cache at all.
//
// Attach while the daemon is mid-restart and the very first GET /agents
// fails. There is nothing to serve, so the sidebar says "no subagents
// running" — which is not "we don't know yet", it is a false statement
// about a live system. If the cache treats that failed fetch as a
// completed fetch, the lie is pinned for the full TTL: the uncached
// code this replaced was wrong for one poll, and a TTL-only cache would
// be wrong for five seconds. Recovery must track the cold-retry
// interval instead.
func TestSubagents_ColdErrorIsNotCachedAsData(t *testing.T) {
	t.Parallel()
	rs := startRosterServer(t)
	rs.setAgents([]attach.AgentInfo{{Name: "watcher", Status: "running"}})
	rs.setFail(true)
	a := newRosterAdapter(t, rs)

	if got := a.Subagents(); len(got) != 0 {
		t.Fatalf("cold error: got %+v, want no rows (nothing known yet)", got)
	}
	// A failed fetch must not be recorded as good data, or the full TTL
	// applies to it.
	a.subagents.mu.Lock()
	haveGood := a.subagents.haveGood
	a.subagents.mu.Unlock()
	if haveGood {
		t.Fatal("a failed cold fetch was recorded as a good roster")
	}

	// Daemon recovers. Budget is comfortably under subagentsCacheTTL:
	// passing at ~subagentsCacheTTL is the regression, not a pass.
	rs.setFail(false)
	const budget = 2 * time.Second
	if budget >= subagentsCacheTTL {
		t.Fatalf("test budget %v must stay under the %v TTL to be meaningful",
			budget, subagentsCacheTTL)
	}
	start := time.Now()
	for time.Since(start) < budget {
		if got := a.Subagents(); len(got) == 1 && got[0].Name == "watcher" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("roster still empty %v after the daemon recovered; "+
		"the cold-path error is being served as data until the %v TTL lapses",
		budget, subagentsCacheTTL)
}

// waitForRefreshDone blocks until the in-flight background refresh
// clears its guard, so the assertion after it isn't racing the fetch.
func waitForRefreshDone(t *testing.T, a *Adapter) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		a.subagents.mu.Lock()
		refreshing := a.subagents.refreshing
		a.subagents.mu.Unlock()
		if !refreshing {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("background subagent refresh never completed")
}
