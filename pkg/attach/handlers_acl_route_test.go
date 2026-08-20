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
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/auth"
)

// aclRouteFixture wires handlers + registry + mux with the ACL routes
// registered, so tests drive the real pattern-match + lookup + authorize
// chain rather than calling the handler methods directly.
func aclRouteFixture(t *testing.T, store SessionACLStore) (*http.ServeMux, *SessionRegistry) {
	t.Helper()
	reg := NewSessionRegistry()
	if store != nil {
		reg = NewSessionRegistryWithStore(store)
	}
	h := &handlers{reg: reg, pool: newBroadcasterPool(), enforceACL: true}
	mux := http.NewServeMux()
	h.registerSessionACL(mux)
	// /inject is what a Contributor is actually here to reach; wiring
	// it lets the end-to-end test assert the capability rather than
	// the bookkeeping. Registered without the shutdown drain gate the
	// daemon uses — these tests are about who may write, not about
	// what happens to a write during shutdown.
	h.routeSession(mux, "POST", "inject", auth.ActionSessionWrite, h.doInject)
	return mux, reg
}

func aclRequest(t *testing.T, method, path, body string, caller auth.Caller) (*http.Request, *httptest.ResponseRecorder) {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r = r.WithContext(auth.WithCaller(r.Context(), caller))
	return r, httptest.NewRecorder()
}

func decodeACL(t *testing.T, rr *httptest.ResponseRecorder) sessionACLResponse {
	t.Helper()
	var got sessionACLResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal acl response %q: %v", rr.Body.String(), err)
	}
	return got
}

// TestSessionACL_ContributorCanWriteAfterPatch is the #797 case end to
// end, and the only test here that would still matter if the others
// were deleted: an identity that is nobody on the session is 404'd by
// /inject, the owner PATCHes it into `contributors`, and the same
// request now lands. Before this change there was no request a caller
// could make that would flip that outcome.
func TestSessionACL_ContributorCanWriteAfterPatch(t *testing.T) {
	t.Parallel()
	mux, reg := aclRouteFixture(t, nil)
	if _, err := reg.RegisterOwned(&stubRegistrant{app: "core-agent", user: "u", sid: "s-inc"}, "lookout@example.com"); err != nil {
		t.Fatalf("RegisterOwned: %v", err)
	}
	responder := auth.Caller{Identity: "oncall@example.com"}

	r, rr := aclRequest(t, http.MethodPost, "/sessions/core-agent/s-inc/inject", `{"message":"what broke?"}`, responder)
	mux.ServeHTTP(rr, r)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("pre-PATCH inject by a non-ACL identity: status = %d, want 404 (the masked deny); body=%q", rr.Code, rr.Body.String())
	}

	r, rr = aclRequest(t, http.MethodPatch, "/sessions/core-agent/s-inc/acl",
		`{"contributors":["oncall@example.com"]}`, auth.Caller{Identity: "lookout@example.com"})
	mux.ServeHTTP(rr, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("PATCH /acl by owner: status = %d, want 200; body=%q", rr.Code, rr.Body.String())
	}
	if got := decodeACL(t, rr); !reflect.DeepEqual(got.Contributors, []string{"oncall@example.com"}) {
		t.Errorf("PATCH response contributors = %v, want [oncall@example.com]", got.Contributors)
	}

	r, rr = aclRequest(t, http.MethodPost, "/sessions/core-agent/s-inc/inject", `{"message":"what broke?"}`, responder)
	mux.ServeHTTP(rr, r)
	if rr.Code != http.StatusOK {
		t.Errorf("post-PATCH inject by the new contributor: status = %d, want 200; body=%q", rr.Code, rr.Body.String())
	}
}

