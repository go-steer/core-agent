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
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/auth"
)

// newDeleteRequest builds a DELETE request through the ServeMux
// pattern so PathValue("app")/PathValue("sid") resolve. Bypasses
// the auth middleware chain — caller sets Caller directly if
// needed, matching handlers_create_session_test.go's pattern.
func newDeleteRequest(t *testing.T, target string, caller auth.Caller) (*http.Request, *httptest.ResponseRecorder) {
	t.Helper()
	r := httptest.NewRequest(http.MethodDelete, target, nil)
	if caller.Identity != "" {
		r = r.WithContext(auth.WithCaller(r.Context(), caller))
	}
	return r, httptest.NewRecorder()
}

// deleteHandlerFixture wires a handlers{} + registry + pool in the
// minimum shape the delete handlers need. Returns the handlers set
// AND the mux (routes registered) so tests can drive the whole
// pattern-match path rather than calling the handler methods
// directly (which would bypass Go 1.22 PathValue extraction).
func deleteHandlerFixture(t *testing.T, enforceACL bool, aclStore SessionACLStore) (*handlers, *http.ServeMux, *SessionRegistry) {
	t.Helper()
	var reg *SessionRegistry
	if aclStore != nil {
		reg = NewSessionRegistryWithStore(aclStore)
	} else {
		reg = NewSessionRegistry()
	}
	h := &handlers{
		reg:        reg,
		pool:       NewBroadcasterPool(),
		enforceACL: enforceACL,
	}
	mux := http.NewServeMux()
	h.registerOperatorState(mux)
	return h, mux, reg
}

// TestDeleteSession_Qualified_Success — DELETE /sessions/{app}/{sid}
// removes the entry and returns 204. Single-user posture
// (enforceACL=false) so auth is a no-op.
func TestDeleteSession_Qualified_Success(t *testing.T) {
	t.Parallel()
	_, mux, reg := deleteHandlerFixture(t, false, nil)
	ag := &stubRegistrant{app: "core-agent", user: "u", sid: "s1"}
	if _, err := reg.Register(ag); err != nil {
		t.Fatalf("Register: %v", err)
	}
	r, rr := newDeleteRequest(t, "/sessions/core-agent/s1", auth.Caller{})
	mux.ServeHTTP(rr, r)

	if rr.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204; body=%q", rr.Code, rr.Body.String())
	}
	if _, err := reg.Lookup(context.Background(), "core-agent", "s1"); err == nil {
		t.Errorf("entry still resolvable after DELETE")
	}
}

// TestDeleteSession_Shortcut_Success — /sessions/{sid} single-
// segment form works too.
func TestDeleteSession_Shortcut_Success(t *testing.T) {
	t.Parallel()
	_, mux, reg := deleteHandlerFixture(t, false, nil)
	ag := &stubRegistrant{app: "core-agent", user: "u", sid: "s-shortcut"}
	if _, err := reg.Register(ag); err != nil {
		t.Fatalf("Register: %v", err)
	}
	r, rr := newDeleteRequest(t, "/sessions/s-shortcut", auth.Caller{})
	mux.ServeHTTP(rr, r)

	if rr.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204; body=%q", rr.Code, rr.Body.String())
	}
	if reg.Len() != 0 {
		t.Errorf("registry Len = %d, want 0 after DELETE", reg.Len())
	}
}

// TestDeleteSession_DefaultBootstrapGuarded — the bootstrap
// "default" session MUST be refused, even in single-user posture.
// Deleting it would leave the daemon in a broken state
// (unresolvable /events + inject for the startup agent).
func TestDeleteSession_DefaultBootstrapGuarded(t *testing.T) {
	t.Parallel()
	_, mux, reg := deleteHandlerFixture(t, false, nil)
	ag := &stubRegistrant{app: "core-agent", user: "local", sid: defaultBootstrapSessionID}
	if _, err := reg.Register(ag); err != nil {
		t.Fatalf("Register: %v", err)
	}
	r, rr := newDeleteRequest(t, "/sessions/core-agent/"+defaultBootstrapSessionID, auth.Caller{})
	mux.ServeHTTP(rr, r)

	if rr.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 for default-session delete; body=%q", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "default") {
		t.Errorf("error body should mention the guard; got %q", rr.Body.String())
	}
	// Registry entry must survive the refused delete.
	if _, err := reg.Lookup(context.Background(), "core-agent", defaultBootstrapSessionID); err != nil {
		t.Errorf("default session should still be resolvable after guard: %v", err)
	}
}

// TestDeleteSession_NotFound — unknown sid returns 404.
func TestDeleteSession_NotFound(t *testing.T) {
	t.Parallel()
	_, mux, _ := deleteHandlerFixture(t, false, nil)
	r, rr := newDeleteRequest(t, "/sessions/core-agent/does-not-exist", auth.Caller{})
	mux.ServeHTTP(rr, r)

	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404; body=%q", rr.Code, rr.Body.String())
	}
}

