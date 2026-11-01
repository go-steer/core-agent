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

// Originally derived from go-steer/mast@d239f89 (internal/cli/autoresume_test.go).

package compose

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	adksession "google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v3/internal/effects"
	"github.com/go-steer/core-agent/v3/pkg/eventlog"
)

func arPred() effects.ReadOnlyPredicate {
	return effects.NewPredicate(map[string]bool{
		"read_tool":  false,
		"write_tool": true,
	})
}

func arUserText(text string, ts time.Time) *adksession.Event {
	ev := adksession.NewEvent(context.Background(), "inv-user")
	ev.Author = "user"
	ev.Timestamp = ts
	ev.Content = genai.NewContentFromText(text, genai.RoleUser)
	return ev
}

func arModelText(text string, ts time.Time) *adksession.Event {
	ev := adksession.NewEvent(context.Background(), "inv-model")
	ev.Author = "core_agent"
	ev.Timestamp = ts
	ev.Content = genai.NewContentFromText(text, genai.RoleModel)
	return ev
}

func arSubAgentText(author, text string, ts time.Time) *adksession.Event {
	ev := adksession.NewEvent(context.Background(), "inv-sub")
	ev.Author = author
	ev.Timestamp = ts
	ev.Content = genai.NewContentFromText(text, genai.RoleModel)
	return ev
}

func arModelCall(id, tool string, ts time.Time) *adksession.Event {
	ev := adksession.NewEvent(context.Background(), "inv-"+id)
	ev.Author = "core_agent"
	ev.Timestamp = ts
	ev.Content = &genai.Content{
		Role:  genai.RoleModel,
		Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{ID: id, Name: tool, Args: map[string]any{}}}},
	}
	return ev
}

func arParkedCall(id, tool string, ts time.Time) *adksession.Event {
	ev := arModelCall(id, tool, ts)
	ev.LongRunningToolIDs = []string{id}
	return ev
}

func seedARSession(t *testing.T, h *eventlog.Handle, userID, sid string, events ...*adksession.Event) {
	t.Helper()
	ctx := context.Background()
	if _, err := h.Service.Create(ctx, &adksession.CreateRequest{
		AppName: "core-agent", UserID: userID, SessionID: sid,
	}); err != nil {
		t.Fatalf("Create(%q): %v", sid, err)
	}
	for i, ev := range events {
		resp, err := h.Service.Get(ctx, &adksession.GetRequest{
			AppName: "core-agent", UserID: userID, SessionID: sid,
		})
		if err != nil {
			t.Fatalf("Get(%q) for event %d: %v", sid, i, err)
		}
		if err := h.Service.AppendEvent(ctx, resp.Session, ev); err != nil {
			t.Fatalf("AppendEvent(%q, %d): %v", sid, i, err)
		}
	}
}

func TestAutoResumeRepairsDanglingReadOnly(t *testing.T) {
	t.Parallel()
	h := openSchedLeaseHandle(t)
	now := time.Now().UTC()
	seedARSession(t, h, "alice", "s-repair",
		arUserText("hi", now.Add(-2*time.Minute)),
		arModelCall("call-1", "read_tool", now.Add(-time.Minute)),
	)

	var resumed []string
	var gotRepair *genai.Content
	ar := NewAutoResumer(AutoResumeConfig{
		Handle:          h,
		AppName:         "core-agent",
		IsReadOnly:      arPred(),
		FreshnessWindow: time.Hour,
		Resume: func(_ context.Context, _, sessionID string, repair *genai.Content) error {
			resumed = append(resumed, sessionID)
			gotRepair = repair
			return nil
		},
	})
	if n := ar.Run(context.Background()); n != 1 {
		t.Fatalf("Run() = %d, want 1", n)
	}
	if len(resumed) != 1 || resumed[0] != "s-repair" {
		t.Fatalf("resumed = %v, want [s-repair]", resumed)
	}
	if gotRepair == nil || len(gotRepair.Parts) != 1 || gotRepair.Parts[0].FunctionResponse == nil {
		t.Fatalf("gotRepair = %+v, want 1 FunctionResponse part", gotRepair)
	}
	if gotRepair.Parts[0].FunctionResponse.ID != "call-1" || gotRepair.Parts[0].FunctionResponse.Name != "read_tool" {
		t.Errorf("FunctionResponse = %+v, want ID=call-1 Name=read_tool", gotRepair.Parts[0].FunctionResponse)
	}
}

