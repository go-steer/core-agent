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

//go:build !no_tui

package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	adkmodel "google.golang.org/adk/model"
	"google.golang.org/adk/session"
	"google.golang.org/genai"

	coretui "github.com/go-steer/core-tui/tui"

	"github.com/go-steer/core-agent/v2/pkg/agent"
	"github.com/go-steer/core-agent/v2/pkg/attachadapter"
	"github.com/go-steer/core-agent/v2/pkg/eventlog"
)

// subagentLogAdapter builds the in-process TUI adapter over an agent
// backed by a real sqlite event log — the --session-db shape, which is
// the only one that has subagent turns to read.
func subagentLogAdapter(t *testing.T) (*coreAgentAdapter, *eventlog.Handle) {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "session.db")
	handle, err := eventlog.Open(context.Background(), sqlite.Open(dsn))
	if err != nil {
		t.Fatalf("eventlog.Open: %v", err)
	}
	t.Cleanup(func() { _ = handle.Close() })

	inner, err := agent.New(&cumulativeUsageLLM{finalIn: 1, finalOut: 1},
		agent.WithName("test"),
		agent.WithSession("operator", "s1"),
		agent.WithEventLog(handle),
	)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	return &coreAgentAdapter{inner: inner, attachAd: attachadapter.New(inner)}, handle
}

// appendSubagentTurn writes one event under branch, the way the
// subagent runners do: through the PARENT's session.Service, tagged so
// the turn lands in the parent's log under the child's branch label.
func appendSubagentTurn(t *testing.T, a *coreAgentAdapter, h *eventlog.Handle, branch, id, text string) {
	t.Helper()
	ctx := context.Background()
	app, user, sid := a.inner.AppName(), a.inner.UserID(), a.inner.SessionID()
	got, err := h.Service.Get(ctx, &session.GetRequest{AppName: app, UserID: user, SessionID: sid})
	if err != nil || got == nil || got.Session == nil {
		if _, cerr := h.Service.Create(ctx, &session.CreateRequest{
			AppName: app, UserID: user, SessionID: sid,
		}); cerr != nil {
			t.Fatalf("session Create: %v", cerr)
		}
		got, err = h.Service.Get(ctx, &session.GetRequest{AppName: app, UserID: user, SessionID: sid})
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
	if err := h.Service.AppendEvent(ctx, got.Session, ev); err != nil {
		t.Fatalf("AppendEvent(%s): %v", id, err)
	}
}

// TestLocalSubagentEvents_ReadsTheLog is the in-process half of the
// drill-down: the local TUI has no HTTP layer, so it resolves the name
// and pages the log directly — and must reach the same answer the
// attach endpoint would, including the declared-name → spawned-instance
// mapping (#694).
func TestLocalSubagentEvents_ReadsTheLog(t *testing.T) {
	t.Parallel()
	a, h := subagentLogAdapter(t)
	appendSubagentTurn(t, a, h, "bg.cluster-1", "sa-1", "listing nodes")
	appendSubagentTurn(t, a, h, "bg.cluster-1", "sa-2", "3 nodes ready")
	appendSubagentTurn(t, a, h, "sub.audit", "sa-3", "unrelated")

	page, err := a.SubagentEvents(context.Background(), "cluster", 0)
	if err != nil {
		t.Fatalf("SubagentEvents: %v", err)
	}
	if len(page.Events) != 2 {
		t.Fatalf("got %d turns, want 2 (the audit turn must not leak in): %+v",
			len(page.Events), page.Events)
	}
	if page.Events[0].Text != "listing nodes" || page.Events[1].Text != "3 nodes ready" {
		t.Errorf("turns = %q / %q, want them in seq order",
			page.Events[0].Text, page.Events[1].Text)
	}
	if page.NextSince == 0 {
		t.Error("NextSince = 0, want a resume cursor for the tail")
	}

	next, err := a.SubagentEvents(context.Background(), "cluster", page.NextSince)
	if err != nil {
		t.Fatalf("SubagentEvents(since): %v", err)
	}
	if len(next.Events) != 0 {
		t.Errorf("resume returned %d turns, want 0", len(next.Events))
	}
}

// TestLocalSubagentEvents_UnknownNameIsTyped keeps the local path from
// painting a plausible empty log for a typo.
func TestLocalSubagentEvents_UnknownNameIsTyped(t *testing.T) {
	t.Parallel()
	a, h := subagentLogAdapter(t)
	appendSubagentTurn(t, a, h, "bg.cluster-1", "sa-1", "listing nodes")

	_, err := a.SubagentEvents(context.Background(), "clustr", 0)
	var nf *coretui.SubagentNotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("err = %v (%T), want *coretui.SubagentNotFoundError", err, err)
	}
	if len(nf.Available) != 1 || nf.Available[0] != "cluster" {
		t.Errorf("Available = %v, want [cluster]", nf.Available)
	}
}

// TestLocalSubagentEvents_NoEventLogIsAnError covers a session started
// without --session-db: there is no log, so every name is unreadable.
// Reporting that beats an empty page that reads as "the subagent did
// nothing".
func TestLocalSubagentEvents_NoEventLogIsAnError(t *testing.T) {
	t.Parallel()
	inner, err := agent.New(&cumulativeUsageLLM{finalIn: 1, finalOut: 1}, agent.WithName("test"))
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	a := &coreAgentAdapter{inner: inner, attachAd: attachadapter.New(inner)}

	if _, err := a.SubagentEvents(context.Background(), "cluster", 0); err == nil {
		t.Fatal("SubagentEvents without an event log should error")
	} else if !strings.Contains(err.Error(), "--session-db") {
		t.Errorf("error %q should name the flag that would fix it", err)
	}
}
