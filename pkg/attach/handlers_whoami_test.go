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
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/auth"
)

// middlewareAuthSource runs one request through the caller middleware
// and returns the auth-source verdict it stamped onto the context
// (plus the response status, for the 401 paths).
func middlewareAuthSource(t *testing.T, cfg callerMiddlewareConfig, mutate func(*http.Request)) (string, int) {
	t.Helper()
	var source string
	var sawHandler bool
	h := callerMiddlewareWithConfig(cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawHandler = true
		source, _ = authSourceFromContext(r.Context())
	}))
	req := httptest.NewRequest(http.MethodGet, "/whoami", nil)
	if mutate != nil {
		mutate(req)
	}
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)
	if !sawHandler {
		return "", rw.Code
	}
	return source, rw.Code
}

// TestAuthSource_ServerVerdictOnly pins the #385 fix: the auth source
// reported to /whoami is the caller middleware's verdict about what
// the SERVER verified — never a re-derivation from spoofable request
// headers. A forged Authorization header, an unverified client cert,
// or a forged IAP header must all classify as anonymous.
func TestAuthSource_ServerVerdictOnly(t *testing.T) {
	t.Parallel()

	bearerTable := auth.NewBearerTokenAuth(
		[]auth.User{{Identity: "alice@example.com", Token: "sekret"}},
		nil, nil,
	)

	cases := []struct {
		name   string
		cfg    callerMiddlewareConfig
		mutate func(*http.Request)
		want   string
	}{
		{
			// The headline spoof: a bearer-looking header with NO
			// server-side validator behind it is not "bearer".
			name:   "forged Authorization header with anonymous auth → anonymous",
			cfg:    callerMiddlewareConfig{},
			mutate: func(r *http.Request) { r.Header.Set("Authorization", "Bearer forged") },
			want:   whoAmISourceAnonymous,
		},
		{
			name:   "forged X-Attach-Token with anonymous auth → anonymous",
			cfg:    callerMiddlewareConfig{},
			mutate: func(r *http.Request) { r.Header.Set(HeaderAttachToken, "forged") },
			want:   whoAmISourceAnonymous,
		},
		{
			// IAP headers are client-forgeable; the server validates
			// no gateway assertion today, so they never move Source.
			name: "forged IAP headers → anonymous",
			cfg:  callerMiddlewareConfig{},
			mutate: func(r *http.Request) {
				r.Header.Set("X-Goog-Authenticated-User-Email", "accounts.google.com:alice@example.com")
				r.Header.Set("X-Goog-Iap-Jwt-Assertion", "eyJ...")
			},
			want: whoAmISourceAnonymous,
		},
		{
			// A presented-but-unverified client cert (VerifiedChains
			// empty) is not "mtls" — only our own listener's
			// RequireAndVerifyClientCert verification counts.
			name: "unverified peer certificate → anonymous",
			cfg:  callerMiddlewareConfig{},
			mutate: func(r *http.Request) {
				r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{{}}}
			},
			want: whoAmISourceAnonymous,
		},
		{
			name: "listener-verified client cert → mtls",
			cfg:  callerMiddlewareConfig{},
			mutate: func(r *http.Request) {
				r.TLS = &tls.ConnectionState{
					PeerCertificates: []*x509.Certificate{{}},
					VerifiedChains:   [][]*x509.Certificate{{{}}},
				}
			},
			want: whoAmISourceMTLS,
		},
		{
			// Transport-level bearer gate: AuthConfig.Middleware
			// already 401'd token-less requests before this
			// middleware, so the config flag alone proves the
			// credential was verified.
			name: "transport bearer configured → bearer",
			cfg:  callerMiddlewareConfig{transportBearerConfigured: true},
			want: whoAmISourceBearer,
		},
		{
			name: "bearer table hit → bearer",
			cfg:  callerMiddlewareConfig{authenticator: bearerTable},
			mutate: func(r *http.Request) {
				r.Header.Set("Authorization", "Bearer sekret")
			},
			want: whoAmISourceBearer,
		},
		{
			// Wrong token + no enforcement falls back to the
			// anonymous caller — and the SOURCE must say so, not
			// echo the (rejected) bearer header.
			name: "bearer table miss falls back → anonymous",
			cfg:  callerMiddlewareConfig{authenticator: bearerTable},
			mutate: func(r *http.Request) {
				r.Header.Set("Authorization", "Bearer wrong")
			},
			want: whoAmISourceAnonymous,
		},
		{
			// bearer wins over a verified cert when both are present
			// (matches the pre-#385 precedence: asserted > bearer >
			// mtls > anonymous).
			name: "bearer beats mtls when both verified",
			cfg:  callerMiddlewareConfig{transportBearerConfigured: true},
			mutate: func(r *http.Request) {
				r.TLS = &tls.ConnectionState{
					PeerCertificates: []*x509.Certificate{{}},
					VerifiedChains:   [][]*x509.Certificate{{{}}},
				}
			},
			want: whoAmISourceBearer,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, code := middlewareAuthSource(t, tc.cfg, tc.mutate)
			if code != http.StatusOK {
				t.Fatalf("status = %d, want 200", code)
			}
			if got != tc.want {
				t.Errorf("auth source = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestAuthSource_ValidatedProxyAssertionIsAsserted covers the one
// path that may legitimately report "asserted": the middleware
// validated the proxy assertion (requester on the proxy allowlist,
// asserted identity provisioned). A forged assertion header 401s —
// it can never downgrade into a plausible-looking Source.
func TestAuthSource_ValidatedProxyAssertionIsAsserted(t *testing.T) {
	t.Parallel()
	authn := auth.NewBearerTokenAuth(
		[]auth.User{
			{Identity: "sa:slack-bot", Token: "bot-token"},
			{Identity: "alice@example.com", Token: "alice-token"},
		},
		nil,
		[]string{"sa:slack-bot"},
	)
	cfg := callerMiddlewareConfig{authenticator: authn}

	got, code := middlewareAuthSource(t, cfg, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer bot-token")
		r.Header.Set(auth.HeaderAssertedCaller, "alice@example.com")
	})
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if got != whoAmISourceAsserted {
		t.Errorf("auth source = %q, want asserted", got)
	}

	// Non-allowlisted requester forging the assertion header → 401,
	// never a stamped source.
	_, code = middlewareAuthSource(t, cfg, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer alice-token")
		r.Header.Set(auth.HeaderAssertedCaller, "sa:slack-bot")
	})
	if code != http.StatusUnauthorized {
		t.Errorf("forged proxy assertion status = %d, want 401", code)
	}
}

func TestWhoAmI_HandlerBody(t *testing.T) {
	t.Parallel()
	h := &handlers{}
	// Simulate the caller middleware having stamped an admin caller
	// plus its auth-source verdict.
	req := httptest.NewRequest(http.MethodGet, "/whoami", nil)
	ctx := auth.WithCaller(req.Context(), auth.Caller{
		Identity: "alice@example.com",
		Admin:    true,
	})
	ctx = withAuthSource(ctx, whoAmISourceBearer)
	req = req.WithContext(ctx)
	rw := httptest.NewRecorder()

	h.doWhoAmI(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rw.Code)
	}
	body, _ := io.ReadAll(rw.Body)
	var resp whoAmIResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, body)
	}
	if resp.Identity != "alice@example.com" {
		t.Errorf("Identity = %q, want alice@example.com", resp.Identity)
	}
	if !resp.Admin {
		t.Error("Admin should be true")
	}
	if resp.Source != whoAmISourceBearer {
		t.Errorf("Source = %q, want bearer", resp.Source)
	}
	if resp.ProxyBy != "" {
		t.Errorf("ProxyBy = %q, should be empty when source != asserted", resp.ProxyBy)
	}
}

