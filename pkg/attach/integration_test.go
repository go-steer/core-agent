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
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	adkmodel "google.golang.org/adk/model"
	"google.golang.org/adk/session"

	"github.com/go-steer/core-agent/pkg/eventlog"
)

// eventfulRegistrant extends stubRegistrant with a real eventlog so
// the broadcaster can pump from Stream.Watch. Used by the integration
// tests below.
type eventfulRegistrant struct {
	stubRegistrant
	handle *eventlog.Handle
}

func (e *eventfulRegistrant) EventLog() *eventlog.Handle { return e.handle }

func openTestEventLog(t *testing.T) (*eventlog.Handle, func()) {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "session.db")
	h, err := eventlog.Open(context.Background(), sqlite.Open(dsn))
	if err != nil {
		t.Fatalf("eventlog.Open: %v", err)
	}
	return h, func() { _ = h.Close() }
}

// startTestServer spins up an attach Server on an ephemeral TCP port
// with no auth, returns its base URL plus a cleanup func.
func startTestServer(t *testing.T, reg *SessionRegistry) (string, func()) {
	t.Helper()
	srv, err := NewServer(Options{Registry: reg, Addr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	// Wait for the listener to bind (Addr is empty until then).
	deadline := time.Now().Add(time.Second)
	var base string
	for time.Now().Before(deadline) {
		if a := srv.Addr(); a != "" {
			base = "http://" + a
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if base == "" {
		_ = srv.Close()
		t.Fatalf("listener never bound")
	}
	return base, func() {
		_ = srv.Close()
		select {
		case err := <-errCh:
			if err != nil {
				t.Logf("ListenAndServe returned: %v", err)
			}
		case <-time.After(time.Second):
			t.Logf("ListenAndServe did not exit promptly")
		}
	}
}

// appendTestEvent writes a small text event to the session's
// eventlog. Used by integration tests to produce something the
// broadcaster can stream.
func appendTestEvent(t *testing.T, h *eventlog.Handle, appName, userID, sessionID, text string) {
	t.Helper()
	ctx := context.Background()
	// Ensure session exists.
	if _, err := h.Service.Create(ctx, &session.CreateRequest{
		AppName: appName, UserID: userID, SessionID: sessionID,
	}); err != nil {
		t.Fatalf("session Create: %v", err)
	}
	getResp, err := h.Service.Get(ctx, &session.GetRequest{
		AppName: appName, UserID: userID, SessionID: sessionID,
	})
	if err != nil {
		t.Fatalf("session Get: %v", err)
	}
	ev := session.NewEvent("evt-" + text)
	ev.Author = "test"
	ev.LLMResponse = adkmodel.LLMResponse{}
	// Attach a synthetic detail in CustomMetadata so receivers can
	// assert on something specific.
	ev.CustomMetadata = map[string]any{"text": text}
	if err := h.Service.AppendEvent(ctx, getResp.Session, ev); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
}

func TestIntegration_ListAndStreamEvents(t *testing.T) {
	t.Parallel()
	h, cleanupLog := openTestEventLog(t)
	defer cleanupLog()

	reg := NewSessionRegistry()
	ag := &eventfulRegistrant{
		stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
		handle:         h,
	}
	if _, err := reg.Register(ag); err != nil {
		t.Fatal(err)
	}

	base, cleanupSrv := startTestServer(t, reg)
	defer cleanupSrv()

	// GET /sessions should report our registration.
	resp, err := http.Get(base + "/sessions")
	if err != nil {
		t.Fatalf("GET /sessions: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /sessions status %d: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `"sessionID":"s1"`) {
		t.Errorf("GET /sessions missing s1: %s", body)
	}

	// Subscribe to /events and wait for the first frame after appending one event.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		base+"/sessions/core-agent/s1/events", nil)
	streamResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer streamResp.Body.Close()
	if streamResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(streamResp.Body)
		t.Fatalf("subscribe status %d: %s", streamResp.StatusCode, body)
	}

	// Generate one event for the broadcaster to deliver. We do this
	// after subscribing so the live-tail path (not just replay)
	// exercises end-to-end.
	go func() {
		time.Sleep(100 * time.Millisecond)
		appendTestEvent(t, h, "core-agent", "u", "s1", "hello-attach")
	}()

	// Read SSE frames until we see one carrying our payload.
	scanner := bufio.NewScanner(streamResp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var dataLines []string
	deadline := time.Now().Add(2 * time.Second)
	for scanner.Scan() && time.Now().Before(deadline) {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			dataLines = append(dataLines, strings.TrimPrefix(line, "data: "))
			if strings.Contains(line, "hello-attach") {
				cancel() // unblock the body reader
				return
			}
		}
	}
	t.Errorf("did not receive a frame containing 'hello-attach'. Got %d data lines: %v",
		len(dataLines), dataLines)
}

func TestIntegration_InjectEndpoint(t *testing.T) {
	t.Parallel()
	h, cleanupLog := openTestEventLog(t)
	defer cleanupLog()

	reg := NewSessionRegistry()
	ag := &eventfulRegistrant{
		stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
		handle:         h,
	}
	if _, err := reg.Register(ag); err != nil {
		t.Fatal(err)
	}

	base, cleanupSrv := startTestServer(t, reg)
	defer cleanupSrv()

	body, _ := json.Marshal(map[string]string{"message": "operator nudge"})
	resp, err := http.Post(base+"/sessions/core-agent/s1/inject",
		"application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST inject: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("inject status %d: %s", resp.StatusCode, respBody)
	}
	if len(ag.injected) != 1 || ag.injected[0] != "operator nudge" {
		t.Errorf("Inject called with %v, want [\"operator nudge\"]", ag.injected)
	}
}

func TestIntegration_WakeEndpoint(t *testing.T) {
	t.Parallel()
	h, cleanupLog := openTestEventLog(t)
	defer cleanupLog()

	reg := NewSessionRegistry()
	ag := &eventfulRegistrant{
		stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
		handle:         h,
	}
	if _, err := reg.Register(ag); err != nil {
		t.Fatal(err)
	}

	base, cleanupSrv := startTestServer(t, reg)
	defer cleanupSrv()

	// Empty body wake — just fires RequestWake, no inject.
	resp, err := http.Post(base+"/sessions/core-agent/s1/wake", "application/json", nil)
	if err != nil {
		t.Fatalf("POST wake: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("wake status %d: %s", resp.StatusCode, respBody)
	}
	if ag.wakes != 1 {
		t.Errorf("RequestWake called %d times, want 1", ag.wakes)
	}
	if len(ag.injected) != 0 {
		t.Errorf("empty-body wake should not Inject; got %v", ag.injected)
	}

	// Wake with a prompt should inject + wake.
	body, _ := json.Marshal(map[string]string{"prompt": "rescan now"})
	resp2, err := http.Post(base+"/sessions/core-agent/s1/wake",
		"application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST wake with prompt: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp2.Body)
		t.Fatalf("wake-with-prompt status %d: %s", resp2.StatusCode, respBody)
	}
	if ag.wakes != 2 {
		t.Errorf("RequestWake count = %d, want 2", ag.wakes)
	}
	if len(ag.injected) != 1 || ag.injected[0] != "rescan now" {
		t.Errorf("Inject(prompt) = %v, want [\"rescan now\"]", ag.injected)
	}
}

func TestIntegration_ShortcutAmbiguityIs409(t *testing.T) {
	t.Parallel()
	h, cleanupLog := openTestEventLog(t)
	defer cleanupLog()

	reg := NewSessionRegistry()
	for _, app := range []string{"app1", "app2"} {
		ag := &eventfulRegistrant{
			stubRegistrant: stubRegistrant{app: app, user: "u", sid: "shared"},
			handle:         h,
		}
		if _, err := reg.Register(ag); err != nil {
			t.Fatal(err)
		}
	}

	base, cleanupSrv := startTestServer(t, reg)
	defer cleanupSrv()

	// Shortcut path against the ambiguous SessionID should return 409.
	resp, err := http.Post(base+"/sessions/shared/wake", "application/json", nil)
	if err != nil {
		t.Fatalf("POST wake shortcut: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("status %d, want 409 Conflict. Body: %s", resp.StatusCode, body)
	}
}

func TestIntegration_NoEventLogReturns412(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	// Plain stubRegistrant (no eventlog).
	ag := &stubRegistrant{app: "a", user: "u", sid: "s"}
	if _, err := reg.Register(ag); err != nil {
		t.Fatal(err)
	}

	base, cleanupSrv := startTestServer(t, reg)
	defer cleanupSrv()

	resp, err := http.Get(base + "/sessions/a/s/events")
	if err != nil {
		t.Fatalf("GET events: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPreconditionFailed {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("status %d, want 412 (no eventlog). Body: %s", resp.StatusCode, body)
	}
}

func TestIntegration_BearerTokenAuth(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	srv, err := NewServer(Options{
		Registry: reg,
		Addr:     "127.0.0.1:0",
		Auth:     AuthConfig{BearerToken: "secret"},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	defer func() {
		_ = srv.Close()
		<-errCh
	}()

	deadline := time.Now().Add(time.Second)
	for srv.Addr() == "" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	base := "http://" + srv.Addr()

	// Without token → 401.
	resp, err := http.Get(base + "/sessions")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no-token status = %d, want 401", resp.StatusCode)
	}

	// With wrong token → 401.
	req, _ := http.NewRequest(http.MethodGet, base+"/sessions", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong-token status = %d, want 401", resp.StatusCode)
	}

	// With correct token → 200.
	req, _ = http.NewRequest(http.MethodGet, base+"/sessions", nil)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("good-token status = %d, want 200", resp.StatusCode)
	}
}

func TestIntegration_ReadOnlyBlocksWrites(t *testing.T) {
	t.Parallel()
	h, cleanupLog := openTestEventLog(t)
	defer cleanupLog()

	reg := NewSessionRegistry()
	ag := &eventfulRegistrant{
		stubRegistrant: stubRegistrant{app: "a", user: "u", sid: "s"},
		handle:         h,
	}
	if _, err := reg.Register(ag); err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(Options{
		Registry: reg,
		Addr:     "127.0.0.1:0",
		Auth:     AuthConfig{ReadOnly: true},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	defer func() {
		_ = srv.Close()
		<-errCh
	}()
	deadline := time.Now().Add(time.Second)
	for srv.Addr() == "" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	base := "http://" + srv.Addr()

	// Read endpoint still works.
	resp, _ := http.Get(base + "/sessions")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /sessions status = %d, want 200 even in readonly", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// Write endpoint denied.
	body, _ := json.Marshal(map[string]string{"message": "x"})
	resp, _ = http.Post(base+"/sessions/a/s/inject", "application/json", bytes.NewReader(body))
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("POST inject in readonly = %d, want 403", resp.StatusCode)
	}
	_ = resp.Body.Close()
	if len(ag.injected) != 0 {
		t.Errorf("readonly should not have invoked Inject; got %v", ag.injected)
	}
}

func TestIntegration_AddrEphemeralPort(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	srv, err := NewServer(Options{Registry: reg, Addr: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	defer func() {
		_ = srv.Close()
		<-errCh
	}()
	deadline := time.Now().Add(time.Second)
	for srv.Addr() == "" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !strings.HasPrefix(srv.Addr(), "127.0.0.1:") {
		t.Errorf("Addr = %q, want 127.0.0.1:<port>", srv.Addr())
	}
	if srv.Addr() == "127.0.0.1:0" {
		t.Errorf("Addr still shows :0 — ephemeral port resolution didn't happen")
	}
}

// Sanity: NewServer rejects invalid Addr/UnixSocket combinations.
func TestNewServer_ValidatesOptions(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	cases := []struct {
		name string
		opts Options
		want string
	}{
		{"no registry", Options{Addr: ":7777"}, "Registry is required"},
		{"no addr or socket", Options{Registry: reg}, "exactly one"},
		{"both addr and socket", Options{Registry: reg, Addr: ":7777", UnixSocket: "/x"}, "mutually exclusive"},
		{"tls half", Options{Registry: reg, Addr: ":7777", Auth: AuthConfig{TLSCertFile: "/x"}}, "TLSCertFile and TLSKeyFile must be set together"},
		{"card provider half", Options{Registry: reg, Addr: ":7777", AgentCard: AgentCardConfig{Description: "x", Provider: AgentCardProvider{Organization: "x"}}}, "Provider.Organization and Provider.URL"},
	}
	for _, c := range cases {
		_, err := NewServer(c.opts)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want error containing %q, got %v", c.name, c.want, err)
		}
	}
	_ = fmt.Errorf // silence unused-import if fmt drops
}

// TestIntegration_AgentCard end-to-end fetches /.well-known/agent-card.json
// through the real http.Server, validates against the vendored A2A
// schema, and confirms the endpoint is unauthenticated even when the
// rest of the listener requires a bearer token.
func TestIntegration_AgentCard(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	// Register a session with skills so the card has something to
	// auto-populate from. fixtureRegistrant comes from
	// agentcard_test.go — value type implementing Registrant +
	// SkillsProvider.
	if _, err := reg.Register(fixtureRegistrant{
		app: "core-agent", user: "local", sid: "s-int",
		skills: []SkillInfo{
			{Name: "alpha", Description: "auto alpha"},
		},
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	srv, err := NewServer(Options{
		Registry: reg,
		Addr:     "127.0.0.1:0",
		Auth:     AuthConfig{BearerToken: "secret"},
		AgentCard: AgentCardConfig{
			// No ExternalURL — exercises the request-derived URL path.
			Description: "Integration-test agent",
			Version:     "v0.0.0-test",
		},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	defer func() {
		_ = srv.Close()
		<-errCh
	}()
	deadline := time.Now().Add(time.Second)
	for srv.Addr() == "" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	base := "http://" + srv.Addr()

	// 1) Public fetch — no auth header — must succeed.
	resp, err := http.Get(base + "/.well-known/agent-card.json")
	if err != nil {
		t.Fatalf("GET card: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("card: status %d, body %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q, want application/json; charset=utf-8", ct)
	}
	body, _ := io.ReadAll(resp.Body)

	var card map[string]any
	if err := json.Unmarshal(body, &card); err != nil {
		t.Fatalf("parse card: %v\n%s", err, body)
	}
	if card["name"] != "core-agent" {
		t.Errorf("card name = %v, want core-agent (registrant fallback)", card["name"])
	}
	if want := "http://" + srv.Addr(); card["url"] != want {
		t.Errorf("card url = %v, want %v — should be derived from the Host header on this request", card["url"], want)
	}
	skills, _ := card["skills"].([]any)
	if len(skills) != 1 {
		t.Fatalf("skills: got %d, want 1 — body=%s", len(skills), body)
	}
	if first, _ := skills[0].(map[string]any); first["id"] != "alpha" {
		t.Errorf("skills[0].id = %v, want alpha", first["id"])
	}

	// 2) Other endpoints with no auth header must 401 — confirms the
	//    card bypass doesn't accidentally disarm the middleware.
	resp2, err := http.Get(base + "/sessions")
	if err != nil {
		t.Fatalf("GET /sessions: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Errorf("/sessions: got %d, want %d (bearer required)", resp2.StatusCode, http.StatusUnauthorized)
	}

	// 3) Disabled-by-default sanity: a fresh server with no AgentCard
	//    returns 404 from the regular mux.
	regBare := NewSessionRegistry()
	srvBare, err := NewServer(Options{Registry: regBare, Addr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("NewServer bare: %v", err)
	}
	bareErr := make(chan error, 1)
	go func() { bareErr <- srvBare.ListenAndServe() }()
	defer func() { _ = srvBare.Close(); <-bareErr }()
	for srvBare.Addr() == "" && time.Now().Before(time.Now().Add(time.Second)) {
		time.Sleep(5 * time.Millisecond)
	}
	respBare, err := http.Get("http://" + srvBare.Addr() + "/.well-known/agent-card.json")
	if err != nil {
		t.Fatalf("GET bare card: %v", err)
	}
	defer respBare.Body.Close()
	if respBare.StatusCode != http.StatusNotFound {
		t.Errorf("disabled card endpoint: got %d, want %d", respBare.StatusCode, http.StatusNotFound)
	}
}
