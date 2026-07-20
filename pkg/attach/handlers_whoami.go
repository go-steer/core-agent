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
	"strings"

	"github.com/go-steer/core-agent/v2/pkg/auth"
)

// GET /whoami — resolved caller identity for the current request.
// Session-agnostic; the standard middleware runs so a listener that
// requires a bearer token still 401s an unauthenticated caller
// exactly like every other route. When the middleware allows through
// (anonymous or authenticated), the handler returns whatever the
// resolver stamped onto the context.
//
// Companion to the v1.4.0 `capabilities.caller_id` display hint
// (see events.go + capabilities.go): the SSE stream advertises the
// identity as a fast path so clients don't need a second fetch, but
// this endpoint is the canonical source and carries the admin flag +
// auth-source discriminator too.

// WhoAmIResponse is the response shape of GET /whoami. Source is a
// coarse discriminator so client-side auth-debug flows can show
// "authenticated via bearer" vs "impersonating via asserted-caller"
// without needing to inspect the request headers themselves.
//
// Consumers MUST tolerate unknown Source values — a future
// authenticator (K8s SA, OIDC/JWT) will add its own tag.
type WhoAmIResponse struct {
	Identity string `json:"identity"`
	Admin    bool   `json:"admin,omitempty"`
	Source   string `json:"source"`
	// ProxyBy carries the credential that asserted this identity when
	// source == "asserted" — the bot/service identity behind the
	// X-Asserted-Caller header. Empty for non-proxy paths.
	ProxyBy string `json:"proxy_by,omitempty"`
}

// Source values for WhoAmIResponse.Source. String constants so
// downstream tools can switch on them without a Go dependency.
const (
	// WhoAmISourceBearer — Authorization: Bearer or X-Attach-Token
	// header. Covers both static-table bearer auth and any future
	// bearer-flavored authenticator (OIDC/JWT bearer, etc.).
	WhoAmISourceBearer = "bearer"
	// WhoAmISourceMTLS — client presented a TLS certificate that
	// passed RequireAndVerifyClientCert.
	WhoAmISourceMTLS = "mtls"
	// WhoAmISourceIAP — request came through an identity gateway
	// (Google IAP / Cloud Run IAM, Cloudflare Access, etc.) that
	// stamps an authenticated-user header. Best-effort detection —
	// only Google IAP's X-Goog-Authenticated-User-Email is probed
	// today; other gateways add to the known-headers list as we
	// integrate them.
	WhoAmISourceIAP = "iap"
	// WhoAmISourceAsserted — request came from a proxying credential
	// that used X-Asserted-Caller to assert another identity. The
	// proxying identity is exposed via ProxyBy.
	WhoAmISourceAsserted = "asserted"
	// WhoAmISourceAnonymous — no credential was presented and the
	// listener allowed the request through (AllowAnonymous=true or
	// multi-session disabled). Identity is the daemon's configured
	// default (typically "anon").
	WhoAmISourceAnonymous = "anonymous"
)

// registerWhoAmI wires GET /whoami onto the mux. Called from
// handlers.register alongside the session-scoped routes; kept in
// its own file so the "who am I" concept stays readable.
func (h *handlers) registerWhoAmI(mux *http.ServeMux) {
	mux.HandleFunc("GET /whoami", h.doWhoAmI)
}

func (h *handlers) doWhoAmI(w http.ResponseWriter, r *http.Request) {
	c, _ := auth.CallerFromContext(r.Context())
	proxyBy, _ := auth.ProxyByFromContext(r.Context())
	resp := WhoAmIResponse{
		Identity: c.Identity,
		Admin:    c.Admin,
		Source:   detectAuthSource(r, c, proxyBy),
		ProxyBy:  proxyBy,
	}
	writeJSON(w, http.StatusOK, resp)
}

// detectAuthSource classifies how the request was authenticated.
// Precedence matches operator intent: an asserted-caller path wins
// over the underlying bearer (so /whoami shows the impersonated
// user's audit-relevant path), then bearer, then mTLS, then IAP,
// then anonymous.
func detectAuthSource(r *http.Request, c auth.Caller, proxyBy string) string {
	if proxyBy != "" {
		return WhoAmISourceAsserted
	}
	if r.Header.Get(HeaderAttachToken) != "" ||
		strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		return WhoAmISourceBearer
	}
	if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
		return WhoAmISourceMTLS
	}
	if r.Header.Get("X-Goog-Authenticated-User-Email") != "" ||
		r.Header.Get("X-Goog-Iap-Jwt-Assertion") != "" {
		return WhoAmISourceIAP
	}
	// Callers falling through to here presented no recognizable
	// credential. The identity might still be non-anon if the
	// operator configured a custom default via
	// attach.multi_session.default_identity — but the AUTH SOURCE
	// is still "anonymous" because no credential was validated.
	_ = c
	return WhoAmISourceAnonymous
}
