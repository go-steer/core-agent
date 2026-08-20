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

// Wire-format drift guard for the attachclient RPC surface (#396).
//
// Every test in this file drives the REAL attach.Server over a real
// TCP listener — no handler mocks — so a change to the server's JSON
// shapes, status-code mapping, or SSE framing that the client can't
// parse fails here first. The Registrant behind the registry is a
// real *agent.Agent (echo model) wrapped in attachadapter.New, the
// same shape production daemons register.
package attachclient

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	adkmodel "google.golang.org/adk/model"
	"google.golang.org/adk/session"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/pkg/agent"
	"github.com/go-steer/core-agent/v2/pkg/attach"
	"github.com/go-steer/core-agent/v2/pkg/attachadapter"
	"github.com/go-steer/core-agent/v2/pkg/auth"
	"github.com/go-steer/core-agent/v2/pkg/eventlog"
	"github.com/go-steer/core-agent/v2/pkg/models/mock"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

const (
	testToken  = "attachclient-test-token"
	testUserID = "operator-user"
	testSID    = "s1"
)

// rpcHarness is one live attach.Server + the Client under test.
type rpcHarness struct {
	base    string
	client  *Client
	broker  *attach.PromptBroker
	handle  *eventlog.Handle
	adapter *attachadapter.Adapter
	reg     *attach.SessionRegistry
}

// sessionPath returns the /sessions/<app>/<sid> prefix for the
// harness's primary registered session.
func (h *rpcHarness) sessionPath() string {
	return "/sessions/" + h.adapter.AppName() + "/" + h.adapter.SessionID()
}

// newEchoAdapter builds a real echo-model agent wrapped in the attach
// adapter — the exact Registrant shape production daemons register.
func newEchoAdapter(t *testing.T, handle *eventlog.Handle, userID, sid string, opts ...attachadapter.Option) *attachadapter.Adapter {
	t.Helper()
	m, err := mock.NewEcho().Model(context.Background(), "echo")
	if err != nil {
		t.Fatalf("echo model: %v", err)
	}
	a, err := agent.New(m, agent.WithSession(userID, sid), agent.WithEventLog(handle))
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	return attachadapter.New(a, opts...)
}

// harnessConfig tweaks optional server wiring per test.
type harnessConfig struct {
	withFactory bool
}

