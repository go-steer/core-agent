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
// loopbackHostGuard validates the HTTP Host header on every request
// (both reads and writes) when the attach listener is bound to a
// loopback address (#1358).
//
// Why this is needed alongside browserWriteGuard:
//   - A DNS-rebinding page at http://evil.example:7777 initially
//     resolves evil.example to the attacker's IP, then rebinds DNS to
//     127.0.0.1. Because the browser considers subsequent requests to
//     http://evil.example:7777 same-origin, it sends Host:
//     evil.example:7777 and Origin: http://evil.example:7777.
//   - Without Host validation, GET endpoints (/sessions, /events SSE)
//     are readable directly across the rebound origin, and
//     originAllowed's self-origin check (u.Host == r.Host) treats the
//     attacker's own origin as a trusted reverse-proxy hostname.
//
// Only loopback literals (127.0.0.0/8, ::1), "localhost", and
// operator-configured AllowedHosts pass; everything else is rejected
// with 421 Misdirected Request before reaching any handler.
func loopbackHostGuard(allowedHosts []string, next http.Handler) http.Handler {
	allow := normalizeHostSet(allowedHosts)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !requestHostAllowed(r.Host, allow) {
			http.Error(w, fmt.Sprintf("misdirected request: Host %q is not a loopback address or an allowed host for this loopback listener (#1358); "+
				"set attach.allowed_hosts in config if a reverse proxy forwards to loopback under a custom hostname", r.Host),
				http.StatusMisdirectedRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func normalizeHostSet(hosts []string) map[string]struct{} {
	if len(hosts) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(hosts))
	for _, h := range hosts {
		h = strings.TrimSpace(h)
		if h == "" {
			continue
		}
		if host, _, err := net.SplitHostPort(h); err == nil && host != "" {
			h = host
		}
		h = strings.TrimPrefix(strings.TrimSuffix(h, "]"), "[")
		out[strings.ToLower(h)] = struct{}{}
	}
	return out
}

func isLoopbackHost(hostname string) bool {
	if strings.EqualFold(hostname, "localhost") {
		return true
	}
	ip := net.ParseIP(hostname)
	return ip != nil && ip.IsLoopback()
}

func extractHostname(hostport string) string {
	hostport = strings.TrimSpace(hostport)
	if hostport == "" {
		return ""
	}
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	// Bare IPv6 in brackets "[::1]" or bare hostname/IPv4 without port.
	if strings.HasPrefix(hostport, "[") && strings.HasSuffix(hostport, "]") {
		return hostport[1 : len(hostport)-1]
	}
	return hostport
}

func requestHostAllowed(requestHost string, allowedHosts map[string]struct{}) bool {
	h := extractHostname(requestHost)
	if h == "" {
		return false
	}
	if isLoopbackHost(h) {
		return true
	}
	if len(allowedHosts) > 0 {
		_, ok := allowedHosts[strings.ToLower(h)]
		return ok
	}
	return false
}

func browserWriteGuardWithHosts(loopbackListener bool, allowedHosts []string, next http.Handler) http.Handler {
	allow := normalizeHostSet(allowedHosts)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isWriteMethod(r.Method) {
			if origin := r.Header.Get("Origin"); origin != "" && !originAllowedWithHosts(origin, r.Host, loopbackListener, allow) {
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
	return originAllowedWithHosts(origin, requestHost, false, nil)
}

func originAllowedWithHosts(origin, requestHost string, loopbackListener bool, allowedHosts map[string]struct{}) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return false // includes "null" and malformed values
	}
	host := u.Hostname()
	if isLoopbackHost(host) {
		return true
	}
	// On a loopback listener, the self-origin fallback (u.Host ==
	// requestHost) is only trusted when requestHost itself passed the
	// loopback/AllowedHosts allowlist (#1358). Otherwise a DNS-rebound
	// browser request with Host: evil.example:7777 and Origin:
	// http://evil.example:7777 would satisfy u.Host == requestHost.
	if loopbackListener && !requestHostAllowed(requestHost, allowedHosts) {
		return false
	}
	// Self origin: the origin's host:port equals the Host the request
	// was addressed to. Scheme is deliberately not compared — behind a
	// TLS-terminating proxy the browser origin is https while the
	// listener sees http, and the host match is the meaningful part.
	return strings.EqualFold(u.Host, requestHost)
}