func TestAutoResumeBlocksOnAmbiguousMutatingAndUnblocksAfterAckEffects(t *testing.T) {
	t.Parallel()
	h := openSchedLeaseHandle(t)
	ctx := context.Background()
	now := time.Now().UTC()
	seedARSession(t, h, "alice", "s-mut",
		arUserText("deploy", now.Add(-2*time.Minute)),
		arModelCall("call-w", "write_tool", now.Add(-time.Minute)),
	)

	var resumed []string
	ar := NewAutoResumer(AutoResumeConfig{
		Handle:          h,
		AppName:         "core-agent",
		IsReadOnly:      arPred(),
		FreshnessWindow: time.Hour,
		Resume: func(_ context.Context, _, sessionID string, _ *genai.Content) error {
			resumed = append(resumed, sessionID)
			return nil
		},
	})

	if n := ar.Run(ctx); n != 0 {
		t.Fatalf("Run() before AckEffects = %d, want 0 (H1 ambiguous-effect block)", n)
	}
	if len(resumed) != 0 {
		t.Fatalf("resumed before AckEffects = %v, want empty", resumed)
	}

	if _, err := h.AckEffects(ctx, "core-agent", "alice", "s-mut", "operator@example.com", "verified idempotent"); err != nil {
		t.Fatalf("AckEffects: %v", err)
	}

	if n := ar.Run(ctx); n != 1 {
		t.Fatalf("Run() after AckEffects = %d, want 1", n)
	}
	if len(resumed) != 1 || resumed[0] != "s-mut" {
		t.Fatalf("resumed after AckEffects = %v, want [s-mut]", resumed)
	}
}

func TestAutoResumeSkipsReservedSubAndParkedSessions(t *testing.T) {
	t.Parallel()
	h := openSchedLeaseHandle(t)
	now := time.Now().UTC()

	seedARSession(t, h, "system", "s-ops:mast-ops", arUserText("ops", now))
	seedARSession(t, h, "alice", "s-parent:sub:worker", arUserText("sub", now))
	seedARSession(t, h, "_system", "__mast_scheduling__", arUserText("sched", now))
	seedARSession(t, h, "alice", "s-parked",
		arUserText("approve", now.Add(-time.Minute)),
		arParkedCall("park-1", "write_tool", now),
	)
	seedARSession(t, h, "alice", "s-done",
		arUserText("hi", now.Add(-time.Minute)),
		arModelText("completed", now),
	)
	seedARSession(t, h, "alice", "s-sub-trail",
		arUserText("hi", now.Add(-time.Minute)),
		arSubAgentText("sub_worker", "partial", now),
	)
	seedARSession(t, h, "alice", "s-deleg",
		arUserText("hi", now.Add(-time.Minute)),
		arModelCall("call-d", "sub_worker", now),
	)
	seedARSession(t, h, "alice", "s-multi-repair",
		arUserText("hi", now.Add(-2*time.Minute)),
		arModelCall("call-a", "read_tool", now.Add(-time.Minute)),
		arModelCall("call-b", "read_tool", now),
	)
	seedARSession(t, h, "alice", "s-eligible",
		arUserText("unfinished user turn", now),
	)

	var resumed []string
	ar := NewAutoResumer(AutoResumeConfig{
		Handle:          h,
		AppName:         "core-agent",
		IsReadOnly:      arPred(),
		SubAgentNames:   map[string]bool{"sub_worker": true},
		FreshnessWindow: time.Hour,
		Resume: func(_ context.Context, _, sessionID string, _ *genai.Content) error {
			resumed = append(resumed, sessionID)
			return nil
		},
	})

	if n := ar.Run(context.Background()); n != 1 {
		t.Fatalf("Run() = %d, want 1 (only s-eligible); resumed=%v", n, resumed)
	}
	if len(resumed) != 1 || resumed[0] != "s-eligible" {
		t.Fatalf("resumed = %v, want [s-eligible]", resumed)
	}
}

