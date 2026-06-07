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
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// sseFrame is one parsed Server-Sent Event frame.
type sseFrame struct {
	Event string // event-type name (e.g. "capabilities", "agent")
	Data  string // raw data block (JSON in our case)
}

// readSSEFrames consumes frames from r until ctx is done or the
// stream closes, returning each as a typed sseFrame. Handles the
// SSE wire format: `event:` line, `data:` line, blank-line
// terminator. Comment lines (`:` prefix) and unrelated lines are
// tolerated per spec.
//
// Cancellation safety: returns whatever frames have been fully
// parsed so far. Partial in-flight frames are dropped on ctx done.
func readSSEFrames(t *testing.T, r io.Reader) <-chan sseFrame {
	t.Helper()
	out := make(chan sseFrame, 16)
	go func() {
		defer close(out)
		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		var current sseFrame
		for scanner.Scan() {
			line := scanner.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				current.Event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				current.Data = strings.TrimPrefix(line, "data: ")
			case line == "":
				// End of frame — emit if we have anything useful.
				if current.Event != "" || current.Data != "" {
					out <- current
					current = sseFrame{}
				}
			}
		}
	}()
	return out
}

// awaitFrame pulls frames from ch until one matches predicate or
// the deadline passes. Returns the matched frame plus all the
// frames seen before it (in arrival order). Useful in tests that
// want to assert on both "what was first" and "did the awaited
// event eventually arrive".
func awaitFrame(t *testing.T, ch <-chan sseFrame, deadline time.Duration, match func(sseFrame) bool) (sseFrame, []sseFrame) {
	t.Helper()
	var prior []sseFrame
	timer := time.NewTimer(deadline)
	defer timer.Stop()
	for {
		select {
		case f, ok := <-ch:
			if !ok {
				return sseFrame{}, prior
			}
			if match(f) {
				return f, prior
			}
			prior = append(prior, f)
		case <-timer.C:
			return sseFrame{}, prior
		}
	}
}

// emitTargetStub is a Registrant that also implements EmitTarget so
// the broadcaster wires its Emit method as the agent-side callback.
// Captures the wired emitter so tests can drive Emit() externally
// without spinning up a full agent.
type emitTargetStub struct {
	eventfulRegistrant
	mu      sync.Mutex
	wiredFn func(string, any)
}

func (e *emitTargetStub) SetAttachEmitter(f func(string, any)) {
	e.mu.Lock()
	e.wiredFn = f
	e.mu.Unlock()
}

func (e *emitTargetStub) fire(eventType string, payload any) {
	e.mu.Lock()
	fn := e.wiredFn
	e.mu.Unlock()
	if fn != nil {
		fn(eventType, payload)
	}
}

// TestEvents_BootFrameOrder is the load-bearing assertion of the
// protocol contract: on every newly-opened stream the server MUST
// emit `capabilities` first, then `status-update`, optionally
// followed by `usage-update`. The test asserts both the ordering
// and the payload field names so a wire-format regression fails
// here visibly.
func TestEvents_BootFrameOrder(t *testing.T) {
	t.Parallel()
	h, cleanupLog := openTestEventLog(t)
	defer cleanupLog()

	reg := NewSessionRegistry()
	ag := &emitTargetStub{
		eventfulRegistrant: eventfulRegistrant{
			stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "boot"},
			handle:         h,
		},
	}
	if _, err := reg.Register(ag); err != nil {
		t.Fatal(err)
	}

	base, cleanupSrv := startTestServer(t, reg)
	defer cleanupSrv()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		base+"/sessions/core-agent/boot/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("subscribe status %d: %s", resp.StatusCode, body)
	}

	frames := readSSEFrames(t, resp.Body)

	// First frame: capabilities with the right shape.
	first := mustReadFrame(t, frames, time.Second, "first boot frame")
	if first.Event != EventCapabilities {
		t.Fatalf("first frame event = %q, want %q", first.Event, EventCapabilities)
	}
	var caps Capabilities
	if err := json.Unmarshal([]byte(first.Data), &caps); err != nil {
		t.Fatalf("capabilities JSON: %v (data=%s)", err, first.Data)
	}
	if caps.ProtocolVersion != ProtocolVersion {
		t.Errorf("capabilities.protocol_version = %q, want %q", caps.ProtocolVersion, ProtocolVersion)
	}
	if len(caps.EventTypes) == 0 {
		t.Errorf("capabilities.event_types should be non-empty")
	}
	for _, required := range []string{EventStatusUpdate, EventUsageUpdate, EventInbox, EventTurnComplete, EventTurnError} {
		if !contains(caps.EventTypes, required) {
			t.Errorf("capabilities.event_types missing %q (got %v)", required, caps.EventTypes)
		}
	}

	// Second frame: status-update with turn_state.
	second := mustReadFrame(t, frames, time.Second, "second boot frame")
	if second.Event != EventStatusUpdate {
		t.Fatalf("second frame event = %q, want %q", second.Event, EventStatusUpdate)
	}
	var status StatusUpdate
	if err := json.Unmarshal([]byte(second.Data), &status); err != nil {
		t.Fatalf("status-update JSON: %v (data=%s)", err, second.Data)
	}
	if status.TurnState == "" {
		t.Errorf("status-update.turn_state should always be present per spec; got empty")
	}
}

