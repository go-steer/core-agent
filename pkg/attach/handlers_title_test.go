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
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/auth"
)

// settableRegistrant is the shape a rename-capable host has: the
// SessionTitleProvider read plus the SetSessionTitle write. It
// normalizes on the way in the way *agent.Agent does (trim, cap), so
// the tests can tell an echoed request apart from a read-back store.
type settableRegistrant struct {
	*stubRegistrant
	title string
	sets  int
}

func (s *settableRegistrant) SessionTitle() string { return s.title }

func (s *settableRegistrant) SetSessionTitle(title string) {
	s.sets++
	title = strings.TrimSpace(title)
	if r := []rune(title); len(r) > 12 {
		title = string(r[:12])
	}
	s.title = title
}

// titleFailingStore is a store that is wired and broken — SetTitle
// errors with something that is not ErrSessionACLNotFound, which is
// the difference between "nowhere to persist" and "persisting failed".
type titleFailingStore struct{ *failingStore }

func (titleFailingStore) SetTitle(context.Context, string, string, string, string) error {
	return errors.New("disk on fire")
}

func titleRouteFixture(t *testing.T, store SessionACLStore) (*http.ServeMux, *SessionRegistry) {
	t.Helper()
	reg := NewSessionRegistry()
	if store != nil {
		reg = NewSessionRegistryWithStore(store)
	}
	h := &handlers{reg: reg, pool: newBroadcasterPool(), enforceACL: true}
	mux := http.NewServeMux()
	h.registerSessionTitle(mux)
	return mux, reg
}

func decodeTitle(t *testing.T, rr *httptest.ResponseRecorder) SessionTitleResponse {
	t.Helper()
	var got SessionTitleResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal title response %q: %v", rr.Body.String(), err)
	}
	return got
}

// TestSessionTitle_RenameIsLiveAndPersisted is the #808 part-5 case end
// to end: the operator renames a session, the picker's live half shows
// the new name immediately, and the durable row carries it so the name
// survives eviction. Both halves, because GET /sessions reads the two
// from different places and a rename that only lands in one of them
// looks like the feature working right up until the session goes idle.
func TestSessionTitle_RenameIsLiveAndPersisted(t *testing.T) {
	t.Parallel()
	store := newTestACLStore(t)
	mux, reg := titleRouteFixture(t, store)
	agent := &settableRegistrant{stubRegistrant: &stubRegistrant{app: "core-agent", user: "alice@example.com", sid: "s-1"}}
	if _, err := reg.RegisterOwned(agent, "alice@example.com"); err != nil {
		t.Fatalf("RegisterOwned: %v", err)
	}

	r, rr := aclRequest(t, http.MethodPost, "/sessions/core-agent/s-1/title",
		`{"title":"  payments incident review  "}`, auth.Caller{Identity: "alice@example.com"})
	mux.ServeHTTP(rr, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", rr.Code, rr.Body.String())
	}

	got := decodeTitle(t, rr)
	// The registrant normalizes; the response must report what the
	// picker will actually show, not what the caller asked for.
	if got.Title != "payments inc" {
		t.Errorf("response title = %q, want the STORED %q — the body echoes the request instead of reading back", got.Title, "payments inc")
	}
	if !got.Persisted {
		t.Errorf("persisted = false with a store wired; body=%q", rr.Body.String())
	}
	if got.Session != "s-1" {
		t.Errorf("session = %q, want s-1", got.Session)
	}
	if agent.title != "payments inc" {
		t.Errorf("live title = %q, want the rename applied", agent.title)
	}

	row, err := store.Get(context.Background(), "core-agent", "alice@example.com", "s-1")
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if row.Title != "payments inc" {
		t.Errorf("persisted title = %q — a rename that lives only in memory reverts at the next restart", row.Title)
	}
}

