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
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/auth"
)

// Regression tests for #384: peer endpoints were accepted unvalidated
// (credential exfiltration via hostile Endpoint strings), deregister
// had no authorization, and GET /peers returned every registration_id
// — any caller could enumerate and delete all peers.

func TestPeerRegistry_EndpointValidation(t *testing.T) {
	t.Parallel()
	r := NewPeerRegistry()
	defer func() { _ = r.Close() }()

	bad := []string{
		"javascript:alert(1)",
		"/relative/path",
		"relative",
		"ftp://host:21/dir",
		"unix:///var/run/agent.sock",
		"http://",   // no host
		"https://",  // no host
		"//host:80", // scheme-less
		"http:opaque",
	}
	for _, ep := range bad {
		if _, err := r.Register(RegisterRequest{Name: "n", Endpoint: ep}); !errors.Is(err, ErrPeerEndpointInvalid) {
			t.Errorf("Register(Endpoint=%q): want ErrPeerEndpointInvalid, got %v", ep, err)
		}
	}

	good := []string{
		"http://10.0.0.1:7777",
		"https://agent.example.com",
		"https://[::1]:7777",
	}
	for i, ep := range good {
		if _, err := r.Register(RegisterRequest{Name: "g" + string(rune('a'+i)), Endpoint: ep}); err != nil {
			t.Errorf("Register(Endpoint=%q): want ok, got %v", ep, err)
		}
	}
}

// withCaller stamps a fixed Caller onto every request — stands in for
// the caller middleware so handler-level authorization is exercised
// in isolation.
func withCaller(c auth.Caller, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(auth.WithCaller(r.Context(), c)))
	})
}

// peerHubMux builds the peer handler mux in multi-session posture
// (requireAuth on, anon identity "anon").
func peerHubMux(reg *PeerRegistry) *http.ServeMux {
	mux := http.NewServeMux()
	newPeerHandlers(reg, true, "anon").register(mux)
	return mux
}

func doPeerReq(t *testing.T, h http.Handler, c auth.Caller, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	withCaller(c, h).ServeHTTP(rec, req)
	return rec
}

func TestPeerHandlers_RegisterRejectsBadEndpoint(t *testing.T) {
	t.Parallel()
	reg := NewPeerRegistry()
	defer func() { _ = reg.Close() }()
	mux := peerHubMux(reg)
	alice := auth.Caller{Identity: "alice"}

	for _, ep := range []string{"javascript:alert(1)", "/relative", "ftp://x/y", "http://"} {
		body, _ := json.Marshal(RegisterRequest{Name: "n", Endpoint: ep})
		rec := doPeerReq(t, mux, alice, http.MethodPost, "/peers", string(body))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("POST /peers with Endpoint=%q = %d, want 400", ep, rec.Code)
		}
	}
	if reg.Len() != 0 {
		t.Fatalf("no bad endpoint should have registered; Len = %d", reg.Len())
	}
}

func TestPeerHandlers_DeregisterOwnerScoped(t *testing.T) {
	t.Parallel()
	reg := NewPeerRegistry()
	defer func() { _ = reg.Close() }()
	mux := peerHubMux(reg)
	alice := auth.Caller{Identity: "alice"}
	bob := auth.Caller{Identity: "bob"}
	admin := auth.Caller{Identity: "root", Admin: true}

	registerAs := func(c auth.Caller, name string) string {
		body, _ := json.Marshal(RegisterRequest{Name: name, Endpoint: "http://10.0.0.9:7777"})
		rec := doPeerReq(t, mux, c, http.MethodPost, "/peers", string(body))
		if rec.Code != http.StatusCreated {
			t.Fatalf("register as %s: %d: %s", c.Identity, rec.Code, rec.Body.String())
		}
		var p Peer
		if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
			t.Fatalf("decode register response: %v", err)
		}
		if p.RegistrationID == "" {
			t.Fatalf("register as %s: response must carry registration_id (registrant is the owner)", c.Identity)
		}
		return p.RegistrationID
	}

	// Non-owner → 403, registration survives.
	id := registerAs(alice, "peer-1")
	rec := doPeerReq(t, mux, bob, http.MethodDelete, "/peers/"+id, "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("DELETE by non-owner = %d, want 403", rec.Code)
	}
	if reg.Len() != 1 {
		t.Fatalf("non-owner delete must not remove the peer; Len = %d", reg.Len())
	}

	// Owner → 204.
	rec = doPeerReq(t, mux, alice, http.MethodDelete, "/peers/"+id, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE by owner = %d, want 204", rec.Code)
	}
	if reg.Len() != 0 {
		t.Fatalf("owner delete should remove the peer; Len = %d", reg.Len())
	}

	// Admin → 204 for someone else's registration.
	id = registerAs(alice, "peer-2")
	rec = doPeerReq(t, mux, admin, http.MethodDelete, "/peers/"+id, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE by admin = %d, want 204", rec.Code)
	}
	if reg.Len() != 0 {
		t.Fatalf("admin delete should remove the peer; Len = %d", reg.Len())
	}

	// Unknown id stays idempotent for authenticated callers.
	rec = doPeerReq(t, mux, alice, http.MethodDelete, "/peers/reg-doesnotexist", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE unknown id = %d, want 204 (idempotent)", rec.Code)
	}
}

