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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// REST-response conformance tests (#536). The SSE event shapes have
// been fixture-pinned since v1.4.0, but the REST endpoint *response*
// shapes lived only as prose in docs/attach-mode-design.md — nothing
// normative for clients to test against. That gap is exactly how
// mast-web's bundled mock invented snake_case names for the sessions
// list (session_id/app_name/user_id), passed its own tests, and
// rendered `undefined` against every real backend (mast-web#41).
//
// These fixtures are the canonical wire shapes for the JSON REST
// responses; downstream clients mirror them the same way they mirror
// the SSE fixtures. Versioned independently of the SSE protocol
// (rest-*-v1) — bump the suffix on any wire-shape change and keep the
// old fixture frozen, per testdata/conformance/README.md.

func TestConformance_RESTSessionsListV1(t *testing.T) {
	t.Parallel()
	// The handler wraps []sessionDescriptor in a {"sessions": ...}
	// envelope (listSessions in handlers.go). One active row (in the
	// in-memory registry) and one idle row (persisted-only, resumes
	// lazily on /events) — the two Status values clients must render.
	resp := map[string]any{
		"sessions": []sessionDescriptor{
			{
				AppName:     "core-agent",
				UserID:      "alice@example.com",
				SessionID:   "s-1a2b3c",
				HasEventLog: true,
				Status:      sessionStatusActive,
				// Active rows come from Entry.LastTouchedAt(), which is
				// time.Unix(0, ns) in the daemon's local zone — the wire
				// carries RFC 3339 with arbitrary precision and zone
				// offset. The fixture deliberately shows that shape so
				// clients parse rather than pattern-match a "...Z" layout.
				LastTouchedAt: time.Date(2026, 7, 30, 12, 34, 56, 789012345, time.FixedZone("", 2*60*60)),
			},
			{
				AppName:       "core-agent",
				UserID:        "alice@example.com",
				SessionID:     "s-9z8y7x",
				HasEventLog:   true,
				Status:        sessionStatusIdle,
				LastTouchedAt: time.Date(2026, 7, 29, 8, 0, 0, 0, time.UTC),
			},
		},
	}
	assertMatchesConformanceFixture(t,
		"testdata/conformance/rest-sessions-list-v1.json",
		resp)
}

func TestConformance_RESTCreateSessionV1(t *testing.T) {
	t.Parallel()
	resp := createSessionResponse{
		AppName:   "core-agent",
		UserID:    "alice@example.com",
		SessionID: "s-1a2b3c",
		URL:       "http://daemon.example.com:8844/sessions/core-agent/s-1a2b3c",
	}
	assertMatchesConformanceFixture(t,
		"testdata/conformance/rest-create-session-v1.json",
		resp)
}

func TestConformance_RESTWhoAmIV1(t *testing.T) {
	t.Parallel()
	// The asserted-proxy variant exercises every field: admin and
	// proxy_by are omitempty, so the all-populated shape is the one
	// that pins their names.
	resp := whoAmIResponse{
		Identity: "release-bot@example.com",
		Admin:    true,
		Source:   whoAmISourceAsserted,
		ProxyBy:  "gateway-svc@example.com",
	}
	assertMatchesConformanceFixture(t,
		"testdata/conformance/rest-whoami-v1.json",
		resp)
}

// TestConformance_RESTSessionsListV1_LiveHandlerAgreesWithFixture
// drives the REAL listSessions handler and checks its response
// against the fixture's key structure — the envelope key and the
// per-row field-name set. The struct-construction test above pins
// the sessionDescriptor tags; this one pins the handler's envelope
// (a map literal in listSessions), so a refactor to a typed envelope
// with a different tag, or writeJSON growing sibling fields, fails
// here instead of drifting past a mock-tested-against-itself —
// the exact #536 failure class, one layer up. Values (timestamps,
// ids) intentionally aren't compared: the fixture's are canonical
// examples, the live ones are whatever the harness produced.
func TestConformance_RESTSessionsListV1_LiveHandlerAgreesWithFixture(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistryWithStore(newTestACLStore(t))
	if _, err := reg.RegisterOwned(&stubRegistrant{app: "core-agent", user: "alice@example.com", sid: "s-live"}, "alice@example.com"); err != nil {
		t.Fatalf("RegisterOwned: %v", err)
	}
	h := &handlers{reg: reg, pool: newBroadcasterPool()}

	r := httptest.NewRequest(http.MethodGet, "/sessions", nil)
	rr := httptest.NewRecorder()
	h.listSessions(rr, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("listSessions status = %d, body: %s", rr.Code, rr.Body.String())
	}

	live := decodeSessionsEnvelope(t, rr.Body.Bytes(), "live handler")
	fixtureBytes, err := os.ReadFile("testdata/conformance/rest-sessions-list-v1.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	fixture := decodeSessionsEnvelope(t, fixtureBytes, "fixture")

	if len(live) == 0 || len(fixture) == 0 {
		t.Fatalf("rows: live=%d fixture=%d, want both non-empty", len(live), len(fixture))
	}
	liveKeys := sortedKeys(live[0])
	fixtureKeys := sortedKeys(fixture[0])
	if !reflect.DeepEqual(liveKeys, fixtureKeys) {
		t.Errorf("live row field names %v != fixture field names %v — update the fixture (new REST-shape version) and rest_conformance_test.go together", liveKeys, fixtureKeys)
	}
}

// TestConformance_RESTSessionsListV1_EmptyIsArrayNotNull pins the
// zero-session body byte-for-byte: {"sessions":[]}. `[]` vs `null`
// is the classic client breaker, and it currently hinges on the
// make([]sessionDescriptor, 0, n) idiom in listSessions — a drive-by
// `var out []sessionDescriptor` would ship null with no other test
// noticing (Go decoders treat them identically).
func TestConformance_RESTSessionsListV1_EmptyIsArrayNotNull(t *testing.T) {
	t.Parallel()
	h := &handlers{reg: NewSessionRegistryWithStore(newTestACLStore(t)), pool: newBroadcasterPool()}
	r := httptest.NewRequest(http.MethodGet, "/sessions", nil)
	rr := httptest.NewRecorder()
	h.listSessions(rr, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("listSessions status = %d", rr.Code)
	}
	if got := strings.TrimSpace(rr.Body.String()); got != `{"sessions":[]}` {
		t.Errorf("empty list body = %s, want {\"sessions\":[]} (never null)", got)
	}
}

// decodeSessionsEnvelope asserts the body is a single-key
// {"sessions": [...]} envelope of objects and returns the rows.
func decodeSessionsEnvelope(t *testing.T, body []byte, label string) []map[string]any {
	t.Helper()
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("%s: unmarshal envelope: %v", label, err)
	}
	if len(envelope) != 1 {
		t.Fatalf("%s: envelope keys = %v, want exactly [sessions]", label, sortedRawKeys(envelope))
	}
	raw, ok := envelope["sessions"]
	if !ok {
		t.Fatalf("%s: envelope keys = %v, want key %q", label, sortedRawKeys(envelope), "sessions")
	}
	var rows []map[string]any
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatalf("%s: unmarshal rows: %v", label, err)
	}
	return rows
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedRawKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