// newRPCHarness stands up a real attach.Server on 127.0.0.1:0 with a
// bearer token (so the client's auth stamping is exercised too), a
// registered echo agent with an eventlog + prompt broker, and returns
// a Client pointed at it. Everything is torn down via t.Cleanup.
func newRPCHarness(t *testing.T, cfg harnessConfig) *rpcHarness {
	t.Helper()

	dsn := filepath.Join(t.TempDir(), "session.db")
	handle, err := eventlog.Open(context.Background(), sqlite.Open(dsn))
	if err != nil {
		t.Fatalf("eventlog.Open: %v", err)
	}
	t.Cleanup(func() { _ = handle.Close() })

	broker := attach.NewPromptBroker()
	t.Cleanup(broker.Close)

	adapter := newEchoAdapter(t, handle, testUserID, testSID, attachadapter.WithPromptBroker(broker))
	reg := attach.NewSessionRegistry()
	if _, err := reg.Register(adapter); err != nil {
		t.Fatalf("Register: %v", err)
	}

	opts := attach.Options{
		Registry:        reg,
		Addr:            "127.0.0.1:0",
		Auth:            attach.AuthConfig{BearerToken: testToken},
		DefaultCaller:   auth.Caller{Identity: "op@local"},
		ShutdownTimeout: 2 * time.Second,
	}
	if cfg.withFactory {
		var n atomic.Int64
		opts.SessionFactory = func(ctx context.Context, caller auth.Caller) (attach.Registrant, context.CancelFunc, error) {
			sid := fmt.Sprintf("s-new-%d", n.Add(1))
			return newEchoAdapter(t, handle, "factory-user", sid), nil, nil
		}
	}

	srv, err := attach.NewServer(opts)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if err := srv.Bind(); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve() }()
	t.Cleanup(func() {
		_ = srv.Close()
		select {
		case err := <-errCh:
			if err != nil {
				t.Logf("Serve returned: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Logf("Serve did not exit promptly")
		}
	})

	base := "http://" + srv.Addr()
	parsed, err := ParseURL(base)
	if err != nil {
		t.Fatalf("ParseURL(%q): %v", base, err)
	}

	return &rpcHarness{
		base:    base,
		client:  New(parsed, testToken, 5*time.Second),
		broker:  broker,
		handle:  handle,
		adapter: adapter,
		reg:     reg,
	}
}

// appendSessionEvent writes one event with a recognizable
// CustomMetadata marker into the harness's eventlog so the
// broadcaster has something to stream.
func (h *rpcHarness) appendSessionEvent(t *testing.T, text string) {
	t.Helper()
	ctx := context.Background()
	app, user, sid := h.adapter.AppName(), h.adapter.UserID(), h.adapter.SessionID()
	if _, err := h.handle.Service.Create(ctx, &session.CreateRequest{
		AppName: app, UserID: user, SessionID: sid,
	}); err != nil {
		t.Fatalf("session Create: %v", err)
	}
	resp, err := h.handle.Service.Get(ctx, &session.GetRequest{
		AppName: app, UserID: user, SessionID: sid,
	})
	if err != nil {
		t.Fatalf("session Get: %v", err)
	}
	ev := session.NewEvent("evt-" + text)
	ev.Author = "test"
	ev.LLMResponse = adkmodel.LLMResponse{}
	ev.CustomMetadata = map[string]any{"text": text}
	if err := h.handle.Service.AppendEvent(ctx, resp.Session, ev); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
}

// ---- /sessions (list + create) ------------------------------------

func TestClientListSessions_AgainstRealServer(t *testing.T) {
	t.Parallel()
	h := newRPCHarness(t, harnessConfig{})

	got, err := h.client.ListSessions(context.Background())
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ListSessions returned %d rows, want 1: %+v", len(got), got)
	}
	want := SessionDescriptor{
		App:         h.adapter.AppName(),
		User:        testUserID,
		SessionID:   testSID,
		HasEventLog: true,
	}
	// Zero the server-clock fields before comparing the identity half;
	// they're asserted separately below.
	row := got[0]
	status, touched := row.Status, row.LastTouchedAt
	row.Status, row.LastTouchedAt = "", time.Time{}
	if row != want {
		t.Errorf("ListSessions[0] = %+v, want %+v", row, want)
	}
	// The picker orders and labels rows off these two, so a listener
	// that reports them must survive the round-trip.
	if status != "active" {
		t.Errorf("Status = %q, want %q", status, "active")
	}
	if touched.IsZero() {
		t.Errorf("LastTouchedAt not decoded from the /sessions row")
	}
}

func TestClientNewSession_CreatesOwnedSession(t *testing.T) {
	t.Parallel()
	h := newRPCHarness(t, harnessConfig{withFactory: true})

	resp, err := h.client.NewSession(context.Background())
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if resp.SessionID != "s-new-1" {
		t.Errorf("SessionID = %q, want s-new-1", resp.SessionID)
	}
	if resp.AppName != h.adapter.AppName() {
		t.Errorf("AppName = %q, want %q", resp.AppName, h.adapter.AppName())
	}
	if resp.UserID != "factory-user" {
		t.Errorf("UserID = %q, want factory-user", resp.UserID)
	}
	wantSuffix := "/sessions/" + resp.AppName + "/" + resp.SessionID
	if !strings.HasPrefix(resp.URL, "http://") || !strings.HasSuffix(resp.URL, wantSuffix) {
		t.Errorf("URL = %q, want http://<host>%s", resp.URL, wantSuffix)
	}

	// The new session is immediately visible to ListSessions — the
	// decoded response and the registry agree.
	rows, err := h.client.ListSessions(context.Background())
	if err != nil {
		t.Fatalf("ListSessions after create: %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("ListSessions after create returned %d rows, want 2: %+v", len(rows), rows)
	}
}

func TestClientNewSession_NoFactoryIs501(t *testing.T) {
	t.Parallel()
	h := newRPCHarness(t, harnessConfig{}) // no SessionFactory

	_, err := h.client.NewSession(context.Background())
	if err == nil {
		t.Fatal("NewSession without a server-side factory should error")
	}
	var se *httpStatusError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v (%T), want *httpStatusError", err, err)
	}
	if se.statusCode != 501 {
		t.Errorf("status = %d, want 501 (no SessionFactory configured)", se.statusCode)
	}
	if se.PermanentStreamErr() {
		t.Errorf("501 should not classify as permanent (it's a deployment-capability miss, not a revoked session)")
	}
}

