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
// Token comparison is constant-time (subtle.ConstantTimeCompare) to
// avoid leaking match prefixes through response timing. The table is
// indexed by token for O(1) lookup; identities are not exposed by the
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
type BearerTokenAuth struct {
	tokens          map[string]Caller // token → Caller
	identityToToken map[string]string // identity → token (for asserted-caller validation)
	proxyAllowed    map[string]struct{}
}

// NewBearerTokenAuth builds an authenticator from a parsed user table
// (typically the result of LoadUsersFile). adminIdentities marks the
// listed identities as Admin Callers; proxyIdentities marks them as
// permitted to use X-Asserted-Caller.
//
// Empty tokens in the users slice are skipped — a misconfigured row
// shouldn't authenticate every credential-less request. Duplicate
// tokens are last-write-wins; the loader should reject duplicates
// upstream but the authenticator is defensive.
func NewBearerTokenAuth(users []User, adminIdentities, proxyIdentities []string) *BearerTokenAuth {
	adminSet := stringSet(adminIdentities)
	proxySet := stringSet(proxyIdentities)

	tokens := make(map[string]Caller, len(users))
	identityToToken := make(map[string]string, len(users))
	for _, u := range users {
		if u.Token == "" || u.Identity == "" {
			continue
		}
		c := Caller{
			Identity: u.Identity,
			Labels:   u.Labels,
		}
		if _, ok := adminSet[u.Identity]; ok {
			c.Admin = true
		}
		tokens[u.Token] = c
		identityToToken[u.Identity] = u.Token
	}
	return &BearerTokenAuth{
		tokens:          tokens,
		identityToToken: identityToToken,
		proxyAllowed:    proxySet,
	}
}

// Authenticate resolves the request's bearer token against the table.
// Returns ErrUnauthenticated when no token is presented or the token
// is not in the table.
func (b *BearerTokenAuth) Authenticate(r *http.Request) (Caller, error) {
	token := extractToken(r)
	if token == "" {
		return Caller{}, ErrUnauthenticated
	}
	for known, c := range b.tokens {
		// Constant-time compare per token; the loop itself is not
		// constant-time across table sizes, but the only signal it
		// leaks is "how many tokens does the daemon have configured,"
		// which is not sensitive (and operators control directly).
		if subtle.ConstantTimeCompare([]byte(token), []byte(known)) == 1 {
			return c, nil
		}
	}
	return Caller{}, ErrUnauthenticated
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
	_, ok := b.identityToToken[identity]
	return ok
}

// LookupIdentity returns the Caller registered for the given identity,
// or zero-Caller + false when the identity is not in the table. Used
// by the proxy path to materialize the asserted Caller (preserving
// Labels and Admin flag from the user table entry).
func (b *BearerTokenAuth) LookupIdentity(identity string) (Caller, bool) {
	token, ok := b.identityToToken[identity]
	if !ok {
		return Caller{}, false
	}
	c, ok := b.tokens[token]
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
