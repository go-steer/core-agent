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
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	coretui "github.com/go-steer/core-tui/tui"

	"github.com/go-steer/core-agent/v2/internal/attachclient"
	"github.com/go-steer/core-agent/v2/pkg/attach"
)

// newPauseAdapter builds an Adapter pointed at a test server, matching
// the shape the interrupt tests use.
func newPauseAdapter(t *testing.T, srv *httptest.Server) *Adapter {
	t.Helper()
	parsed, err := attachclient.ParseURL(srv.URL + "/sessions/s1")
	if err != nil {
		t.Fatal(err)
	}
	return New(attachclient.New(parsed, "", 0), "/sessions/s1")
}

// The wire hop: coretui's /pause → Pauser.Pause → POST
// /sessions/<sid>/pause. Same class of defect as #803's silently-dead
// capability — without the route being exercised, /pause reports
// success against a daemon that never heard about it.
func TestAdapter_Pause_ForwardsToRemoteEndpoint(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	var gotReason atomic.Value // string

	mux := http.NewServeMux()
	mux.HandleFunc("POST /sessions/{sid}/pause", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var req attach.PauseRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotReason.Store(req.Reason)
		_ = json.NewEncoder(w).Encode(attach.PauseResponse{
			Session: "s1", Paused: true, Transitioned: true,
			State: attach.AgentStatePaused, Reason: req.Reason,
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	a := newPauseAdapter(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := a.Pause(ctx, "maintenance"); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("pause calls = %d, want 1", got)
	}
	if got, _ := gotReason.Load().(string); got != "maintenance" {
		t.Errorf("reason on the wire = %q, want %q", got, "maintenance")
	}

	// The ack must land in the cache immediately. Waiting for the pause
	// frame to come back around the stream would leave PauseState
	// answering "not paused" for the intervening tick, flickering the
	// banner off under the operator right after they asked for it.
	if st := a.PauseState(); !st.Paused {
		t.Error("PauseState().Paused = false right after a successful Pause, want true")
	}
	if st := a.PauseState(); st.Reason != "maintenance" {
		t.Errorf("PauseState().Reason = %q, want %q", st.Reason, "maintenance")
	}
	// /pause is the disposition that explicitly kills nothing, so the
	// banner must not claim work was interrupted.
	if st := a.PauseState(); st.Interrupted {
		t.Error("PauseState().Interrupted = true after a plain /pause, want false")
	}
}

// A failed Pause must not move the cache. Reporting the gate as closed
// when the daemon never closed it is worse than the error: the operator
// sees a paused banner and believes the agent is held.
func TestAdapter_Pause_FailureLeavesStateUntouched(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /sessions/{sid}/pause", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not implemented", http.StatusNotImplemented)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	a := newPauseAdapter(t, srv)
	if err := a.Pause(context.Background(), "nope"); err == nil {
		t.Fatal("expected error propagation on 501, got nil")
	}
	if st := a.PauseState(); st.Paused {
		t.Error("PauseState().Paused = true after a failed Pause, want false")
	}
}

// Resume's disposition has to reach the wire intact: /abandon and
// /continue differ only by this string, and a dropped steer is the
// failure that makes the whole hold pointless.
func TestAdapter_Resume_ForwardsModeAndSteer(t *testing.T) {
	t.Parallel()

	var got atomic.Value // attach.ResumeRequest

	mux := http.NewServeMux()
	mux.HandleFunc("POST /sessions/{sid}/resume", func(w http.ResponseWriter, r *http.Request) {
		var req attach.ResumeRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		got.Store(req)
		_ = json.NewEncoder(w).Encode(attach.ResumeResponse{
			Session: "s1", Resumed: true, Mode: req.Mode, State: attach.AgentStateIdle,
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	a := newPauseAdapter(t, srv)
	// Seed a paused gate so the clear-on-resume assertion is meaningful
	// rather than passing against an already-zero cache.
	a.applyPauseInfo(coretui.PauseInfo{Paused: true, Reason: "held"})

	err := a.Resume(context.Background(), coretui.ResumeRequest{
		Mode:  coretui.ResumeModeSteer,
		Steer: "check the pods instead",
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}

	req, _ := got.Load().(attach.ResumeRequest)
	if req.Mode != attach.ResumeModeSteer {
		t.Errorf("Mode on the wire = %q, want %q", req.Mode, attach.ResumeModeSteer)
	}
	if req.Steer != "check the pods instead" {
		t.Errorf("Steer on the wire = %q, want the operator's text", req.Steer)
	}
	if st := a.PauseState(); st.Paused {
		t.Error("PauseState().Paused = true after a successful Resume, want false")
	}
}

// core-tui and pkg/attach were built to the same wire vocabulary, so
// the mode strings must be equal, not merely convertible. If either
// side renamed one, the mapping would compile and silently send a mode
// the server rejects.
func TestResumeModes_MatchTheWireVocabulary(t *testing.T) {
	t.Parallel()
	pairs := []struct {
		name            string
		coretui, onWire string
	}{
		{"steer", coretui.ResumeModeSteer, attach.ResumeModeSteer},
		{"continue", coretui.ResumeModeContinue, attach.ResumeModeContinue},
		{"abandon", coretui.ResumeModeAbandon, attach.ResumeModeAbandon},
	}
	for _, p := range pairs {
		if p.coretui != p.onWire {
			t.Errorf("%s: core-tui %q != attach %q", p.name, p.coretui, p.onWire)
		}
	}
	if coretui.PauseStatePaused != attach.PauseStatePaused {
		t.Errorf("paused state: core-tui %q != attach %q",
			coretui.PauseStatePaused, attach.PauseStatePaused)
	}
	if coretui.PauseStateResumed != attach.PauseStateResumed {
		t.Errorf("resumed state: core-tui %q != attach %q",
			coretui.PauseStateResumed, attach.PauseStateResumed)
	}
}

// A pause frame off the stream has to do both jobs — project into the
// transcript AND update the polled cache. core-tui's Pauser docs are
// explicit that a host implementing the push still needs the poll,
// and dropping either half is invisible until an operator is looking
// at a banner that does not match the session.
func TestConsumeTypedFrame_PauseUpdatesCacheAndProjects(t *testing.T) {
	t.Parallel()

	a := &Adapter{}
	at := time.Now().UTC().Truncate(time.Second)
	ev, ok := a.consumeTypedFrame(attach.Frame{
		Type: attach.EventPause,
		TypedData: &attach.PauseEvent{
			State:       attach.PauseStatePaused,
			Reason:      "operator hold",
			Interrupted: true,
			At:          at,
		},
	})

	if !ok {
		t.Fatal("consumeTypedFrame dropped a pause frame; core-tui renders the transition from it")
	}
	if ev.Pause == nil {
		t.Fatal("Event.Pause is nil, want the projected frame")
	}
	if ev.Pause.State != coretui.PauseStatePaused || !ev.Pause.Interrupted {
		t.Errorf("projected event = %+v, want paused+interrupted", *ev.Pause)
	}
	if ev.Pause.Reason != "operator hold" {
		t.Errorf("projected reason = %q, want %q", ev.Pause.Reason, "operator hold")
	}
	if !ev.Pause.At.Equal(at) {
		t.Errorf("projected At = %v, want %v", ev.Pause.At, at)
	}

	st := a.PauseState()
	if !st.Paused || !st.Interrupted {
		t.Errorf("PauseState() = %+v, want paused+interrupted", st)
	}
	if !st.Since.Equal(at) {
		t.Errorf("PauseState().Since = %v, want the frame's At %v", st.Since, at)
	}

	// ...and the resumed frame clears it.
	if _, ok := a.consumeTypedFrame(attach.Frame{
		Type:      attach.EventPause,
		TypedData: &attach.PauseEvent{State: attach.PauseStateResumed, Mode: attach.ResumeModeAbandon},
	}); !ok {
		t.Fatal("consumeTypedFrame dropped the resumed frame")
	}
	if st := a.PauseState(); st.Paused {
		t.Error("PauseState().Paused = true after a resumed frame, want false")
	}
}

// Spec §2.8 says tolerate unknown states. Folding one into "resumed"
// by falling through a default would silently reopen the banner on a
// gate the host still considers closed.
func TestApplyPauseEvent_UnknownStateChangesNothing(t *testing.T) {
	t.Parallel()

	a := &Adapter{}
	a.applyPauseInfo(coretui.PauseInfo{Paused: true, Reason: "held"})
	a.applyPauseEvent(&attach.PauseEvent{State: "quiesced"})

	st := a.PauseState()
	if !st.Paused || st.Reason != "held" {
		t.Errorf("PauseState() = %+v, want the prior held state untouched", st)
	}
}

// Attaching to an already-paused session must render the banner. The
// transition happened before this client connected, and the adapter
// drops replayed frames older than connectedAt, so the stream can
// never supply it — only the /status seed can.
func TestAdapter_SeedsPauseFromStatusOnAttach(t *testing.T) {
	t.Parallel()

	since := time.Now().UTC().Add(-5 * time.Minute).Truncate(time.Second)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /sessions/{sid}/status", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(attach.StatusInfo{
			State:       attach.AgentStatePaused,
			ModelName:   "gemini-3.5-flash",
			PausedSince: since,
			PauseReason: "waiting on the operator",
			Interrupted: true,
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	a := newPauseAdapter(t, srv)
	if st := a.PauseState(); st.Paused {
		t.Fatal("PauseState() reports paused before any status read; the seed is what should set it")
	}

	// Status() is what core-tui calls on its snapshot tick; the seed
	// rides along with it rather than adding a second poll.
	if got := a.Status(); got.State != attach.AgentStatePaused {
		t.Fatalf("Status().State = %q, want %q", got.State, attach.AgentStatePaused)
	}

	st := a.PauseState()
	if !st.Paused {
		t.Fatal("PauseState().Paused = false after a paused /status, want true")
	}
	if !st.Since.Equal(since) {
		t.Errorf("Since = %v, want %v", st.Since, since)
	}
	if st.Reason != "waiting on the operator" {
		t.Errorf("Reason = %q, want the status reason", st.Reason)
	}
	if !st.Interrupted {
		t.Error("Interrupted = false, want true — /status carries the bit and it is the first thing an operator reads")
	}
}

// The seed fires once. After that the stream owns the gate, and a 1 Hz
// status refresh folding in on every tick would race it: a refresh
// already in flight when a resume lands returns the pre-resume answer
// and would flip the banner back on.
func TestAdapter_StatusSeedDoesNotClobberALaterResume(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /sessions/{sid}/status", func(w http.ResponseWriter, r *http.Request) {
		// The daemon still says "paused" — a stale read.
		_ = json.NewEncoder(w).Encode(attach.StatusInfo{
			State: attach.AgentStatePaused, ModelName: "m", PauseReason: "held",
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	a := newPauseAdapter(t, srv)
	_ = a.Status() // seeds paused
	if st := a.PauseState(); !st.Paused {
		t.Fatal("setup: expected the first status read to seed a paused gate")
	}

	// A resumed frame arrives on the stream.
	a.applyPauseEvent(&attach.PauseEvent{State: attach.PauseStateResumed})
	if st := a.PauseState(); st.Paused {
		t.Fatal("setup: expected the resumed frame to clear the gate")
	}

	// Force another status round-trip past the cache TTL. It still
	// reports paused, and must be ignored.
	a.status.mu.Lock()
	a.status.lastFetch = time.Time{}
	a.status.mu.Unlock()
	_ = a.Status()

	if st := a.PauseState(); st.Paused {
		t.Error("a later /status read re-paused the gate after a resumed frame; the seed must fire once only")
	}
}
