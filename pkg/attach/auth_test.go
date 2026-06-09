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
)

func newMiddlewareHandler(t *testing.T, cfg AuthConfig) http.Handler {
	t.Helper()
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	return cfg.Middleware(next)
}

func TestMiddleware_XAttachTokenAccepted(t *testing.T) {
	h := newMiddlewareHandler(t, AuthConfig{BearerToken: "secret"})
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set(HeaderAttachToken, "secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (X-Attach-Token correct)", rec.Code)
	}
}

func TestMiddleware_XAttachTokenWrong_NoFallthroughToBearer(t *testing.T) {
	// Issue #112's precedence decision: X-Attach-Token wrong should
	// 401 immediately even if a correct Authorization header is also
	// present. The operator explicitly sent the side-channel header
	// and deserves to hear it was rejected — not have it silently
	// overridden by another credential they may have forgotten was set.
	h := newMiddlewareHandler(t, AuthConfig{BearerToken: "secret"})
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set(HeaderAttachToken, "wrong")
	req.Header.Set("Authorization", "Bearer secret") // would have passed on its own
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 (X-Attach-Token wrong should not fall through)", rec.Code)
	}
	if got := rec.Header().Get("WWW-Authenticate"); got == "" {
		t.Errorf("WWW-Authenticate header missing on 401")
	}
}

func TestMiddleware_BothHeadersCorrect_XAttachTokenWins(t *testing.T) {
	// Both headers carrying the right token — request proceeds (and
	// since X-Attach-Token takes precedence, the Authorization header
	// isn't even examined; can't easily assert that without spies,
	// but the request succeeding either way confirms no crash on
	// dual-header inputs).
	h := newMiddlewareHandler(t, AuthConfig{BearerToken: "secret"})
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set(HeaderAttachToken, "secret")
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (both headers correct)", rec.Code)
	}
}

func TestMiddleware_AuthorizationStillWorks_NoXAttachTokenHeader(t *testing.T) {
	// Back-compat: existing direct-attach clients (no gateway) keep
	// using Authorization: Bearer and see no change.
	h := newMiddlewareHandler(t, AuthConfig{BearerToken: "secret"})
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (Authorization-only path)", rec.Code)
	}
}

func TestMiddleware_AuthorizationWrong(t *testing.T) {
	h := newMiddlewareHandler(t, AuthConfig{BearerToken: "secret"})
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 (Authorization wrong)", rec.Code)
	}
}

func TestMiddleware_NeitherHeader(t *testing.T) {
	h := newMiddlewareHandler(t, AuthConfig{BearerToken: "secret"})
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 (neither header)", rec.Code)
	}
	if got := rec.Header().Get("WWW-Authenticate"); got == "" {
		t.Errorf("WWW-Authenticate header missing on 401")
	}
}

func TestMiddleware_NoAuthConfigured(t *testing.T) {
	// Zero-value BearerToken means auth is off entirely. Useful over
	// Unix sockets / already-trusted transports. Verify both headers'
	// presence (or absence) is tolerated without rejection.
	h := newMiddlewareHandler(t, AuthConfig{})
	cases := []struct {
		name    string
		headers map[string]string
	}{
		{"no headers", nil},
		{"only X-Attach-Token", map[string]string{HeaderAttachToken: "anything"}},
		{"only Authorization", map[string]string{"Authorization": "Bearer anything"}},
		{"both headers", map[string]string{
			HeaderAttachToken: "anything",
			"Authorization":   "Bearer anything",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/x", nil)
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Errorf("status = %d, want 200 (auth disabled)", rec.Code)
			}
		})
	}
}

func TestMiddleware_ReadOnlyForbidsWritesEvenWithGoodToken(t *testing.T) {
	// ReadOnly enforcement runs AFTER auth, so a correctly-authed
	// write still gets 403. Verify both auth paths (X-Attach-Token
	// and Authorization) honor it.
	h := newMiddlewareHandler(t, AuthConfig{BearerToken: "secret", ReadOnly: true})
	cases := []struct {
		name    string
		headers map[string]string
	}{
		{"via X-Attach-Token", map[string]string{HeaderAttachToken: "secret"}},
		{"via Authorization", map[string]string{"Authorization": "Bearer secret"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/inject", nil)
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Errorf("status = %d, want 403 (read-only write)", rec.Code)
			}
		})
	}
}

func TestExtractBearer(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"Bearer abc", "abc"},
		{"Bearer  abc  ", "abc"},
		{"bearer abc", ""}, // case-sensitive prefix per RFC 6750
		{"", ""},
		{"Token abc", ""},
	}
	for _, tc := range cases {
		if got := extractBearer(tc.in); got != tc.want {
			t.Errorf("extractBearer(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
