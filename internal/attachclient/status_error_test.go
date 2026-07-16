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
	"testing"
)

// TestHTTPStatusError_ErrorPreservesFormat guards the on-the-wire
// error-string format. Historically these errors were produced by
// fmt.Errorf("<op>: status %d: %s", ...); the typed replacement
// must render identically so callers that grep log lines don't
// silently break.
func TestHTTPStatusError_ErrorPreservesFormat(t *testing.T) {
	t.Parallel()
	e := &httpStatusError{op: "stream", statusCode: 404, body: "session not found"}
	want := "stream: status 404: session not found"
	if got := e.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

// TestHTTPStatusError_PermanentClassification pins the classification
// contract the core-tui PermanentStreamError interface consumes.
// A regression here means the TUI goes back to looping the reconnect
// on 404/401/403 forever — the exact UX bug the classification exists
// to prevent.
func TestHTTPStatusError_PermanentClassification(t *testing.T) {
	t.Parallel()
	cases := []struct {
		code int
		want bool
	}{
		{400, false}, // bad request — transient (typically caller-side, not "session gone")
		{401, true},  // token invalid / revoked
		{403, true},  // ACL revoked
		{404, true},  // session evicted
		{408, false}, // request timeout — retryable
		{429, false}, // rate limit — retryable
		{500, false}, // server error — retryable
		{502, false}, // bad gateway — retryable
		{503, false}, // unavailable — retryable
		{504, false}, // gateway timeout — retryable
	}
	for _, tc := range cases {
		e := &httpStatusError{op: "stream", statusCode: tc.code, body: "x"}
		if got := e.PermanentStreamErr(); got != tc.want {
			t.Errorf("status %d: PermanentStreamErr() = %v, want %v", tc.code, got, tc.want)
		}
	}
}

// TestHTTPStatusError_SatisfiesPermanentStreamErrorInterface is the
// compile-time signal that core-tui's duck-typed interface will
// resolve against this error. Re-declaring the shape locally (rather
// than importing core-tui's package) mirrors what the TUI does — the
// classification is via runtime type-assertion, no import cycle.
func TestHTTPStatusError_SatisfiesPermanentStreamErrorInterface(t *testing.T) {
	t.Parallel()
	type permanentStreamError interface {
		error
		PermanentStreamErr() bool
	}
	var _ permanentStreamError = (*httpStatusError)(nil)
	// Also verify a non-nil value satisfies at runtime — belts +
	// suspenders in case the interface is checked dynamically.
	var e permanentStreamError = &httpStatusError{op: "stream", statusCode: 404, body: ""}
	if !e.PermanentStreamErr() {
		t.Errorf("interface-typed 404 should still classify as permanent")
	}
}
