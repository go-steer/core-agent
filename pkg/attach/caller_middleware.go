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
	"net/http"

	"github.com/go-steer/core-agent/pkg/auth"
)

// callerMiddleware resolves the per-request Caller via the configured
// Authenticator and attaches it to the request context. Downstream
// handlers read the Caller via auth.CallerFromContext.
//
// α.1 posture: this middleware is purely additive. Even if the
// Authenticator returns ErrUnauthenticated (e.g., a BearerTokenAuth
// instance was wired but the request carries no token), the middleware
// downgrades to the AnonymousAuth fallback rather than returning 401.
// No existing handler reads the Caller yet, so this preserves today's
// observable behavior end-to-end.
//
// α.2 flips the posture: callers can opt into 401 on unauthenticated
// requests (via the multi_session.allow_anonymous config knob) and
// handlers begin enforcing ACL on the resolved Caller. The wiring
// topology does not change between α.1 and α.2 — only the behavior on
// authentication failure.
func callerMiddleware(authn auth.Authenticator, fallback auth.Caller, next http.Handler) http.Handler {
	if authn == nil {
		authn = auth.AnonymousAuth{Caller: fallback}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := authn.Authenticate(r)
		if err != nil {
			// α.1: never 401 here — α.2 adds the enforcement path. The
			// fallback identity is what audit logs / handlers see for
			// any request that didn't authenticate. Anonymous by
			// design (see auth.Anonymous).
			c = fallback
			if c.Identity == "" {
				c = auth.Anonymous
			}
		}
		ctx := auth.WithCaller(r.Context(), c)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
