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

package coretuiremote

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	coretui "github.com/go-steer/core-tui/tui"

	"github.com/go-steer/core-agent/v2/internal/attachclient"
	"github.com/go-steer/core-agent/v2/pkg/attach"
)

// answeringPrompter answers for the operator with a fixed outcome and
// records which entry point the bridge used: AskApprovalDetailed is
// what puts "r" on the prompt, so calling it against a daemon that
// would drop the reason is the bug, not a detail.
type answeringPrompter struct {
	out         coretui.PermissionOutcome
	plainCalls  int
	detailCalls int
}

func (p *answeringPrompter) AskApproval(context.Context, coretui.PermissionRequest) (coretui.PermissionDecision, error) {
	p.plainCalls++
	return p.out.Decision, nil
}

func (p *answeringPrompter) AskApprovalDetailed(context.Context, coretui.PermissionRequest) (coretui.PermissionOutcome, error) {
	p.detailCalls++
	return p.out, nil
}

// fakeBridgeHost reports a fixed version and, unless setLater is used,
// reports it as already known.
type fakeBridgeHost struct {
	version string
	known   chan struct{} // nil until known() first runs
	mu      sync.Mutex
	notes   []error
}

func (h *fakeBridgeHost) DaemonProtocolVersion() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.version
}

func (h *fakeBridgeHost) DaemonProtocolKnown() <-chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.known == nil {
		h.known = make(chan struct{})
		close(h.known)
	}
	return h.known
}

// pendingBridgeHost is a host whose version is not known yet; learn
// supplies it the way the adapter's first capabilities frame does.
func pendingBridgeHost() *fakeBridgeHost {
	return &fakeBridgeHost{known: make(chan struct{})}
}

func (h *fakeBridgeHost) learn(version string) {
	h.mu.Lock()
	h.version = version
	h.mu.Unlock()
	close(h.known)
}

func (h *fakeBridgeHost) NotifyOperator(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.notes = append(h.notes, err)
}

func (h *fakeBridgeHost) gotNotes() []error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]error(nil), h.notes...)
}

// respondRecorder is a /perms/respond that records every body. With
// rejectReason set it answers a body carrying a reason the way a
// 1.15.0+ daemon answers a reason it refuses: 400, prompt still pending.
type respondRecorder struct {
	rejectReason bool
	status       int // when non-zero, every request gets this status
	mu           sync.Mutex
	bodies       []attach.PromptResponse
}

func (r *respondRecorder) serve(t *testing.T) *attachclient.Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /sessions/{sid}/perms/respond", func(w http.ResponseWriter, req *http.Request) {
		raw, _ := io.ReadAll(req.Body)
		var body attach.PromptResponse
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("respond body %q: %v", raw, err)
		}
		r.mu.Lock()
		r.bodies = append(r.bodies, body)
		r.mu.Unlock()
		switch {
		case r.status != 0:
			http.Error(w, "nope", r.status)
		case r.rejectReason && body.Reason != "":
			http.Error(w, "perms/respond: reason is 900 bytes, over the 500-byte limit", http.StatusBadRequest)
		default:
			_ = json.NewEncoder(w).Encode(attach.PromptRespondResponse{Acknowledged: true})
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	parsed, err := attachclient.ParseURL(srv.URL + "/sessions/s1")
	if err != nil {
		t.Fatal(err)
	}
	return attachclient.New(parsed, "", 0)
}

func (r *respondRecorder) got() []attach.PromptResponse {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]attach.PromptResponse(nil), r.bodies...)
}

var testPromptFrame = attach.PromptFrame{ID: "p1", Kind: "bash", ToolName: "bash", Detail: "kubectl delete ns prod"}

// TestRemotePrompt_DenyReasonReachesTheDaemon is the attach half of
// #1165: against a daemon that takes a reason, the prompt offers "r"
// and the reason the operator typed is in the /perms/respond body.
func TestRemotePrompt_DenyReasonReachesTheDaemon(t *testing.T) {
	t.Parallel()
	for _, version := range []string{"1.15.0", "1.16.0", "v1.15.0", "1.15.1"} {
		t.Run(version, func(t *testing.T) {
			t.Parallel()
			rec := &respondRecorder{}
			client := rec.serve(t)
			p := &answeringPrompter{out: coretui.PermissionOutcome{Decision: coretui.DecisionDeny, Reason: "use the staging cluster"}}
			host := &fakeBridgeHost{version: version}

			handleRemotePromptFrame(context.Background(), client, "/sessions/s1", p, testPromptFrame, io.Discard, host)

			if p.detailCalls != 1 || p.plainCalls != 0 {
				t.Errorf("detailed calls = %d, plain = %d; want AskApprovalDetailed so the prompt offers \"r\"", p.detailCalls, p.plainCalls)
			}
			bodies := rec.got()
			if len(bodies) != 1 {
				t.Fatalf("got %d respond requests, want 1: %+v", len(bodies), bodies)
			}
			if b := bodies[0]; b.ID != "p1" || b.Decision != "deny" || b.Reason != "use the staging cluster" {
				t.Errorf("respond body = %+v, want a deny for p1 carrying the reason", b)
			}
			if len(host.gotNotes()) != 0 {
				t.Errorf("unexpected operator notes: %v", host.gotNotes())
			}
		})
	}
}

