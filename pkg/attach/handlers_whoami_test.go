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

func TestDetectAuthSource_PrecedenceMatrix(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		headers map[string]string
		tls     *tls.ConnectionState
		proxyBy string
		caller  auth.Caller
		want    string
	}{
		{
			name:    "asserted wins over bearer",
			headers: map[string]string{"Authorization": "Bearer x"},
			proxyBy: "sa:slack-bot",
			want:    WhoAmISourceAsserted,
		},
		{
			name:    "bearer via Authorization",
			headers: map[string]string{"Authorization": "Bearer sekret"},
			want:    WhoAmISourceBearer,
		},
		{
			name:    "bearer via X-Attach-Token",
			headers: map[string]string{HeaderAttachToken: "sekret"},
			want:    WhoAmISourceBearer,
		},
		{
			name: "mtls when client cert present",
			tls: &tls.ConnectionState{
				PeerCertificates: []*x509.Certificate{{}},
			},
			want: WhoAmISourceMTLS,
		},
		{
			name:    "iap header X-Goog-Authenticated-User-Email",
			headers: map[string]string{"X-Goog-Authenticated-User-Email": "accounts.google.com:alice@example.com"},
			want:    WhoAmISourceIAP,
		},
		{
			name:    "iap header X-Goog-Iap-Jwt-Assertion",
			headers: map[string]string{"X-Goog-Iap-Jwt-Assertion": "eyJ..."},
			want:    WhoAmISourceIAP,
		},
		{
			name: "anonymous when nothing is present",
			want: WhoAmISourceAnonymous,
		},
		{
			// Non-Bearer Authorization schemes (Basic, Digest, custom)
			// don't count as bearer — we only recognize the schemes
			// our authenticators produce.
			name:    "non-Bearer Authorization → anonymous",
			headers: map[string]string{"Authorization": "Basic dXNlcjpwYXNz"},
			want:    WhoAmISourceAnonymous,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/whoami", nil)
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			req.TLS = tc.tls
			got := detectAuthSource(req, tc.caller, tc.proxyBy)
			if got != tc.want {
				t.Errorf("source = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestWhoAmI_HandlerBody(t *testing.T) {
	t.Parallel()
	h := &handlers{}
	// Simulate the auth middleware having stamped an admin caller.
	req := httptest.NewRequest(http.MethodGet, "/whoami", nil)
	req.Header.Set("Authorization", "Bearer sekret")
	ctx := auth.WithCaller(req.Context(), auth.Caller{
		Identity: "alice@example.com",
		Admin:    true,
	})
	req = req.WithContext(ctx)
	rw := httptest.NewRecorder()

	h.doWhoAmI(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rw.Code)
	}
	body, _ := io.ReadAll(rw.Body)
	var resp WhoAmIResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, body)
	}
	if resp.Identity != "alice@example.com" {
		t.Errorf("Identity = %q, want alice@example.com", resp.Identity)
	}
	if !resp.Admin {
		t.Error("Admin should be true")
	}
	if resp.Source != WhoAmISourceBearer {
		t.Errorf("Source = %q, want bearer", resp.Source)
	}
	if resp.ProxyBy != "" {
		t.Errorf("ProxyBy = %q, should be empty when source != asserted", resp.ProxyBy)
	}
}

func TestWhoAmI_ProxyByStamped(t *testing.T) {
	t.Parallel()
	h := &handlers{}
	req := httptest.NewRequest(http.MethodGet, "/whoami", nil)
	req.Header.Set("Authorization", "Bearer bot-token")
	ctx := auth.WithCaller(req.Context(), auth.Caller{Identity: "alice@example.com"})
	ctx = auth.WithProxyBy(ctx, "sa:slack-bot")
	req = req.WithContext(ctx)
	rw := httptest.NewRecorder()

	h.doWhoAmI(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rw.Code)
	}
	var resp WhoAmIResponse
	_ = json.NewDecoder(rw.Body).Decode(&resp)
	if resp.Source != WhoAmISourceAsserted {
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
// /whoami just like every other endpoint — no accidental bypass.
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
	var body WhoAmIResponse
	if err := json.NewDecoder(resp2.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Source != WhoAmISourceBearer {
		t.Errorf("Source = %q, want bearer", body.Source)
	}
}