// TestEvents_EmitDispatch verifies that broadcaster.Emit pushes a
// typed event onto every connected subscriber's stream with the
// correct event name and payload field shape. Covers the path
// agents will take (via SetAttachEmitter) without depending on a
// full agent.
func TestEvents_EmitDispatch(t *testing.T) {
	t.Parallel()
	h, cleanupLog := openTestEventLog(t)
	defer cleanupLog()

	reg := NewSessionRegistry()
	ag := &emitTargetStub{
		eventfulRegistrant: eventfulRegistrant{
			stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "emit"},
			handle:         h,
		},
	}
	if _, err := reg.Register(ag); err != nil {
		t.Fatal(err)
	}
	base, cleanupSrv := startTestServer(t, reg)
	defer cleanupSrv()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		base+"/sessions/core-agent/emit/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer resp.Body.Close()

	frames := readSSEFrames(t, resp.Body)

	// Drain the boot frames first so we don't conflate them with the
	// event we're about to emit.
	_ = mustReadFrame(t, frames, time.Second, "boot:capabilities")
	_ = mustReadFrame(t, frames, time.Second, "boot:status-update")

	// Trigger an emit via the wired-from-Subscribe callback.
	expected := InboxEvent{
		State:    InboxStateQueued,
		PromptID: "p-test-001",
		QueuedAt: time.Date(2026, 6, 7, 9, 42, 11, 0, time.UTC),
	}
	ag.fire(EventInbox, expected)

	got, prior := awaitFrame(t, frames, 2*time.Second, func(f sseFrame) bool {
		return f.Event == EventInbox
	})
	if got.Event == "" {
		t.Fatalf("never received inbox event; saw %d prior frames: %v", len(prior), prior)
	}

	var actual InboxEvent
	if err := json.Unmarshal([]byte(got.Data), &actual); err != nil {
		t.Fatalf("inbox event JSON: %v (data=%s)", err, got.Data)
	}
	if actual.State != expected.State || actual.PromptID != expected.PromptID {
		t.Errorf("inbox payload mismatch:\n got  %+v\n want %+v", actual, expected)
	}
}

// TestEvents_LegacyAgentStillFlows is the back-compat contract:
// pre-protocol clients that consume only the `event: agent` frames
// (with the legacy Frame JSON shape) must keep working unchanged.
// This test asserts that legacy frames still arrive after the new
// boot frames + typed events.
func TestEvents_LegacyAgentStillFlows(t *testing.T) {
	t.Parallel()
	h, cleanupLog := openTestEventLog(t)
	defer cleanupLog()

	reg := NewSessionRegistry()
	ag := &eventfulRegistrant{
		stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "legacy"},
		handle:         h,
	}
	if _, err := reg.Register(ag); err != nil {
		t.Fatal(err)
	}
	base, cleanupSrv := startTestServer(t, reg)
	defer cleanupSrv()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		base+"/sessions/core-agent/legacy/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer resp.Body.Close()

	// Append an event after subscribe so it flows through live tail.
	go func() {
		time.Sleep(100 * time.Millisecond)
		appendTestEvent(t, h, "core-agent", "u", "legacy", "back-compat-payload")
	}()

	frames := readSSEFrames(t, resp.Body)

	// Find an `event: agent` frame whose data contains our payload.
	got, prior := awaitFrame(t, frames, 3*time.Second, func(f sseFrame) bool {
		return f.Event == EventAgent && strings.Contains(f.Data, "back-compat-payload")
	})
	if got.Event == "" {
		t.Fatalf("legacy event: agent frame never arrived; saw %d prior: %v", len(prior), prior)
	}

	// Ensure the legacy data block is still the Frame JSON shape
	// (with seq + event fields), not a typed event payload.
	if !strings.Contains(got.Data, `"seq"`) || !strings.Contains(got.Data, `"event"`) {
		t.Errorf("legacy frame should carry Frame{Seq, Event} JSON; got %s", got.Data)
	}
}

// mustReadFrame blocks until one frame arrives on ch or the deadline
// passes; fails the test with a clear label on timeout.
func mustReadFrame(t *testing.T, ch <-chan sseFrame, deadline time.Duration, label string) sseFrame {
	t.Helper()
	select {
	case f, ok := <-ch:
		if !ok {
			t.Fatalf("channel closed before %s arrived", label)
		}
		return f
	case <-time.After(deadline):
		t.Fatalf("timeout waiting for %s", label)
		return sseFrame{}
	}
}

// contains is a small helper for asserting slice membership in tests.
func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