// TestSessionACL_PatchIsOwnerOnly pins the gate. A Contributor has
// read+write on the session and must still not be able to widen the
// ACL — otherwise the first identity you add can add everyone else,
// and ActionSessionAdmin stops meaning anything.
func TestSessionACL_PatchIsOwnerOnly(t *testing.T) {
	t.Parallel()
	mux, reg := aclRouteFixture(t, nil)
	if _, err := reg.RegisterOwnedWithACL(&stubRegistrant{app: "core-agent", user: "u", sid: "s-1"}, auth.SessionACL{
		Owner:        "owner@example.com",
		Viewers:      []string{"viewer@example.com"},
		Contributors: []string{"contrib@example.com"},
	}, nil); err != nil {
		t.Fatalf("RegisterOwnedWithACL: %v", err)
	}

	for _, tc := range []struct {
		name   string
		caller auth.Caller
		method string
		want   int
	}{
		{"contributor patch", auth.Caller{Identity: "contrib@example.com"}, http.MethodPatch, http.StatusNotFound},
		{"contributor read", auth.Caller{Identity: "contrib@example.com"}, http.MethodGet, http.StatusNotFound},
		{"viewer read", auth.Caller{Identity: "viewer@example.com"}, http.MethodGet, http.StatusNotFound},
		{"stranger read", auth.Caller{Identity: "nobody@example.com"}, http.MethodGet, http.StatusNotFound},
		{"owner read", auth.Caller{Identity: "owner@example.com"}, http.MethodGet, http.StatusOK},
		{"admin read", auth.Caller{Identity: "ops@example.com", Admin: true}, http.MethodGet, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, rr := aclRequest(t, tc.method, "/sessions/core-agent/s-1/acl", `{"viewers":["mallory@example.com"]}`, tc.caller)
			mux.ServeHTTP(rr, r)
			if rr.Code != tc.want {
				t.Errorf("status = %d, want %d; body=%q", rr.Code, tc.want, rr.Body.String())
			}
		})
	}
	// And the denied PATCH really was a no-op, not merely a 404 on the
	// way out.
	if got := reg.List()[0].CurrentACL().Viewers; !reflect.DeepEqual(got, []string{"viewer@example.com"}) {
		t.Errorf("viewers after the denied PATCH = %v, want the original [viewer@example.com]", got)
	}
}

// TestSessionACL_PatchLeavesOmittedListsAlone is what makes this a
// PATCH. `viewers` and `contributors` are *[]string so an omitted
// field is distinguishable from `[]`: omitted means "leave it", `[]`
// means "clear it". With plain slices, adding a contributor would
// silently wipe the viewers.
func TestSessionACL_PatchLeavesOmittedListsAlone(t *testing.T) {
	t.Parallel()
	mux, reg := aclRouteFixture(t, nil)
	if _, err := reg.RegisterOwnedWithACL(&stubRegistrant{app: "core-agent", user: "u", sid: "s-2"}, auth.SessionACL{
		Owner:        "owner@example.com",
		Viewers:      []string{"viewer@example.com"},
		Contributors: []string{"contrib@example.com"},
	}, nil); err != nil {
		t.Fatalf("RegisterOwnedWithACL: %v", err)
	}
	owner := auth.Caller{Identity: "owner@example.com"}

	r, rr := aclRequest(t, http.MethodPatch, "/sessions/core-agent/s-2/acl",
		`{"contributors":["contrib@example.com","second@example.com"]}`, owner)
	mux.ServeHTTP(rr, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", rr.Code, rr.Body.String())
	}
	got := decodeACL(t, rr)
	if !reflect.DeepEqual(got.Viewers, []string{"viewer@example.com"}) {
		t.Errorf("viewers = %v after a contributors-only PATCH; an omitted list must be left alone", got.Viewers)
	}
	if !reflect.DeepEqual(got.Contributors, []string{"contrib@example.com", "second@example.com"}) {
		t.Errorf("contributors = %v", got.Contributors)
	}

	// An explicit empty list is the clear instruction.
	r, rr = aclRequest(t, http.MethodPatch, "/sessions/core-agent/s-2/acl", `{"viewers":[]}`, owner)
	mux.ServeHTTP(rr, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("clear: status = %d, want 200; body=%q", rr.Code, rr.Body.String())
	}
	got = decodeACL(t, rr)
	if len(got.Viewers) != 0 {
		t.Errorf("viewers = %v after `\"viewers\":[]`, want cleared", got.Viewers)
	}
	if len(got.Contributors) != 2 {
		t.Errorf("contributors = %v; clearing viewers must not touch them", got.Contributors)
	}
}