func TestAutoResumeCrashLoopBreakerAndBootCap(t *testing.T) {
	t.Parallel()
	h := openSchedLeaseHandle(t)
	ctx := context.Background()
	base := time.Now().UTC().Add(-30 * time.Minute)

	seedARSession(t, h, "alice", "s-loop", arUserText("crash me", base))

	calls := 0
	ar := NewAutoResumer(AutoResumeConfig{
		Handle:  h,
		AppName: "core-agent",
		Resume: func(_ context.Context, _, _ string, _ *genai.Content) error {
			calls++
			return errors.New("boom")
		},
	})

	for i := range AutoResumeMaxAttempts {
		ar.Run(ctx)
		if calls != i+1 {
			t.Fatalf("after pass %d, calls = %d, want %d", i+1, calls, i+1)
		}
	}
	// Fourth pass within the 10-minute window must be blocked by the M2 breaker.
	ar.Run(ctx)
	if calls != AutoResumeMaxAttempts {
		t.Fatalf("4th pass within window invoked Resume: calls = %d, want %d", calls, AutoResumeMaxAttempts)
	}

	// Verify oldest-first ordering and per-boot cap.
	h2 := openSchedLeaseHandle(t)
	for i := range 6 {
		sid := fmt.Sprintf("s-order-%d", i)
		seedARSession(t, h2, "alice", sid, arUserText("work", base.Add(time.Duration(i)*time.Minute)))
	}
	var order []string
	arCap := NewAutoResumer(AutoResumeConfig{
		Handle:     h2,
		AppName:    "core-agent",
		MaxPerBoot: 3,
		Resume: func(_ context.Context, _, sessionID string, _ *genai.Content) error {
			order = append(order, sessionID)
			return nil
		},
	})
	if n := arCap.Run(ctx); n != 3 {
		t.Fatalf("Run() with MaxPerBoot=3 returned %d, want 3", n)
	}
	wantOrder := []string{"s-order-0", "s-order-1", "s-order-2"}
	for i, want := range wantOrder {
		if order[i] != want {
			t.Errorf("order[%d] = %q, want %q (oldest-first)", i, order[i], want)
		}
	}
}

func TestAutoResumeSupersessionAndLeaseLost(t *testing.T) {
	t.Parallel()
	h := openSchedLeaseHandle(t)
	ctx := context.Background()
	now := time.Now().UTC()
	seedARSession(t, h, "alice", "s-super", arUserText("hi", now.Add(-time.Minute)))

	resumed := false
	ar := NewAutoResumer(AutoResumeConfig{
		Handle:  h,
		AppName: "core-agent",
		BeforeResume: func(sessionID string) {
			// Simulate a concurrent turn advancing the session after scan.
			resp, err := h.Service.Get(ctx, &adksession.GetRequest{
				AppName: "core-agent", UserID: "alice", SessionID: sessionID,
			})
			if err == nil {
				_ = h.Service.AppendEvent(ctx, resp.Session, arModelText("concurrent reply", now))
			}
		},
		Resume: func(_ context.Context, _, _ string, _ *genai.Content) error {
			resumed = true
			return nil
		},
	})
	if n := ar.Run(ctx); n != 0 || resumed {
		t.Fatalf("superseded session should not resume: n=%d resumed=%v", n, resumed)
	}

	// LeaseLost stops the pass mid-flight.
	h2 := openSchedLeaseHandle(t)
	seedARSession(t, h2, "alice", "s-lease-1", arUserText("1", now.Add(-2*time.Minute)))
	seedARSession(t, h2, "alice", "s-lease-2", arUserText("2", now.Add(-time.Minute)))
	leaseLost := make(chan struct{})
	var leaseResumed []string
	arLease := NewAutoResumer(AutoResumeConfig{
		Handle:    h2,
		AppName:   "core-agent",
		LeaseLost: leaseLost,
		Resume: func(_ context.Context, _, sessionID string, _ *genai.Content) error {
			leaseResumed = append(leaseResumed, sessionID)
			close(leaseLost)
			return nil
		},
	})
	if n := arLease.Run(ctx); n != 1 {
		t.Fatalf("Run() with LeaseLost closed after first candidate = %d, want 1; resumed=%v", n, leaseResumed)
	}
}

func TestRepairContentShape(t *testing.T) {
	t.Parallel()
	in := []effects.DanglingIntent{
		{ToolName: "read_tool", CallID: "call-1"},
		{ToolName: "lookup_thing", CallID: "call-2"},
	}
	c := repairContent(in)
	if c.Role != genai.RoleUser {
		t.Errorf("repair content role = %q, want %q", c.Role, genai.RoleUser)
	}
	if len(c.Parts) != len(in) {
		t.Fatalf("repair parts = %d, want %d", len(c.Parts), len(in))
	}
	for i, p := range c.Parts {
		fr := p.FunctionResponse
		if fr == nil {
			t.Fatalf("part %d has no FunctionResponse", i)
		}
		if fr.Name != in[i].ToolName {
			t.Errorf("part %d name = %q, want %q", i, fr.Name, in[i].ToolName)
		}
		if fr.ID != in[i].CallID {
			t.Errorf("part %d id = %q, want %q", i, fr.ID, in[i].CallID)
		}
		if _, ok := fr.Response["error"]; !ok {
			t.Errorf("part %d response missing the \"error\" key: %v", i, fr.Response)
		}
	}
}
