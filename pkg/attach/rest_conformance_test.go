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

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/adk/session"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/internal/subagentlog"
	"github.com/go-steer/core-agent/v2/pkg/auth"
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

// TestConformance_RESTSessionsListV2 pins the same envelope one
// wire-shape version on: rows gained an optional `title` (protocol
// 1.6.0, #808). The v1 test above stays exactly as it was and keeps
// passing — a descriptor with no title marshals to the v1 shape, which
// is the whole claim `omitempty` makes here.
//
// The two rows are deliberately asymmetric: the titled row pins the
// field name, and the untitled one pins its ABSENCE. A client must
// treat a missing `title` as "fall back to the session ID", and it will
// meet missing titles constantly — against a pre-1.6.0 daemon, before a
// session's first turn lands, and wherever titling is switched off.
func TestConformance_RESTSessionsListV2(t *testing.T) {
	t.Parallel()
	resp := map[string]any{
		"sessions": []sessionDescriptor{
			{
				AppName:       "core-agent",
				UserID:        "alice@example.com",
				SessionID:     "s-1a2b3c",
				HasEventLog:   true,
				Status:        sessionStatusActive,
				LastTouchedAt: time.Date(2026, 7, 30, 12, 34, 56, 789012345, time.FixedZone("", 2*60*60)),
				Title:         "Debug the payment webhook retries",
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
		"testdata/conformance/rest-sessions-list-v2.json",
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

// TestConformance_RESTSessionACLV1 pins the GET/PATCH
// /sessions/{sid}/acl response (#797). Both lists are present and
// non-empty in the fixture, because this is the one endpoint whose
// entire job is reporting who is on the ACL — a client that had to
// distinguish a missing key from an empty list would be guessing about
// authorization, so the handler always emits `[]` rather than `null`
// and the second test below pins that.
func TestConformance_RESTSessionACLV1(t *testing.T) {
	t.Parallel()
	resp := aclResponse(auth.SessionACL{
		Owner:        "lookout@example.com",
		Viewers:      []string{"sre-readonly@example.com"},
		Contributors: []string{"oncall@example.com", "incident-bot@example.com"},
	})
	assertMatchesConformanceFixture(t,
		"testdata/conformance/rest-session-acl-v1.json",
		resp)
}

// TestConformance_RESTSessionACLV1_EmptyListsAreArrays is the half the
// fixture can't show. Normalized() reports an absent list as nil, which
// marshals to `null`; a client iterating the response would then need a
// nil check on the happy path of a brand-new session, whose ACL is
// owner-only by construction. That is the common case, not the edge.
func TestConformance_RESTSessionACLV1_EmptyListsAreArrays(t *testing.T) {
	t.Parallel()
	raw, err := json.Marshal(aclResponse(auth.SessionACL{Owner: "alice@example.com"}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got, want := string(raw), `{"owner":"alice@example.com","viewers":[],"contributors":[]}`; got != want {
		t.Errorf("owner-only ACL marshals to %s\nwant                            %s", got, want)
	}
}

// TestConformance_RESTPermsRespondV1 pins the POST
// /sessions/{sid}/perms/respond body, which gained `approver`
// (protocol 1.10.0, #830). The fixture shows the attributed shape
// because that is the one that pins the field name; the test below
// pins its absence, which is the case a client meets on any daemon
// that verified nobody.
func TestConformance_RESTPermsRespondV1(t *testing.T) {
	t.Parallel()
	resp := PromptRespondResponse{
		Acknowledged: true,
		Approver:     "oncall@example.com",
	}
	assertMatchesConformanceFixture(t,
		"testdata/conformance/rest-perms-respond-v1.json",
		resp)
}

// TestConformance_RESTPermsRespondV1_UnattributedOmitsApprover is the
// half the fixture can't show. An unauthenticated loopback listener
// verifies no identity, so the server records no approver — and it
// must omit the key rather than emit `""`, because a client rendering
// "approved by <empty>" is the same lie as recording the anonymous
// placeholder in the first place. It also pins that the pre-#830
// `{"acknowledged":true}` body still marshals byte-identically, which
// is what makes the field additive.
func TestConformance_RESTPermsRespondV1_UnattributedOmitsApprover(t *testing.T) {
	t.Parallel()
	raw, err := json.Marshal(PromptRespondResponse{Acknowledged: true})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got, want := string(raw), `{"acknowledged":true}`; got != want {
		t.Errorf("unattributed respond marshals to %s\nwant                          %s", got, want)
	}
}

// TestConformance_RESTSessionTitleV1 pins the POST
// /sessions/{sid}/title body (protocol 1.10.0, #808).
func TestConformance_RESTSessionTitleV1(t *testing.T) {
	t.Parallel()
	resp := SessionTitleResponse{
		Session:   "s-incident-4412",
		Title:     "payments latency incident",
		Persisted: true,
	}
	assertMatchesConformanceFixture(t,
		"testdata/conformance/rest-session-title-v1.json",
		resp)
}

// TestConformance_RESTSessionTitleV1_ClearedAndUnpersisted is the half
// the fixture can't show, and it is the shape most callers will
// actually see: `persisted` is NOT omitempty, because false is the
// normal answer for a daemon with no ACL store and a client has to be
// able to read it rather than infer it from a missing key. `title` is,
// so a cleared title reads as absent rather than as `""` — the same
// "no name" the picker shows for a session that never had one.
func TestConformance_RESTSessionTitleV1_ClearedAndUnpersisted(t *testing.T) {
	t.Parallel()
	raw, err := json.Marshal(SessionTitleResponse{Session: "s-1"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got, want := string(raw), `{"session":"s-1","persisted":false}`; got != want {
		t.Errorf("cleared+unpersisted title marshals to %s\nwant                                %s", got, want)
	}
}

// TestConformance_RESTInjectV1 pins the POST /sessions/{sid}/inject
// body, which gained `woke` in protocol 1.10.0 (#698).
//
// The fixture is the DEFERRED case on purpose: false is the value a
// client has to be able to read, and pinning the shape that carries it
// is what stops `woke` from ever acquiring an omitempty (which would
// make "did not wake" indistinguishable from a pre-1.10.0 daemon that
// always did).
//
// It is now also the shape a daemon returns when it cannot name a
// prompt_id — a registrant without IdentifyingInjector (#840). Kept
// frozen: a client that can read this body must keep working, since
// the capability is optional and the key's absence is a live case, not
// a legacy one.
func TestConformance_RESTInjectV1(t *testing.T) {
	t.Parallel()
	resp := InjectResponse{
		Injected: "second alert corroborates the first",
		Session:  "s-incident-4412",
		Woke:     false,
	}
	assertMatchesConformanceFixture(t,
		"testdata/conformance/rest-inject-v1.json",
		resp)
}

// TestConformance_RESTInjectV2 pins the same body with `prompt_id`
// populated (#840) — the shape every in-tree host actually produces,
// since attachadapter implements the capability.
//
// A second fixture rather than an edit to v1: the two are both live
// answers from a conforming daemon, and a client has to handle each.
// Pinning only the populated one would let the key quietly become
// mandatory, which would break a host wrapping a registrant that
// cannot name ids.
func TestConformance_RESTInjectV2(t *testing.T) {
	t.Parallel()
	resp := InjectResponse{
		Injected: "second alert corroborates the first",
		Session:  "s-incident-4412",
		Woke:     false,
		PromptID: "0199c3a1-6b2e-7f04-9c11-2d8ae4f01b73",
	}
	assertMatchesConformanceFixture(t,
		"testdata/conformance/rest-inject-v2.json",
		resp)
}

// TestConformance_RESTWakeV1 pins POST /sessions/{sid}/wake, whose
// body was a hand-rolled map literal until #840 gave it a type. The
// fixture is what makes that retyping provably wire-identical: `woken`
// and `prompt` keep their names AND `prompt` keeps its presence on an
// empty value, which an accidental omitempty would silently drop.
func TestConformance_RESTWakeV1(t *testing.T) {
	t.Parallel()
	resp := WakeResponse{
		Woken:    "s-incident-4412",
		Prompt:   "rescan the node pool",
		PromptID: "0199c3a1-6b2e-7f04-9c11-2d8ae4f01b73",
	}
	assertMatchesConformanceFixture(t,
		"testdata/conformance/rest-wake-v1.json",
		resp)

	// A bare wake still carries `prompt`, empty — the pre-#840 map
	// literal always emitted the key, and a client reading it as
	// "the prompt that was queued" must not see it vanish.
	raw, err := json.Marshal(WakeResponse{Woken: "s-1"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got, want := string(raw), `{"woken":"s-1","prompt":""}`; got != want {
		t.Errorf("bare wake marshals to %s\nwant                  %s", got, want)
	}
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

// TestConformance_RESTSessionsListV2_LiveHandlerAgreesWithFixture
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
//
// The registrant implements SessionTitleProvider so the live row
// carries a `title` and matches the fixture's first row key-for-key.
// That makes this a probe of the capability plumbing too: drop the
// type assertion in entryTitle and the key sets diverge here.
func TestConformance_RESTSessionsListV2_LiveHandlerAgreesWithFixture(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistryWithStore(newTestACLStore(t))
	titled := &titledRegistrant{
		stubRegistrant: &stubRegistrant{app: "core-agent", user: "alice@example.com", sid: "s-live"},
		title:          "Debug the payment webhook retries",
	}
	if _, err := reg.RegisterOwned(titled, "alice@example.com"); err != nil {
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
	fixtureBytes, err := os.ReadFile("testdata/conformance/rest-sessions-list-v2.json")
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

// TestConformance_RESTSubagentEventsV1 pins the subagent turn-history
// envelope (#638). Its Frame rows reuse the SSE frame shape, which is
// already fixture-pinned, so the value here is the envelope around
// them: the paging contract (next_since/truncated) is the part a
// client gets silently wrong, and `branches` is a normative statement
// about which launch-path spellings the server searched — a client
// that renders it tells the operator why an empty list is empty.
func TestConformance_RESTSubagentEventsV1(t *testing.T) {
	t.Parallel()
	// A truncated page: the operator asked for more turns than the
	// limit allowed, so next_since is a resume cursor, not the end.
	// Populating both flags is what pins their names — neither is
	// omitempty, but a rename would otherwise sail past the tests.
	resp := SubagentEventsResponse{
		Agent:           "cluster",
		ParentSessionID: "s-1a2b3c",
		Branches:        subagentlog.BranchPrefixes("cluster"),
		Events: []Frame{{
			Seq: 41,
			Event: &session.Event{
				ID:           "e-7f3a",
				InvocationID: "inv-1",
				Author:       "cluster",
				Branch:       "bg.cluster",
				Timestamp:    time.Date(2026, 8, 12, 9, 15, 0, 0, time.UTC),
				LLMResponse: adkmodel.LLMResponse{
					Content: &genai.Content{
						Role:  genai.RoleModel,
						Parts: []*genai.Part{{Text: "listing nodes in cluster prod-1"}},
					},
				},
			},
		}},
		NextSince: 41,
		Truncated: true,
	}
	assertMatchesConformanceFixture(t,
		"testdata/conformance/rest-subagent-events-v1.json",
		resp)
}

// TestConformance_RESTSubagentEventsV1_EmptyIsArrayNotNull pins the
// no-turns body: an unknown or never-run subagent answers 200 with
// `"events": []`, not null and not 404 (the log, not the manager, is
// the source of truth). Same `[]`-vs-null client breaker as the
// sessions list, and it hinges on the same explicit-empty-slice
// idiom in doSubagentEvents.
func TestConformance_RESTSubagentEventsV1_EmptyIsArrayNotNull(t *testing.T) {
	t.Parallel()
	body, err := json.Marshal(SubagentEventsResponse{
		Agent:    "cluster",
		Branches: subagentlog.BranchPrefixes("cluster"),
		Events:   []Frame{},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(body), `"events":[]`) {
		t.Errorf("empty body = %s, want an empty events array (never null)", body)
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
