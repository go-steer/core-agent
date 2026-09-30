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

package attachadapter

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"google.golang.org/adk/session"

	"github.com/go-steer/core-agent/v2/pkg/agent"
	"github.com/go-steer/core-agent/v2/pkg/attach"
	"github.com/go-steer/core-agent/v2/pkg/eventlog"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

// permModeFixture is an adapter over an agent with an eventlog-backed
// session and a gate in mode start.
func permModeFixture(t *testing.T, sid string, start permissions.Mode) (*Adapter, *permissions.Gate, func() []*session.Event) {
	t.Helper()
	h, err := eventlog.Open(context.Background(),
		sqlite.Open(filepath.Join(t.TempDir(), "session.db")))
	if err != nil {
		t.Fatalf("eventlog.Open: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })
	gate := permissions.New(permissions.Options{Mode: start})
	a := newEchoAgent(t, agent.WithEventLog(h), agent.WithSession("u", sid), agent.WithGate(gate))
	if _, err := h.Service.Create(context.Background(), &session.CreateRequest{
		AppName: a.AppName(), UserID: "u", SessionID: sid,
	}); err != nil {
		t.Fatalf("session Create: %v", err)
	}
	rows := func() []*session.Event {
		getResp, err := h.Service.Get(context.Background(), &session.GetRequest{
			AppName: a.AppName(), UserID: "u", SessionID: sid,
		})
		if err != nil {
			t.Fatalf("session Get: %v", err)
		}
		var out []*session.Event
		for ev := range getResp.Session.Events().All() {
			if ev.Author == attach.PermModeEventAuthor {
				out = append(out, ev)
			}
		}
		return out
	}
	return New(a), gate, rows
}

// The change reaches the session's gate and leaves one audit row naming
// who did it and the transition. Both operator surfaces — POST
// /perms/mode and the local TUI's Shift+Tab — land here.
func TestAttachSetPermMode_ChangesTheGateAndWritesOneRow(t *testing.T) {
	t.Parallel()
	ad, gate, rows := permModeFixture(t, "s-mode", permissions.ModeAsk)

	resp, err := ad.AttachSetPermMode(attach.PermModeRequest{Mode: "yolo", Caller: "alice@example.com"})
	if err != nil {
		t.Fatalf("AttachSetPermMode: %v", err)
	}
	if resp.Previous != "ask" || resp.Mode != "yolo" {
		t.Errorf("response = %+v, want ask → yolo", resp)
	}
	if gate.Mode() != permissions.ModeYolo {
		t.Errorf("gate mode = %q, want yolo", gate.Mode())
	}
	got := rows()
	if len(got) != 1 {
		t.Fatalf("audit rows = %d, want exactly 1", len(got))
	}
	meta := got[0].CustomMetadata
	if meta["from"] != "ask" || meta["to"] != "yolo" || meta["caller"] != "alice@example.com" || meta["source"] != "operator" {
		t.Errorf("audit row metadata = %v, want operator alice@example.com ask → yolo", meta)
	}
}

// The in-process TUI has no verified identity. Its row must carry no
// caller at all rather than a placeholder that reads as an attribution.
func TestAttachSetPermMode_NoCallerRecordsNoCaller(t *testing.T) {
	t.Parallel()
	ad, _, rows := permModeFixture(t, "s-mode-local", permissions.ModeAsk)
	if _, err := ad.AttachSetPermMode(attach.PermModeRequest{Mode: "plan"}); err != nil {
		t.Fatalf("AttachSetPermMode: %v", err)
	}
	got := rows()
	if len(got) != 1 {
		t.Fatalf("audit rows = %d, want 1", len(got))
	}
	if c, ok := got[0].CustomMetadata["caller"]; ok {
		t.Errorf("caller = %v, want the key absent for an unattributed change", c)
	}
}

// Asking for the mode the session is already in is a 200 that changes
// nothing, so it is not an event.
func TestAttachSetPermMode_SameModeWritesNothing(t *testing.T) {
	t.Parallel()
	ad, _, rows := permModeFixture(t, "s-mode-same", permissions.ModePlan)
	resp, err := ad.AttachSetPermMode(attach.PermModeRequest{Mode: "plan", Caller: "alice@example.com"})
	if err != nil {
		t.Fatalf("AttachSetPermMode: %v", err)
	}
	if resp.Previous != "plan" || resp.Mode != "plan" {
		t.Errorf("response = %+v, want plan → plan", resp)
	}
	if n := len(rows()); n != 0 {
		t.Errorf("audit rows = %d, want 0 for a no-op", n)
	}
}

// "allow" is config-only and anything else unknown is refused, before
// the gate is touched — not silently ignored, which is what
// Gate.SetMode does with an unknown value.
func TestAttachSetPermMode_RefusesModesOutsideTheChip(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"allow", "bypassPermissions", "", "YOLO"} {
		ad, gate, rows := permModeFixture(t, "s-mode-refuse", permissions.ModeAsk)
		_, err := ad.AttachSetPermMode(attach.PermModeRequest{Mode: mode})
		if !errors.Is(err, attach.ErrPermModeNotSettable) {
			t.Errorf("mode %q: err = %v, want ErrPermModeNotSettable", mode, err)
		}
		if gate.Mode() != permissions.ModeAsk {
			t.Errorf("mode %q: gate moved to %q", mode, gate.Mode())
		}
		if n := len(rows()); n != 0 {
			t.Errorf("mode %q: audit rows = %d, want 0", mode, n)
		}
	}
}

func TestAttachSetPermMode_NoGateIsNotRegistered(t *testing.T) {
	t.Parallel()
	if _, err := New(nil).AttachSetPermMode(attach.PermModeRequest{Mode: "ask"}); !errors.Is(err, attach.ErrCapabilityNotRegistered) {
		t.Errorf("nil agent: err = %v, want ErrCapabilityNotRegistered", err)
	}
}

// On a multi-session daemon each session's gate is derived from one
// template. Changing one session's mode must not move its neighbour's
// or the template's.
func TestAttachSetPermMode_ChangesOneSessionOnly(t *testing.T) {
	t.Parallel()
	template := permissions.New(permissions.Options{Mode: permissions.ModeAsk})
	gateA := template.DeriveForSession("a", nil)
	gateB := template.DeriveForSession("b", nil)
	adA := New(newEchoAgent(t, agent.WithSession("u", "a"), agent.WithGate(gateA)))

	if _, err := adA.AttachSetPermMode(attach.PermModeRequest{Mode: "yolo"}); err != nil {
		t.Fatalf("AttachSetPermMode: %v", err)
	}
	if gateA.Mode() != permissions.ModeYolo {
		t.Errorf("session a = %q, want yolo", gateA.Mode())
	}
	if gateB.Mode() != permissions.ModeAsk || template.Mode() != permissions.ModeAsk {
		t.Errorf("session b = %q, template = %q; both must stay ask", gateB.Mode(), template.Mode())
	}
}
