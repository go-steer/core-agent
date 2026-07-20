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
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/auth"
)

func TestAuthorize_DisabledIsAlwaysAllow(t *testing.T) {
	t.Parallel()
	// With enforceACL=false (single-user / pre-multi-session
	// deployments), authorize must short-circuit to allow regardless
	// of Caller or ACL — preserves the α.1 no-behavior-change posture.
	h := &handlers{enforceACL: false}
	entry := &Entry{ACL: auth.SessionACL{Owner: "owner@example.com"}}
	rr := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	// Stranger context — would deny under enforcement.
	r = r.WithContext(auth.WithCaller(r.Context(), auth.Caller{Identity: "stranger@example.com"}))

	if !h.authorize(rr, r, entry, auth.ActionSessionRead) {
		t.Error("enforceACL=false must always allow")
	}
}

func TestAuthorize_OwnerSeesOwnSession(t *testing.T) {
	t.Parallel()
	h := &handlers{enforceACL: true}
	entry := &Entry{ACL: auth.SessionACL{Owner: "owner@example.com"}}
	rr := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r = r.WithContext(auth.WithCaller(r.Context(), auth.Caller{Identity: "owner@example.com"}))

	if !h.authorize(rr, r, entry, auth.ActionSessionRead) {
		t.Error("owner must pass SessionRead on their own session")
	}
	// Reset response for the next call.
	rr = httptest.NewRecorder()
	if !h.authorize(rr, r, entry, auth.ActionSessionWrite) {
		t.Error("owner must pass SessionWrite on their own session")
	}
}

func TestAuthorize_StrangerGets404NotForbidden(t *testing.T) {
	t.Parallel()
	// The design's "no leaking activity patterns" invariant: deny
	// surfaces as 404, not 403 — an attacker can't distinguish "you
	// don't have access" from "this session doesn't exist."
	h := &handlers{enforceACL: true}
	entry := &Entry{ACL: auth.SessionACL{Owner: "owner@example.com"}}
	rr := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r = r.WithContext(auth.WithCaller(r.Context(), auth.Caller{Identity: "stranger@example.com"}))

	if h.authorize(rr, r, entry, auth.ActionSessionRead) {
		t.Fatal("stranger must NOT be authorized for SessionRead")
	}
	if rr.Code != http.StatusNotFound {
		t.Errorf("denied auth must return 404 (not 403, intentionally — see design doc); got %d", rr.Code)
	}
}

func TestAuthorize_ContributorWriteAllowedAdminDenied(t *testing.T) {
	t.Parallel()
	h := &handlers{enforceACL: true}
	entry := &Entry{ACL: auth.SessionACL{
		Owner:        "owner@example.com",
		Contributors: []string{"contrib@example.com"},
	}}
	rr := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r = r.WithContext(auth.WithCaller(r.Context(), auth.Caller{Identity: "contrib@example.com"}))

	if !h.authorize(rr, r, entry, auth.ActionSessionWrite) {
		t.Error("contributor must pass SessionWrite")
	}
	rr = httptest.NewRecorder()
	if h.authorize(rr, r, entry, auth.ActionSessionAdmin) {
		t.Error("contributor must NOT pass SessionAdmin (modify ACL is owner-only)")
	}
	if rr.Code != http.StatusNotFound {
		t.Errorf("contributor denied SessionAdmin must 404 (not 403); got %d", rr.Code)
	}
}

func TestAuthorize_ViewerReadOnly(t *testing.T) {
	t.Parallel()
	h := &handlers{enforceACL: true}
	entry := &Entry{ACL: auth.SessionACL{
		Owner:   "owner@example.com",
		Viewers: []string{"viewer@example.com"},
	}}
	rr := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r = r.WithContext(auth.WithCaller(r.Context(), auth.Caller{Identity: "viewer@example.com"}))

	if !h.authorize(rr, r, entry, auth.ActionSessionRead) {
		t.Error("viewer must pass SessionRead")
	}
	rr = httptest.NewRecorder()
	if h.authorize(rr, r, entry, auth.ActionSessionWrite) {
		t.Error("viewer must NOT pass SessionWrite")
	}
}

func TestAuthorize_AdminBypassesEverything(t *testing.T) {
	t.Parallel()
	h := &handlers{enforceACL: true}
	// Unowned legacy entry (Owner="") — even this should grant Admin
	// access, since Admin trumps the ACL check.
	entry := &Entry{ACL: auth.SessionACL{}}
	rr := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r = r.WithContext(auth.WithCaller(r.Context(), auth.Caller{Identity: "ops@example.com", Admin: true}))

	if !h.authorize(rr, r, entry, auth.ActionSessionAdmin) {
		t.Error("admin must bypass every ACL check, including for legacy unowned sessions")
	}
}
