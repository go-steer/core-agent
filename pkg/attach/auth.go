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
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
)

// AuthConfig describes how the attach server authenticates clients.
// Zero value (all fields empty) accepts everything — only safe over a
// Unix socket or other already-trusted transport. Per the design doc:
// mTLS is the primary auth; bearer-token is the fallback for when
// you don't have cert infrastructure handy.
type AuthConfig struct {
	// TLSCertFile / TLSKeyFile enable HTTPS on the listener. Required
	// together. Files are loaded once at server start.
	TLSCertFile string
	TLSKeyFile  string

	// ClientCAFile, when set alongside TLSCertFile/TLSKeyFile,
	// enables mTLS: clients must present a cert signed by this CA.
	// If empty, the server is HTTPS-only (server auth, no client
	// auth) — clients then rely on BearerToken (or no auth) for
	// authorization.
	ClientCAFile string

	// BearerToken, when non-empty, requires every request to carry
	// Authorization: Bearer <token>. Compared in constant time.
	// Works alongside mTLS; both must pass if both are configured.
	BearerToken string

	// ReadOnly disables every write endpoint (POST /inject, POST
	// /wake) regardless of auth. Read endpoints (GET /sessions,
	// GET /events) stay open. Useful for read-only mirrors.
	ReadOnly bool
}

// LoadTLSConfig builds a *tls.Config from the AuthConfig's TLS
// material. Returns nil (no TLS) when neither TLSCertFile nor
// TLSKeyFile is set. Returns an error if exactly one is set, or if
// the files can't be read.
func (a AuthConfig) LoadTLSConfig() (*tls.Config, error) {
	hasCert := a.TLSCertFile != ""
	hasKey := a.TLSKeyFile != ""
	if hasCert != hasKey {
		return nil, errors.New("attach: AuthConfig: TLSCertFile and TLSKeyFile must be set together")
	}
	if !hasCert {
		return nil, nil
	}
	cert, err := tls.LoadX509KeyPair(a.TLSCertFile, a.TLSKeyFile)
	if err != nil {
		return nil, fmt.Errorf("attach: load server cert/key: %w", err)
	}
	cfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}
	if a.ClientCAFile != "" {
		caPEM, err := os.ReadFile(a.ClientCAFile)
		if err != nil {
			return nil, fmt.Errorf("attach: read client CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil, fmt.Errorf("attach: client CA %q has no parseable PEM blocks", a.ClientCAFile)
		}
		cfg.ClientCAs = pool
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return cfg, nil
}

// HeaderAttachToken is the side-channel header callers can use to
// present the attach token when an identity gateway (Cloud Run IAM,
// IAP, Cloudflare Access, etc.) owns the Authorization header for
// its own validation. Checked before Authorization: Bearer so a
// request carrying both gets evaluated against this header first.
const HeaderAttachToken = "X-Attach-Token"

// Middleware returns an http.Handler that wraps next with attach-token
// validation + ReadOnly enforcement. mTLS, if configured, is enforced
// by the TLS handshake itself (ClientAuth = RequireAndVerifyClientCert)
// so by the time a request reaches this middleware, the cert has
// already been validated.
//
// Token validation accepts the attach token from either of two
// headers, checked in order:
//
//  1. X-Attach-Token — the side-channel header for gateway-fronted
//     deploys. If present but doesn't match, returns 401 immediately
//     (no fall-through to Authorization); this matches operator
//     intent: "I explicitly sent this; tell me if it's wrong."
//  2. Authorization: Bearer <token> — the default path for direct
//     attach (local, GKE internal LB, anywhere the operator owns
//     the Authorization header).
//
// Returns 401 Unauthorized on missing/wrong token; 403 Forbidden when
// ReadOnly + the request is a write.
func (a AuthConfig) Middleware(next http.Handler) http.Handler {
	wantToken := a.BearerToken
	readOnly := a.ReadOnly
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if wantToken != "" {
			if !checkAttachToken(w, r, wantToken) {
				return
			}
		}
		if readOnly && isWriteMethod(r.Method) {
			http.Error(w, "this listener is read-only (--attach-readonly); writes are disabled", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// checkAttachToken validates the request's attach token against want.
// Returns true to allow the request to proceed, false after writing
// a 401 response (caller returns without invoking next).
//
// Precedence: X-Attach-Token wins when present (right or wrong) —
// rationale documented on Middleware. Otherwise falls through to
// Authorization: Bearer.
func checkAttachToken(w http.ResponseWriter, r *http.Request, want string) bool {
	wantBytes := []byte(want)
	if side := r.Header.Get(HeaderAttachToken); side != "" {
		if subtle.ConstantTimeCompare([]byte(side), wantBytes) == 1 {
			return true
		}
		writeAttachUnauthorized(w)
		return false
	}
	got := extractBearer(r.Header.Get("Authorization"))
	if got == "" || subtle.ConstantTimeCompare([]byte(got), wantBytes) != 1 {
		writeAttachUnauthorized(w)
		return false
	}
	return true
}

// writeAttachUnauthorized centralizes the 401 response so both the
// X-Attach-Token and Authorization branches stay in sync — same
// status, same WWW-Authenticate hint (operators relying on the
// Bearer-realm signal don't lose it just because the request used
// the side-channel header).
func writeAttachUnauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="attach"`)
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}

// extractBearer pulls the token out of an "Authorization: Bearer X"
// header. Returns empty string when the header is missing or doesn't
// have the Bearer scheme.
func extractBearer(headerValue string) string {
	const prefix = "Bearer "
	if !strings.HasPrefix(headerValue, prefix) {
		return ""
	}
	return strings.TrimSpace(headerValue[len(prefix):])
}

// isWriteMethod returns true for HTTP methods that mutate state. Used
// by ReadOnly enforcement. GET and HEAD are read; everything else
// (POST, PUT, PATCH, DELETE) is write.
func isWriteMethod(m string) bool {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	return true
}