// ---- SSE /events ---------------------------------------------------

// TestClientStream_CapabilitiesBootFrameFirst regression-guards the
// #385 ordering fix: the capabilities frame MUST be the first frame on
// every newly-opened stream, before any snapshot or live frame.
func TestClientStream_CapabilitiesBootFrameFirst(t *testing.T) {
	t.Parallel()
	h := newRPCHarness(t, harnessConfig{})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	frames, err := h.client.Stream(ctx, h.sessionPath(), 0)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	first, ok := <-frames
	if !ok {
		t.Fatal("stream closed before delivering any frame")
	}
	if first.Type != attach.EventCapabilities {
		t.Fatalf("first frame Type = %q, want %q (capabilities-first is a protocol invariant, #385)",
			first.Type, attach.EventCapabilities)
	}
	caps, isCaps := first.TypedData.(*attach.Capabilities)
	if !isCaps || caps == nil {
		t.Fatalf("first frame TypedData = %T, want *attach.Capabilities", first.TypedData)
	}
	if caps.ProtocolVersion == "" {
		t.Error("capabilities.protocol_version is empty")
	}
	if len(caps.EventTypes) == 0 {
		t.Error("capabilities.event_types is empty")
	}

	// Live-tail: an event appended to the eventlog after subscribe
	// arrives as a legacy frame with the payload intact.
	h.appendSessionEvent(t, "hello-rpc")

	deadline := time.After(5 * time.Second)
	for {
		select {
		case frame, ok := <-frames:
			if !ok {
				t.Fatal("stream closed before the live event arrived")
			}
			if frame.Type != "" {
				continue // boot status-update / usage-update frames
			}
			if frame.Event == nil {
				t.Fatalf("legacy frame with nil Event: %+v", frame)
			}
			if frame.Seq <= 0 {
				t.Errorf("legacy frame Seq = %d, want > 0", frame.Seq)
			}
			if got := frame.Event.CustomMetadata["text"]; got != "hello-rpc" {
				t.Errorf("event CustomMetadata[text] = %v, want hello-rpc", got)
			}
			return
		case <-deadline:
			t.Fatal("timed out waiting for the live-tail frame")
		}
	}
}

func TestClientStream_UnknownSessionIs404(t *testing.T) {
	t.Parallel()
	h := newRPCHarness(t, harnessConfig{})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := h.client.Stream(ctx, "/sessions/"+h.adapter.AppName()+"/absent", 0)
	if err == nil {
		t.Fatal("Stream against a missing session should fail synchronously")
	}
	var se *httpStatusError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v (%T), want *httpStatusError", err, err)
	}
	if se.statusCode != 404 {
		t.Errorf("status = %d, want 404", se.statusCode)
	}
	if !se.PermanentStreamErr() {
		t.Error("404 must classify as permanent so the TUI stops its reconnect loop")
	}
}

// ---- /perms/stream + /perms/respond --------------------------------

func TestClientPromptStreamAndRespond_RoundTrip(t *testing.T) {
	t.Parallel()
	h := newRPCHarness(t, harnessConfig{})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	frames, err := h.client.PromptStream(ctx, h.sessionPath())
	if err != nil {
		t.Fatalf("PromptStream: %v", err)
	}

	// The gate side: AskApproval blocks until the operator responds.
	type approval struct {
		decision permissions.Decision
		err      error
	}
	done := make(chan approval, 1)
	go func() {
		d, err := h.broker.AskApproval(ctx, permissions.PromptRequest{
			Kind:     permissions.PromptKindBash,
			ToolName: "bash",
			Detail:   "git push origin main",
			Verb:     "git",
		})
		done <- approval{d, err}
	}()

	var frame attach.PromptFrame
	select {
	case frame = <-frames:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the prompt frame")
	}
	if frame.ID == "" {
		t.Fatal("prompt frame has empty id")
	}
	if frame.Kind != "bash" || frame.ToolName != "bash" || frame.Detail != "git push origin main" || frame.Verb != "git" {
		t.Errorf("prompt frame fields drifted: %+v", frame)
	}

	if err := h.client.RespondToPrompt(ctx, h.sessionPath(), frame.ID, "allow-once"); err != nil {
		t.Fatalf("RespondToPrompt: %v", err)
	}

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("AskApproval: %v", got.err)
		}
		if got.decision != permissions.DecisionAllowOnce {
			t.Errorf("decision = %v, want DecisionAllowOnce", got.decision)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("AskApproval did not unblock after RespondToPrompt")
	}
}

