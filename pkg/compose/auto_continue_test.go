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

package compose

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	adkmodel "google.golang.org/adk/model"
	"google.golang.org/adk/session"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/pkg/agent"
	"github.com/go-steer/core-agent/v2/pkg/auth"
	"github.com/go-steer/core-agent/v2/pkg/eventlog"
)

const acUser = "alice"
const acSID = "sid-ac"

// seedAC opens a temp eventlog and seeds the (core-agent, alice,
// sid-ac) session with the given events.
func seedAC(t *testing.T, events ...*session.Event) *eventlog.Handle {
	t.Helper()
	ctx := context.Background()
	h, err := eventlog.Open(ctx, sqlite.Open(filepath.Join(t.TempDir(), "s.db")))
	if err != nil {
		t.Fatalf("eventlog.Open: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })
	if _, err := h.Service.Create(ctx, &session.CreateRequest{AppName: "core-agent", UserID: acUser, SessionID: acSID}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	resp, err := h.Service.Get(ctx, &session.GetRequest{AppName: "core-agent", UserID: acUser, SessionID: acSID})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	for _, ev := range events {
		if err := h.Service.AppendEvent(ctx, resp.Session, ev); err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
	}
	return h
}

func acUserEvent(text string, ts time.Time) *session.Event {
	ev := session.NewEvent("inv-u")
	ev.Author = "user"
	ev.Timestamp = ts
	ev.LLMResponse = adkmodel.LLMResponse{
		Content: &genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{{Text: text}}},
	}
	return ev
}

// acModelEvent stamps ts explicitly: the session service orders
// events by timestamp, so fixtures must be monotonic like real
// histories (a "later" event with an older stamp would reorder).
func acModelEvent(text string, ts time.Time) *session.Event {
	ev := session.NewEvent("inv-m")
	ev.Author = "core_agent"
	ev.Timestamp = ts
	ev.LLMResponse = adkmodel.LLMResponse{
		Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: text}}},
	}
	return ev
}

// acAgent builds an agent bound to the seeded triple without starting
// a wake loop, so the test can observe the inbox deterministically.
func acAgent(t *testing.T, h *eventlog.Handle) *agent.Agent {
	t.Helper()
	ag, err := agent.New(stubLLM{}, agent.WithEventLog(h), agent.WithSession(acUser, acSID))
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	return ag
}

func acDeps(h *eventlog.Handle, freshness time.Duration) SessionFactoryDeps {
	return SessionFactoryDeps{
		DaemonCtx:             context.Background(),
		EventlogHandle:        h,
		AutoContinueEnabled:   true,
		AutoContinueFreshness: freshness,
	}
}

func TestMaybeAutoContinue_QueuesNoteForFreshInterruption(t *testing.T) {
	t.Parallel()
	h := seedAC(t,
		acModelEvent("earlier answer", time.Now().Add(-10*time.Minute)),
		acUserEvent("hello? anyone?", time.Now().Add(-5*time.Minute)),
	)
	ag := acAgent(t, h)
	maybeAutoContinue(acDeps(h, time.Hour), auth.Caller{Identity: acUser}, acSID, ag)

	msgs := ag.DrainInbox()
	if len(msgs) != 1 {
		t.Fatalf("inbox has %d messages, want 1 continuation note", len(msgs))
	}
	if !strings.Contains(msgs[0], "interrupted by a daemon restart") {
		t.Errorf("inbox message = %q, want the continuation system note", msgs[0])
	}
	// The run lock must be released again — a held lease would block
	// autonomous resume and the next auto-continue attempt.
	lock, err := h.AcquireLock(context.Background(), "core-agent", acUser, acSID)
	if err != nil {
		t.Fatalf("run lock still held after maybeAutoContinue: %v", err)
	}
	lock.Release()
}

func TestMaybeAutoContinue_SkipsCompletedAndStaleAndLocked(t *testing.T) {
	t.Parallel()
	t.Run("completed turn", func(t *testing.T) {
		t.Parallel()
		h := seedAC(t, acUserEvent("q", time.Now().Add(-2*time.Minute)), acModelEvent("done", time.Now().Add(-time.Minute)))
		ag := acAgent(t, h)
		maybeAutoContinue(acDeps(h, time.Hour), auth.Caller{Identity: acUser}, acSID, ag)
		if msgs := ag.DrainInbox(); len(msgs) != 0 {
			t.Errorf("inbox = %v, want empty for a completed tail", msgs)
		}
	})
	t.Run("stale interruption", func(t *testing.T) {
		t.Parallel()
		h := seedAC(t, acUserEvent("hello?", time.Now().Add(-3*time.Hour)))
		ag := acAgent(t, h)
		maybeAutoContinue(acDeps(h, time.Hour), auth.Caller{Identity: acUser}, acSID, ag)
		if msgs := ag.DrainInbox(); len(msgs) != 0 {
			t.Errorf("inbox = %v, want empty beyond the freshness window", msgs)
		}
	})
	t.Run("zero freshness disables the window", func(t *testing.T) {
		t.Parallel()
		h := seedAC(t, acUserEvent("hello?", time.Now().Add(-3*time.Hour)))
		ag := acAgent(t, h)
		maybeAutoContinue(acDeps(h, 0), auth.Caller{Identity: acUser}, acSID, ag)
		if msgs := ag.DrainInbox(); len(msgs) != 1 {
			t.Errorf("inbox = %v, want the note (freshness 0 = always continue)", msgs)
		}
	})
	t.Run("run lock held elsewhere", func(t *testing.T) {
		t.Parallel()
		h := seedAC(t, acUserEvent("hello?", time.Now()))
		lock, err := h.AcquireLock(context.Background(), "core-agent", acUser, acSID)
		if err != nil {
			t.Fatalf("AcquireLock: %v", err)
		}
		defer lock.Release()
		ag := acAgent(t, h)
		maybeAutoContinue(acDeps(h, time.Hour), auth.Caller{Identity: acUser}, acSID, ag)
		if msgs := ag.DrainInbox(); len(msgs) != 0 {
			t.Errorf("inbox = %v, want empty while another holder owns the session", msgs)
		}
	})
}