// An empty title is the operator clearing a bad name, which is a real
// instruction: it has to reach the durable row too, or the cleared name
// comes back at the next restart. This is the case the eviction
// write-through deliberately can't serve — it skips empty titles so an
// untitled eviction can't wipe a hand-set name — so if PersistTitle
// routed through that path instead of SetTitle, only this test notices.
func TestSessionTitle_EmptyClearsAndPersistsTheClear(t *testing.T) {
	t.Parallel()
	store := newTestACLStore(t)
	mux, reg := titleRouteFixture(t, store)
	agent := &settableRegistrant{
		stubRegistrant: &stubRegistrant{app: "core-agent", user: "alice@example.com", sid: "s-2"},
		title:          "wrong name",
	}
	if _, err := reg.RegisterOwned(agent, "alice@example.com"); err != nil {
		t.Fatalf("RegisterOwned: %v", err)
	}
	if err := store.SetTitle(context.Background(), "core-agent", "alice@example.com", "s-2", "wrong name"); err != nil {
		t.Fatalf("seed SetTitle: %v", err)
	}

	r, rr := aclRequest(t, http.MethodPost, "/sessions/core-agent/s-2/title", `{"title":""}`, auth.Caller{Identity: "alice@example.com"})
	mux.ServeHTTP(rr, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", rr.Code, rr.Body.String())
	}
	if got := decodeTitle(t, rr); got.Title != "" || !got.Persisted {
		t.Errorf("clear response = %+v, want an empty title and persisted=true", got)
	}
	if agent.title != "" {
		t.Errorf("live title = %q after a clear", agent.title)
	}
	row, err := store.Get(context.Background(), "core-agent", "alice@example.com", "s-2")
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if row.Title != "" {
		t.Errorf("persisted title = %q after a clear, want it cleared on disk too", row.Title)
	}
}

// The title key is a pointer so an omitted one and an empty one stay
// different requests. readJSON tolerates unknown fields, so without
// this guard `{"name":"x"}` — or any typo — would decode to the zero
// value, silently CLEAR the session's title, and answer 200.
func TestSessionTitle_OmittedTitleIsRefusedNotTreatedAsAClear(t *testing.T) {
	t.Parallel()
	mux, reg := titleRouteFixture(t, nil)
	agent := &settableRegistrant{
		stubRegistrant: &stubRegistrant{app: "core-agent", user: "alice@example.com", sid: "s-3"},
		title:          "keep me",
	}
	if _, err := reg.RegisterOwned(agent, "alice@example.com"); err != nil {
		t.Fatalf("RegisterOwned: %v", err)
	}

	for _, body := range []string{`{}`, `{"name":"typo"}`} {
		r, rr := aclRequest(t, http.MethodPost, "/sessions/core-agent/s-3/title", body, auth.Caller{Identity: "alice@example.com"})
		mux.ServeHTTP(rr, r)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("body %s: status = %d, want 400; body=%q", body, rr.Code, rr.Body.String())
		}
		if !strings.Contains(rr.Body.String(), `{"title":""}`) {
			t.Errorf("body %s: the 400 should name the way to clear a title; got %q", body, rr.Body.String())
		}
	}
	if agent.sets != 0 {
		t.Errorf("SetSessionTitle called %d times on a refused request, want 0", agent.sets)
	}
	if agent.title != "keep me" {
		t.Errorf("title = %q after refused requests, want unchanged", agent.title)
	}
}

// A host that never wired the capability gets 501, not a 200 that did
// nothing. The read half (#808's inferred titles) shipped as an
// optional interface, so a registrant with SessionTitle() and no
// SetSessionTitle is a real, currently-shipping shape — including
// every subagent registrant.
func TestSessionTitle_UnsettableCapabilityIs501(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		agent Registrant
	}{
		{"plain registrant", &stubRegistrant{app: "core-agent", user: "u", sid: "s-4"}},
		{"read-only title provider", &titledRegistrant{
			stubRegistrant: &stubRegistrant{app: "core-agent", user: "u", sid: "s-4"},
			title:          "inferred",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mux, reg := titleRouteFixture(t, nil)
			if _, err := reg.RegisterOwned(tc.agent, "alice@example.com"); err != nil {
				t.Fatalf("RegisterOwned: %v", err)
			}
			r, rr := aclRequest(t, http.MethodPost, "/sessions/core-agent/s-4/title", `{"title":"x"}`, auth.Caller{Identity: "alice@example.com"})
			mux.ServeHTTP(rr, r)
			if rr.Code != http.StatusNotImplemented {
				t.Errorf("status = %d, want 501; body=%q", rr.Code, rr.Body.String())
			}
		})
	}
}