func TestClientRespondToPrompt_ErrorMapping(t *testing.T) {
	t.Parallel()
	h := newRPCHarness(t, harnessConfig{})
	ctx := context.Background()

	// Unknown decision string → 400 before the broker is consulted.
	err := h.client.RespondToPrompt(ctx, h.sessionPath(), "some-id", "maybe-later")
	var se *httpStatusError
	if !errors.As(err, &se) || se.statusCode != 400 {
		t.Errorf("bad decision: err = %v, want *httpStatusError with 400", err)
	}

	// Valid decision, unknown prompt id → 404.
	err = h.client.RespondToPrompt(ctx, h.sessionPath(), "no-such-prompt", "deny")
	se = nil
	if !errors.As(err, &se) || se.statusCode != 404 {
		t.Errorf("unknown id: err = %v, want *httpStatusError with 404", err)
	}
}

func TestClientPromptStream_NoBrokerIs501(t *testing.T) {
	t.Parallel()
	h := newRPCHarness(t, harnessConfig{})

	// A second session whose adapter has NO prompt broker wired.
	bare := newEchoAdapter(t, h.handle, "u2", "s2")
	if _, err := h.reg.Register(bare); err != nil {
		t.Fatalf("Register bare adapter: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := h.client.PromptStream(ctx, "/sessions/"+bare.AppName()+"/s2")
	if err == nil {
		t.Fatal("PromptStream without a broker should fail synchronously")
	}
	var se *httpStatusError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v (%T), want *httpStatusError", err, err)
	}
	if se.statusCode != 501 {
		t.Errorf("status = %d, want 501 (capability not registered)", se.statusCode)
	}
}

// ---- /interrupt, /pause, /resume --------------------------------------

func TestClientInterrupt_IdleAgent(t *testing.T) {
	t.Parallel()
	h := newRPCHarness(t, harnessConfig{})

	resp, err := h.client.Interrupt(context.Background(), h.sessionPath(), false /* hold */, false)
	if err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	if resp.Interrupted {
		t.Error("Interrupted = true on an idle agent, want false (nothing in flight)")
	}
	if resp.Session != testSID {
		t.Errorf("Session = %q, want %q", resp.Session, testSID)
	}
	if resp.Paused {
		t.Error("Paused = true with hold=false, want false (opt-out of the v1.5.0 park)")
	}
}

// An interrupt against an IDLE agent still parks it. The operator meant
// "stop"; without the hold the next scheduler tick, wake, or
// auto-continue drives a turn seconds later and the stop reads as
// having done nothing — which is the reported symptom this whole
// surface exists to fix.
func TestClientInterrupt_HoldParksIdleAgent(t *testing.T) {
	t.Parallel()
	h := newRPCHarness(t, harnessConfig{})
	ctx := context.Background()
	path := h.sessionPath()

	resp, err := h.client.Interrupt(ctx, path, true /* hold */, false)
	if err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	if resp.Interrupted {
		t.Error("Interrupted = true on an idle agent, want false")
	}
	if !resp.Paused {
		t.Fatal("Paused = false after hold interrupt, want true")
	}

	status, err := h.client.Status(ctx, path)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.State != attach.AgentStatePaused {
		t.Errorf("Status.State = %q, want %q", status.State, attach.AgentStatePaused)
	}
	if status.PauseReason == "" {
		t.Error("Status.PauseReason is empty, want the operator-interrupt reason")
	}
	if status.PausedSince.IsZero() {
		t.Error("Status.PausedSince is zero, want the park timestamp")
	}
	if status.Interrupted {
		t.Error("Status.Interrupted = true, want false (nothing was in flight to kill)")
	}
}

func TestClientPauseResume_RoundTrip(t *testing.T) {
	t.Parallel()
	h := newRPCHarness(t, harnessConfig{})
	ctx := context.Background()
	path := h.sessionPath()

	pr, err := h.client.Pause(ctx, path, "maintenance")
	if err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if !pr.Paused || !pr.Transitioned {
		t.Fatalf("Pause = %+v, want paused+transitioned", pr)
	}
	if pr.Reason != "maintenance" {
		t.Errorf("Pause.Reason = %q, want %q", pr.Reason, "maintenance")
	}

	// Second pause is idempotent: still paused, but not this call's
	// doing — a client can stay quiet on a redundant press.
	again, err := h.client.Pause(ctx, path, "maintenance")
	if err != nil {
		t.Fatalf("Pause (repeat): %v", err)
	}
	if !again.Paused || again.Transitioned {
		t.Errorf("repeat Pause = %+v, want paused=true transitioned=false", again)
	}

	rr, err := h.client.Resume(ctx, path, attach.ResumeRequest{Steer: "check the pods instead"})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if !rr.Resumed {
		t.Error("Resumed = false, want true")
	}
	if rr.Mode != attach.ResumeModeSteer {
		t.Errorf("Mode = %q, want %q (defaulted from non-empty steer)", rr.Mode, attach.ResumeModeSteer)
	}
	if rr.State != attach.AgentStateIdle {
		t.Errorf("State = %q, want %q", rr.State, attach.AgentStateIdle)
	}

	// Resuming an agent that isn't paused is a 200 no-op, not an error:
	// two operator surfaces racing the same click shouldn't produce a
	// spurious failure.
	noop, err := h.client.Resume(ctx, path, attach.ResumeRequest{})
	if err != nil {
		t.Fatalf("Resume (not paused): %v", err)
	}
	if noop.Resumed {
		t.Error("Resumed = true on an unpaused agent, want false")
	}
	if noop.Mode != attach.ResumeModeContinue {
		t.Errorf("Mode = %q, want %q (defaulted from empty steer)", noop.Mode, attach.ResumeModeContinue)
	}
}

// The steer text has to reach the inbox, framed — a resume that opens
// the gate but drops the operator's instruction is the failure mode
// that makes the whole park pointless.
func TestClientResume_QueuesFramedSteer(t *testing.T) {
	t.Parallel()
	h := newRPCHarness(t, harnessConfig{})
	ctx := context.Background()
	path := h.sessionPath()

	if _, err := h.client.Pause(ctx, path, ""); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if _, err := h.client.Resume(ctx, path, attach.ResumeRequest{Steer: "look at node pressure"}); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	queued := h.adapter.Agent().DrainInbox()
	if len(queued) != 1 {
		t.Fatalf("inbox has %d messages, want 1: %q", len(queued), queued)
	}
	if !strings.Contains(queued[0], "look at node pressure") {
		t.Errorf("queued message %q does not carry the steer text", queued[0])
	}
	if !strings.Contains(queued[0], "interrupted") {
		t.Errorf("queued message %q is not interrupt-framed; the model would treat the "+
			"cancelled turn as a normal gap and re-run it", queued[0])
	}
}

func TestClientResume_RejectsUnknownMode(t *testing.T) {
	t.Parallel()
	h := newRPCHarness(t, harnessConfig{})

	_, err := h.client.Resume(context.Background(), h.sessionPath(),
		attach.ResumeRequest{Mode: "sideways"})
	var se *httpStatusError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v (%T), want *httpStatusError", err, err)
	}
	if se.statusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", se.statusCode)
	}
}

