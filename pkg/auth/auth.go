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

// Package auth defines the per-caller identity primitive used by the
// multi-session attach layer. See docs/multi-session-design.md for the
// full design.
//
// The package is intentionally substrate-only: it defines Caller,
// Authenticator, the authorization matrix, and the static user table
// loader. Wiring into the attach server, per-session sub-gates, and
// audit-log threading are layered on in later phases (see issue #162).
//
// Backward compatibility: when no Authenticator is configured, callers
// should default to AnonymousAuth — every request resolves to the same
// "anon" identity and the multi-session features behave as if disabled.
package auth

import (
	"context"
	"errors"
)

// Caller is the opaque identity attached to every authenticated request
// entering the daemon. Subsequent code uses it for authorization
// (see Authorize) and audit (see eventlog metadata, layered in α.2).
//
// Identity is a stable opaque ID — typically an email ("alice@example.com"),
// a service-account marker ("sa:platform-agent"), or "anon" for
// unauthenticated requests when anonymous access is allowed.
//
// Labels carry free-form metadata from the auth source (e.g.,
// {"team": "platform", "issuer": "https://oidc.example.com"}). Available
// to authorization logic and audit consumers; not part of the Identity
// equality check.
//
// Admin grants the see-everything role; set per configuration
// (attach.multi_session.admin_identities). Admin Callers pass every
// Authorize check.
type Caller struct {
	Identity string
	Labels   map[string]string
	Admin    bool
}

// Anonymous is the conventional Caller for unauthenticated requests
// when AnonymousAuth is in effect. Callers of AnonymousAuth may override
// the identity via configuration (attach.multi_session.default_identity)
// but "anon" is the documented default.
var Anonymous = Caller{Identity: "anon"}

// ErrUnauthenticated is returned by Authenticator.Authenticate when no
// valid credential is present on the request. Callers map this to a
// 401 response.
var ErrUnauthenticated = errors.New("auth: unauthenticated")

// ErrAssertedCallerForbidden is returned when a non-proxy Caller
// attempts to use the X-Asserted-Caller header. Callers map this to a
// 401 response and should log the attempt — it indicates either
// misconfiguration or a credential that should not be assertable.
var ErrAssertedCallerForbidden = errors.New("auth: caller is not permitted to assert identities")

// ErrAssertedCallerUnknown is returned when a proxy Caller asserts an
// identity that is not present in the configured user table. Callers
// map this to a 401 response.
var ErrAssertedCallerUnknown = errors.New("auth: asserted identity is not provisioned")

type callerKey struct{}
type proxyByKey struct{}

// WithCaller returns a new context carrying c. Use in middleware that
// has resolved the request's Caller; downstream code reads it via
// CallerFromContext.
func WithCaller(ctx context.Context, c Caller) context.Context {
	return context.WithValue(ctx, callerKey{}, c)
}

// CallerFromContext returns the Caller previously stored on ctx by
// WithCaller. ok is false when no Caller is present (e.g., code paths
// reached before the authenticator middleware, or in tests).
func CallerFromContext(ctx context.Context) (c Caller, ok bool) {
	c, ok = ctx.Value(callerKey{}).(Caller)
	return c, ok
}

// WithProxyBy returns a new context carrying the proxying identity
// alongside the effective Caller. Set when the request was routed via
// the proxy path (X-Asserted-Caller header) so audit logs can capture
// BOTH the effective Caller and the credential that asserted it
// (e.g., effective="alice@", proxy_by="sa:slack-bot").
//
// Pair with WithCaller (which carries the effective identity);
// downstream code reads via ProxyByFromContext.
func WithProxyBy(ctx context.Context, by string) context.Context {
	return context.WithValue(ctx, proxyByKey{}, by)
}

// ProxyByFromContext returns the proxying identity previously stored
// on ctx by WithProxyBy. ok is false (and the string empty) when the
// request did not go through the proxy path.
func ProxyByFromContext(ctx context.Context) (by string, ok bool) {
	by, ok = ctx.Value(proxyByKey{}).(string)
	if !ok || by == "" {
		return "", false
	}
	return by, true
}
