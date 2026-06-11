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
	"errors"
	"log"
	"net/http"

	"github.com/go-steer/core-agent/pkg/auth"
)

// callerMiddlewareConfig packages the per-server settings the
// middleware needs. Separated from Options so the middleware can be
// constructed cheaply in tests without spinning up a full Server.
type callerMiddlewareConfig struct {
	// authenticator resolves the inbound credential to a Caller.
	// Nil defaults to AnonymousAuth.
	authenticator auth.Authenticator
	// fallback is the Caller used when authenticator returns
	// ErrUnauthenticated AND enforceAuthentication is false. Zero
	// resolves to auth.Anonymous.
	fallback auth.Caller
	// enforceAuthentication, when true, returns 401 on
	// ErrUnauthenticated instead of falling back to the anonymous
	// Caller. Set when MultiSessionEnabled && !AllowAnonymous.
	enforceAuthentication bool
	// proxyHeader names the header a proxy Caller uses to assert
	// another identity. Empty defaults to auth.HeaderAssertedCaller.
	// Only honored when the resolved Authenticator implements
	// AuthenticatorWithProxy.
	proxyHeader string
}

// callerMiddleware preserves the α.1 signature for callers that don't
// need the enforcement / proxy plumbing. Delegates to
// callerMiddlewareWithConfig with the no-enforcement default — every
// existing test path keeps working without ceremony.
func callerMiddleware(authn auth.Authenticator, fallback auth.Caller, next http.Handler) http.Handler {
	return callerMiddlewareWithConfig(callerMiddlewareConfig{
		authenticator: authn,
		fallback:      fallback,
	}, next)
}

// callerMiddlewareWithConfig resolves the per-request Caller and
// (when configured) the proxy-asserted identity, then attaches both
// to the request context for downstream handlers to read via
// auth.CallerFromContext + auth.ProxyByFromContext.
//
// Behavior matrix:
//
//   - enforceAuthentication=false (default): ErrUnauthenticated
//     downgrades to fallback Caller. No 401 path. Preserves the α.1
//     no-behavior-change posture.
//   - enforceAuthentication=true (multi-session with
//     AllowAnonymous=false): ErrUnauthenticated returns 401.
//
// Proxy resolution happens AFTER the base Caller resolves, only if
// the request carries the proxy header AND the authenticator
// implements AuthenticatorWithProxy AND the base Caller is on the
// proxy allowlist AND the asserted identity is a provisioned user.
// Any failure of those preconditions returns 401 — a bad proxy
// assertion is a security event, not a fall-back-to-anonymous case.
func callerMiddlewareWithConfig(cfg callerMiddlewareConfig, next http.Handler) http.Handler {
	authn := cfg.authenticator
	if authn == nil {
		authn = auth.AnonymousAuth{Caller: cfg.fallback}
	}
	header := cfg.proxyHeader
	if header == "" {
		header = auth.HeaderAssertedCaller
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := authn.Authenticate(r)
		if err != nil {
			if cfg.enforceAuthentication {
				writeUnauthorized(w, err)
				return
			}
			c = cfg.fallback
			if c.Identity == "" {
				c = auth.Anonymous
			}
		}

		// Proxy resolution path. When set, the asserting Caller must
		// be allowlisted to proxy AND the asserted identity must be
		// provisioned. Either failure returns 401 — silently
		// falling back to the non-proxy identity would mask
		// misconfiguration that could let a compromised bot
		// impersonate users.
		var proxyBy string
		if asserted := r.Header.Get(header); asserted != "" {
			effective, by, err := resolveProxyAssertion(authn, c, asserted)
			if err != nil {
				// Log and return 401 — both ErrAssertedCallerForbidden
				// and ErrAssertedCallerUnknown surface here. The
				// log line uses the proxying caller's identity so
				// operators can correlate suspicious requests.
				log.Printf("attach: proxy assertion rejected: requester=%q asserted=%q: %v", //nolint:gosec // forensic audit line for rejected proxy attempts; %q escapes control chars in the user-supplied header value
					c.Identity, asserted, err)
				writeUnauthorized(w, err)
				return
			}
			c, proxyBy = effective, by
		}

		ctx := auth.WithCaller(r.Context(), c)
		if proxyBy != "" {
			ctx = auth.WithProxyBy(ctx, proxyBy)
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// resolveProxyAssertion validates that requester is permitted to
// assert asserted, and returns the effective Caller (looked up from
// the user table where available) plus the proxying identity for
// audit log threading. Returns ErrAssertedCallerForbidden when the
// requester isn't on the proxy allowlist, ErrAssertedCallerUnknown
// when the asserted identity isn't provisioned.
func resolveProxyAssertion(authn auth.Authenticator, requester auth.Caller, asserted string) (auth.Caller, string, error) {
	proxer, ok := authn.(auth.AuthenticatorWithProxy)
	if !ok {
		// The authenticator doesn't support proxying — any asserted
		// header on this path is an operator misconfiguration. Treat
		// as forbidden (rather than silently dropping the header)
		// so the operator sees the failure mode.
		return auth.Caller{}, "", auth.ErrAssertedCallerForbidden
	}
	if !proxer.CanProxyAs(requester) {
		return auth.Caller{}, "", auth.ErrAssertedCallerForbidden
	}
	// Materialize the asserted Caller from the provisioned table so
	// downstream code sees the same Labels / Admin flag as a direct
	// auth would have. For authenticators that don't expose a
	// lookup (e.g., future OIDC adapter that mints Callers per
	// request), fall back to a minimal Caller carrying just the
	// asserted Identity — better than rejecting outright, since
	// claim-based auth has no static table to check against.
	if lookup, ok := authn.(interface {
		LookupIdentity(string) (auth.Caller, bool)
	}); ok {
		c, ok := lookup.LookupIdentity(asserted)
		if !ok {
			return auth.Caller{}, "", auth.ErrAssertedCallerUnknown
		}
		return c, requester.Identity, nil
	}
	return auth.Caller{Identity: asserted}, requester.Identity, nil
}

// writeUnauthorized writes a 401 with a stable error body so clients
// can distinguish transport-level auth failure (401 from
// AuthConfig.Middleware) from per-caller resolution failure (this
// path). Includes a WWW-Authenticate hint pointing at the bearer
// realm so machine clients can react.
func writeUnauthorized(w http.ResponseWriter, err error) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="attach-multisession"`)
	msg := "unauthorized"
	switch {
	case errors.Is(err, auth.ErrAssertedCallerForbidden):
		msg = "asserted-caller header rejected: caller is not permitted to proxy"
	case errors.Is(err, auth.ErrAssertedCallerUnknown):
		msg = "asserted-caller header rejected: identity is not provisioned"
	case errors.Is(err, auth.ErrUnauthenticated):
		msg = "unauthorized: no valid credential"
	}
	http.Error(w, msg, http.StatusUnauthorized)
}
