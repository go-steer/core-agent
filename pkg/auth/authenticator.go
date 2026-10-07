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

package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"
)

// Authenticator extracts a Caller from an inbound HTTP request, or
// returns ErrUnauthenticated when no valid credential is present.
//
// Implementations shipped in α.1:
//   - AnonymousAuth — single fixed Caller; the default for single-user
//     deployments and any deployment with multi-session disabled.
//   - BearerTokenAuth — token → Caller lookup against a static table
//     loaded from users.json (see LoadUsersFile).
//
// Future implementations (designed but not in α.1): OIDC/JWT, mTLS
// (subject DN → Caller), K8s ServiceAccount (TokenReview).
type Authenticator interface {
	Authenticate(r *http.Request) (Caller, error)
}

// AuthenticatorWithProxy is the optional extension implemented by
// authenticators that support the proxy pattern (a Caller authorized
// to assert other Callers via the X-Asserted-Caller header).
//
// The bot integration use case: a Slack/GChat bot authenticates as
// itself, then asserts the human user's identity per request so audit
// logs and per-caller MCP credentials attribute to the human, not the
// bot. CanProxyAs reports whether the resolved Caller is on the
// configured proxy allowlist.
//
// Authenticators that don't implement this interface implicitly deny
// all proxy assertions.
type AuthenticatorWithProxy interface {
	Authenticator
	CanProxyAs(c Caller) bool
}

// HeaderAssertedCaller is the conventional header name a proxy Caller
// uses to assert the effective identity. The actual header name is
// operator-configurable (attach.multi_session.asserted_caller_header)
// but this constant is the default.
const HeaderAssertedCaller = "X-Asserted-Caller"

// AnonymousAuth resolves every request to the same Caller. It is the
// default Authenticator wired into pkg/attach when multi-session is
// disabled — every request is "anon" (or whatever default identity the
// operator configured) and downstream code sees a Caller-on-context
// just like in multi-session deployments.
//
// Zero value resolves to the package-level Anonymous Caller. Set
// Caller explicitly to override the identity (the operator-facing
// knob is attach.multi_session.default_identity).
type AnonymousAuth struct {
	Caller Caller
}

// Authenticate ignores the request and returns the configured Caller.
// Never returns an error.
func (a AnonymousAuth) Authenticate(_ *http.Request) (Caller, error) {
	if a.Caller.Identity == "" {
		return Anonymous, nil
	}
	return a.Caller, nil
}

// BearerTokenAuth validates the request's bearer token against a static
// table loaded from users.json. Returns the matched Caller, or
// ErrUnauthenticated when no token is presented or the token is unknown.
//
// Comparison is of SHA-256 digests, constant-time
// (subtle.ConstantTimeCompare) to avoid leaking match prefixes through
// response timing; see Authenticate. Identities are not exposed by the
// lookup path.
//
// Accepted headers, in order:
//
//  1. X-Attach-Token (matches the existing daemon-level side-channel
//     header used when an identity gateway owns Authorization)
//  2. Authorization: Bearer <token>
//
// Proxy semantics: a Caller resolved here is permitted to assert other
// identities via X-Asserted-Caller only if it appears in the
// ProxyIdentities allowlist. See CanProxyAs.
//
// The authenticator holds digests, never tokens (#1213). Every row —
// a token_sha256 row, or a legacy plaintext row hashed here at
// construction — is reduced to the SHA-256 of its token, and a request
// is matched by hashing the presented token and comparing digests. One
// comparison path serves both row shapes, so a plaintext row can't
// authenticate anything its hashed form wouldn't.
type BearerTokenAuth struct {
	rows       []bearerRow       // every usable row, in table order
	byIdentity map[string]Caller // identity → Caller (for asserted-caller validation)
	// proxyAllowed is the operator's X-Asserted-Caller allowlist.
	proxyAllowed map[string]struct{}
}

// bearerRow is one credential the authenticator accepts.
type bearerRow struct {
	digest [sha256.Size]byte
	caller Caller
}

