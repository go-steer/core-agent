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

// captureCallerAndProxyHandler records the Caller + ProxyBy on the
// request context so proxy tests can assert what the middleware threaded
// through. Mirrors captureCallerHandler (caller_middleware_test.go) but
// also captures ProxyBy.
func captureCallerAndProxyHandler(c *auth.Caller, proxyBy *string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ := auth.CallerFromContext(r.Context())
		by, _ := auth.ProxyByFromContext(r.Context())
		*c = got
		*proxyBy = by
		w.WriteHeader(http.StatusOK)
	})
}

func newBearerAuthForProxy(t *testing.T) *auth.BearerTokenAuth {
	t.Helper()
	return auth.NewBearerTokenAuth(
		[]auth.User{
			{Identity: "sa:slack-bot", Token: "tok_bot", Labels: map[string]string{"kind": "service"}},
			{Identity: "alice@example.com", Token: "tok_alice", Labels: map[string]string{"team": "platform"}},
		},
		nil,
		[]string{"sa:slack-bot"}, // proxy allowlist
	)
}

func TestCallerMiddleware_ProxyAssertionSucceeds(t *testing.T) {
	t.Parallel()
	var got auth.Caller
	var by string
	h := callerMiddlewareWithConfig(callerMiddlewareConfig{
		authenticator: newBearerAuthForProxy(t),
	}, captureCallerAndProxyHandler(&got, &by))

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer tok_bot")
	r.Header.Set(auth.HeaderAssertedCaller, "alice@example.com")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", rr.Code, rr.Body.String())
	}
	if got.Identity != "alice@example.com" {
		t.Errorf("effective Caller: got %q, want %q (proxy must materialize the asserted identity)", got.Identity, "alice@example.com")
	}
	if got.Labels["team"] != "platform" {
		t.Errorf("Labels must come from the table entry of the asserted identity; got %v", got.Labels)
	}
	if by != "sa:slack-bot" {
		t.Errorf("ProxyBy: got %q, want %q", by, "sa:slack-bot")
	}
}

func TestCallerMiddleware_ProxyAssertionRejectedForNonProxyCaller(t *testing.T) {
	t.Parallel()
	var got auth.Caller
	var by string
	h := callerMiddlewareWithConfig(callerMiddlewareConfig{
		authenticator: newBearerAuthForProxy(t),
	}, captureCallerAndProxyHandler(&got, &by))

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	// Alice authenticates as herself but tries to assert as bob — must 401
	// because alice is not on the proxy allowlist.
	r.Header.Set("Authorization", "Bearer tok_alice")
	r.Header.Set(auth.HeaderAssertedCaller, "bob@example.com")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for non-proxy caller asserting another identity; got %d (body: %s)", rr.Code, rr.Body.String())
	}
}

func TestCallerMiddleware_ProxyAssertionRejectedForUnknownIdentity(t *testing.T) {
	t.Parallel()
	var got auth.Caller
	var by string
	h := callerMiddlewareWithConfig(callerMiddlewareConfig{
		authenticator: newBearerAuthForProxy(t),
	}, captureCallerAndProxyHandler(&got, &by))

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer tok_bot")
	r.Header.Set(auth.HeaderAssertedCaller, "ghost@example.com") // not provisioned
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 when asserted identity is not provisioned; got %d", rr.Code)
	}
}

func TestCallerMiddleware_EnforceAuthenticationReturns401(t *testing.T) {
	t.Parallel()
	var got auth.Caller
	var by string
	h := callerMiddlewareWithConfig(callerMiddlewareConfig{
		authenticator:         newBearerAuthForProxy(t),
		enforceAuthentication: true,
	}, captureCallerAndProxyHandler(&got, &by))

	// No Authorization → ErrUnauthenticated → 401 (enforcement on).
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("enforceAuthentication=true must 401 on missing credential; got %d", rr.Code)
	}
}

func TestCallerMiddleware_ProxyHeaderIgnoredWhenAbsent(t *testing.T) {
	t.Parallel()
	// Bot authenticates as itself with NO X-Asserted-Caller header.
	// The effective Caller should be the bot, ProxyBy unset.
	var got auth.Caller
	var by string
	h := callerMiddlewareWithConfig(callerMiddlewareConfig{
		authenticator: newBearerAuthForProxy(t),
	}, captureCallerAndProxyHandler(&got, &by))

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer tok_bot")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if got.Identity != "sa:slack-bot" {
		t.Errorf("effective Caller: got %q, want %q", got.Identity, "sa:slack-bot")
	}
	if by != "" {
		t.Errorf("ProxyBy must be empty when no assertion header; got %q", by)
	}
}

func TestCallerMiddleware_ProxyHeaderForbiddenForNonProxyAuthenticator(t *testing.T) {
	t.Parallel()
	// AnonymousAuth does not implement AuthenticatorWithProxy. Any
	// X-Asserted-Caller header must be rejected with 401 rather than
	// silently dropped — silent drop would hide misconfiguration.
	var got auth.Caller
	var by string
	h := callerMiddlewareWithConfig(callerMiddlewareConfig{
		authenticator: auth.AnonymousAuth{Caller: auth.Caller{Identity: "anon"}},
	}, captureCallerAndProxyHandler(&got, &by))

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set(auth.HeaderAssertedCaller, "alice@example.com")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("AnonymousAuth + asserted-caller header must 401 (no proxy capability); got %d", rr.Code)
	}
}