// Park and release have to reach a WATCHING client, not just the one
// that pressed the button. A second operator surface (mast-web next to
// a TUI) renders the steer prompt off this frame; without it, one
// client shows "paused" and the other keeps drawing a running turn.
//
// This is also the end-to-end proof that the emit path survives the
// hops: agent.emitPause -> broadcaster.Emit -> SSE frame -> the
// client's typed decoder.
func TestClientStream_PauseFramesReachTheStream(t *testing.T) {
	t.Parallel()
	h := newRPCHarness(t, harnessConfig{})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	path := h.sessionPath()

	frames, err := h.client.Stream(ctx, path, 0)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	// Drain the boot frames so the pause events below are the next
	// typed frames we care about; the reader loop skips anything else
	// anyway, this just keeps the buffered channel moving.
	if first, ok := <-frames; !ok || first.Type != attach.EventCapabilities {
		t.Fatalf("first frame = %+v (open=%v), want capabilities", first, ok)
	}

	if _, err := h.client.Pause(ctx, path, "maintenance"); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	paused := nextPauseFrame(t, frames)
	if paused.State != attach.PauseStatePaused {
		t.Errorf("pause frame State = %q, want %q", paused.State, attach.PauseStatePaused)
	}
	if paused.Reason != "maintenance" {
		t.Errorf("pause frame Reason = %q, want %q", paused.Reason, "maintenance")
	}
	if paused.Interrupted {
		t.Error("pause frame Interrupted = true for a plain pause, want false")
	}
	if paused.At.IsZero() {
		t.Error("pause frame At is zero, want the park timestamp")
	}

	if _, err := h.client.Resume(ctx, path, attach.ResumeRequest{Steer: "drain node-3 first"}); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	resumed := nextPauseFrame(t, frames)
	if resumed.State != attach.PauseStateResumed {
		t.Errorf("resume frame State = %q, want %q", resumed.State, attach.PauseStateResumed)
	}
	if resumed.Mode != attach.ResumeModeSteer {
		t.Errorf("resume frame Mode = %q, want %q — a watching client renders what the "+
			"operator chose, not just that something changed", resumed.Mode, attach.ResumeModeSteer)
	}
}

