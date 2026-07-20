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
	"strings"
	"testing"
	"time"

	"google.golang.org/adk/model"
	"google.golang.org/adk/session"
	"google.golang.org/genai"

	coretui "github.com/go-steer/core-tui/tui"

	"github.com/go-steer/core-agent/v2/internal/attachclient"
	"github.com/go-steer/core-agent/v2/pkg/attach"
)

// Compile-time: Adapter satisfies coretui.LiveAgent.
var _ coretui.LiveAgent = (*Adapter)(nil)

func TestAdapter_Events_StreamsAllNonEmptyEvents(t *testing.T) {
	t.Parallel()
	fs := startFakeServer(t)

	// Mix of an early "historical" event (old timestamp), a partial
	// model event, and a final model event. Unlike Run, Events
	// should yield ALL of them — observer mode wants history.
	old := time.Now().Add(-time.Hour)
	fs.streamFrames = []attach.Frame{
		{Seq: 1, Event: &session.Event{
			Author:      "user",
			Timestamp:   old,
			LLMResponse: model.LLMResponse{Content: &genai.Content{Parts: []*genai.Part{{Text: "earlier prompt"}}}, Partial: false},
		}},
		{Seq: 2, Event: &session.Event{
			Author:      "model",
			LLMResponse: model.LLMResponse{Content: &genai.Content{Parts: []*genai.Part{{Text: "streaming"}}}, Partial: true},
		}},
		{Seq: 3, Event: &session.Event{
			Author:      "model",
			LLMResponse: model.LLMResponse{Content: &genai.Content{Parts: []*genai.Part{{Text: " done"}}}, Partial: false},
		}},
	}

	parsed, _ := attachclient.ParseURL(fs.URL + "/sessions/s1")
	client := attachclient.New(parsed, "", 0)
	a := New(client, "/sessions/s1")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var events []coretui.Event
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev, err := range a.Events(ctx) {
			if err != nil {
				t.Errorf("yield err: %v", err)
				return
			}
			events = append(events, ev)
			if len(events) == 3 {
				cancel() // we've seen everything we expect; tear down
			}
		}
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		cancel()
		<-done
		t.Fatal("Events iterator did not return after frames + cancel")
	}

	if len(events) != 3 {
		t.Fatalf("got %d events, want 3: %+v", len(events), events)
	}
	// Crucially: the historical "earlier prompt" event is NOT filtered
	// out (replay filter is dropped for observer mode).
	if events[0].Text != "earlier prompt" {
		t.Errorf("event[0].Text = %q, want \"earlier prompt\" (history must be visible in observer mode)", events[0].Text)
	}
	if events[1].Text != "streaming" || !events[1].Partial {
		t.Errorf("event[1] = %+v", events[1])
	}
	if events[2].Text != " done" || events[2].Partial {
		t.Errorf("event[2] = %+v", events[2])
	}
}

func TestAdapter_Events_DoesNotStopOnTurnEnd(t *testing.T) {
	t.Parallel()
	fs := startFakeServer(t)

	// A model event with TurnComplete=true would stop Run, but
	// Events must keep ranging — observer mode is continuous.
	fs.streamFrames = []attach.Frame{
		{Seq: 1, Event: &session.Event{
			Author:      "model",
			LLMResponse: model.LLMResponse{Content: &genai.Content{Parts: []*genai.Part{{Text: "first answer"}}}, Partial: false, TurnComplete: true},
		}},
		{Seq: 2, Event: &session.Event{
			Author:      "model",
			LLMResponse: model.LLMResponse{Content: &genai.Content{Parts: []*genai.Part{{Text: "second answer"}}}, Partial: false},
		}},
	}

	parsed, _ := attachclient.ParseURL(fs.URL + "/sessions/s1")
	client := attachclient.New(parsed, "", 0)
	a := New(client, "/sessions/s1")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var events []coretui.Event
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev, err := range a.Events(ctx) {
			if err != nil {
				return
			}
			events = append(events, ev)
			if len(events) == 2 {
				cancel()
			}
		}
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		cancel()
		<-done
		t.Fatal("Events iterator did not return after frames + cancel")
	}

	if len(events) != 2 {
		t.Fatalf("got %d events, want 2 (Events must keep ranging across turn-end): %+v", len(events), events)
	}
}

