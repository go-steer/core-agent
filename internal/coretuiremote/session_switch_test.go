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

// Tests for the coretui.SessionSwitcher wiring + the upgraded /new
// slash that now returns SwitchTo (core-tui v0.10.0, issues #48 / #53).

package coretuiremote

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	coretui "github.com/go-steer/core-tui/tui"

	"github.com/go-steer/core-agent/internal/attachclient"
)

// startSessionsServer wires the two daemon-scoped endpoints the
// SessionSwitcher path exercises: GET /sessions (enumeration) +
// POST /sessions (creation). Each test can override the responses
// via the returned control struct.
type sessionsServer struct {
	*httptest.Server

	// list is what GET /sessions returns. Tests set this before
	// invoking Sessions() / SwitchToSession() / /new.
	list []attachclient.SessionDescriptor

	// listStatus overrides the response status for GET /sessions
	// (default 200). Set to 500 to exercise the defensive nil-on-
	// error branch in Sessions().
	listStatus int

	// created is what POST /sessions returns (the /new path). Tests
	// set this to drive the /new slash upgrade.
	created attachclient.NewSessionResponse

	// peers is what GET /peers returns. Empty (default) means the
	// handler returns 404, which the client treats as "no peer
	// registration wired" and returns nil / no error — matches
	// production behavior on daemons without peer registration.
	// Multi-daemon tests populate this to opt into peer fan-out.
	peers []attachclient.PeerDescriptor
}

func startSessionsServer(t *testing.T) *sessionsServer {
	t.Helper()
	fs := &sessionsServer{listStatus: http.StatusOK}
	mux := http.NewServeMux()

	mux.HandleFunc("GET /sessions", func(w http.ResponseWriter, r *http.Request) {
		if fs.listStatus != http.StatusOK {
			w.WriteHeader(fs.listStatus)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"sessions": fs.list})
	})

	mux.HandleFunc("POST /sessions", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(fs.created)
	})

	mux.HandleFunc("GET /peers", func(w http.ResponseWriter, r *http.Request) {
		if len(fs.peers) == 0 {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"peers": fs.peers})
	})

	fs.Server = httptest.NewServer(mux)
	t.Cleanup(fs.Close)
	return fs
}

// newTestAdapter builds an Adapter that talks to fs at the given
// session path. Reuses the same client construction path
// production uses (attachclient.ParseURL + New).
func newTestAdapter(t *testing.T, fs *sessionsServer, sessionPath string) *Adapter {
	t.Helper()
	parsed, err := attachclient.ParseURL(fs.URL + sessionPath)
	if err != nil {
		t.Fatalf("parseURL: %v", err)
	}
	return New(attachclient.New(parsed, "", 0), sessionPath)
}

// TestSessions_MarksCurrent — three sessions enumerate, exactly one
// matches the Adapter's current sessionPath, that row is marked
// Current=true, the rest false.
func TestSessions_MarksCurrent(t *testing.T) {
	t.Parallel()
	fs := startSessionsServer(t)
	fs.list = []attachclient.SessionDescriptor{
		{App: "core-agent", SessionID: "one"},
		{App: "core-agent", SessionID: "two"},
		{App: "core-agent", SessionID: "three"},
	}
	a := newTestAdapter(t, fs, "/sessions/core-agent/two")

	got := a.Sessions()
	if len(got) != 3 {
		t.Fatalf("Sessions() len = %d, want 3", len(got))
	}
	for _, s := range got {
		want := s.ID == "two"
		if s.Current != want {
			t.Errorf("session %s: Current=%v, want %v", s.ID, s.Current, want)
		}
	}
	// Display should be App/SessionID when App is present.
	if got[1].Display != "core-agent/two" {
		t.Errorf("Display[1] = %q, want core-agent/two", got[1].Display)
	}
}

// TestSessions_DisplayFallback — bare (no App) descriptors surface
// with Display equal to the SessionID.
func TestSessions_DisplayFallback(t *testing.T) {
	t.Parallel()
	fs := startSessionsServer(t)
	fs.list = []attachclient.SessionDescriptor{{SessionID: "bare"}}
	a := newTestAdapter(t, fs, "/sessions/other")

	got := a.Sessions()
	if len(got) != 1 || got[0].Display != "bare" {
		t.Fatalf("Sessions() = %+v, want single bare row", got)
	}
	if got[0].Current {
		t.Errorf("bare row should not be marked Current when sessionPath is elsewhere")
	}
}

