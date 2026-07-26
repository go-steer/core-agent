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
	"testing"
)

// Regression tests for #383: a malicious page the operator visits
// could fire a CORS simple request (Content-Type: text/plain, body
// {"message":...}) at http://localhost:7777/sessions/default/inject —
// no preflight, side effect lands even though the response is
// unreadable.

// csrfTestServer spins up a full server (so the Bind middleware chain,
// including browserWriteGuard, is in play) with one injectable stub
// session.
func csrfTestServer(t *testing.T) (base string, ag *stubRegistrant, done func()) {
	t.Helper()
	reg := NewSessionRegistry()
	ag = &stubRegistrant{app: "core-agent", user: "u", sid: "default"}
	if _, err := reg.Register(ag); err != nil {
		t.Fatalf("Register: %v", err)
	}
	base, done = startTestServer(t, reg)
	return base, ag, done
}

func doReq(t *testing.T, method, url, contentType, origin, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// TestCSRF_TextPlainPostRejected: the classic simple-request vector —
// text/plain POST with a JSON-shaped body — must 415 before the
// handler runs.
func TestCSRF_TextPlainPostRejected(t *testing.T) {
	t.Parallel()
	base, ag, done := csrfTestServer(t)
	defer done()

	resp := doReq(t, http.MethodPost, base+"/sessions/default/inject",
		"text/plain", "", `{"message":"pwn"}`)
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("text/plain POST /inject = %d, want 415", resp.StatusCode)
	}
	if len(ag.injected) != 0 {
		t.Fatalf("415 must fire before the handler; injected = %v", ag.injected)
	}

	// Body-less, header-less POST (fetch with no body) — same fate.
	resp = doReq(t, http.MethodPost, base+"/sessions/default/interrupt", "", "", "")
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("no-Content-Type POST /interrupt = %d, want 415", resp.StatusCode)
	}

	// DELETE is a write method too.
	resp = doReq(t, http.MethodDelete, base+"/sessions/default", "text/plain", "", "")
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("text/plain DELETE /sessions/default = %d, want 415", resp.StatusCode)
	}
}

// TestCSRF_CrossOriginRejected: any non-loopback, non-self Origin on
// a write → 403, even with the correct Content-Type.
func TestCSRF_CrossOriginRejected(t *testing.T) {
	t.Parallel()
	base, ag, done := csrfTestServer(t)
	defer done()

	for _, origin := range []string{
		"https://evil.example",
		"http://attacker.lan:8080",
		"null", // sandboxed iframe / file:// page
	} {
		resp := doReq(t, http.MethodPost, base+"/sessions/default/inject",
			"application/json", origin, `{"message":"pwn"}`)
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("Origin %q POST /inject = %d, want 403", origin, resp.StatusCode)
		}
	}
	if len(ag.injected) != 0 {
		t.Fatalf("403 must fire before the handler; injected = %v", ag.injected)
	}
}

// TestCSRF_NativeAndLocalClientsPass: no Origin (curl, TUI, SDKs),
// loopback origins (local SPA dev server), and self origins (the /ui
// SPA behind a proxy) all reach the handler.
func TestCSRF_NativeAndLocalClientsPass(t *testing.T) {
	t.Parallel()
	base, ag, done := csrfTestServer(t)
	defer done()

	// No Origin + application/json → OK.
	resp := doReq(t, http.MethodPost, base+"/sessions/default/inject",
		"application/json", "", `{"message":"one"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("no-Origin JSON POST /inject = %d, want 200", resp.StatusCode)
	}

	// Charset parameter on the media type is fine.
	resp = doReq(t, http.MethodPost, base+"/sessions/default/inject",
		"application/json; charset=utf-8", "", `{"message":"two"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("JSON+charset POST /inject = %d, want 200", resp.StatusCode)
	}

	// Loopback origins → OK.
	for _, origin := range []string{"http://localhost:3000", "http://127.0.0.1:9999", "http://[::1]:8080"} {
		resp = doReq(t, http.MethodPost, base+"/sessions/default/inject",
			"application/json", origin, `{"message":"three"}`)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("loopback Origin %q POST /inject = %d, want 200", origin, resp.StatusCode)
		}
	}

	if len(ag.injected) != 5 {
		t.Fatalf("injected %d messages, want 5: %v", len(ag.injected), ag.injected)
	}
}

// TestCSRF_SelfOriginPass: an Origin whose host equals the request's
// Host header (same-origin SPA behind a non-loopback hostname) passes.
func TestCSRF_SelfOriginPass(t *testing.T) {
	t.Parallel()
	base, ag, done := csrfTestServer(t)
	defer done()

	req, err := http.NewRequest(http.MethodPost, base+"/sessions/default/inject",
		strings.NewReader(`{"message":"self"}`))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://agent.example.com:7777")
	req.Host = "agent.example.com:7777" // as a proxy would present it
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("self-origin POST /inject = %d, want 200", resp.StatusCode)
	}
	if len(ag.injected) != 1 {
		t.Fatalf("injected = %v, want the self-origin message", ag.injected)
	}
}

// TestCSRF_ReadsUnaffected: GET endpoints (including SSE /events) see
// neither the Origin nor the Content-Type check.
func TestCSRF_ReadsUnaffected(t *testing.T) {
	t.Parallel()
	base, _, done := csrfTestServer(t)
	defer done()

	// GET /sessions with a hostile Origin and no Content-Type → 200.
	resp := doReq(t, http.MethodGet, base+"/sessions", "", "https://evil.example", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /sessions with hostile Origin = %d, want 200", resp.StatusCode)
	}

	// GET /events with a hostile Origin: reaches the handler (the stub
	// session has no eventlog, so the handler's own 412 is the proof —
	// NOT a 403/415 from the guard).
	resp = doReq(t, http.MethodGet, base+"/sessions/default/events", "", "https://evil.example", "")
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("GET /events with hostile Origin = %d, want 412 (handler reached)", resp.StatusCode)
	}
}

func TestOriginAllowed(t *testing.T) {
	t.Parallel()
	cases := []struct {
		origin, host string
		want         bool
	}{
		{"http://localhost:3000", "127.0.0.1:7777", true},
		{"http://127.0.0.1:7777", "127.0.0.1:7777", true},
		{"http://[::1]:8080", "127.0.0.1:7777", true},
		{"https://myhost:7777", "myhost:7777", true},
		{"https://MYHOST:7777", "myhost:7777", true},
		{"https://myhost:7777", "otherhost:7777", false},
		{"https://evil.example", "127.0.0.1:7777", false},
		{"null", "127.0.0.1:7777", false},
		{"", "127.0.0.1:7777", false},
		{"not a url", "127.0.0.1:7777", false},
	}
	for _, c := range cases {
		if got := originAllowed(c.origin, c.host); got != c.want {
			t.Errorf("originAllowed(%q, %q) = %v, want %v", c.origin, c.host, got, c.want)
		}
	}
}

func TestIsJSONContentType(t *testing.T) {
	t.Parallel()
	cases := []struct {
		ct   string
		want bool
	}{
		{"application/json", true},
		{"application/json; charset=utf-8", true},
		{"APPLICATION/JSON", true}, // mime.ParseMediaType lowercases
		{"text/plain", false},
		{"application/x-www-form-urlencoded", false},
		{"multipart/form-data; boundary=x", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isJSONContentType(c.ct); got != c.want {
			t.Errorf("isJSONContentType(%q) = %v, want %v", c.ct, got, c.want)
		}
	}
}