// nextPauseFrame reads until the next typed `pause` frame, skipping the
// status/usage boot frames and any live-tail eventlog frames.
func nextPauseFrame(t *testing.T, frames <-chan attach.Frame) *attach.PauseEvent {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case frame, ok := <-frames:
			if !ok {
				t.Fatal("stream closed before a pause frame arrived")
			}
			if frame.Type != attach.EventPause {
				continue
			}
			p, isPause := frame.TypedData.(*attach.PauseEvent)
			if !isPause || p == nil {
				t.Fatalf("pause frame TypedData = %T, want *attach.PauseEvent", frame.TypedData)
			}
			return p
		case <-deadline:
			t.Fatal("timed out waiting for a pause frame")
			return nil
		}
	}
}

func TestClientStopAgent_NoSuchSubagent(t *testing.T) {
	t.Parallel()
	h := newRPCHarness(t, harnessConfig{})

	err := h.client.StopAgent(context.Background(), h.sessionPath(), "ghost")
	var se *httpStatusError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v (%T), want *httpStatusError", err, err)
	}
	if se.statusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (an operator aiming at a runaway needs to know they missed)", se.statusCode)
	}
}

// ---- session-scoped reads + writes over the real wire ----------------

func TestClientSessionReadsAndWrites(t *testing.T) {
	t.Parallel()
	h := newRPCHarness(t, harnessConfig{})
	ctx := context.Background()
	path := h.sessionPath()

	status, err := h.client.Status(ctx, path)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.State != attach.AgentStateIdle {
		t.Errorf("Status.State = %q, want %q", status.State, attach.AgentStateIdle)
	}
	if status.ModelName != "echo" {
		t.Errorf("Status.ModelName = %q, want echo", status.ModelName)
	}

	tools, err := h.client.Tools(ctx, path)
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	if len(tools) != 0 {
		t.Errorf("Tools = %+v, want empty (echo agent has none)", tools)
	}

	usage, err := h.client.Usage(ctx, path)
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	if usage.Overall.Turns != 0 {
		t.Errorf("Usage.Overall.Turns = %d, want 0 (fresh agent)", usage.Overall.Turns)
	}

	// No PeerRegistry configured → server 404s /peers → client maps
	// that to (nil, nil), not an error.
	peers, err := h.client.ListPeers(ctx)
	if err != nil {
		t.Fatalf("ListPeers: %v", err)
	}
	if peers != nil {
		t.Errorf("ListPeers = %+v, want nil when peer registration is disabled", peers)
	}

	if err := h.client.Inject(ctx, path, "operator note"); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if err := h.client.Wake(ctx, path); err != nil {
		t.Fatalf("Wake: %v", err)
	}
	// End-to-end proof for {"wake": false} (#698): client, handler and
	// adapter all have to agree, and the adapter is the layer where a
	// missing forward turns into a 501 that only shows up in production.
	if err := h.client.QueueContext(ctx, path, "deferred context"); err != nil {
		t.Fatalf("QueueContext: %v", err)
	}
	if n := h.adapter.Agent().PendingInboxCount(); n != 2 {
		t.Errorf("agent inbox holds %d messages, want the injected one plus the deferred one", n)
	}
}

