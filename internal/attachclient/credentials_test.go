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

package attachclient

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/go-steer/core-agent/pkg/attach"
)

// staticTokenSource returns the same oauth2.Token on every call —
// substitutes for idtoken.NewTokenSource in tests that don't need
// real ADC. Mirrors golang.org/x/oauth2.StaticTokenSource but lets
// us inject explicit values without depending on its constructor.
type staticTokenSource struct {
	token *oauth2.Token
	err   error
}

func (s *staticTokenSource) Token() (*oauth2.Token, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.token, nil
}

func TestBearerCreds_Apply(t *testing.T) {
	cases := []struct {
		name   string
		token  string
		wantOK bool // true when header should be set
	}{
		{"token set → Authorization stamped", "abc123", true},
		{"empty token → no-op (auth disabled)", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := BearerCreds{Token: tc.token}
			req := httptest.NewRequest(http.MethodGet, "/x", nil)
			if err := c.Apply(req); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			got := req.Header.Get("Authorization")
			if tc.wantOK {
				if got != "Bearer "+tc.token {
					t.Errorf("Authorization = %q, want %q", got, "Bearer "+tc.token)
				}
			} else {
				if got != "" {
					t.Errorf("Authorization = %q, want empty (auth disabled)", got)
				}
			}
			// BearerCreds should never set X-Attach-Token (that's the
			// GoogleIDTokenCreds path's responsibility).
			if v := req.Header.Get(attach.HeaderAttachToken); v != "" {
				t.Errorf("X-Attach-Token = %q, want empty (BearerCreds should not set it)", v)
			}
		})
	}
}

func TestGoogleIDTokenCreds_Apply_StampsBothHeaders(t *testing.T) {
	src := &staticTokenSource{
		token: &oauth2.Token{
			AccessToken: "fake.id.token.payload",
			Expiry:      time.Now().Add(time.Hour),
		},
	}
	c := GoogleIDTokenCreds{Source: src, AttachToken: "attach-secret-xyz"}
	req := httptest.NewRequest(http.MethodGet, "/x", nil)

	if err := c.Apply(req); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	gotAuth := req.Header.Get("Authorization")
	wantAuth := "Bearer fake.id.token.payload"
	if gotAuth != wantAuth {
		t.Errorf("Authorization = %q, want %q", gotAuth, wantAuth)
	}
	gotAttach := req.Header.Get(attach.HeaderAttachToken)
	if gotAttach != "attach-secret-xyz" {
		t.Errorf("X-Attach-Token = %q, want %q", gotAttach, "attach-secret-xyz")
	}
}

func TestGoogleIDTokenCreds_Apply_NoAttachToken_OmitsXAttachToken(t *testing.T) {
	// Posture B: IAM is the sole gate, no --attach-token configured
	// server-side. The TUI still stamps Authorization with the ID
	// token (for the gateway) but omits X-Attach-Token entirely.
	src := &staticTokenSource{
		token: &oauth2.Token{AccessToken: "id-token", Expiry: time.Now().Add(time.Hour)},
	}
	c := GoogleIDTokenCreds{Source: src /* AttachToken intentionally empty */}
	req := httptest.NewRequest(http.MethodGet, "/x", nil)

	if err := c.Apply(req); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer id-token" {
		t.Errorf("Authorization = %q, want %q", got, "Bearer id-token")
	}
	if got := req.Header.Get(attach.HeaderAttachToken); got != "" {
		t.Errorf("X-Attach-Token = %q, want empty (Posture B — no attach token configured)", got)
	}
}

func TestGoogleIDTokenCreds_Apply_NilSourceErrors(t *testing.T) {
	c := GoogleIDTokenCreds{Source: nil, AttachToken: "x"}
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	err := c.Apply(req)
	if err == nil {
		t.Fatalf("Apply with nil Source: want error, got nil")
	}
}

func TestGoogleIDTokenCreds_Apply_SourceErrorPropagated(t *testing.T) {
	// Token source failure (ADC unavailable, metadata server unreachable,
	// etc.) propagates so the caller can surface a clear error instead
	// of sending an unauthenticated request that would 401 mysteriously.
	wantErr := errors.New("metadata server timeout")
	src := &staticTokenSource{err: wantErr}
	c := GoogleIDTokenCreds{Source: src, AttachToken: "x"}
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	err := c.Apply(req)
	if err == nil {
		t.Fatalf("Apply with failing Source: want error, got nil")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("error = %v, want wrapping %v", err, wantErr)
	}
	// Defensive: headers must not be partially stamped on failure
	// (would leave an invalid request that might 401 with confusing
	// "wrong token" message vs. the actual "couldn't mint token" cause).
	if got := req.Header.Get("Authorization"); got != "" {
		t.Errorf("Authorization stamped despite error: %q", got)
	}
	if got := req.Header.Get(attach.HeaderAttachToken); got != "" {
		t.Errorf("X-Attach-Token stamped despite error: %q", got)
	}
}

func TestClient_auth_UsesCredentialsWhenSet(t *testing.T) {
	// Regression guard: Client.auth() must prefer Credentials over
	// the legacy Token field when both are present. Token is kept on
	// the struct only as a back-compat carrier; Credentials wins.
	parsed, err := ParseURL("http://localhost:7777")
	if err != nil {
		t.Fatalf("ParseURL: %v", err)
	}
	c := NewWithCredentials(parsed, BearerCreds{Token: "from-creds"}, 0)
	c.Token = "from-legacy-field" // would have been used by the old code path
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	if err := c.auth(req); err != nil {
		t.Fatalf("auth: %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer from-creds" {
		t.Errorf("Authorization = %q, want %q (Credentials should win over legacy Token)", got, "Bearer from-creds")
	}
}

func TestClient_auth_FallsBackToTokenWhenNoCredentials(t *testing.T) {
	// Back-compat: the legacy New(parsed, token, ...) constructor
	// populates Token AND wraps it in BearerCreds. But Client values
	// constructed via direct struct literals (tests, future code)
	// might set only Token. That path still works.
	parsed, _ := ParseURL("http://localhost:7777")
	c := &Client{URL: parsed, Token: "legacy-token", http: newHTTPClient(parsed, time.Second)}
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	if err := c.auth(req); err != nil {
		t.Fatalf("auth: %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer legacy-token" {
		t.Errorf("Authorization = %q, want %q (legacy Token fallback)", got, "Bearer legacy-token")
	}
}