// TestWhoAmI_HandlerIgnoresRawHeaders is the handler-level pin of the
// #385 contract: with NO middleware verdict on the context, a request
// dressed in every spoofable credential header still reports
// anonymous — the handler no longer probes headers itself.
func TestWhoAmI_HandlerIgnoresRawHeaders(t *testing.T) {
	t.Parallel()
	h := &handlers{}
	req := httptest.NewRequest(http.MethodGet, "/whoami", nil)
	req.Header.Set("Authorization", "Bearer forged")
	req.Header.Set(HeaderAttachToken, "forged")
	req.Header.Set("X-Goog-Authenticated-User-Email", "accounts.google.com:eve@example.com")
	req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{{}}}
	req = req.WithContext(auth.WithCaller(req.Context(), auth.Anonymous))
	rw := httptest.NewRecorder()

	h.doWhoAmI(rw, req)

	var resp whoAmIResponse
	_ = json.NewDecoder(rw.Body).Decode(&resp)
	if resp.Source != whoAmISourceAnonymous {
		t.Errorf("Source = %q, want anonymous (handler must not trust raw headers)", resp.Source)
	}
}

func TestWhoAmI_ProxyByStamped(t *testing.T) {
	t.Parallel()
	h := &handlers{}
	req := httptest.NewRequest(http.MethodGet, "/whoami", nil)
	req.Header.Set("Authorization", "Bearer bot-token")
	ctx := auth.WithCaller(req.Context(), auth.Caller{Identity: "alice@example.com"})
	ctx = auth.WithProxyBy(ctx, "sa:slack-bot")
	ctx = withAuthSource(ctx, whoAmISourceAsserted)
	req = req.WithContext(ctx)
	rw := httptest.NewRecorder()

	h.doWhoAmI(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rw.Code)
	}
	var resp whoAmIResponse
	_ = json.NewDecoder(rw.Body).Decode(&resp)
	if resp.Source != whoAmISourceAsserted {
		t.Errorf("Source = %q, want asserted (proxy path wins over bearer)", resp.Source)
	}
	if resp.ProxyBy != "sa:slack-bot" {
		t.Errorf("ProxyBy = %q, want sa:slack-bot", resp.ProxyBy)
	}
	if resp.Identity != "alice@example.com" {
		t.Errorf("Identity = %q, want the effective (asserted) identity, not the bot's", resp.Identity)
	}
}