// TestSessionACL_PatchRefusesOwnerChange — the field is accepted only
// so it can be refused with a reason. Silently dropping it would leave
// the caller believing it transferred a session, which is the same
// class of invisible failure #797 was filed about.
func TestSessionACL_PatchRefusesOwnerChange(t *testing.T) {
	t.Parallel()
	mux, reg := aclRouteFixture(t, nil)
	if _, err := reg.RegisterOwned(&stubRegistrant{app: "core-agent", user: "u", sid: "s-3"}, "owner@example.com"); err != nil {
		t.Fatalf("RegisterOwned: %v", err)
	}
	owner := auth.Caller{Identity: "owner@example.com"}

	r, rr := aclRequest(t, http.MethodPatch, "/sessions/core-agent/s-3/acl", `{"owner":"someone@example.com"}`, owner)
	mux.ServeHTTP(rr, r)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("owner change: status = %d, want 400; body=%q", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "not transferable") {
		t.Errorf("400 body should say why; got %q", rr.Body.String())
	}
	if got := reg.List()[0].CurrentACL().Owner; got != "owner@example.com" {
		t.Errorf("owner = %q after the refused PATCH, want unchanged", got)
	}

	// An explicitly empty owner is a transfer to nobody. The registry
	// reads a zero Owner as "the mutator left it alone", so without the
	// handler's own guard this would 200 as a no-op and the caller would
	// believe it un-owned the session.
	r, rr = aclRequest(t, http.MethodPatch, "/sessions/core-agent/s-3/acl", `{"owner":""}`, owner)
	mux.ServeHTTP(rr, r)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("clearing owner: status = %d, want 400; body=%q", rr.Code, rr.Body.String())
	}

	// Echoing back the CURRENT owner is accepted — a client that GETs,
	// edits and PATCHes the whole document shouldn't have to strip the
	// field.
	r, rr = aclRequest(t, http.MethodPatch, "/sessions/core-agent/s-3/acl",
		`{"owner":"owner@example.com","viewers":["v@example.com"]}`, owner)
	mux.ServeHTTP(rr, r)
	if rr.Code != http.StatusOK {
		t.Errorf("round-tripping the current owner: status = %d, want 200; body=%q", rr.Code, rr.Body.String())
	}
}

// TestSessionACL_PatchNormalizesIdentities — the lists cross a trust
// boundary, and containsIdentity matches exactly, so a pasted
// identity with a trailing space produces an ACL that reads correct
// and denies anyway. The denial surfaces as 404, which makes it about
// the least debuggable failure the API has.
func TestSessionACL_PatchNormalizesIdentities(t *testing.T) {
	t.Parallel()
	mux, reg := aclRouteFixture(t, nil)
	if _, err := reg.RegisterOwned(&stubRegistrant{app: "core-agent", user: "u", sid: "s-4"}, "owner@example.com"); err != nil {
		t.Fatalf("RegisterOwned: %v", err)
	}

	r, rr := aclRequest(t, http.MethodPatch, "/sessions/core-agent/s-4/acl",
		`{"contributors":["  a@example.com  ","","a@example.com","b@example.com","   "]}`,
		auth.Caller{Identity: "owner@example.com"})
	mux.ServeHTTP(rr, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", rr.Code, rr.Body.String())
	}
	want := []string{"a@example.com", "b@example.com"}
	if got := decodeACL(t, rr).Contributors; !reflect.DeepEqual(got, want) {
		t.Errorf("contributors = %v, want %v (trimmed, empties dropped, de-duplicated, order preserved)", got, want)
	}
	if got := reg.List()[0].CurrentACL().Contributors; !reflect.DeepEqual(got, want) {
		t.Errorf("stored contributors = %v, want %v — the response must report what actually authorizes", got, want)
	}
}

