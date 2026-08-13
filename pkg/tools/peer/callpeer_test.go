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

package peer

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/adk/session"
	"google.golang.org/adk/tool"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/pkg/attach"
	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

// ---- fake peer daemon ----

// sseFrame is one scripted SSE event the fake peer writes after the
// prompt lands.
type sseFrame struct {
	name string // SSE event name ("agent", "turn-complete", ...)
	data any    // marshaled as the data block
}

// fakePeer is a minimal attach-mode daemon: enough of POST /sessions,
// POST .../inject and GET .../events for call_peer to drive a full
// delegation against it. Using a real HTTP server (rather than a
// stubbed client) is deliberate — the auth header, the SSE framing,
// and the subscribe-before-inject ordering are exactly the parts worth
// covering.
type fakePeer struct {
	srv *httptest.Server

	// requireToken, when set, makes every request without a matching
	// bearer header a 401.
	requireToken string
	// noSessionFactory replies 501 to POST /sessions, like a peer
	// running without multi-session.
	noSessionFactory bool
	// script produces the frames written after the prompt arrives. nil
	// means "write nothing and hold the stream open".
	script func(prompt string) []sseFrame
	// closeStreamAfterScript ends the SSE response once the script is
	// drained instead of holding it open.
	closeStreamAfterScript bool

	mu       sync.Mutex
	injected []string
	sessions int
	// route records the order the peer's endpoints were hit, so a test
	// can assert the stream was subscribed before the prompt landed.
	route []string

	// prompts carries the injected message to the events handler. The
	// stream is opened first, so this must be buffered: inject must not
	// block waiting for the reader to reach its select.
	prompts chan string
}

func newFakePeer(t *testing.T) *fakePeer {
	t.Helper()
	p := &fakePeer{prompts: make(chan string, 4)}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /sessions", p.handleNewSession)
	mux.HandleFunc("POST /sessions/{app}/{sid}/inject", p.handleInject)
	mux.HandleFunc("GET /sessions/{app}/{sid}/events", p.handleEvents)
	p.srv = httptest.NewServer(p.authed(mux))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *fakePeer) authed(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p.requireToken != "" && r.Header.Get("Authorization") != "Bearer "+p.requireToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (p *fakePeer) handleNewSession(w http.ResponseWriter, _ *http.Request) {
	if p.noSessionFactory {
		http.Error(w, "no session factory configured", http.StatusNotImplemented)
		return
	}
	p.mu.Lock()
	p.sessions++
	id := fmt.Sprintf("s-%d", p.sessions)
	p.route = append(p.route, "new-session")
	p.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"app": "core-agent", "user": "caller", "sessionID": id,
		"url": p.srv.URL + "/sessions/core-agent/" + id,
	})
}