// NewBearerTokenAuth builds an authenticator from a parsed user table
// (typically the result of LoadUsersFile). adminIdentities marks the
// listed identities as Admin Callers; proxyIdentities marks them as
// permitted to use X-Asserted-Caller.
//
// Rows without an identity, or without exactly one usable credential
// (a plaintext token, or a well-formed token_sha256), are skipped — a
// misconfigured row shouldn't authenticate every credential-less
// request. Duplicates are last-write-wins; the loader rejects them
// upstream but the authenticator is defensive.
func NewBearerTokenAuth(users []User, adminIdentities, proxyIdentities []string) *BearerTokenAuth {
	adminSet := stringSet(adminIdentities)
	proxySet := stringSet(proxyIdentities)

	rows := make([]bearerRow, 0, len(users))
	byIdentity := make(map[string]Caller, len(users))
	for _, u := range users {
		if u.Identity == "" {
			continue
		}
		sum, ok := u.digest()
		if !ok {
			continue
		}
		c := Caller{
			Identity: u.Identity,
			Labels:   u.Labels,
		}
		if _, ok := adminSet[u.Identity]; ok {
			c.Admin = true
		}
		rows = append(rows, bearerRow{digest: sum, caller: c})
		byIdentity[u.Identity] = c
	}
	return &BearerTokenAuth{
		rows:         rows,
		byIdentity:   byIdentity,
		proxyAllowed: proxySet,
	}
}

// Authenticate resolves the request's bearer token against the table.
// Returns ErrUnauthenticated when no token is presented or the token
// is not in the table.
//
// The presented token is hashed and its digest compared against every
// row with subtle.ConstantTimeCompare, with no early exit: the time
// taken depends on the table size, which the operator controls and is
// not secret, and not on which row matched or how much of a digest
// agreed.
func (b *BearerTokenAuth) Authenticate(r *http.Request) (Caller, error) {
	token := extractToken(r)
	if token == "" {
		return Caller{}, ErrUnauthenticated
	}
	presented := sha256.Sum256([]byte(token))
	var (
		matched Caller
		found   int
	)
	for i := range b.rows {
		hit := subtle.ConstantTimeCompare(presented[:], b.rows[i].digest[:])
		if hit == 1 {
			matched = b.rows[i].caller
		}
		found |= hit
	}
	if found != 1 {
		return Caller{}, ErrUnauthenticated
	}
	return matched, nil
}

// CanProxyAs reports whether c is on the operator-configured proxy
// allowlist. Returns false for callers not in the allowlist and for
// the zero-value Caller (defense against accidental authorization).
func (b *BearerTokenAuth) CanProxyAs(c Caller) bool {
	if c.Identity == "" {
		return false
	}
	_, ok := b.proxyAllowed[c.Identity]
	return ok
}

// HasIdentity reports whether the named identity exists in the user
// table. Used by the proxy path: a bot can only assert identities the
// operator has provisioned (see ErrAssertedCallerUnknown).
func (b *BearerTokenAuth) HasIdentity(identity string) bool {
	_, ok := b.byIdentity[identity]
	return ok
}

// LookupIdentity returns the Caller registered for the given identity,
// or zero-Caller + false when the identity is not in the table. Used
// by the proxy path to materialize the asserted Caller (preserving
// Labels and Admin flag from the user table entry).
func (b *BearerTokenAuth) LookupIdentity(identity string) (Caller, bool) {
	c, ok := b.byIdentity[identity]
	return c, ok
}

// extractToken pulls the bearer token from either the X-Attach-Token
// side-channel header (matches the existing pkg/attach convention) or
// the Authorization: Bearer header. Returns empty string when neither
// is present.
func extractToken(r *http.Request) string {
	if side := r.Header.Get("X-Attach-Token"); side != "" {
		return side
	}
	const prefix = "Bearer "
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, prefix) {
		return ""
	}
	return strings.TrimSpace(auth[len(prefix):])
}

func stringSet(xs []string) map[string]struct{} {
	out := make(map[string]struct{}, len(xs))
	for _, x := range xs {
		if x == "" {
			continue
		}
		out[x] = struct{}{}
	}
	return out
}