// TestAdapter_Events_YieldsErrorOnStreamCloseAndReconnects exercises
// the auto-reconnect path: when the SSE stream closes mid-flight
// (daemon restart, network drop), Events yields a transient error
// frame (rendered as a RoleError row by core-tui) and keeps the
// iterator alive for the next attempt instead of returning.
func TestAdapter_Events_YieldsErrorOnStreamCloseAndReconnects(t *testing.T) {
	t.Parallel()
	fs := startFakeServer(t)

	// One frame then the fake server's handler returns — that's an
	// EOF on the SSE body, the adapter's frames channel closes.
	fs.streamFrames = []attach.Frame{
		{Seq: 1, Event: &session.Event{
			Author:      "model",
			LLMResponse: model.LLMResponse{Content: &genai.Content{Parts: []*genai.Part{{Text: "before disconnect"}}}, Partial: false},
		}},
	}
	fs.streamHoldOpen = false // close after yielding frames

	parsed, _ := attachclient.ParseURL(fs.URL + "/sessions/s1")
	client := attachclient.New(parsed, "", 0)
	a := New(client, "/sessions/s1")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	gotEvent := false
	gotError := false
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev, err := range a.Events(ctx) {
			if err != nil {
				// The reconnect-after-disconnect error frame.
				gotError = true
				cancel() // saw what we wanted; tear down
				return
			}
			if ev.Text == "before disconnect" {
				gotEvent = true
			}
		}
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		cancel()
		<-done
		t.Fatal("iterator did not yield the reconnect error within deadline")
	}

	if !gotEvent {
		t.Error("did not see the pre-disconnect event")
	}
	if !gotError {
		t.Error("did not see the post-disconnect transient error (reconnect path is missing)")
	}
}

// TestAdapter_Inject_SurfaceErrorViaEvents pins that a failed
// Inject (typically: daemon down) doesn't silently swallow the
// error — it shows up via the Events iterator as a transient
// error frame, the same way reconnect failures do. Without this
// path, an operator typing into a disconnected TUI saw their
// textarea accept the prompt with no indication anything failed.
func TestAdapter_Inject_SurfaceErrorViaEvents(t *testing.T) {
	t.Parallel()

	// Point the client at an address that will refuse connections
	// — simulates "daemon is down" without spinning a fake server.
	parsed, _ := attachclient.ParseURL("http://127.0.0.1:1") // port 1: nothing listens
	client := attachclient.New(parsed, "", 100*time.Millisecond)
	a := New(client, "/sessions/s1")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Drain Events in a goroutine; collect error frames.
	var injectErrSeen bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		for _, err := range a.Events(ctx) {
			if err == nil {
				continue
			}
			if strings.Contains(err.Error(), "inject failed") {
				injectErrSeen = true
				cancel()
				return
			}
		}
	}()

	// Wait for Events to enter its disconnect-backoff state.
	time.Sleep(200 * time.Millisecond)

	// Trigger an Inject — daemon-is-down → injects fails →
	// queued onto injectErrs → drained by Events into yield.
	if err := a.Inject("hello"); err == nil {
		t.Error("Inject against unreachable host: want error, got nil")
	}

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		cancel()
		<-done
		t.Fatal("Events iterator never yielded an inject-error frame")
	}

	if !injectErrSeen {
		t.Error("inject failure was not surfaced via Events iterator")
	}
}

func TestAdapter_Events_CtxCancelEndsIterator(t *testing.T) {
	t.Parallel()
	fs := startFakeServer(t)
	fs.streamFrames = []attach.Frame{}

	parsed, _ := attachclient.ParseURL(fs.URL + "/sessions/s1")
	client := attachclient.New(parsed, "", 0)
	a := New(client, "/sessions/s1")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range a.Events(ctx) {
			// drain
		}
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Events iterator did not return after ctx cancel")
	}
}