// persisted=false is two different situations and the body has to tell
// them apart: a daemon with no ACL store at all (the single-session
// --attach-listen case, where the rename is simply process-lifetime and
// there is nothing to report) versus a store that was wired and failed
// (where the operator's rename WILL revert and they should know). Both
// are 200, because the rename did take effect either way.
func TestSessionTitle_PersistedFalseCases(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		store      SessionACLStore
		wantDetail bool
	}{
		{"no store wired", nil, false},
		{"store wired and failing", titleFailingStore{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mux, reg := titleRouteFixture(t, tc.store)
			agent := &settableRegistrant{stubRegistrant: &stubRegistrant{app: "core-agent", user: "u", sid: "s-5"}}
			// Registered through the un-persisted path and given an
			// owner after the fact: RegisterOwned Puts the row, which
			// the failing store rejects before the test starts.
			if _, err := reg.Register(agent); err != nil {
				t.Fatalf("Register: %v", err)
			}
			reg.List()[0].setACL(auth.SessionACL{Owner: "alice@example.com"})
			r, rr := aclRequest(t, http.MethodPost, "/sessions/core-agent/s-5/title", `{"title":"renamed"}`, auth.Caller{Identity: "alice@example.com"})
			mux.ServeHTTP(rr, r)
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%q", rr.Code, rr.Body.String())
			}
			got := decodeTitle(t, rr)
			if got.Persisted {
				t.Errorf("persisted = true, want false")
			}
			if got.Title != "renamed" {
				t.Errorf("title = %q — the rename is live regardless of persistence", got.Title)
			}
			if hasDetail := got.Detail != ""; hasDetail != tc.wantDetail {
				t.Errorf("detail = %q, wantDetail = %v", got.Detail, tc.wantDetail)
			}
			if agent.title != "renamed" {
				t.Errorf("live title = %q, want the rename applied even though persistence didn't", agent.title)
			}
		})
	}
}

// ActionSessionWrite, deliberately: a title is a display label, and the
// people who should be able to fix a wrong name are the people working
// in the session. A viewer must still not be able to rename it — a
// read-only participant relabelling other people's sessions is exactly
// the confusion the ACL exists to prevent.
func TestSessionTitle_RequiresSessionWrite(t *testing.T) {
	t.Parallel()
	mux, reg := titleRouteFixture(t, nil)
	agent := &settableRegistrant{stubRegistrant: &stubRegistrant{app: "core-agent", user: "u", sid: "s-6"}}
	if _, err := reg.RegisterOwnedWithACL(agent, auth.SessionACL{
		Owner:        "owner@example.com",
		Viewers:      []string{"viewer@example.com"},
		Contributors: []string{"contrib@example.com"},
	}, nil); err != nil {
		t.Fatalf("RegisterOwnedWithACL: %v", err)
	}

	for _, tc := range []struct {
		name   string
		caller auth.Caller
		want   int
	}{
		{"owner", auth.Caller{Identity: "owner@example.com"}, http.StatusOK},
		{"contributor", auth.Caller{Identity: "contrib@example.com"}, http.StatusOK},
		{"admin", auth.Caller{Identity: "ops@example.com", Admin: true}, http.StatusOK},
		{"viewer", auth.Caller{Identity: "viewer@example.com"}, http.StatusNotFound},
		{"stranger", auth.Caller{Identity: "nobody@example.com"}, http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, rr := aclRequest(t, http.MethodPost, "/sessions/core-agent/s-6/title", `{"title":"by `+tc.name+`"}`, tc.caller)
			mux.ServeHTTP(rr, r)
			if rr.Code != tc.want {
				t.Errorf("status = %d, want %d; body=%q", rr.Code, tc.want, rr.Body.String())
			}
		})
	}
	if agent.title != "by admin" {
		t.Errorf("title = %q — the denied renames must have been no-ops, leaving the last allowed one", agent.title)
	}
}

func TestSessionTitle_ShortcutFormAndMalformedBody(t *testing.T) {
	t.Parallel()
	mux, reg := titleRouteFixture(t, nil)
	agent := &settableRegistrant{stubRegistrant: &stubRegistrant{app: "core-agent", user: "u", sid: "s-7"}}
	if _, err := reg.RegisterOwned(agent, "alice@example.com"); err != nil {
		t.Fatalf("RegisterOwned: %v", err)
	}
	alice := auth.Caller{Identity: "alice@example.com"}

	r, rr := aclRequest(t, http.MethodPost, "/sessions/s-7/title", `{"title":"shortcut"}`, alice)
	mux.ServeHTTP(rr, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("shortcut form: status = %d, want 200; body=%q", rr.Code, rr.Body.String())
	}
	if agent.title != "shortcut" {
		t.Errorf("title = %q after the shortcut-form POST", agent.title)
	}

	for _, body := range []string{"", "{", `{"title":42}`} {
		r, rr := aclRequest(t, http.MethodPost, "/sessions/core-agent/s-7/title", body, alice)
		mux.ServeHTTP(rr, r)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("body %q: status = %d, want 400; body=%q", body, rr.Code, rr.Body.String())
		}
	}
}
