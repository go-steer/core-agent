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

	"github.com/go-steer/core-agent/pkg/auth"
)

// captureCallerHandler records whichever Caller it sees on the request
// context. Used to assert what callerMiddleware put there.
func captureCallerHandler(out *auth.Caller, sawCaller *bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, ok := auth.CallerFromContext(r.Context())
		*out = c
		*sawCaller = ok
		w.WriteHeader(http.StatusOK)
	})
}

func TestCallerMiddleware_NilAuthenticatorFallsBackToAnonymous(t *testing.T) {
	t.Parallel()
	var got auth.Caller
	var saw bool
	h := callerMiddleware(nil, auth.Caller{}, captureCallerHandler(&got, &saw))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))

	if !saw {
		t.Fatal("downstream handler saw no Caller on context; middleware must always set one")
	}
	if got.Identity != "anon" {
		t.Errorf("nil-Authenticator default Caller: got %q, want %q (auth.Anonymous default)", got.Identity, "anon")
	}
}

func TestCallerMiddleware_DefaultCallerHonored(t *testing.T) {
	t.Parallel()
	var got auth.Caller
	var saw bool
	h := callerMiddleware(nil, auth.Caller{Identity: "daemon-user"}, captureCallerHandler(&got, &saw))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))

	if got.Identity != "daemon-user" {
		t.Errorf("DefaultCaller override not honored: got %q, want %q", got.Identity, "daemon-user")
	}
}

func TestCallerMiddleware_BearerAuthenticatorResolvesIdentity(t *testing.T) {
	t.Parallel()
	authn := auth.NewBearerTokenAuth([]auth.User{
		{Identity: "alice@example.com", Token: "tok_alice", Labels: map[string]string{"team": "platform"}},
	}, nil, nil)

	var got auth.Caller
	var saw bool
	h := callerMiddleware(authn, auth.Caller{}, captureCallerHandler(&got, &saw))

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer tok_alice")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)

	if got.Identity != "alice@example.com" {
		t.Errorf("authenticated Caller not threaded to handler: got %q, want %q", got.Identity, "alice@example.com")
	}
	if got.Labels["team"] != "platform" {
		t.Errorf("Labels not threaded: got %v", got.Labels)
	}
}

func TestCallerMiddleware_FailedAuthFallsBackInAlpha1(t *testing.T) {
	t.Parallel()
	// α.1 posture: an unauthenticated request must NOT 401 — the
	// middleware degrades to the fallback Caller so no existing
	// behavior changes. α.2 will flip this to a 401 path when
	// multi_session.allow_anonymous=false.
	authn := auth.NewBearerTokenAuth([]auth.User{
		{Identity: "alice@example.com", Token: "tok_alice"},
	}, nil, nil)

	var got auth.Caller
	var saw bool
	h := callerMiddleware(authn, auth.Caller{Identity: "anon"}, captureCallerHandler(&got, &saw))

	rr := httptest.NewRecorder()
	// No Authorization header → ErrUnauthenticated → fallback path.
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))

	if rr.Code != http.StatusOK {
		t.Errorf("α.1: failed auth must NOT 401 (would change observable behavior); got status %d", rr.Code)
	}
	if got.Identity != "anon" {
		t.Errorf("α.1 fallback: got %q, want %q", got.Identity, "anon")
	}
}