// TestDeleteSession_MultiSession_NonOwnerNonAdminGets404 — in
// multi-session posture, a caller that is neither Admin nor Owner
// gets 404 (masked as not-found so activity patterns aren't
// enumerable — matches the operator-endpoint convention).
func TestDeleteSession_MultiSession_NonOwnerNonAdminGets404(t *testing.T) {
	t.Parallel()
	store := newTestACLStore(t)
	_, mux, reg := deleteHandlerFixture(t, true /* enforceACL */, store)
	ag := &stubRegistrant{app: "core-agent", user: "owner-user", sid: "s1"}
	if _, err := reg.RegisterOwned(ag, "owner@example.com"); err != nil {
		t.Fatalf("RegisterOwned: %v", err)
	}

	// Caller is a stranger — not admin, not owner.
	stranger := auth.Caller{Identity: "stranger@example.com"}
	r, rr := newDeleteRequest(t, "/sessions/core-agent/s1", stranger)
	mux.ServeHTTP(rr, r)

	if rr.Code != http.StatusNotFound {
		t.Errorf("stranger delete: status = %d, want 404 (masked)", rr.Code)
	}
	// Entry must survive the denied delete.
	if _, err := reg.Lookup(context.Background(), "core-agent", "s1"); err != nil {
		t.Errorf("entry should still exist after denied delete: %v", err)
	}
}

// TestDeleteSession_MultiSession_OwnerAllowed — the session's Owner
// passes ActionSessionAdmin and can hard-delete their own session.
// Also verifies the persisted ACL row is deleted (so a subsequent
// Lookup miss can't resume via the aclStore).
func TestDeleteSession_MultiSession_OwnerAllowed(t *testing.T) {
	t.Parallel()
	store := newTestACLStore(t)
	_, mux, reg := deleteHandlerFixture(t, true, store)
	ag := &stubRegistrant{app: "core-agent", user: "owner-user", sid: "s1"}
	if _, err := reg.RegisterOwned(ag, "owner@example.com"); err != nil {
		t.Fatalf("RegisterOwned: %v", err)
	}
	// Sanity: ACL row was persisted.
	if row, err := store.Get(context.Background(), "core-agent", "owner-user", "s1"); err != nil || row.SessionID == "" {
		t.Fatalf("pre-delete ACL row missing: err=%v row=%+v", err, row)
	}

	owner := auth.Caller{Identity: "owner@example.com"}
	r, rr := newDeleteRequest(t, "/sessions/core-agent/s1", owner)
	mux.ServeHTTP(rr, r)

	if rr.Code != http.StatusNoContent {
		t.Errorf("owner delete: status = %d, want 204; body=%q", rr.Code, rr.Body.String())
	}
	// ACL row must be gone (HardDelete calls aclStore.Delete).
	if _, err := store.Get(context.Background(), "core-agent", "owner-user", "s1"); !errors.Is(err, ErrSessionACLNotFound) {
		t.Errorf("ACL row survived hard-delete; Get err=%v, want ErrSessionACLNotFound", err)
	}
}

// TestDeleteSession_MultiSession_AdminAllowed — an Admin caller
// can hard-delete any owned session, matching the standard
// ActionSessionAdmin matrix.
func TestDeleteSession_MultiSession_AdminAllowed(t *testing.T) {
	t.Parallel()
	store := newTestACLStore(t)
	_, mux, reg := deleteHandlerFixture(t, true, store)
	ag := &stubRegistrant{app: "core-agent", user: "owner-user", sid: "s1"}
	if _, err := reg.RegisterOwned(ag, "owner@example.com"); err != nil {
		t.Fatalf("RegisterOwned: %v", err)
	}

	admin := auth.Caller{Identity: "sre@example.com", Admin: true}
	r, rr := newDeleteRequest(t, "/sessions/core-agent/s1", admin)
	mux.ServeHTTP(rr, r)

	if rr.Code != http.StatusNoContent {
		t.Errorf("admin delete: status = %d, want 204; body=%q", rr.Code, rr.Body.String())
	}
}

// TestRegistry_HardDelete_IdempotentOnMissing — HardDelete on a
// key that isn't in the registry AND has no persisted row must
// not error. Matches the Unregister-idempotency contract callers
// depend on for retry paths.
func TestRegistry_HardDelete_IdempotentOnMissing(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	if err := reg.HardDelete(context.Background(), "core-agent", "u", "does-not-exist"); err != nil {
		t.Errorf("HardDelete on missing entry: %v", err)
	}
}

// TestRegistry_HardDelete_FiresCancelOnEvict — parity with
// Unregister: the wake-loop cancel must fire so tied goroutines
// don't leak past the session's lifetime.
func TestRegistry_HardDelete_FiresCancelOnEvict(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	ag := &stubRegistrant{app: "core-agent", user: "u", sid: "s1"}
	var cancelFired bool
	cancel := func() { cancelFired = true }
	if _, err := reg.RegisterOwnedWithCancel(ag, "owner@example.com", cancel); err != nil {
		t.Fatalf("RegisterOwnedWithCancel: %v", err)
	}
	if err := reg.HardDelete(context.Background(), "core-agent", "u", "s1"); err != nil {
		t.Fatalf("HardDelete: %v", err)
	}
	if !cancelFired {
		t.Errorf("cancelOnEvict never fired on HardDelete")
	}
}

// TestBroadcasterPool_Remove_ReturnsNilOnMiss — Remove must NOT
// lazily construct a broadcaster (unlike For). Missing key ⇒ nil.
func TestBroadcasterPool_Remove_ReturnsNilOnMiss(t *testing.T) {
	t.Parallel()
	pool := NewBroadcasterPool()
	entry := &Entry{AppName: "core-agent", UserID: "u", SessionID: "never-existed"}
	if b := pool.Remove(entry); b != nil {
		t.Errorf("Remove on missing entry = %v, want nil", b)
	}
}