// TestSessions_NilOnEnumerationError — GET /sessions 500 returns
// nil (not error) so the picker renders cleanly with an "empty" body.
func TestSessions_NilOnEnumerationError(t *testing.T) {
	t.Parallel()
	fs := startSessionsServer(t)
	fs.listStatus = http.StatusInternalServerError
	a := newTestAdapter(t, fs, "/sessions/core-agent/one")

	if got := a.Sessions(); got != nil {
		t.Errorf("Sessions() on enumeration error = %+v, want nil", got)
	}
}

// TestSwitchToSession_QualifiesPath — resolveSessionPath consults
// the enumeration and picks the App-qualified form for the target.
func TestSwitchToSession_QualifiesPath(t *testing.T) {
	t.Parallel()
	fs := startSessionsServer(t)
	fs.list = []attachclient.SessionDescriptor{
		{App: "core-agent", SessionID: "one"},
		{App: "core-agent", SessionID: "two"},
	}
	a := newTestAdapter(t, fs, "/sessions/core-agent/one")

	tgt, err := a.SwitchToSession("two")
	if err != nil {
		t.Fatalf("SwitchToSession: %v", err)
	}
	if tgt.Agent == nil {
		t.Fatal("SwitchToSession returned nil Agent")
	}
	next, ok := tgt.Agent.(*Adapter)
	if !ok {
		t.Fatalf("Agent is %T, want *Adapter", tgt.Agent)
	}
	if next.SessionPath() != "/sessions/core-agent/two" {
		t.Errorf("new sessionPath = %q, want /sessions/core-agent/two", next.SessionPath())
	}
	if next == a {
		t.Errorf("SwitchToSession returned the same Adapter instance")
	}
	if !strings.Contains(tgt.Note, "two") {
		t.Errorf("Note = %q, want to mention session id", tgt.Note)
	}
}

// TestSwitchToSession_BareForm — when the enumeration returns bare
// (no App) descriptors, the resulting sessionPath is the shortcut
// form /sessions/<sid>.
func TestSwitchToSession_BareForm(t *testing.T) {
	t.Parallel()
	fs := startSessionsServer(t)
	fs.list = []attachclient.SessionDescriptor{{SessionID: "target"}}
	a := newTestAdapter(t, fs, "/sessions/current")

	tgt, err := a.SwitchToSession("target")
	if err != nil {
		t.Fatalf("SwitchToSession: %v", err)
	}
	next := tgt.Agent.(*Adapter)
	if next.SessionPath() != "/sessions/target" {
		t.Errorf("bare form path = %q, want /sessions/target", next.SessionPath())
	}
}

// TestSwitchToSession_NoopWhenCurrent — targeting the currently-
// attached session returns the same Adapter with an "already
// attached" note; the /switch UX safety when an operator types
// their current sid.
func TestSwitchToSession_NoopWhenCurrent(t *testing.T) {
	t.Parallel()
	fs := startSessionsServer(t)
	a := newTestAdapter(t, fs, "/sessions/core-agent/here")

	tgt, err := a.SwitchToSession("here")
	if err != nil {
		t.Fatalf("SwitchToSession: %v", err)
	}
	if tgt.Agent != a {
		t.Errorf("Agent = %v, want same Adapter (no-op)", tgt.Agent)
	}
	if !strings.Contains(strings.ToLower(tgt.Note), "already") {
		t.Errorf("Note = %q, want 'already attached' phrasing", tgt.Note)
	}
}

// TestSwitchToSession_EmptyID — empty arg is an error, not a silent
// no-op (avoids accidentally attaching to /sessions/).
func TestSwitchToSession_EmptyID(t *testing.T) {
	t.Parallel()
	fs := startSessionsServer(t)
	a := newTestAdapter(t, fs, "/sessions/core-agent/one")

	_, err := a.SwitchToSession("")
	if err == nil {
		t.Fatal("empty session ID should error")
	}
}