// Every other answer goes over the wire exactly as it did before deny
// reasons existed: no reason field, the same decision strings.
func TestRemotePrompt_NoReasonOnAllowsOrPlainDeny(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		out  coretui.PermissionOutcome
		wire string
	}{
		{coretui.PermissionOutcome{Decision: coretui.DecisionDeny}, "deny"},
		{coretui.PermissionOutcome{Decision: coretui.DecisionAllowOnce}, "allow-once"},
		{coretui.PermissionOutcome{Decision: coretui.DecisionAllowSession}, "allow-session"},
		{coretui.PermissionOutcome{Decision: coretui.DecisionAllowAlways}, "allow-always"},
		// core-tui never sets one on an allow; if it ever did, the
		// daemon would 400 it and the approval would be lost.
		{coretui.PermissionOutcome{Decision: coretui.DecisionAllowOnce, Reason: "stray"}, "allow-once"},
	} {
		t.Run(tc.wire+"/"+tc.out.Reason, func(t *testing.T) {
			t.Parallel()
			rec := &respondRecorder{}
			client := rec.serve(t)
			p := &answeringPrompter{out: tc.out}
			handleRemotePromptFrame(context.Background(), client, "/sessions/s1", p, testPromptFrame, io.Discard, &fakeBridgeHost{version: "1.16.0"})
			bodies := rec.got()
			if len(bodies) != 1 || bodies[0].Decision != tc.wire || bodies[0].Reason != "" {
				t.Errorf("respond bodies = %+v, want one %q with no reason", bodies, tc.wire)
			}
		})
	}
}

// Against a daemon that can't take a reason — or whose version isn't
// known yet — the bridge must not offer "r" at all. A pre-1.15.0 daemon
// answers 200 and drops the field, so the operator would believe the
// model read words it never saw.
func TestRemotePrompt_OldOrUnknownDaemonDoesNotOfferReason(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		host PromptBridgeHost
	}{
		{"nil host", nil},
		{"version not yet known", &fakeBridgeHost{}},
		{"1.14.0", &fakeBridgeHost{version: "1.14.0"}},
		{"1.9.3", &fakeBridgeHost{version: "1.9.3"}},
		{"unparseable", &fakeBridgeHost{version: "latest"}},
		{"another major", &fakeBridgeHost{version: "2.0.0"}},
		{"pre-release of 1.15.0", &fakeBridgeHost{version: "1.15.0-rc.1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := &respondRecorder{}
			client := rec.serve(t)
			p := &answeringPrompter{out: coretui.PermissionOutcome{Decision: coretui.DecisionDeny, Reason: "would be dropped"}}
			handleRemotePromptFrame(context.Background(), client, "/sessions/s1", p, testPromptFrame, io.Discard, tc.host)
			if p.detailCalls != 0 || p.plainCalls != 1 {
				t.Errorf("detailed calls = %d, plain = %d; want plain AskApproval, which never offers \"r\"", p.detailCalls, p.plainCalls)
			}
			bodies := rec.got()
			if len(bodies) != 1 || bodies[0].Decision != "deny" || bodies[0].Reason != "" {
				t.Errorf("respond bodies = %+v, want one plain deny", bodies)
			}
		})
	}
}

// A refused reason must not cost the operator their deny. The daemon's
// 400 leaves the prompt pending, so the bridge re-sends a plain deny
// and tells the operator their reason didn't make it.
func TestRemotePrompt_RefusedReasonFallsBackToPlainDeny(t *testing.T) {
	t.Parallel()
	rec := &respondRecorder{rejectReason: true}
	client := rec.serve(t)
	p := &answeringPrompter{out: coretui.PermissionOutcome{Decision: coretui.DecisionDeny, Reason: "too long, say"}}
	host := &fakeBridgeHost{version: "1.15.0"}

	handleRemotePromptFrame(context.Background(), client, "/sessions/s1", p, testPromptFrame, io.Discard, host)

	bodies := rec.got()
	if len(bodies) != 2 {
		t.Fatalf("got %d respond requests, want the reasoned deny then a plain one: %+v", len(bodies), bodies)
	}
	if bodies[0].Reason == "" || bodies[1].Decision != "deny" || bodies[1].Reason != "" || bodies[1].ID != "p1" {
		t.Errorf("respond bodies = %+v, want a reasoned deny followed by a plain deny for p1", bodies)
	}
	if notes := host.gotNotes(); len(notes) != 1 || !strings.Contains(notes[0].Error(), "denied without it") {
		t.Errorf("operator notes = %v, want one saying the deny went without the reason", notes)
	}
}

