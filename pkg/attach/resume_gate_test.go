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
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/auth"
)

// The #484 fixes: a Lookup miss lazily RESUMES a session (full agent
// construction + eventlog replay + wake loop), so (1) a caller the
// persisted ACL rejects and (2) a caller the cost limiter rejects
// must both be turned away BEFORE the resumer runs — previously both
// forced the daemon to do the exact work the ACL/limiter exists to
// bound, on every session-scoped endpoint.

// gateFixtureRow persists one alice-owned row into a real ACL store
// and returns a registry wired with that store + the stub resumer.
func gateFixture(t *testing.T, resumer *stubResumer) *SessionRegistry {
	t.Helper()
	store := newTestACLStore(t)
	row := SessionACLRow{
		AppName:   "core-agent",
		UserID:    "u",
		SessionID: "sess-x",
		Owner:     "alice@example.com",
	}
	if err := store.Put(context.Background(), row); err != nil {
		t.Fatalf("store.Put: %v", err)
	}
	resumer.addRow(row)
	return NewSessionRegistryWithStore(store).WithResumer(resumer)
}

func TestRegistry_LookupGated_DenyBlocksResume(t *testing.T) {
	t.Parallel()
	resumer := &stubResumer{}
	reg := gateFixture(t, resumer)

	denied := errors.New("gate says no")
	_, err := reg.lookupGated(context.Background(), "core-agent", "sess-x", func(SessionACLRow) error {
		return denied
	})
	if !errors.Is(err, denied) {
		t.Fatalf("lookupGated error = %v, want the gate's error verbatim", err)
	}
	if calls := atomic.LoadInt32(&resumer.calls); calls != 0 {
		t.Fatalf("resumer ran %d time(s) despite the gate denying — resume work must not precede authorization", calls)
	}

	// The same lookup with an allowing gate resumes normally — the
	// gate saw the persisted row, not a fabrication.
	var seen SessionACLRow
	entry, err := reg.lookupGated(context.Background(), "core-agent", "sess-x", func(row SessionACLRow) error {
		seen = row
		return nil
	})
	if err != nil {
		t.Fatalf("lookupGated (allow): %v", err)
	}
	if entry.CurrentACL().Owner != "alice@example.com" {
		t.Errorf("resumed ACL.Owner = %q, want alice@example.com", entry.CurrentACL().Owner)
	}
	if seen.Owner != "alice@example.com" {
		t.Errorf("gate saw row owner %q, want the persisted alice row", seen.Owner)
	}
	if calls := atomic.LoadInt32(&resumer.calls); calls != 1 {
		t.Errorf("resumer calls = %d, want 1", calls)
	}
}

// TestResumeGate_StrangerGets404WithoutResume is the handler-level
// half: with ACL enforcement on, a stranger hitting a session-scoped
// endpoint for an idle-but-resumable session gets the standard
// indistinguishable 404 — and the resumer never constructs anything.
// The owner making the same request resumes and succeeds.
func TestResumeGate_StrangerGets404WithoutResume(t *testing.T) {
	t.Parallel()
	resumer := &stubResumer{}
	reg := gateFixture(t, resumer)

	h := newHandlers(reg, newBroadcasterPool())
	h.enforceACL = true
	mux := http.NewServeMux()
	h.register(mux)

	do := func(identity string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/sessions/core-agent/sess-x/status", nil)
		r = r.WithContext(auth.WithCaller(r.Context(), auth.Caller{Identity: identity}))
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, r)
		return rr
	}

	if rr := do("stranger@example.com"); rr.Code != http.StatusNotFound {
		t.Fatalf("stranger status = %d, want 404", rr.Code)
	}
	if calls := atomic.LoadInt32(&resumer.calls); calls != 0 {
		t.Fatalf("resumer ran %d time(s) for an unauthorized caller — existence/cost oracle plus wasted construction", calls)
	}

	if rr := do("alice@example.com"); rr.Code != http.StatusOK {
		t.Fatalf("owner status = %d, want 200 (gate must not block authorized resume)", rr.Code)
	}
	if calls := atomic.LoadInt32(&resumer.calls); calls != 1 {
		t.Errorf("resumer calls = %d, want 1 (owner's lookup resumes)", calls)
	}
}

// TestRateLimit_429BeforeLookupAndResume pins the limiter-vs-lookup
// order on the cost-limited routes: an over-limit caller is 429'd
// BEFORE the entry lookup runs, so it can neither force a lazy
// resume nor learn whether the session exists. Pre-#484 the limiter
// wrapped only the innermost handler — the resume had already
// happened by the time the 429 fired.
func TestRateLimit_429BeforeLookupAndResume(t *testing.T) {
	t.Parallel()
	resumer := &stubResumer{}
	reg := gateFixture(t, resumer)
	if _, err := reg.Register(&stubRegistrant{app: "core-agent", user: "u", sid: "live"}); err != nil {
		t.Fatal(err)
	}

	h := newHandlers(reg, newBroadcasterPool())
	h.costLimit = newCostRateLimiter(CostRateLimit{PerMinute: 1, Burst: 1})
	mux := http.NewServeMux()
	h.register(mux)

	post := func(path string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, path, nil))
		return rr
	}

	// Burn the anonymous caller's only token on the in-memory session
	// (501: stubRegistrant has no compact capability — past the limiter).
	if rr := post("/sessions/core-agent/live/slash/compact"); rr.Code != http.StatusNotImplemented {
		t.Fatalf("first call = %d, want 501", rr.Code)
	}

	// Over-limit call against the idle-but-resumable session: 429,
	// and the resumer must never have run.
	if rr := post("/sessions/core-agent/sess-x/slash/compact"); rr.Code != http.StatusTooManyRequests {
		t.Fatalf("over-limit call = %d, want 429 before lookup", rr.Code)
	}
	if calls := atomic.LoadInt32(&resumer.calls); calls != 0 {
		t.Fatalf("resumer ran %d time(s) for a rate-limited caller — the limiter must bound resume work, not follow it", calls)
	}
}
