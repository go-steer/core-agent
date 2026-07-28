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
	"net/http"
	"strconv"
	"strings"
)

// headerProtocolVersion is the request/response header carrying the SSE
// event-stream protocol version. On an /events stream request the
// client MAY declare the version it speaks; the server always echoes
// the version it speaks on the response so a client can detect skew
// even before the `capabilities` frame arrives. Mirrors the X-Attach-*
// header convention (see HeaderAttachToken).
const headerProtocolVersion = "X-Attach-Protocol-Version" //nolint:gosec // header name, not a credential

// queryProtocolVersion is the query-param equivalent of
// headerProtocolVersion, for URL-only clients (curl, browsers) that
// can't set a custom header. Takes precedence over the header when both
// are present.
const queryProtocolVersion = "protocol"

// clientProtocolVersion extracts the client-declared protocol version
// from the request: the ?protocol= query param wins, falling back to
// the X-Attach-Protocol-Version header. Returns "" when the client
// declared nothing — the pre-negotiation back-compat case, which every
// client shipped before this negotiation existed.
func clientProtocolVersion(r *http.Request) string {
	if v := strings.TrimSpace(r.URL.Query().Get(queryProtocolVersion)); v != "" {
		return v
	}
	return strings.TrimSpace(r.Header.Get(headerProtocolVersion))
}

// protocolMajor parses the major component of a semver-ish version
// string. "1.4.0", "v2", and "1.4.0-rc.1" all parse (leading "v" is
// tolerated; the major is everything up to the first '.', '-', or '+').
// Returns ok=false when the string has no leading non-negative integer
// major component.
func protocolMajor(version string) (int, bool) {
	v := strings.TrimPrefix(strings.TrimSpace(version), "v")
	if v == "" {
		return 0, false
	}
	if end := strings.IndexAny(v, ".-+"); end >= 0 {
		v = v[:end]
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// negotiateProtocolVersion enforces SSE protocol-version compatibility
// on an /events stream request. It always stamps the response's
// X-Attach-Protocol-Version header with the version this server speaks
// so the client learns it even on success. When the client declares a
// version (via the ?protocol= query param or the X-Attach-Protocol-Version
// header):
//
//   - a malformed value → 400 Bad Request. The client asked for
//     something we can't reason about; failing loudly beats guessing a
//     major and streaming frames it might mis-parse.
//   - a major-version mismatch → 409 Conflict. A different major means
//     a breaking wire-shape change (per semver + the protocol's
//     documented "clients fall back on major mismatch" contract), so
//     the server refuses rather than emit frames the client would
//     silently mis-render.
//
// Minor/patch differences within the same major are accepted: the
// protocol's additive-minor convention means an older client tolerates
// unknown fields and an older server simply omits newer ones. When the
// client declares nothing the request is accepted unchanged, preserving
// back-compat for every pre-negotiation client.
//
// Returns true when the caller may proceed; false (after writing the
// error response) when the request was rejected.
func negotiateProtocolVersion(w http.ResponseWriter, r *http.Request) bool {
	w.Header().Set(headerProtocolVersion, protocolVersion)

	declared := clientProtocolVersion(r)
	if declared == "" {
		return true // no declaration → back-compat accept
	}
	clientMajor, ok := protocolMajor(declared)
	if !ok {
		http.Error(w, fmt.Sprintf(
			"malformed protocol version %q (expected semver like %q)",
			declared, protocolVersion), http.StatusBadRequest)
		return false
	}
	serverMajor, _ := protocolMajor(protocolVersion)
	if clientMajor != serverMajor {
		http.Error(w, fmt.Sprintf(
			"protocol version mismatch: client speaks %q (major %d), server speaks %q (major %d); "+
				"upgrade the client or connect to a compatible daemon",
			declared, clientMajor, protocolVersion, serverMajor), http.StatusConflict)
		return false
	}
	return true
}