// TestSwitchToSession_UnknownFallsBackToShortcut — when the target
// isn't in the enumeration, resolveSessionPath returns the shortcut
// form so the downstream attach surfaces the "not found" via a real
// HTTP error rather than an opaque enumeration miss here.
func TestSwitchToSession_UnknownFallsBackToShortcut(t *testing.T) {
	t.Parallel()
	fs := startSessionsServer(t)
	fs.list = []attachclient.SessionDescriptor{{App: "core-agent", SessionID: "known"}}
	a := newTestAdapter(t, fs, "/sessions/core-agent/known")

	tgt, err := a.SwitchToSession("bogus")
	if err != nil {
		t.Fatalf("SwitchToSession: %v", err)
	}
	next := tgt.Agent.(*Adapter)
	if next.SessionPath() != "/sessions/bogus" {
		t.Errorf("unknown target path = %q, want /sessions/bogus (shortcut fallback)", next.SessionPath())
	}
}

// TestSwitchToSession_EnumerationError — GET /sessions 500 → error
// returned. Distinct from Sessions() which swallows the error to
// keep the picker cleanly openable; SwitchToSession is a commit
// point and needs to fail loudly.
func TestSwitchToSession_EnumerationError(t *testing.T) {
	t.Parallel()
	fs := startSessionsServer(t)
	fs.listStatus = http.StatusInternalServerError
	a := newTestAdapter(t, fs, "/sessions/core-agent/one")

	if _, err := a.SwitchToSession("target"); err == nil {
		t.Fatal("expected error when enumeration fails during switch")
	}
}

// TestSlashNew_ReturnsSwitchTo — /new via invokeAsyncSlash: POST
// /sessions returns a fresh session; the slash returns SlashResult
// with SwitchTo carrying a new Adapter pointing at the returned
// path. Verifies the /new upgrade to core-tui v0.10.0's swap API.
func TestSlashNew_ReturnsSwitchTo(t *testing.T) {
	t.Parallel()
	fs := startSessionsServer(t)
	fs.created = attachclient.NewSessionResponse{
		AppName:   "core-agent",
		SessionID: "fresh",
		URL:       fs.URL + "/sessions/core-agent/fresh",
	}
	a := newTestAdapter(t, fs, "/sessions/core-agent/original")

	res, err := a.invokeAsyncSlash(context.Background(), "new", "")
	if err != nil {
		t.Fatalf("/new invokeAsyncSlash: %v", err)
	}
	if res.SwitchTo == nil {
		t.Fatal("/new returned SlashResult without SwitchTo")
	}
	if res.SwitchTo.Agent == nil {
		t.Fatal("/new SwitchTo.Agent is nil")
	}
	next, ok := res.SwitchTo.Agent.(*Adapter)
	if !ok {
		t.Fatalf("SwitchTo.Agent is %T, want *Adapter", res.SwitchTo.Agent)
	}
	if next.SessionPath() != "/sessions/core-agent/fresh" {
		t.Errorf("new adapter sessionPath = %q, want /sessions/core-agent/fresh", next.SessionPath())
	}
	if !strings.Contains(res.SwitchTo.Note, "fresh") {
		t.Errorf("Note = %q, want to mention new sid", res.SwitchTo.Note)
	}
	if res.SystemMessage != "" {
		t.Errorf("SystemMessage = %q, want empty (all content in Note)", res.SystemMessage)
	}
}

// TestSlashNew_BareAppInResponse — daemon responses without App
// still produce a usable Adapter pointing at the shortcut form.
func TestSlashNew_BareAppInResponse(t *testing.T) {
	t.Parallel()
	fs := startSessionsServer(t)
	fs.created = attachclient.NewSessionResponse{
		SessionID: "solo",
		URL:       fs.URL + "/sessions/solo",
	}
	a := newTestAdapter(t, fs, "/sessions/original")

	res, err := a.invokeAsyncSlash(context.Background(), "new", "")
	if err != nil {
		t.Fatalf("/new: %v", err)
	}
	next := res.SwitchTo.Agent.(*Adapter)
	if next.SessionPath() != "/sessions/solo" {
		t.Errorf("bare-App new sessionPath = %q, want /sessions/solo", next.SessionPath())
	}
}