// ---- /agents/<name>/events ------------------------------------------

// appendBranchedEvent writes one event into the harness's session
// tagged with branch, mirroring what the subagent runners do: they
// wrap the PARENT's session.Service so the child's turns land in the
// same database under a Branch label.
func (h *rpcHarness) appendBranchedEvent(t *testing.T, branch, id, text string) {
	t.Helper()
	ctx := context.Background()
	app, user, sid := h.adapter.AppName(), h.adapter.UserID(), h.adapter.SessionID()
	got, err := h.handle.Service.Get(ctx, &session.GetRequest{
		AppName: app, UserID: user, SessionID: sid,
	})
	if err != nil || got == nil || got.Session == nil {
		if _, cerr := h.handle.Service.Create(ctx, &session.CreateRequest{
			AppName: app, UserID: user, SessionID: sid,
		}); cerr != nil {
			t.Fatalf("session Create: %v", cerr)
		}
		got, err = h.handle.Service.Get(ctx, &session.GetRequest{
			AppName: app, UserID: user, SessionID: sid,
		})
		if err != nil {
			t.Fatalf("session Get: %v", err)
		}
	}
	ev := session.NewEvent(id)
	ev.Author = "cluster"
	ev.Branch = branch
	ev.LLMResponse = adkmodel.LLMResponse{
		Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: text}}},
	}
	if err := h.handle.Service.AppendEvent(ctx, got.Session, ev); err != nil {
		t.Fatalf("AppendEvent(%s): %v", id, err)
	}
}

// TestClientSubagentEvents_RoundTrip drives the turn-log read the
// remote TUI's drill-down sits on: a subagent DECLARED as "cluster"
// wrote its turns under the instance branch "bg.cluster-1", and the
// operator only knows the declared name.
func TestClientSubagentEvents_RoundTrip(t *testing.T) {
	t.Parallel()
	h := newRPCHarness(t, harnessConfig{})
	ctx := context.Background()
	path := h.sessionPath()

	h.appendBranchedEvent(t, "bg.cluster-1", "sa-1", "listing nodes")
	h.appendBranchedEvent(t, "bg.cluster-1", "sa-2", "3 nodes ready")

	resp, err := h.client.SubagentEvents(ctx, path, "cluster", 0, 0)
	if err != nil {
		t.Fatalf("SubagentEvents: %v", err)
	}
	if len(resp.Events) != 2 {
		t.Fatalf("got %d events, want 2 (%+v)", len(resp.Events), resp.Events)
	}
	if resp.NextSince == 0 {
		t.Error("NextSince = 0, want a resume cursor")
	}

	// The cursor is honored: resuming from it yields nothing new,
	// which is what makes the once-a-second tail cheap.
	next, err := h.client.SubagentEvents(ctx, path, "cluster", resp.NextSince, 0)
	if err != nil {
		t.Fatalf("SubagentEvents(since=%d): %v", resp.NextSince, err)
	}
	if len(next.Events) != 0 {
		t.Errorf("resume returned %d events, want 0", len(next.Events))
	}
}

// TestClientSubagentEvents_UnknownNameIsTyped is the #694 contract at
// the client boundary: a name that resolves to nothing must arrive as
// a *SubagentNotFoundError carrying the alternatives, not as a generic
// 404 — and must NOT classify as a permanent stream error, since a
// mistyped /subagents query has nothing to do with the session's
// health.
func TestClientSubagentEvents_UnknownNameIsTyped(t *testing.T) {
	t.Parallel()
	h := newRPCHarness(t, harnessConfig{})
	h.appendBranchedEvent(t, "bg.cluster-1", "sa-1", "listing nodes")

	_, err := h.client.SubagentEvents(context.Background(), h.sessionPath(), "clustr", 0, 0)
	if err == nil {
		t.Fatal("SubagentEvents(clustr) should error")
	}
	var nf *SubagentNotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("err = %v (%T), want *SubagentNotFoundError", err, err)
	}
	if nf.Name != "clustr" {
		t.Errorf("Name = %q, want clustr", nf.Name)
	}
	if len(nf.Available) == 0 || nf.Available[0] != "cluster" {
		t.Errorf("Available = %v, want [cluster]", nf.Available)
	}
	var se *httpStatusError
	if errors.As(err, &se) {
		t.Error("a not-found NAME must not surface as an httpStatusError — it would classify as a permanent stream error and tear down the attach stream")
	}
}

