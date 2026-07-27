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
// Source only ever reports a value the SERVER verified or was
// explicitly configured to trust (#385): the caller-resolution
// middleware stamps its verdict onto the request context and the
// handler echoes it. It is never re-derived from raw request headers
// — an Authorization header the server didn't validate, or a
// gateway-style X-Goog-* header any client can forge, does not
// change Source.
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
	// WhoAmISourceBearer — the server validated a bearer token:
	// either the per-caller authenticator (static-table
	// BearerTokenAuth, or a future bearer-flavored OIDC/JWT
	// authenticator) accepted the credential, or the listener's
	// transport-level bearer gate (Options.Auth.BearerToken) let the
	// request through. The mere PRESENCE of an Authorization /
	// X-Attach-Token header does not produce this value.
	WhoAmISourceBearer = "bearer"
	// WhoAmISourceMTLS — OUR listener verified the client's TLS
	// certificate against its configured CA (ClientAuth =
	// RequireAndVerifyClientCert via Auth.ClientCAFile). A presented
	// -but-unverified certificate does not count.
	WhoAmISourceMTLS = "mtls"
	// WhoAmISourceIAP — reserved for a future verified identity-
	// gateway integration (Google IAP JWT-assertion validation,
	// Cloudflare Access, etc.). NOT currently emitted: the server
	// used to infer it from the X-Goog-Authenticated-User-Email /
	// X-Goog-Iap-Jwt-Assertion request headers, but those are
	// forgeable by any client the listener accepts, so the label
	// was dropped until the server actually validates a gateway
	// assertion (#385). Operators fronting the daemon with a
	// trusted gateway should configure the asserted-caller
	// ProxyHeader path, which reports "asserted".
	WhoAmISourceIAP = "iap"
	// WhoAmISourceAsserted — a proxy-allowlisted credential used the
	// configured asserted-caller header (Options.ProxyHeader,
	// default X-Asserted-Caller) and the middleware VALIDATED the
	// assertion (requester on the proxy allowlist, asserted identity
	// provisioned). The proxying identity is exposed via ProxyBy.
	WhoAmISourceAsserted = "asserted"
	// WhoAmISourceAnonymous — the server verified no credential for
	// this request and the listener allowed it through
	// (AllowAnonymous=true or multi-session disabled). Identity is
	// the daemon's configured default (typically "anon"). Note this
	// covers requests that CARRIED credential-looking headers the
	// server had no validator for.
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
	// Source is the caller middleware's verdict, threaded via the
	// request context — the middleware is the component that
	// actually verified (or declined to verify) the credential, so
	// it is the only trustworthy classifier. The handler must NOT
	// re-derive the source from request headers: pre-#385 it probed
	// Authorization / r.TLS.PeerCertificates / X-Goog-* directly,
	// which let any client (or misbehaving fronting proxy) claim
	// "bearer"/"mtls"/"iap" without the server having validated
	// anything. Absent verdict (no middleware ran) = anonymous:
	// nothing was verified.
	source, ok := authSourceFromContext(r.Context())
	if !ok {
		source = WhoAmISourceAnonymous
	}
	// proxyBy is itself middleware-validated (a failed assertion
	// 401s before any handler runs), so its presence always means
	// the asserted path — keep the two fields consistent even for
	// exotic embeddings that stamp the context themselves.
	if proxyBy != "" {
		source = WhoAmISourceAsserted
	}
	resp := WhoAmIResponse{
		Identity: c.Identity,
		Admin:    c.Admin,
		Source:   source,
		ProxyBy:  proxyBy,
	}
	writeJSON(w, http.StatusOK, resp)
}