// TestWhoAmI_IntegrationBearer confirms the middleware chain gates
// /whoami just like every other endpoint — no accidental bypass —
// and that the transport bearer gate yields Source=bearer end-to-end.
func TestWhoAmI_IntegrationBearerRequired(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	srv, err := NewServer(Options{
		Registry: reg,
		Addr:     "127.0.0.1:0",
		Auth:     AuthConfig{BearerToken: "sekret"},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	defer func() { _ = srv.Close(); <-errCh }()
	for srv.Addr() == "" {
		// tight busy-loop is fine for a test bind — bounded by
		// test timeout.
	}
	base := "http://" + srv.Addr()

	// No token → 401 like any other route.
	resp, err := http.Get(base + "/whoami")
	if err != nil {
		t.Fatalf("GET /whoami no auth: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("/whoami without token = %d, want 401 (middleware gates every route)", resp.StatusCode)
	}

	// Good token → 200 + body.
	req, _ := http.NewRequest(http.MethodGet, base+"/whoami", nil)
	req.Header.Set("Authorization", "Bearer sekret")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /whoami with token: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp2.Body)
		t.Fatalf("/whoami with token = %d, want 200. Body: %s", resp2.StatusCode, body)
	}
	var body whoAmIResponse
	if err := json.NewDecoder(resp2.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Source != whoAmISourceBearer {
		t.Errorf("Source = %q, want bearer", body.Source)
	}
}
