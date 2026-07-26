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
	"fmt"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// browserWriteGuard blocks the browser CSRF vectors against the
// attach listener (#383). A malicious page the operator visits can
// fire a CORS "simple request" (e.g. Content-Type: text/plain POST,
// or a body-less POST) at http://localhost:7777 — no preflight, so
// the side effect lands even though the response stays unreadable.
// The bootstrap session id ("default") is well-known, making
// /sessions/default/inject a one-liner from any web page.
//
// Two checks run on every state-changing request (any method other
// than GET/HEAD/OPTIONS), regardless of token/auth mode:
//
//  1. Origin enforcement: when an Origin header is present it must be
//     a loopback origin (localhost / 127.0.0.0/8 / [::1]) or a self
//     origin (host matching the request's Host), otherwise 403.
//     Browsers always attach Origin to cross-site POSTs; native
//     clients (curl, core-agent-tui, SDKs) send no Origin and pass
//     untouched.
//
//  2. Content-Type: application/json is required, otherwise 415.
//     A cross-site form/fetch simple request can only send
//     text/plain, multipart/form-data, or
//     application/x-www-form-urlencoded — forcing JSON means any
//     browser request with a mutating body must go through a CORS
//     preflight, which the server never approves.
//
// Read endpoints (GET /events, GET /sessions, ...) are unaffected:
// cross-origin reads are already unreadable to the attacking page
// because the server sets no Access-Control-Allow-Origin.
func browserWriteGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isWriteMethod(r.Method) {
			if origin := r.Header.Get("Origin"); origin != "" && !originAllowed(origin, r.Host) {
				http.Error(w, fmt.Sprintf("cross-origin request rejected: Origin %q is neither a loopback origin nor this listener (%q); "+
					"browser pages may not drive the attach API cross-site. Native clients should omit the Origin header.", origin, r.Host),
					http.StatusForbidden)
				return
			}
			if !isJSONContentType(r.Header.Get("Content-Type")) {
				http.Error(w, "unsupported media type: state-changing attach endpoints require \"Content-Type: application/json\" "+
					"(browser cross-site request forgery protection; add the header even when the request has no body)",
					http.StatusUnsupportedMediaType)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// isJSONContentType reports whether the Content-Type header names the
// application/json media type (parameters like charset are fine).
// Empty or unparseable values are rejected — a missing header on a
// write is exactly the body-less simple-request shape we're closing.
func isJSONContentType(ct string) bool {
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return false
	}
	return mt == "application/json"
}

// originAllowed reports whether a browser-supplied Origin header value
// may drive state-changing endpoints: loopback origins (the operator's
// own machine, e.g. the /ui SPA on another local port) and self
// origins (scheme://host matching the listener as seen in the
// request's Host header — same-origin fetches from the /ui SPA carry
// this). Everything else — including the literal "null" origin of
// sandboxed iframes and file:// pages, and unparseable values — is
// rejected.
func originAllowed(origin, requestHost string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return false // includes "null" and malformed values
	}
	host := u.Hostname()
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return true
	}
	// Self origin: the origin's host:port equals the Host the request
	// was addressed to. Scheme is deliberately not compared — behind a
	// TLS-terminating proxy the browser origin is https while the
	// listener sees http, and the host match is the meaningful part.
	return strings.EqualFold(u.Host, requestHost)
}
