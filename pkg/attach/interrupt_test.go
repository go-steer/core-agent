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
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"google.golang.org/adk/session"

	"github.com/go-steer/core-agent/v2/pkg/eventlog"
)

// interruptibleRegistrant extends eventfulRegistrant with an
// InterruptProvider implementation backed by an atomic counter.
// canInterrupt models the agent's "is there a turn in flight"
// state; the test toggles it to simulate idle vs. running.
type interruptibleRegistrant struct {
	eventfulRegistrant
	canInterrupt atomic.Bool
	interrupts   atomic.Int32
}

func (i *interruptibleRegistrant) AttachInterrupt() bool {
	if !i.canInterrupt.Load() {
		return false
	}
	i.canInterrupt.Store(false)
	i.interrupts.Add(1)
	return true
}

func TestIntegration_InterruptEndpoint_CancelsInFlight(t *testing.T) {
	t.Parallel()
	h, cleanupLog := openTestEventLog(t)
	defer cleanupLog()

	reg := NewSessionRegistry()
	ag := &interruptibleRegistrant{
		eventfulRegistrant: eventfulRegistrant{
			stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
			handle:         h,
		},
	}
	ag.canInterrupt.Store(true)
	if _, err := reg.Register(ag); err != nil {
		t.Fatal(err)
	}

	base, cleanupSrv := startTestServer(t, reg)
	defer cleanupSrv()

	// Pre-create the session so the audit-row write has a session
	// to append into. In production this happens via the runner.
	if _, err := h.Service.Create(context.Background(), &session.CreateRequest{
		AppName: "core-agent", UserID: "u", SessionID: "s1",
	}); err != nil {
		t.Fatalf("session Create: %v", err)
	}

	resp, err := http.Post(base+"/sessions/core-agent/s1/interrupt", "application/json", nil)
	if err != nil {
		t.Fatalf("POST interrupt: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}

	var got struct {
		Interrupted bool   `json:"interrupted"`
		Session     string `json:"session"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Interrupted {
		t.Errorf("interrupted = false, want true")
	}
	if got.Session != "s1" {
		t.Errorf("session = %q, want s1", got.Session)
	}
	if hdr := resp.Header.Get("X-Interrupted"); hdr != "" {
		t.Errorf("X-Interrupted header set on cancel-fired response: %q", hdr)
	}
	if ag.interrupts.Load() != 1 {
		t.Errorf("AttachInterrupt called %d times, want 1", ag.interrupts.Load())
	}

	// Audit row should be present in the event log.
	assertAuditRowPresent(t, h, "core-agent", "u", "s1")
}

func TestIntegration_InterruptEndpoint_NothingInFlight(t *testing.T) {
	t.Parallel()
	h, cleanupLog := openTestEventLog(t)
	defer cleanupLog()

	reg := NewSessionRegistry()
	ag := &interruptibleRegistrant{
		eventfulRegistrant: eventfulRegistrant{
			stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
			handle:         h,
		},
	}
	// canInterrupt is false (default zero value) → AttachInterrupt
	// returns false → endpoint responds with interrupted:false +
	// the X-Interrupted header.
	if _, err := reg.Register(ag); err != nil {
		t.Fatal(err)
	}

	base, cleanupSrv := startTestServer(t, reg)
	defer cleanupSrv()

	resp, err := http.Post(base+"/sessions/core-agent/s1/interrupt", "application/json", nil)
	if err != nil {
		t.Fatalf("POST interrupt: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	if hdr := resp.Header.Get("X-Interrupted"); hdr != "nothing-in-flight" {
		t.Errorf("X-Interrupted = %q, want %q", hdr, "nothing-in-flight")
	}
	var got struct {
		Interrupted bool `json:"interrupted"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Interrupted {
		t.Errorf("interrupted = true, want false (agent was idle)")
	}
}

func TestIntegration_InterruptEndpoint_ProviderMissing_412(t *testing.T) {
	t.Parallel()
	// stubRegistrant does NOT implement InterruptProvider — endpoint
	// returns 412 so the operator knows their intent didn't take
	// effect (instead of silently no-op'ing).
	reg := NewSessionRegistry()
	ag := &stubRegistrant{app: "core-agent", user: "u", sid: "s1"}
	if _, err := reg.Register(ag); err != nil {
		t.Fatal(err)
	}
	base, cleanupSrv := startTestServer(t, reg)
	defer cleanupSrv()

	resp, err := http.Post(base+"/sessions/core-agent/s1/interrupt", "application/json", nil)
	if err != nil {
		t.Fatalf("POST interrupt: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Errorf("status %d, want 412", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "InterruptProvider") {
		t.Errorf("body should explain missing provider; got %q", body)
	}
}

func TestIntegration_InterruptEndpoint_ShortcutForm(t *testing.T) {
	t.Parallel()
	h, cleanupLog := openTestEventLog(t)
	defer cleanupLog()

	reg := NewSessionRegistry()
	ag := &interruptibleRegistrant{
		eventfulRegistrant: eventfulRegistrant{
			stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
			handle:         h,
		},
	}
	ag.canInterrupt.Store(true)
	if _, err := reg.Register(ag); err != nil {
		t.Fatal(err)
	}
	base, cleanupSrv := startTestServer(t, reg)
	defer cleanupSrv()
	if _, err := h.Service.Create(context.Background(), &session.CreateRequest{
		AppName: "core-agent", UserID: "u", SessionID: "s1",
	}); err != nil {
		t.Fatalf("session Create: %v", err)
	}

	// Single-segment /sessions/<sid>/interrupt form.
	resp, err := http.Post(base+"/sessions/s1/interrupt", "application/json", nil)
	if err != nil {
		t.Fatalf("POST shortcut interrupt: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if ag.interrupts.Load() != 1 {
		t.Errorf("AttachInterrupt count = %d, want 1", ag.interrupts.Load())
	}
}

// assertAuditRowPresent walks the event log for the named session
// and fails the test if no row with Author="attach/interrupt" is
// found. Uses a short timeout because writes are synchronous through
// session.Service.AppendEvent.
func assertAuditRowPresent(t *testing.T, h *eventlog.Handle, app, user, sid string) {
	t.Helper()
	getResp, err := h.Service.Get(context.Background(), &session.GetRequest{
		AppName: app, UserID: user, SessionID: sid,
	})
	if err != nil {
		t.Fatalf("session Get: %v", err)
	}
	for ev := range getResp.Session.Events().All() {
		if ev.Author == "attach/interrupt" {
			meta := ev.CustomMetadata
			if meta == nil || meta["source"] != "operator" {
				t.Errorf("audit row found but CustomMetadata.source = %v, want \"operator\"", meta)
			}
			return
		}
	}
	t.Errorf("no attach/interrupt audit row in session events")
}