// TestSessionACL_PatchPersistsAndPreservesRowMetadata — an ACL that
// only lives in memory un-does itself at the next restart, and nothing
// surfaces the divergence until a responder is silently 404'd by the
// resumed session. CreatedAt is checked because Put is a whole-row
// upsert that stamps now() over a zero value: writing the row without
// carrying it forward would make every ACL edit look like a brand-new
// session in the operator's list.
func TestSessionACL_PatchPersistsAndPreservesRowMetadata(t *testing.T) {
	t.Parallel()
	store := newTestACLStore(t)
	mux, reg := aclRouteFixture(t, store)
	if _, err := reg.RegisterOwned(&stubRegistrant{app: "core-agent", user: "u", sid: "s-5"}, "owner@example.com"); err != nil {
		t.Fatalf("RegisterOwned: %v", err)
	}
	before, err := store.Get(context.Background(), "core-agent", "u", "s-5")
	if err != nil {
		t.Fatalf("store.Get before: %v", err)
	}
	if err := store.SetTitle(context.Background(), "core-agent", "u", "s-5", "payments incident"); err != nil {
		t.Fatalf("SetTitle: %v", err)
	}

	r, rr := aclRequest(t, http.MethodPatch, "/sessions/core-agent/s-5/acl",
		`{"contributors":["oncall@example.com"]}`, auth.Caller{Identity: "owner@example.com"})
	mux.ServeHTTP(rr, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", rr.Code, rr.Body.String())
	}

	after, err := store.Get(context.Background(), "core-agent", "u", "s-5")
	if err != nil {
		t.Fatalf("store.Get after: %v", err)
	}
	if !reflect.DeepEqual(after.Contributors, []string{"oncall@example.com"}) {
		t.Errorf("persisted contributors = %v; an in-memory-only ACL silently reverts on restart", after.Contributors)
	}
	if !after.CreatedAt.Equal(before.CreatedAt) {
		t.Errorf("CreatedAt = %v, want the original %v — Put stamps now() over a zero value", after.CreatedAt, before.CreatedAt)
	}
	if after.Title != "payments incident" {
		t.Errorf("Title = %q, want %q — an ACL edit must not blank the session's label", after.Title, "payments incident")
	}
}