// TestSwitchToSession_PopulatesFullSwitchTarget is the regression gate
// for issue #274. Before the fix, Adapter.SwitchToSession returned a
// SwitchTarget containing only Agent + Note — core-tui's
// applySwitchTarget gates every other Options field on non-nil, so
// UsageTracker / Branding / Memory / Skills / MCPServers stayed
// pointed at the outgoing session's state. After the fix, /switch
// swaps the full capability payload so the title bar reflects the new
// session id and /stats reads the new session's totals.
func TestSwitchToSession_PopulatesFullSwitchTarget(t *testing.T) {
	t.Parallel()
	fs := startSessionsServer(t)
	fs.list = []attachclient.SessionDescriptor{
		{App: "core-agent", SessionID: "here"},
		{App: "core-agent", SessionID: "there"},
	}
	a := newTestAdapter(t, fs, "/sessions/core-agent/here")

	brandedFor := ""
	a.SetBrander(func(newPath string) *coretui.Branding {
		brandedFor = newPath
		return &coretui.Branding{
			Wordmark:      "core-agent-tui",
			AgentIdentity: "there", // trailing sid of newPath
		}
	})

	tgt, err := a.SwitchToSession("there")
	if err != nil {
		t.Fatalf("SwitchToSession: %v", err)
	}

	next, ok := tgt.Agent.(*Adapter)
	if !ok {
		t.Fatalf("SwitchTarget.Agent is %T, want *Adapter", tgt.Agent)
	}

	// UsageTracker must be set — and it must be the incoming Adapter
	// so /stats reads the new session's snapshot cache, not the
	// outgoing one.
	if tgt.UsageTracker == nil {
		t.Fatal("SwitchTarget.UsageTracker is nil — /stats will keep reporting the outgoing session")
	}
	if tgt.UsageTracker != coretui.UsageTracker(next) {
		t.Errorf("UsageTracker != incoming Adapter; got %T", tgt.UsageTracker)
	}

	// Branding must reflect the incoming session so the title bar
	// updates. The brander closure was invoked with the newly-
	// resolved sessionPath.
	if tgt.Branding == nil {
		t.Fatal("SwitchTarget.Branding is nil — title bar will keep the outgoing session's identity")
	}
	if got, want := tgt.Branding.AgentIdentity, "there"; got != want {
		t.Errorf("Branding.AgentIdentity = %q, want %q", got, want)
	}
	if brandedFor != "/sessions/core-agent/there" {
		t.Errorf("brander invoked with %q, want /sessions/core-agent/there", brandedFor)
	}

	// The brander must propagate to the incoming Adapter so a further
	// /switch from this session still updates the title bar.
	if next.brander == nil {
		t.Error("brander did not propagate to the incoming Adapter — a subsequent /switch would leave the title stale")
	}
}

// TestSwitchToSession_NoBranderLeavesBrandingNil — without a brander
// wired the SwitchTarget omits Branding, matching the pre-fix
// behavior for hosts that opt out of branding refresh. Regression
// gate for the nil-brander branch of buildSwitchTarget.
func TestSwitchToSession_NoBranderLeavesBrandingNil(t *testing.T) {
	t.Parallel()
	fs := startSessionsServer(t)
	fs.list = []attachclient.SessionDescriptor{
		{App: "core-agent", SessionID: "here"},
		{App: "core-agent", SessionID: "there"},
	}
	a := newTestAdapter(t, fs, "/sessions/core-agent/here")
	// intentionally no SetBrander

	tgt, err := a.SwitchToSession("there")
	if err != nil {
		t.Fatalf("SwitchToSession: %v", err)
	}
	if tgt.Branding != nil {
		t.Errorf("Branding = %+v, want nil when no brander is wired", tgt.Branding)
	}
	// UsageTracker still populates — it doesn't depend on brander.
	if tgt.UsageTracker == nil {
		t.Error("UsageTracker should populate even without a brander")
	}
}

// TestCurrentSessionID_Extraction — path-splitting unit test for the
// shortcut + qualified forms. Malformed paths (no /sessions/ prefix)
// return the whole rest so Current-marking degrades to false rather
// than misattributing.
func TestCurrentSessionID_Extraction(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want string
	}{
		{"/sessions/abc", "abc"},
		{"/sessions/core-agent/abc", "abc"},
		{"/sessions/core-agent/nested/abc", "abc"}, // last segment wins
		{"", ""},
		{"garbage", "garbage"},
	}
	for _, tc := range cases {
		if got := currentSessionID(tc.in); got != tc.want {
			t.Errorf("currentSessionID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
