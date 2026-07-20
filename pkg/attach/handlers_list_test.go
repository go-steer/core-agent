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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/auth"
)

// listSessionsFor returns the decoded GET /sessions response body
// for a request carrying the supplied caller identity. Used by the
// union-path tests below.
func listSessionsFor(t *testing.T, h *handlers, caller auth.Caller) []sessionDescriptor {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/sessions", strings.NewReader(""))
	if caller.Identity != "" || caller.Admin {
		r = r.WithContext(auth.WithCaller(r.Context(), caller))
	}
	rr := httptest.NewRecorder()
	h.listSessions(rr, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("listSessions: got status %d, body: %s", rr.Code, rr.Body.String())
	}
	var payload struct {
		Sessions []sessionDescriptor `json:"sessions"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return payload.Sessions
}

func TestListSessions_UnionsInMemoryAndPersisted(t *testing.T) {
	t.Parallel()
	// Simulates a post-restart / post-eviction state: bob's session
	// is live in memory (still running), alice's is persisted-only
	// (evicted or never resumed since restart). Both should appear
	// in alice's caller's list when alice can read hers, with the
	// right Status field distinguishing them.
	store := newTestACLStore(t)
	reg := NewSessionRegistryWithStore(store)
	// Persisted-only session (alice's). Simulating "row on disk,
	// nothing in memory" — the post-restart pre-resume state.
	if err := store.Put(context.Background(), SessionACLRow{
		AppName:   "core-agent",
		UserID:    "alice@example.com",
		SessionID: "sess-alice-persisted",
		Owner:     "alice@example.com",
	}); err != nil {
		t.Fatalf("store.Put: %v", err)
	}
	// In-memory session (bob's, unrelated to alice).
	if _, err := reg.RegisterOwned(&stubRegistrant{app: "core-agent", user: "bob", sid: "sess-bob-live"}, "bob@example.com"); err != nil {
		t.Fatalf("RegisterOwned bob: %v", err)
	}
	h := &handlers{reg: reg, pool: NewBroadcasterPool(), enforceACL: true}

	// Alice: sees exactly her persisted session, status "idle".
	got := listSessionsFor(t, h, auth.Caller{Identity: "alice@example.com"})
	if len(got) != 1 {
		t.Fatalf("alice: expected 1 session, got %d: %+v", len(got), got)
	}
	if got[0].SessionID != "sess-alice-persisted" {
		t.Errorf("alice: got session %q, want sess-alice-persisted", got[0].SessionID)
	}
	if got[0].Status != sessionStatusIdle {
		t.Errorf("alice: got status %q, want %q (persisted-only)", got[0].Status, sessionStatusIdle)
	}

	// Bob: sees his live session, status "active".
	got = listSessionsFor(t, h, auth.Caller{Identity: "bob@example.com"})
	if len(got) != 1 {
		t.Fatalf("bob: expected 1 session, got %d: %+v", len(got), got)
	}
	if got[0].SessionID != "sess-bob-live" {
		t.Errorf("bob: got session %q, want sess-bob-live", got[0].SessionID)
	}
	if got[0].Status != sessionStatusActive {
		t.Errorf("bob: got status %q, want %q (in-memory)", got[0].Status, sessionStatusActive)
	}
}

func TestListSessions_ActiveDedupsPersisted(t *testing.T) {
	t.Parallel()
	// When a session is BOTH in memory and persisted (the normal
	// case for a live registered session), it must appear exactly
	// once — as "active". The persisted-half query would return the
	// same row and the dedup by (app, sid) collapses them.
	store := newTestACLStore(t)
	reg := NewSessionRegistryWithStore(store)
	// RegisterOwned writes to the store AND the in-memory map.
	if _, err := reg.RegisterOwned(&stubRegistrant{app: "core-agent", user: "alice@example.com", sid: "sess-both"}, "alice@example.com"); err != nil {
		t.Fatalf("RegisterOwned: %v", err)
	}
	h := &handlers{reg: reg, pool: NewBroadcasterPool(), enforceACL: true}

	got := listSessionsFor(t, h, auth.Caller{Identity: "alice@example.com"})
	if len(got) != 1 {
		t.Fatalf("expected 1 session (dedup), got %d: %+v", len(got), got)
	}
	if got[0].Status != sessionStatusActive {
		t.Errorf("dedup should keep 'active', got %q", got[0].Status)
	}
}

func TestListSessions_AdminSeesEverything(t *testing.T) {
	t.Parallel()
	// Admin's Authorize bypass should extend through the union —
	// both memory + persisted rows must surface, dedup-safe.
	store := newTestACLStore(t)
	reg := NewSessionRegistryWithStore(store)
	if _, err := reg.RegisterOwned(&stubRegistrant{app: "core-agent", user: "bob@example.com", sid: "sess-live"}, "bob@example.com"); err != nil {
		t.Fatalf("RegisterOwned: %v", err)
	}
	if err := store.Put(context.Background(), SessionACLRow{
		AppName:   "core-agent",
		UserID:    "carol@example.com",
		SessionID: "sess-persisted",
		Owner:     "carol@example.com",
	}); err != nil {
		t.Fatalf("store.Put: %v", err)
	}
	h := &handlers{reg: reg, pool: NewBroadcasterPool(), enforceACL: true}

	got := listSessionsFor(t, h, auth.Caller{Identity: "ops@example.com", Admin: true})
	if len(got) != 2 {
		t.Fatalf("admin: expected 2 sessions, got %d: %+v", len(got), got)
	}
	// Both statuses should appear.
	statuses := map[string]bool{}
	for _, d := range got {
		statuses[d.Status] = true
	}
	if !statuses[sessionStatusActive] || !statuses[sessionStatusIdle] {
		t.Errorf("admin: want both active + idle statuses; got %v", statuses)
	}
}

func TestListSessions_NoStoreFallsBackToMemoryOnly(t *testing.T) {
	t.Parallel()
	// A registry without an aclStore (single-user tests, pre-v2.5
	// tests) must still work — the union path is opt-in.
	reg := NewSessionRegistry()
	if _, err := reg.RegisterOwned(&stubRegistrant{app: "core-agent", user: "u", sid: "sess-only"}, "alice@example.com"); err != nil {
		t.Fatalf("RegisterOwned: %v", err)
	}
	h := &handlers{reg: reg, pool: NewBroadcasterPool(), enforceACL: true}

	got := listSessionsFor(t, h, auth.Caller{Identity: "alice@example.com"})
	if len(got) != 1 {
		t.Fatalf("expected 1 session, got %d", len(got))
	}
	if got[0].Status != sessionStatusActive {
		t.Errorf("got status %q, want active", got[0].Status)
	}
}

func TestListSessions_EnforceACLOffSkipsUnion(t *testing.T) {
	t.Parallel()
	// When enforceACL is off (single-user attach mode), the
	// persisted-half query must not run — its identity-filtering
	// semantics are meaningless without a Caller. The handler
	// falls back to the in-memory-only List() path.
	store := newTestACLStore(t)
	reg := NewSessionRegistryWithStore(store)
	if err := store.Put(context.Background(), SessionACLRow{
		AppName:   "core-agent",
		UserID:    "u",
		SessionID: "sess-persisted-only",
		Owner:     "alice@example.com",
	}); err != nil {
		t.Fatalf("store.Put: %v", err)
	}
	h := &handlers{reg: reg, pool: NewBroadcasterPool(), enforceACL: false}

	// No caller in the request context. With enforceACL=false,
	// list should return only in-memory entries (empty), NOT
	// leak the persisted row across the "single-user pretends
	// every user is anon" boundary.
	got := listSessionsFor(t, h, auth.Caller{})
	if len(got) != 0 {
		t.Errorf("enforceACL=false + empty memory: expected 0 sessions, got %d: %+v", len(got), got)
	}
}
