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

// Auth-credential abstraction for outbound attach requests. The
// Client holds a Credentials value and calls Apply once per request
// to stamp the right headers.
//
// Three impls ship today:
//
//   - BearerCreds carries the legacy direct-attach attach-token. Sets
//     Authorization: Bearer <token>. The zero-value (empty token) is
//     valid — auth disabled, used for Unix-socket attach.
//
//   - GoogleOAuthCreds is the recommended path for Cloud Run IAM (and
//     any gateway that accepts Google access tokens). Wraps a token
//     source from google.FindDefaultCredentials and stamps the
//     resulting OAuth2 access token on Authorization. Mirrors MCP's
//     googleAuthTransport pattern (pkg/mcp/lifecycle.go) so the same
//     ADC story works across attach + MCP. Works with end-user ADC,
//     service-account ADC, metadata server, impersonation — every
//     credential shape google.FindDefaultCredentials accepts.
//
//   - GoogleIDTokenCreds is the audience-bound variant — required by
//     IAP, optional for Cloud Run IAM. Uses idtoken.NewTokenSource
//     which only accepts service-account-shaped credentials (SA JSON
//     key, metadata server, impersonation chain). End-user ADC
//     (gcloud auth application-default login) does NOT work with it
//     — operators need impersonation or a SA key file. Use only when
//     audience-binding actually matters (IAP, or strict ID-token
//     policy on a Cloud Run service).
//
// Future strategies (mTLS, header-cmd escape hatch, Cloudflare Access
// JWT, …) implement the same interface and plug in identically.

package attachclient

import (
	"fmt"
	"net/http"

	"golang.org/x/oauth2"

	"github.com/go-steer/core-agent/v2/pkg/attach"
)

// Credentials stamps authentication headers on outbound requests.
// Implementations must be safe for concurrent use from multiple
// goroutines (the Client uses one Credentials for every request,
// including parallel RPC + SSE).
type Credentials interface {
	// Apply stamps headers on req. Returns an error when the
	// underlying credential source fails to produce a token —
	// callers propagate (the request is not sent in that case).
	Apply(req *http.Request) error
}

// BearerCreds sends the attach token as Authorization: Bearer <token>.
// Zero-value (Token == "") is auth-disabled — Apply is a no-op,
// matching the historical attach-over-Unix-socket convention.
type BearerCreds struct {
	Token string
}

// Apply implements Credentials.
func (c BearerCreds) Apply(req *http.Request) error {
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	return nil
}

// GoogleOAuthCreds wraps a Google OAuth2 access-token source (typically
// google.FindDefaultCredentials's TokenSource) and stamps the access
// token on Authorization. Works with every ADC shape end users actually
// have on their workstations — end-user (authorized_user) creds,
// service-account JSON keys, metadata server, impersonation.
//
// This is the right default for Cloud Run IAM: the gateway accepts
// either OAuth access tokens OR audience-bound ID tokens, and access
// tokens come for free from end-user ADC. Mirrors MCP's
// googleAuthTransport pattern (pkg/mcp/lifecycle.go:296).
//
// AttachToken may be empty when the daemon runs without --attach-token
// ("Posture B"). The header is omitted entirely in that case.
type GoogleOAuthCreds struct {
	Source      oauth2.TokenSource
	AttachToken string
}

// Apply implements Credentials.
func (c GoogleOAuthCreds) Apply(req *http.Request) error {
	if c.Source == nil {
		return fmt.Errorf("attachclient: GoogleOAuthCreds: Source is nil")
	}
	tok, err := c.Source.Token()
	if err != nil {
		return fmt.Errorf("attachclient: fetch Google OAuth access token: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	if c.AttachToken != "" {
		req.Header.Set(attach.HeaderAttachToken, c.AttachToken)
	}
	return nil
}

// GoogleIDTokenCreds is the audience-bound variant. The Source produces
// a Google ID token bound to a specific audience (the gateway's
// expected audience — service URL for Cloud Run, OAuth client ID for
// IAP). Use when audience-binding is required (IAP) or when a
// service explicitly requires ID tokens.
//
// Important constraint: idtoken.NewTokenSource does NOT accept
// end-user (authorized_user) credentials — operators using
// gcloud auth application-default login will hit
// "unsupported credentials type: authorized_user" at construction
// time. Workarounds:
//
//   - Re-login with impersonation:
//     gcloud auth application-default login --impersonate-service-account=SA_EMAIL
//   - Set GOOGLE_APPLICATION_CREDENTIALS to a service-account JSON key
//   - Use --auth=google-oauth instead (Cloud Run IAM accepts access
//     tokens; the audience-binding loss is mostly theoretical)
//
// AttachToken may be empty when the daemon runs without --attach-token
// ("Posture B"). The header is omitted entirely in that case.
type GoogleIDTokenCreds struct {
	Source      oauth2.TokenSource
	AttachToken string
}

// Apply implements Credentials.
func (c GoogleIDTokenCreds) Apply(req *http.Request) error {
	if c.Source == nil {
		return fmt.Errorf("attachclient: GoogleIDTokenCreds: Source is nil")
	}
	tok, err := c.Source.Token()
	if err != nil {
		return fmt.Errorf("attachclient: mint Google ID token: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	if c.AttachToken != "" {
		req.Header.Set(attach.HeaderAttachToken, c.AttachToken)
	}
	return nil
}