// TestSessionACL_PatchRollsBackWhenPersistenceFails mirrors
// registerWithACL's rollback. Reporting 200 for an ACL that vanishes
// at the next restart is the worst of the three outcomes: the caller
// stops retrying and the divergence stays invisible until the session
// resumes.
func TestSessionACL_PatchRollsBackWhenPersistenceFails(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistryWithStore(&failingStore{})
	h := &handlers{reg: reg, pool: newBroadcasterPool(), enforceACL: true}
	mux := http.NewServeMux()
	h.registerSessionACL(mux)
	// registerWithACL rolls back a failed Put, so seed the entry
	// through the un-persisted path and then attach the failing store.
	if _, err := reg.Register(&stubRegistrant{app: "core-agent", user: "u", sid: "s-6"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	reg.List()[0].setACL(auth.SessionACL{Owner: "owner@example.com", Viewers: []string{"before@example.com"}})

	r, rr := aclRequest(t, http.MethodPatch, "/sessions/core-agent/s-6/acl",
		`{"contributors":["oncall@example.com"]}`, auth.Caller{Identity: "owner@example.com"})
	mux.ServeHTTP(rr, r)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%q", rr.Code, rr.Body.String())
	}
	got := reg.List()[0].CurrentACL()
	if len(got.Contributors) != 0 {
		t.Errorf("contributors = %v after a failed persist, want rolled back", got.Contributors)
	}
	if !reflect.DeepEqual(got.Viewers, []string{"before@example.com"}) {
		t.Errorf("viewers = %v after a failed persist, want the pre-PATCH %v", got.Viewers, []string{"before@example.com"})
	}
}

// TestSessionACL_GetShape pins the read. Both lists are emitted even
// when empty: this is the endpoint whose whole purpose is reporting
// who is on the ACL, so a client that had to distinguish a missing key
// from an empty list would be back to guessing.
func TestSessionACL_GetShape(t *testing.T) {
	t.Parallel()
	mux, reg := aclRouteFixture(t, nil)
	if _, err := reg.RegisterOwned(&stubRegistrant{app: "core-agent", user: "u", sid: "s-7"}, "owner@example.com"); err != nil {
		t.Fatalf("RegisterOwned: %v", err)
	}
	r, rr := aclRequest(t, http.MethodGet, "/sessions/core-agent/s-7/acl", "", auth.Caller{Identity: "owner@example.com"})
	mux.ServeHTTP(rr, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", rr.Code, rr.Body.String())
	}
	if got, want := strings.TrimSpace(rr.Body.String()), `{"owner":"owner@example.com","viewers":[],"contributors":[]}`; got != want {
		t.Errorf("body = %s\nwant     %s", got, want)
	}
}

// TestSessionACL_ShortcutFormRoutes — every session-scoped endpoint is
// reachable both ways, and a client on the one-segment form is the
// common case.
func TestSessionACL_ShortcutFormRoutes(t *testing.T) {
	t.Parallel()
	mux, reg := aclRouteFixture(t, nil)
	if _, err := reg.RegisterOwned(&stubRegistrant{app: "core-agent", user: "u", sid: "s-8"}, "owner@example.com"); err != nil {
		t.Fatalf("RegisterOwned: %v", err)
	}
	r, rr := aclRequest(t, http.MethodPatch, "/sessions/s-8/acl",
		`{"viewers":["v@example.com"]}`, auth.Caller{Identity: "owner@example.com"})
	mux.ServeHTTP(rr, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("shortcut PATCH: status = %d, want 200; body=%q", rr.Code, rr.Body.String())
	}
	r, rr = aclRequest(t, http.MethodGet, "/sessions/s-8/acl", "", auth.Caller{Identity: "owner@example.com"})
	mux.ServeHTTP(rr, r)
	if got := decodeACL(t, rr); !reflect.DeepEqual(got.Viewers, []string{"v@example.com"}) {
		t.Errorf("shortcut GET viewers = %v", got.Viewers)
	}
}

// TestSessionACL_PatchMalformedBody — a bad body is the caller's
// error, not a 500, and an absent one is too: PATCH with nothing to
// apply is a request that means nothing.
func TestSessionACL_PatchMalformedBody(t *testing.T) {
	t.Parallel()
	mux, reg := aclRouteFixture(t, nil)
	if _, err := reg.RegisterOwned(&stubRegistrant{app: "core-agent", user: "u", sid: "s-9"}, "owner@example.com"); err != nil {
		t.Fatalf("RegisterOwned: %v", err)
	}
	for _, body := range []string{"", "{", `{"viewers":"not-a-list"}`} {
		r, rr := aclRequest(t, http.MethodPatch, "/sessions/core-agent/s-9/acl", body, auth.Caller{Identity: "owner@example.com"})
		mux.ServeHTTP(rr, r)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("body %q: status = %d, want 400; body=%q", body, rr.Code, rr.Body.String())
		}
	}
}

// TestProtocolVersion_CoversSessionACLRoutes pins the version half of
// the change, following TestProtocolVersion_CoversCanceledKind. New
// endpoints have nothing on the capabilities frame a consumer can
// feature-detect, and a pre-1.10.0 daemon answers the new paths with
// the same 404 it gives an unauthorized caller — so protocol_version
// is the only honest signal, which makes forgetting the bump silent.
// A floor rather than an equality so a later additive minor doesn't
// have to edit this test.
func TestProtocolVersion_CoversSessionACLRoutes(t *testing.T) {
	t.Parallel()
	major, minor, ok := protocolMajorMinor(protocolVersion)
	if !ok {
		t.Fatalf("protocolVersion = %q, not parseable as major.minor", protocolVersion)
	}
	if major != 1 || minor < 10 {
		t.Errorf("protocolVersion = %q, want >= 1.10.0 (GET/PATCH /sessions/{sid}/acl, #797)", protocolVersion)
	}
}