func TestPeerHandlers_ListHidesRegistrationIDFromNonOwners(t *testing.T) {
	t.Parallel()
	reg := NewPeerRegistry()
	defer func() { _ = reg.Close() }()
	mux := peerHubMux(reg)
	alice := auth.Caller{Identity: "alice"}
	bob := auth.Caller{Identity: "bob"}
	admin := auth.Caller{Identity: "root", Admin: true}

	body, _ := json.Marshal(RegisterRequest{Name: "peer-1", Endpoint: "http://10.0.0.9:7777"})
	if rec := doPeerReq(t, mux, alice, http.MethodPost, "/peers", string(body)); rec.Code != http.StatusCreated {
		t.Fatalf("register: %d", rec.Code)
	}

	list := func(c auth.Caller) []Peer {
		rec := doPeerReq(t, mux, c, http.MethodGet, "/peers", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /peers as %s = %d", c.Identity, rec.Code)
		}
		var out struct {
			Peers []Peer `json:"peers"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode list: %v", err)
		}
		return out.Peers
	}

	// Non-owner sees the peer (discovery works) but not the ID.
	got := list(bob)
	if len(got) != 1 || got[0].Name != "peer-1" || got[0].Endpoint == "" {
		t.Fatalf("non-owner list = %+v, want the peer visible with endpoint", got)
	}
	if got[0].RegistrationID != "" {
		t.Fatalf("non-owner list leaked registration_id %q", got[0].RegistrationID)
	}

	// Owner and admin see the ID.
	if got = list(alice); len(got) != 1 || got[0].RegistrationID == "" {
		t.Fatalf("owner list = %+v, want registration_id visible", got)
	}
	if got = list(admin); len(got) != 1 || got[0].RegistrationID == "" {
		t.Fatalf("admin list = %+v, want registration_id visible", got)
	}

	// List redaction must not corrupt registry state: the owner still
	// resolves the real ID afterwards.
	if _, ok := reg.Lookup(got[0].RegistrationID); !ok {
		t.Fatalf("registry lost the registration after redacted lists")
	}
}

func TestPeerHandlers_RequireAuthRejectsAnonymous(t *testing.T) {
	t.Parallel()
	reg := NewPeerRegistry()
	defer func() { _ = reg.Close() }()
	mux := peerHubMux(reg)
	anon := auth.Anonymous // identity "anon" — the middleware fallback

	if rec := doPeerReq(t, mux, anon, http.MethodGet, "/peers", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous GET /peers = %d, want 401", rec.Code)
	}
	body, _ := json.Marshal(RegisterRequest{Name: "n", Endpoint: "http://10.0.0.9:7777"})
	if rec := doPeerReq(t, mux, anon, http.MethodPost, "/peers", string(body)); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous POST /peers = %d, want 401", rec.Code)
	}
	if rec := doPeerReq(t, mux, anon, http.MethodDelete, "/peers/reg-x", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous DELETE /peers/{id} = %d, want 401", rec.Code)
	}
}

// TestPeerHandlers_SingleUserModeStaysOpen: without multi-session
// (requireAuth=false) the transport token remains the only gate and
// every caller shares one identity — register/list/deregister keep
// working end-to-end, IDs visible to that shared identity.
func TestPeerHandlers_SingleUserModeStaysOpen(t *testing.T) {
	t.Parallel()
	reg := NewPeerRegistry()
	defer func() { _ = reg.Close() }()
	mux := http.NewServeMux()
	newPeerHandlers(reg, false, "anon").register(mux)
	anon := auth.Anonymous

	body, _ := json.Marshal(RegisterRequest{Name: "n", Endpoint: "http://10.0.0.9:7777"})
	rec := doPeerReq(t, mux, anon, http.MethodPost, "/peers", string(body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("single-user POST /peers = %d, want 201", rec.Code)
	}
	var p Peer
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("decode: %v", err)
	}
	rec = doPeerReq(t, mux, anon, http.MethodGet, "/peers", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), p.RegistrationID) {
		t.Fatalf("single-user GET /peers = %d body %q, want 200 with the id visible to the shared identity", rec.Code, rec.Body.String())
	}
	rec = doPeerReq(t, mux, anon, http.MethodDelete, "/peers/"+p.RegistrationID, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("single-user DELETE = %d, want 204", rec.Code)
	}
}