func (p *fakePeer) handleInject(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Message string `json:"message"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	p.mu.Lock()
	p.injected = append(p.injected, body.Message)
	p.route = append(p.route, "inject")
	p.mu.Unlock()
	p.prompts <- body.Message
	w.WriteHeader(http.StatusOK)
}

func (p *fakePeer) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "no flusher", http.StatusInternalServerError)
		return
	}
	p.mu.Lock()
	p.route = append(p.route, "events")
	p.mu.Unlock()
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	var prompt string
	select {
	case prompt = <-p.prompts:
	case <-r.Context().Done():
		return
	}
	if p.script != nil {
		for _, f := range p.script(prompt) {
			buf, err := json.Marshal(f.data)
			if err != nil {
				return
			}
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", f.name, buf); err != nil {
				return
			}
			flusher.Flush()
		}
	}
	if p.closeStreamAfterScript {
		return
	}
	<-r.Context().Done()
}

func (p *fakePeer) injectedMessages() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.injected...)
}

func (p *fakePeer) routeOrder() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.route...)
}

func (p *fakePeer) sessionCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sessions
}

// ---- frame builders ----

// agentFrame builds one `event: agent` SSE frame around a session.Event.
func agentFrame(seq int64, author string, partial bool, parts ...*genai.Part) sseFrame {
	return sseFrame{name: attach.EventAgent, data: attach.Frame{
		Seq: seq,
		Event: &session.Event{
			ID: fmt.Sprintf("e-%d", seq), Author: author,
			LLMResponse: adkmodel.LLMResponse{
				Partial: partial,
				Content: &genai.Content{Role: genai.RoleModel, Parts: parts},
			},
		},
	}}
}

// modelFrame is a final (non-partial) model-authored event carrying text.
func modelFrame(seq int64, text string) sseFrame {
	return agentFrame(seq, "assistant", false, &genai.Part{Text: text})
}

// partialFrame is a streaming chunk: text, but not the final answer.
func partialFrame(seq int64, text string) sseFrame {
	return agentFrame(seq, "assistant", true, &genai.Part{Text: text})
}

// toolCallFrame is a model event that calls a tool — mid-turn, not an
// answer.
func toolCallFrame(seq int64, name string) sseFrame {
	return agentFrame(seq, "assistant", false, &genai.Part{FunctionCall: &genai.FunctionCall{Name: name}})
}

// userFrame is the peer echoing the injected prompt back into its own
// log. It must never end up in the answer.
func userFrame(seq int64, text string) sseFrame {
	return agentFrame(seq, "user", false, &genai.Part{Text: text})
}

// narrateAndCallFrame is the common real-model shape: a sentence of
// narration in the SAME event as the tool call. Text, non-partial, and
// emphatically not the end of the turn.
func narrateAndCallFrame(seq int64, text, toolName string) sseFrame {
	return agentFrame(seq, "assistant", false,
		&genai.Part{Text: text},
		&genai.Part{FunctionCall: &genai.FunctionCall{Name: toolName}})
}

func turnCompleteFrame() sseFrame {
	return sseFrame{name: attach.EventTurnComplete, data: attach.TurnComplete{
		PromptID: "p-1", Model: "test-model", TokensIn: 10, TokensOut: 5,
	}}
}

func turnErrorFrame(kind, msg string) sseFrame {
	return sseFrame{name: attach.EventTurnError, data: attach.TurnError{Kind: kind, Message: msg}}
}

// ---- harness ----

func yoloGate() *permissions.Gate {
	return permissions.New(permissions.Options{Mode: permissions.ModeYolo})
}

func cfgCallPeer(cp config.CallPeerConfig) *config.Config {
	c := config.DefaultConfig()
	c.Tools.CallPeer = cp
	return c
}

func roster(peers ...Peer) Directory {
	return DirectoryFunc(func() []Peer { return peers })
}

// mustHandler builds a handler with a short timeout so a hung peer
// fails a test in milliseconds instead of two minutes. Tests that care
// about the timeout set h.timeout themselves.
func mustHandler(t *testing.T, gate *permissions.Gate, cfg *config.Config, dir Directory, getenv func(string) string) *handler {
	t.Helper()
	h, err := newHandler(gate, cfg, dir, getenv)
	if err != nil {
		t.Fatalf("newHandler: %v", err)
	}
	h.timeout = 5 * time.Second
	return h
}

// ---- tests ----

func TestCallPeer_DelegatesAndReturnsTheAnswer(t *testing.T) {
	t.Parallel()
	p := newFakePeer(t)
	p.requireToken = "s3cret"
	p.script = func(string) []sseFrame {
		return []sseFrame{
			userFrame(1, "what is the node count in prod-1?"),
			narrateAndCallFrame(2, "Checking the node pool.", "list_nodes"),
			partialFrame(3, "prod-1 has "),
			modelFrame(4, "prod-1 has 12 nodes, all Ready."),
			turnCompleteFrame(),
		}
	}
	h := mustHandler(t, yoloGate(),
		cfgCallPeer(config.CallPeerConfig{Enabled: true, TokenEnv: "PEER_TOKEN"}),
		roster(Peer{Name: "operator-prod-1", Endpoint: p.srv.URL}),
		func(string) string { return "s3cret" })

	res, err := h.run(tool.Context(nil), Args{Peer: "operator-prod-1", Prompt: "what is the node count in prod-1?"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	// Narration + answer, blank-line separated. Not the echoed prompt
	// (author "user"), not the streaming chunk (partial), and the
	// narration event does NOT end the turn just because it had text —
	// it also carried a tool call.
	if want := "Checking the node pool.\n\nprod-1 has 12 nodes, all Ready."; res.Response != want {
		t.Errorf("Response = %q, want %q", res.Response, want)
	}
	if res.Peer != "operator-prod-1" {
		t.Errorf("Peer = %q, want operator-prod-1", res.Peer)
	}
	if res.SessionID != "s-1" {
		t.Errorf("SessionID = %q, want the session the peer opened (s-1)", res.SessionID)
	}
	if res.Truncated {
		t.Error("Truncated = true, want false for an answer well under the cap")
	}
	if got := p.injectedMessages(); len(got) != 1 || got[0] != "what is the node count in prod-1?" {
		t.Errorf("injected = %v, want the prompt verbatim", got)
	}
}

func TestCallPeer_OpensAFreshSessionPerCall(t *testing.T) {
	t.Parallel()
	p := newFakePeer(t)
	p.script = func(string) []sseFrame { return []sseFrame{modelFrame(1, "ok"), turnCompleteFrame()} }
	h := mustHandler(t, yoloGate(), cfgCallPeer(config.CallPeerConfig{Enabled: true}),
		roster(Peer{Name: "ops", Endpoint: p.srv.URL}), nil)

	first, err := h.run(tool.Context(nil), Args{Peer: "ops", Prompt: "one"})
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	second, err := h.run(tool.Context(nil), Args{Peer: "ops", Prompt: "two"})
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if first.SessionID == second.SessionID {
		t.Errorf("both calls landed in session %q; each delegation must open its own", first.SessionID)
	}
	if got := p.sessionCount(); got != 2 {
		t.Errorf("sessions opened = %d, want 2", got)
	}
}

func TestCallPeer_UnknownPeerIsRefusedAndListsTheRoster(t *testing.T) {
	t.Parallel()
	p := newFakePeer(t)
	h := mustHandler(t, yoloGate(), cfgCallPeer(config.CallPeerConfig{Enabled: true}),
		roster(
			Peer{Name: "operator-prod-1", Endpoint: p.srv.URL},
			Peer{Name: "devteam-web", Endpoint: p.srv.URL},
		), nil)

	_, err := h.run(tool.Context(nil), Args{Peer: "operator-prod-2", Prompt: "hi"})
	if err == nil {
		t.Fatal("an unregistered peer name must be refused, not dialed")
	}
	for _, want := range []string{"operator-prod-2", "devteam-web", "operator-prod-1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should name %q (the roster is how the model self-corrects)", err, want)
		}
	}
	if p.sessionCount() != 0 {
		t.Error("no session should be opened for an unknown peer")
	}
}

// A peer name that IS a URL is still just a name. This is the
// anti-SSRF property: the tool has no channel for a destination other
// than the registry.
func TestCallPeer_URLShapedNameIsNotDialed(t *testing.T) {
	t.Parallel()
	p := newFakePeer(t)
	h := mustHandler(t, yoloGate(), cfgCallPeer(config.CallPeerConfig{Enabled: true}),
		roster(Peer{Name: "ops", Endpoint: p.srv.URL}), nil)

	if _, err := h.run(tool.Context(nil), Args{Peer: p.srv.URL, Prompt: "hi"}); err == nil {
		t.Fatal("a URL passed as the peer name must be refused as unknown")
	}
	if p.sessionCount() != 0 {
		t.Error("a URL-shaped peer name reached the network")
	}
}

func TestCallPeer_ArgsCarryNoDestinationOrCredential(t *testing.T) {
	t.Parallel()
	var want = map[string]bool{"peer": true, "prompt": true}
	rt := reflect.TypeOf(Args{})
	for i := range rt.NumField() {
		name := strings.Split(rt.Field(i).Tag.Get("json"), ",")[0]
		if !want[name] {
			t.Errorf("Args has field %q; call_peer's schema must expose nothing but peer + prompt "+
				"(an endpoint, header, or token argument would hand the model the destination)", name)
		}
	}
	if rt.NumField() != len(want) {
		t.Errorf("Args has %d fields, want %d", rt.NumField(), len(want))
	}
}

func TestCallPeer_EmptyNameListsTheRoster(t *testing.T) {
	t.Parallel()
	h := mustHandler(t, yoloGate(), cfgCallPeer(config.CallPeerConfig{Enabled: true}),
		roster(Peer{Name: "ops", Endpoint: "http://example.test"}), nil)

	_, err := h.run(tool.Context(nil), Args{Prompt: "hi"})
	if err == nil || !strings.Contains(err.Error(), "ops") {
		t.Fatalf("empty peer name should return the roster, got %v", err)
	}
}

func TestCallPeer_EmptyRosterSaysSo(t *testing.T) {
	t.Parallel()
	h := mustHandler(t, yoloGate(), cfgCallPeer(config.CallPeerConfig{Enabled: true}), roster(), nil)

	_, err := h.run(tool.Context(nil), Args{Peer: "ops", Prompt: "hi"})
	if err == nil || !strings.Contains(err.Error(), "no peers are registered") {
		t.Fatalf("empty roster error = %v, want a 'no peers are registered' message", err)
	}
}

func TestCallPeer_GateDeniesBeforeAnyNetworkCall(t *testing.T) {
	t.Parallel()
	p := newFakePeer(t)
	pol, err := permissions.NewPolicy(nil, []string{"call_peer:*"})
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	gate := permissions.New(permissions.Options{Mode: permissions.ModeYolo, Policy: pol})
	h := mustHandler(t, gate, cfgCallPeer(config.CallPeerConfig{Enabled: true}),
		roster(Peer{Name: "ops", Endpoint: p.srv.URL}), nil)

	if _, err := h.run(tool.Context(nil), Args{Peer: "ops", Prompt: "hi"}); err == nil {
		t.Fatal("a deny pattern must stop the call (deny wins in every mode, including yolo)")
	}
	if p.sessionCount() != 0 {
		t.Error("the gate ran after the call reached the peer")
	}
}

func TestCallPeer_GateIsKeyedPerPeer(t *testing.T) {
	t.Parallel()
	p := newFakePeer(t)
	p.script = func(string) []sseFrame { return []sseFrame{modelFrame(1, "ok"), turnCompleteFrame()} }
	pol, err := permissions.NewPolicy(nil, []string{"call_peer:blocked"})
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	gate := permissions.New(permissions.Options{Mode: permissions.ModeYolo, Policy: pol})
	h := mustHandler(t, gate, cfgCallPeer(config.CallPeerConfig{Enabled: true}),
		roster(
			Peer{Name: "allowed", Endpoint: p.srv.URL},
			Peer{Name: "blocked", Endpoint: p.srv.URL},
		), nil)

	if _, err := h.run(tool.Context(nil), Args{Peer: "allowed", Prompt: "hi"}); err != nil {
		t.Errorf("allowed peer should pass the gate, got %v", err)
	}
	if _, err := h.run(tool.Context(nil), Args{Peer: "blocked", Prompt: "hi"}); err == nil {
		t.Error("blocked peer should be denied")
	}
}

func TestCallPeer_ConfiguredTokenEnvUnsetIsAnError(t *testing.T) {
	t.Parallel()
	p := newFakePeer(t)
	h := mustHandler(t, yoloGate(),
		cfgCallPeer(config.CallPeerConfig{Enabled: true, TokenEnv: "PEER_TOKEN"}),
		roster(Peer{Name: "ops", Endpoint: p.srv.URL}),
		func(string) string { return "" })

	_, err := h.run(tool.Context(nil), Args{Peer: "ops", Prompt: "hi"})
	if err == nil || !strings.Contains(err.Error(), "PEER_TOKEN") {
		t.Fatalf("error = %v, want a complaint about the unset token_env (not an anonymous request)", err)
	}
	if p.sessionCount() != 0 {
		t.Error("an unauthenticated request was sent despite token_env being configured")
	}
}

func TestCallPeer_TurnErrorSurfacesThePeersKindAndMessage(t *testing.T) {
	t.Parallel()
	p := newFakePeer(t)
	p.script = func(string) []sseFrame {
		return []sseFrame{turnErrorFrame(attach.TurnErrorRateLimited, "quota exhausted")}
	}
	h := mustHandler(t, yoloGate(), cfgCallPeer(config.CallPeerConfig{Enabled: true}),
		roster(Peer{Name: "ops", Endpoint: p.srv.URL}), nil)

	_, err := h.run(tool.Context(nil), Args{Peer: "ops", Prompt: "hi"})
	if err == nil {
		t.Fatal("a turn-error on the peer must fail the call")
	}
	for _, want := range []string{attach.TurnErrorRateLimited, "quota exhausted", "ops"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should contain %q", err, want)
		}
	}
}

func TestCallPeer_TimesOutWhenThePeerNeverFinishes(t *testing.T) {
	t.Parallel()
	p := newFakePeer(t)
	p.script = func(string) []sseFrame { return []sseFrame{toolCallFrame(1, "kubectl_get")} }
	h := mustHandler(t, yoloGate(), cfgCallPeer(config.CallPeerConfig{Enabled: true}),
		roster(Peer{Name: "ops", Endpoint: p.srv.URL}), nil)
	h.timeout = 150 * time.Millisecond

	start := time.Now()
	_, err := h.run(tool.Context(nil), Args{Peer: "ops", Prompt: "hi"})
	if err == nil {
		t.Fatal("a peer that never completes its turn must fail the call, not hang it")
	}
	if !strings.Contains(err.Error(), "no turn completed") {
		t.Errorf("error = %v, want a turn-timeout message", err)
	}
	if !strings.Contains(err.Error(), "s-1") {
		t.Errorf("error = %v, want the peer session id so the operator can go look", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("call took %s; the deadline did not bound it", elapsed)
	}
}

func TestCallPeer_LongAnswerIsCappedAndFlagged(t *testing.T) {
	t.Parallel()
	p := newFakePeer(t)
	p.script = func(string) []sseFrame {
		return []sseFrame{modelFrame(1, strings.Repeat("x", 500)), turnCompleteFrame()}
	}
	h := mustHandler(t, yoloGate(),
		cfgCallPeer(config.CallPeerConfig{Enabled: true, MaxResponseBytes: 64}),
		roster(Peer{Name: "ops", Endpoint: p.srv.URL}), nil)

	res, err := h.run(tool.Context(nil), Args{Peer: "ops", Prompt: "hi"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Truncated {
		t.Error("Truncated = false for an answer over the cap")
	}
	if len(res.Response) > 64 {
		t.Errorf("Response is %d bytes, want at most the 64-byte cap", len(res.Response))
	}
	if res.SessionID == "" {
		t.Error("a truncated answer must still carry the session id — it is the only route to the rest")
	}
}

func TestCallPeer_StreamEndingWithoutATurnIsAnError(t *testing.T) {
	t.Parallel()
	p := newFakePeer(t)
	p.script = func(string) []sseFrame { return []sseFrame{toolCallFrame(1, "kubectl_get")} }
	p.closeStreamAfterScript = true
	h := mustHandler(t, yoloGate(), cfgCallPeer(config.CallPeerConfig{Enabled: true}),
		roster(Peer{Name: "ops", Endpoint: p.srv.URL}), nil)

	_, err := h.run(tool.Context(nil), Args{Peer: "ops", Prompt: "hi"})
	if err == nil || !strings.Contains(err.Error(), "ended before the turn completed") {
		t.Fatalf("error = %v, want a stream-ended-early failure rather than an empty success", err)
	}
}

func TestCallPeer_MissingSessionFactoryExplainsTheFix(t *testing.T) {
	t.Parallel()
	p := newFakePeer(t)
	p.noSessionFactory = true
	h := mustHandler(t, yoloGate(), cfgCallPeer(config.CallPeerConfig{Enabled: true}),
		roster(Peer{Name: "ops", Endpoint: p.srv.URL}), nil)

	_, err := h.run(tool.Context(nil), Args{Peer: "ops", Prompt: "hi"})
	if err == nil || !strings.Contains(err.Error(), "multi_session") {
		t.Fatalf("error = %v, want the 501 to carry the multi_session fix", err)
	}
}

func TestCallPeer_NonHTTPEndpointIsRefused(t *testing.T) {
	t.Parallel()
	h := mustHandler(t, yoloGate(), cfgCallPeer(config.CallPeerConfig{Enabled: true}),
		roster(Peer{Name: "ops", Endpoint: "unix:///tmp/agent.sock"}), nil)

	_, err := h.run(tool.Context(nil), Args{Peer: "ops", Prompt: "hi"})
	if err == nil || !strings.Contains(err.Error(), "not callable") {
		t.Fatalf("error = %v, want a refusal for a non-http endpoint", err)
	}
}

func TestCallPeer_EmptyPromptIsRefused(t *testing.T) {
	t.Parallel()
	p := newFakePeer(t)
	h := mustHandler(t, yoloGate(), cfgCallPeer(config.CallPeerConfig{Enabled: true}),
		roster(Peer{Name: "ops", Endpoint: p.srv.URL}), nil)

	if _, err := h.run(tool.Context(nil), Args{Peer: "ops", Prompt: "   "}); err == nil {
		t.Fatal("a whitespace-only prompt must be refused before the call")
	}
	if p.sessionCount() != 0 {
		t.Error("an empty prompt opened a session on the peer")
	}
}

// ---- construction ----

func TestNew_RequiresGateConfigAndDirectory(t *testing.T) {
	t.Parallel()
	cfg := cfgCallPeer(config.CallPeerConfig{Enabled: true})
	if _, err := New(nil, cfg, roster()); err == nil {
		t.Error("nil gate should be rejected")
	}
	if _, err := New(yoloGate(), nil, roster()); err == nil {
		t.Error("nil cfg should be rejected")
	}
	if _, err := New(yoloGate(), cfg, nil); err == nil {
		t.Error("nil directory should be rejected")
	}
}

func TestNew_HonorsNameAndDescriptionOverrides(t *testing.T) {
	t.Parallel()
	cfg := cfgCallPeer(config.CallPeerConfig{
		Enabled: true, Name: "ask_operator", Description: "Ask the cluster operator.",
	})
	tl, err := New(yoloGate(), cfg, roster())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if tl.Name() != "ask_operator" {
		t.Errorf("tool name = %q, want the configured override", tl.Name())
	}
	if got := tl.Description(); got != "Ask the cluster operator." {
		t.Errorf("description = %q, want the configured override", got)
	}
}

func TestNew_DefaultsAreTheDocumentedOnes(t *testing.T) {
	t.Parallel()
	h, err := newHandler(yoloGate(), cfgCallPeer(config.CallPeerConfig{Enabled: true}), roster(), nil)
	if err != nil {
		t.Fatalf("newHandler: %v", err)
	}
	if h.toolName != DefaultToolName {
		t.Errorf("toolName = %q, want %q", h.toolName, DefaultToolName)
	}
	if h.timeout != DefaultTimeout {
		t.Errorf("timeout = %s, want %s", h.timeout, DefaultTimeout)
	}
	if h.maxBytes != DefaultMaxResponseBytes {
		t.Errorf("maxBytes = %d, want %d", h.maxBytes, DefaultMaxResponseBytes)
	}
	if !strings.Contains(h.description, DefaultToolName) {
		t.Error("the default description should tell the model how to discover the roster")
	}
}

// The gate key follows the configured tool name — an operator who
// renames the tool must rename their allow/deny patterns with it.
func TestCallPeer_RenamedToolMovesTheGateKey(t *testing.T) {
	t.Parallel()
	p := newFakePeer(t)
	// A peer that would happily answer, so the only thing that can stop
	// the call is the gate.
	p.script = func(string) []sseFrame { return []sseFrame{modelFrame(1, "ok"), turnCompleteFrame()} }
	pol, err := permissions.NewPolicy(nil, []string{"ask_operator:ops"})
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	gate := permissions.New(permissions.Options{Mode: permissions.ModeYolo, Policy: pol})
	h := mustHandler(t, gate,
		cfgCallPeer(config.CallPeerConfig{Enabled: true, Name: "ask_operator"}),
		roster(Peer{Name: "ops", Endpoint: p.srv.URL}), nil)

	if _, err := h.run(tool.Context(nil), Args{Peer: "ops", Prompt: "hi"}); err == nil {
		t.Fatal("deny pattern keyed on the renamed tool should apply")
	}
	if p.sessionCount() != 0 {
		t.Error("the renamed tool reached the peer; the deny pattern did not move with the name")
	}
}

func TestDirectoryFunc_NilIsAnEmptyRoster(t *testing.T) {
	t.Parallel()
	var f DirectoryFunc
	if got := f.Peers(); got != nil {
		t.Errorf("nil DirectoryFunc.Peers() = %v, want nil", got)
	}
}

// A peer registered with an empty endpoint (a hand-edited state file,
// say) must fail as a misconfiguration rather than resolving to the
// caller's own base URL.
func TestCallPeer_EndpointlessPeerIsRefused(t *testing.T) {
	t.Parallel()
	h := mustHandler(t, yoloGate(), cfgCallPeer(config.CallPeerConfig{Enabled: true}),
		roster(Peer{Name: "ops"}), nil)

	_, err := h.run(tool.Context(nil), Args{Peer: "ops", Prompt: "hi"})
	if err == nil || !strings.Contains(err.Error(), "no endpoint") {
		t.Fatalf("error = %v, want a no-endpoint refusal", err)
	}
}

// The peer starts its turn the moment the prompt lands, and the typed
// turn-complete event is emitted live rather than replayed from the
// event log. Subscribe first or a fast peer's turn end is simply gone.
func TestCallPeer_SubscribesBeforeInjecting(t *testing.T) {
	t.Parallel()
	p := newFakePeer(t)
	p.script = func(string) []sseFrame {
		return []sseFrame{modelFrame(1, "instant answer"), turnCompleteFrame()}
	}
	h := mustHandler(t, yoloGate(), cfgCallPeer(config.CallPeerConfig{Enabled: true}),
		roster(Peer{Name: "ops", Endpoint: p.srv.URL}), nil)

	res, err := h.run(tool.Context(nil), Args{Peer: "ops", Prompt: "hi"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Response != "instant answer" {
		t.Errorf("Response = %q, want the frames emitted immediately after inject", res.Response)
	}
	want := []string{"new-session", "events", "inject"}
	if got := p.routeOrder(); !reflect.DeepEqual(got, want) {
		t.Errorf("request order = %v, want %v", got, want)
	}
}