// TestClientSubagentEvents_RejectsPathShapedNames stops a name from
// reshaping the URL it becomes a segment of. ".." in particular walks
// the request off this session's subtree into a mux redirect, so it
// must not reach the wire at all.
func TestClientSubagentEvents_RejectsPathShapedNames(t *testing.T) {
	t.Parallel()
	h := newRPCHarness(t, harnessConfig{})
	for _, bad := range []string{"", "..", "a/b", "a b"} {
		_, err := h.client.SubagentEvents(context.Background(), h.sessionPath(), bad, 0, 0)
		if err == nil {
			t.Errorf("SubagentEvents(%q) = nil error, want a rejection", bad)
		}
	}
}

// TestClientSubagentEvents_MissingSessionStaysTransportError guards
// the other side of that line: a 404 for the SESSION is a transport
// condition, so it must keep its httpStatusError classification rather
// than being swallowed as "no such subagent".
func TestClientSubagentEvents_MissingSessionStaysTransportError(t *testing.T) {
	t.Parallel()
	h := newRPCHarness(t, harnessConfig{})

	_, err := h.client.SubagentEvents(context.Background(),
		"/sessions/"+h.adapter.AppName()+"/absent", "cluster", 0, 0)
	if err == nil {
		t.Fatal("SubagentEvents against a missing session should error")
	}
	var nf *SubagentNotFoundError
	if errors.As(err, &nf) {
		t.Fatalf("session 404 was misread as a subagent not-found: %v", err)
	}
	var se *httpStatusError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v (%T), want *httpStatusError", err, err)
	}
	if !se.PermanentStreamErr() {
		t.Error("a session 404 must still classify as permanent")
	}
}

// ---- do/doJSON error paths ------------------------------------------

func TestClientDoJSON_Non2xxMapsToStatusError(t *testing.T) {
	t.Parallel()
	h := newRPCHarness(t, harnessConfig{})

	_, err := h.client.Status(context.Background(), "/sessions/"+h.adapter.AppName()+"/absent")
	if err == nil {
		t.Fatal("Status against a missing session should error")
	}
	var se *httpStatusError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v (%T), want *httpStatusError", err, err)
	}
	if se.statusCode != 404 {
		t.Errorf("status = %d, want 404", se.statusCode)
	}
	if !strings.Contains(err.Error(), "status 404") {
		t.Errorf("error string %q should carry the \"status 404\" grep marker", err.Error())
	}
	if !se.PermanentStreamErr() {
		t.Error("404 must classify as permanent")
	}
}

func TestClientAuth_WrongTokenIs401(t *testing.T) {
	t.Parallel()
	h := newRPCHarness(t, harnessConfig{})

	parsed, err := ParseURL(h.base)
	if err != nil {
		t.Fatalf("ParseURL: %v", err)
	}
	badClient := New(parsed, "wrong-token", 5*time.Second)
	_, err = badClient.ListSessions(context.Background())
	var se *httpStatusError
	if !errors.As(err, &se) || se.statusCode != 401 {
		t.Fatalf("wrong token: err = %v, want *httpStatusError with 401", err)
	}
	if !se.PermanentStreamErr() {
		t.Error("401 must classify as permanent (revoked/invalid token doesn't heal by retrying)")
	}
}

func TestClientDo_ConnectionRefused(t *testing.T) {
	t.Parallel()

	// Reserve a port, then free it so nothing is listening.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	parsed, err := ParseURL("http://" + addr)
	if err != nil {
		t.Fatalf("ParseURL: %v", err)
	}
	c := New(parsed, "", 2*time.Second)

	_, err = c.ListSessions(context.Background())
	if err == nil {
		t.Fatal("ListSessions against a dead port should error")
	}
	var se *httpStatusError
	if errors.As(err, &se) {
		t.Errorf("connection errors must NOT be httpStatusError (they're retryable transport failures): %v", err)
	}

	// The stream path surfaces the same class of error synchronously.
	_, err = c.Stream(context.Background(), "/sessions/app/sid", 0)
	if err == nil {
		t.Fatal("Stream against a dead port should error synchronously")
	}
	if errors.As(err, &se) {
		t.Errorf("stream connection error must NOT be httpStatusError: %v", err)
	}
}