// A failure a retry can't fix (the prompt is gone) is not retried, and
// the operator gets no misleading "denied without it".
func TestRemotePrompt_GonePromptIsNotRetried(t *testing.T) {
	t.Parallel()
	rec := &respondRecorder{status: http.StatusGone}
	client := rec.serve(t)
	p := &answeringPrompter{out: coretui.PermissionOutcome{Decision: coretui.DecisionDeny, Reason: "late"}}
	host := &fakeBridgeHost{version: "1.15.0"}
	var log strings.Builder

	handleRemotePromptFrame(context.Background(), client, "/sessions/s1", p, testPromptFrame, &log, host)

	if n := len(rec.got()); n != 1 {
		t.Errorf("got %d respond requests, want 1", n)
	}
	if len(host.gotNotes()) != 0 {
		t.Errorf("operator notes = %v, want none", host.gotNotes())
	}
	if !strings.Contains(log.String(), "410") {
		t.Errorf("bridge log = %q, want the 410 logged", log.String())
	}
}

// The adapter is the host main wires in: it learns the version from the
// capabilities frame on its event stream, and its notes land on the
// channel the Events loop renders as chat rows.
func TestAdapter_PromptBridgeHost(t *testing.T) {
	t.Parallel()
	a := New(nil, "/sessions/s1")
	if v := a.DaemonProtocolVersion(); v != "" {
		t.Fatalf("version before any capabilities frame = %q, want empty", v)
	}
	if _, emit := a.consumeTypedFrame(attach.Frame{Type: attach.EventCapabilities, TypedData: &attach.Capabilities{ProtocolVersion: "1.16.0"}}); emit {
		t.Error("a capabilities frame must not render")
	}
	if v := a.DaemonProtocolVersion(); v != "1.16.0" {
		t.Errorf("version = %q, want 1.16.0", v)
	}
	if !daemonTakesDenyReason(a) {
		t.Error("an adapter that saw 1.16.0 must opt the bridge in")
	}

	note := errors.New("reason dropped")
	a.NotifyOperator(note)
	select {
	case got := <-a.injectErrs:
		if !errors.Is(got, note) {
			t.Errorf("queued %v, want %v", got, note)
		}
	default:
		t.Fatal("NotifyOperator queued nothing")
	}
	for range cap(a.injectErrs) + 2 {
		a.NotifyOperator(note) // must never block on a full queue
	}
	select {
	case <-a.DaemonProtocolKnown():
	default:
		t.Error("DaemonProtocolKnown not closed after the capabilities frame")
	}
	// A second capabilities frame (every reconnect sends one) updates
	// the version and must not close the channel twice.
	a.consumeTypedFrame(attach.Frame{Type: attach.EventCapabilities, TypedData: &attach.Capabilities{ProtocolVersion: "1.14.0"}})
	if daemonTakesDenyReason(a) {
		t.Error("after reconnecting to a 1.14.0 daemon the bridge must stop opting in")
	}
}

// After a /switch the original adapter's Events loop is stopped, so a
// note queued on it would never render. NotifyOperator follows the
// shared view to the adapter the operator is looking at.
func TestAdapter_NotifyOperatorFollowsTheViewedSession(t *testing.T) {
	t.Parallel()
	a := New(nil, "/sessions/s1")
	b := New(nil, "/sessions/s2")
	a.handOff(b)
	note := errors.New("reason dropped")
	a.NotifyOperator(note)
	select {
	case got := <-b.injectErrs:
		if !errors.Is(got, note) {
			t.Errorf("queued %v, want %v", got, note)
		}
	default:
		t.Fatal("the note did not reach the viewed session's adapter")
	}
}

// The capabilities frame really arrives through the SSE path the TUI
// runs — attachclient's parser, then the Events loop — not only through
// a hand-built Frame.
func TestAdapter_LearnsProtocolFromTheEventStream(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /sessions/{sid}/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "event: capabilities\ndata: {\"protocol_version\":\"1.16.0\",\"event_types\":[\"agent\"]}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	a := newPauseAdapter(t, srv)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() {
		for range a.Events(ctx) { //nolint:revive // drained only for its side effect
		}
	}()
	select {
	case <-a.DaemonProtocolKnown():
	case <-ctx.Done():
		t.Fatal("the capabilities frame on /events never reached the adapter")
	}
	if v := a.DaemonProtocolVersion(); v != "1.16.0" {
		t.Errorf("version = %q, want 1.16.0", v)
	}
}

