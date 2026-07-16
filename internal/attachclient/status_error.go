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

package attachclient

import (
	"fmt"
	"net/http"
)

// httpStatusError wraps a non-2xx HTTP response with the status code
// so callers can distinguish recoverable transport blips from
// terminal conditions (session gone, token revoked, ACL revoked).
//
// This type also implements core-tui's PermanentStreamError interface
// (see [core-tui#51]) so the remote TUI stops retrying attach-stream
// reconnects on 404 / 401 / 403 instead of looping the same error row
// once a second forever. core-tui detects the interface duck-typed;
// there's no import dep on core-tui from this package.
//
// Emitted by every attachclient call site that would previously have
// produced a `fmt.Errorf("<op>: status %d: %s", ...)` — keeps the
// error-string surface unchanged for grep/log-processing continuity
// while adding the typed-classification signal on top.
//
// [core-tui#51]: https://github.com/go-steer/core-tui/issues/51
type httpStatusError struct {
	// op names the failing operation, matches the fmt.Errorf prefix
	// this type replaced (e.g. "stream", "perms/stream", "list peers").
	op string
	// statusCode is the raw HTTP status the server returned.
	statusCode int
	// body is the response body captured for the error message.
	// Kept as a string (not []byte) because we've already consumed
	// the reader and the value is small (log lines / short JSON).
	body string
}

// Error preserves the same "<op>: status <code>: <body>" shape the
// prior fmt.Errorf sites produced. Consumers that grep logs (or the
// tests below) shouldn't see any change.
func (e *httpStatusError) Error() string {
	return fmt.Sprintf("%s: status %d: %s", e.op, e.statusCode, e.body)
}

// PermanentStreamErr satisfies core-tui's PermanentStreamError
// interface. Return true on statuses that will not recover by
// retrying the same URL with the same token:
//
//   - 404: the session was evicted (daemon restart, TTL expiry,
//     operator DELETE /sessions).
//   - 401: the bearer token is invalid or revoked.
//   - 403: the caller is authenticated but the ACL denies session
//     access (typical after an owner rotates the ACL).
//
// Everything else (500s, transport errors, 429) stays retryable —
// those are transient by convention and the TUI's reconnect loop
// eventually succeeds.
func (e *httpStatusError) PermanentStreamErr() bool {
	return e.statusCode == http.StatusNotFound ||
		e.statusCode == http.StatusUnauthorized ||
		e.statusCode == http.StatusForbidden
}