// TestSessionACL_ConcurrentPatchAndAuthorize is the guard on the
// change that made the ACL mutable at all. Entry.ACL used to be a
// plain exported field, safe only because nothing wrote it after
// registration; a PATCH route turns every authorize() into a
// concurrent read of a field being written. Run under -race, which CI
// does, this fails on a plain field and passes on the atomic.
func TestSessionACL_ConcurrentPatchAndAuthorize(t *testing.T) {
	t.Parallel()
	mux, reg := aclRouteFixture(t, nil)
	if _, err := reg.RegisterOwned(&stubRegistrant{app: "core-agent", user: "u", sid: "s-race"}, "owner@example.com"); err != nil {
		t.Fatalf("RegisterOwned: %v", err)
	}
	owner := auth.Caller{Identity: "owner@example.com"}

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			body := `{"contributors":["a@example.com"]}`
			if i%2 == 0 {
				body = `{"contributors":["b@example.com","c@example.com"]}`
			}
			r, rr := aclRequest(t, http.MethodPatch, "/sessions/core-agent/s-race/acl", body, owner)
			mux.ServeHTTP(rr, r)
		}()
		go func() {
			defer wg.Done()
			r, rr := aclRequest(t, http.MethodGet, "/sessions/core-agent/s-race/acl", "", owner)
			mux.ServeHTTP(rr, r)
			if rr.Code != http.StatusOK {
				t.Errorf("concurrent GET: status = %d", rr.Code)
			}
		}()
	}
	wg.Wait()
}

// TestSessionACL_ConcurrentPatchesDontLoseUpdates is the lost-update
// regression, and it is a different bug from the data race above: no
// amount of atomics fixes it. PATCH is read-modify-write — an omitted
// list means "leave it" — so if the handler reads the ACL, computes the
// merge, and only then asks the registry to store it, two PATCHes that
// touch different lists each carry the other's field forward from a
// snapshot taken before the other landed. One edit disappears with a
// 200 and no trace, and the thing that disappeared is an authorization
// decision: the responder the operator just added can't reach the
// session, and finds out as a 404.
//
// The fix is to run the merge inside the registry lock, which is why
// AmendACL takes a mutator instead of a value. Repeated rounds because
// the losing interleaving is a race, not a certainty — against the
// read-outside-the-lock shape this fails within the first handful.
func TestSessionACL_ConcurrentPatchesDontLoseUpdates(t *testing.T) {
	t.Parallel()
	mux, reg := aclRouteFixture(t, nil)
	if _, err := reg.RegisterOwned(&stubRegistrant{app: "core-agent", user: "u", sid: "s-lost"}, "owner@example.com"); err != nil {
		t.Fatalf("RegisterOwned: %v", err)
	}
	owner := auth.Caller{Identity: "owner@example.com"}
	patch := func(body string) {
		r, rr := aclRequest(t, http.MethodPatch, "/sessions/core-agent/s-lost/acl", body, owner)
		mux.ServeHTTP(rr, r)
		if rr.Code != http.StatusOK {
			t.Errorf("PATCH %s: status = %d; body=%q", body, rr.Code, rr.Body.String())
		}
	}
	for round := range 200 {
		patch(`{"viewers":[],"contributors":[]}`)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); patch(`{"viewers":["v@example.com"]}`) }()
		go func() { defer wg.Done(); patch(`{"contributors":["c@example.com"]}`) }()
		wg.Wait()

		r, rr := aclRequest(t, http.MethodGet, "/sessions/core-agent/s-lost/acl", "", owner)
		mux.ServeHTTP(rr, r)
		got := decodeACL(t, rr)
		if len(got.Viewers) != 1 || len(got.Contributors) != 1 {
			t.Fatalf("round %d lost an update: viewers = %v, contributors = %v", round, got.Viewers, got.Contributors)
		}
	}
}