// A dropped /events stream may reconnect to a restarted daemon at a
// different version, so the adapter forgets the old one until the new
// stream's capabilities frame says otherwise.
func TestAdapter_ForgetsProtocolWhenTheStreamDrops(t *testing.T) {
	t.Parallel()
	var connects atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("GET /sessions/{sid}/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if connects.Add(1) == 1 {
			// First connection: announce 1.16.0, then drop.
			_, _ = io.WriteString(w, "event: capabilities\ndata: {\"protocol_version\":\"1.16.0\",\"event_types\":[\"agent\"]}\n\n")
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			return
		}
		// Reconnect: never announce anything.
		<-r.Context().Done()
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	a := newPauseAdapter(t, srv)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() {
		for range a.Events(ctx) { //nolint:revive // drained only for its side effect
		}
	}()
	select {
	case <-a.DaemonProtocolKnown():
	case <-ctx.Done():
		t.Fatal("the capabilities frame on /events never reached the adapter")
	}
	for a.DaemonProtocolVersion() != "" {
		select {
		case <-ctx.Done():
			t.Fatalf("version = %q after the stream dropped, want it forgotten", a.DaemonProtocolVersion())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestAwaitDaemonProtocol(t *testing.T) {
	t.Parallel()
	const long = 10 * time.Second
	quick := func(name string, f func()) {
		t.Helper()
		start := time.Now()
		f()
		if d := time.Since(start); d > 2*time.Second {
			t.Errorf("%s: waited %v, want an immediate return", name, d)
		}
	}
	quick("nil host", func() { awaitDaemonProtocol(context.Background(), nil, long) })
	quick("known", func() { awaitDaemonProtocol(context.Background(), &fakeBridgeHost{version: "1.15.0"}, long) })

	h := pendingBridgeHost()
	go func() { time.Sleep(20 * time.Millisecond); h.learn("1.15.0") }()
	quick("learned while waiting", func() { awaitDaemonProtocol(context.Background(), h, long) })

	quick("bounded", func() { awaitDaemonProtocol(context.Background(), pendingBridgeHost(), 20*time.Millisecond) })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	quick("ctx done", func() { awaitDaemonProtocol(ctx, pendingBridgeHost(), long) })
}

// The case the wait exists for: an operator attaches to answer a prompt
// that is already pending. /perms/stream replays it within a round trip,
// usually before the event stream's capabilities frame. The bridge must
// still offer "r" and carry the reason.
func TestRemotePromptBridge_PendingPromptWaitsForTheVersion(t *testing.T) {
	t.Parallel()
	rec := &respondRecorder{}
	responded := make(chan struct{}, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /sessions/{sid}/perms/stream", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		payload, _ := json.Marshal(testPromptFrame)
		_, _ = io.WriteString(w, "data: "+string(payload)+"\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	})
	mux.HandleFunc("POST /sessions/{sid}/perms/respond", func(w http.ResponseWriter, req *http.Request) {
		var body attach.PromptResponse
		_ = json.NewDecoder(req.Body).Decode(&body)
		rec.mu.Lock()
		rec.bodies = append(rec.bodies, body)
		rec.mu.Unlock()
		_ = json.NewEncoder(w).Encode(attach.PromptRespondResponse{Acknowledged: true})
		responded <- struct{}{}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	parsed, err := attachclient.ParseURL(srv.URL + "/sessions/s1")
	if err != nil {
		t.Fatal(err)
	}
	client := attachclient.New(parsed, "", 0)

	host := pendingBridgeHost()
	p := &answeringPrompter{out: coretui.PermissionOutcome{Decision: coretui.DecisionDeny, Reason: "wait for the canary"}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go func() {
		time.Sleep(100 * time.Millisecond) // the frame is in hand; the version is not
		host.learn("1.16.0")
	}()
	done := make(chan struct{})
	go func() {
		defer close(done)
		runRemotePromptBridge(ctx, client, "/sessions/s1", p, io.Discard, host)
	}()
	select {
	case <-responded:
	case <-ctx.Done():
		t.Fatal("the bridge never answered the pending prompt")
	}
	cancel()
	<-done
	if p.detailCalls != 1 || p.plainCalls != 0 {
		t.Errorf("detailed calls = %d, plain = %d; a prompt pending at attach must still offer \"r\"", p.detailCalls, p.plainCalls)
	}
	if b := rec.got(); len(b) != 1 || b[0].Reason != "wait for the canary" {
		t.Errorf("respond bodies = %+v, want one deny carrying the reason", b)
	}
}
