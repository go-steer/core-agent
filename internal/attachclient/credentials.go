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
// Two impls ship today:
//
//   - BearerCreds carries the legacy direct-attach attach-token. Sets
//     Authorization: Bearer <token>. The zero-value (empty token) is
//     valid — auth disabled, used for Unix-socket attach.
//
//   - GoogleIDTokenCreds is for gateway-fronted deploys (Cloud Run IAM,
//     IAP, etc). Mints a Google ID token via the supplied TokenSource
//     (typically idtoken.NewTokenSource backed by Application Default
//     Credentials), stamps it on Authorization for the gateway, and
//     stamps the core-agent attach token on X-Attach-Token so the
//     daemon's own auth check (pkg/attach/auth.go) still validates.
//
// Future strategies (mTLS, header-cmd escape hatch, Cloudflare Access
// JWT, …) implement the same interface and plug in identically.

package attachclient

import (
	"fmt"
	"net/http"

	"golang.org/x/oauth2"

	"github.com/go-steer/core-agent/pkg/attach"
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

// GoogleIDTokenCreds is the gateway-fronted auth path. The Source
// produces a Google ID token audience-bound to the gateway (service
// URL for Cloud Run, OAuth client ID for IAP); the token rides on
// Authorization so the gateway can validate it. The AttachToken (if
// set) rides on X-Attach-Token so core-agent's own middleware
// validates against the daemon's --attach-token.
//
// AttachToken may be empty when the daemon is configured without
// --attach-token ("Posture B" — IAM is the sole gate). The header
// is omitted entirely in that case, and the daemon's auth-disabled
// middleware path accepts the request.
type GoogleIDTokenCreds struct {
	Source      oauth2.TokenSource
	AttachToken string
}

// Apply implements Credentials. Mints (or fetches from cache) the
// ID token, stamps both headers. The underlying token source
// (typically idtoken.NewTokenSource) caches the token until expiry
// — Apply is cheap on repeat calls within the cache window.
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
