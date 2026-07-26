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

package subsession

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	adkmodel "google.golang.org/adk/model"
	"google.golang.org/adk/session"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/pkg/eventlog"
)

// openTestEventLog returns a Handle backed by a fresh on-disk SQLite
// database. Duplicated (rather than exported) so this package's tests
// stay self-contained.
func openTestEventLog(t *testing.T) (*eventlog.Handle, func()) {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "session.db")
	h, err := eventlog.Open(context.Background(), sqlite.Open(dsn))
	if err != nil {
		t.Fatalf("eventlog.Open: %v", err)
	}
	return h, func() { _ = h.Close() }
}

// makeTestEvent constructs a minimal session.Event for the
// branch-injecting tests.
func makeTestEvent(id, branch string) *session.Event {
	return &session.Event{
		ID:        id,
		Author:    "tester",
		Branch:    branch,
		Timestamp: time.Now(),
		LLMResponse: adkmodel.LLMResponse{
			Content: &genai.Content{
				Role:  genai.RoleModel,
				Parts: []*genai.Part{{Text: "x"}},
			},
		},
	}
}

func TestComposeBranch(t *testing.T) {
	t.Parallel()
	cases := []struct{ parent, this, want string }{
		{"", "", ""},
		{"", "research", "research"},
		{"parent", "", "parent"},
		{"parent", "research", "parent.research"},
		{"a.b", "c", "a.b.c"},
	}
	for _, c := range cases {
		if got := ComposeBranch(c.parent, c.this); got != c.want {
			t.Errorf("ComposeBranch(%q,%q)=%q, want %q", c.parent, c.this, got, c.want)
		}
	}
}

func TestCurrentDepth_DefaultsZeroAndReadsContext(t *testing.T) {
	t.Parallel()
	if d := CurrentDepth(context.Background()); d != 0 {
		t.Errorf("default depth = %d, want 0", d)
	}
	ctx := WithDepth(context.Background(), 7)
	if d := CurrentDepth(ctx); d != 7 {
		t.Errorf("depth from context = %d, want 7", d)
	}
}

// TestDeriveSessionID covers the standalone-construction and
// no-invocation-component edges: the parent prefix and invocation
// suffix are each dropped when empty, so deterministic name-addressed
// callers (subtask / background spawn, which pass "") keep their
// historical IDs.
func TestDeriveSessionID(t *testing.T) {
	t.Parallel()
	cases := []struct{ parent, branch, invocation, want string }{
		{"", "research", "", "sub:research"},
		{"sess-1", "research", "", "sess-1:sub:research"},
		{"", "research", "fc-1", "sub:research:fc-1"},
		{"sess-1", "research", "fc-1", "sess-1:sub:research:fc-1"},
	}
	for _, c := range cases {
		if got := DeriveSessionID(c.parent, c.branch, c.invocation); got != c.want {
			t.Errorf("DeriveSessionID(%q,%q,%q)=%q, want %q", c.parent, c.branch, c.invocation, got, c.want)
		}
	}
}

func TestBranchInjectingService_StampsEmptyBranch(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestEventLog(t)
	defer cleanup()
	ctx := context.Background()
	if _, err := h.Service.Create(ctx, &session.CreateRequest{
		AppName: "app", UserID: "u", SessionID: "branch-test",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	resp, err := h.Service.Get(ctx, &session.GetRequest{
		AppName: "app", UserID: "u", SessionID: "branch-test",
	})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	wrapped := &BranchInjectingService{Inner: h.Service, Branch: "research"}
	ev := makeTestEvent("ev-1", "")
	if err := wrapped.AppendEvent(ctx, resp.Session, ev); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	if ev.Branch != "research" {
		t.Errorf("Branch should be stamped on the event; got %q", ev.Branch)
	}
}

func TestBranchInjectingService_PreservesPresetBranch(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestEventLog(t)
	defer cleanup()
	ctx := context.Background()
	if _, err := h.Service.Create(ctx, &session.CreateRequest{
		AppName: "app", UserID: "u", SessionID: "preset",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	resp, err := h.Service.Get(ctx, &session.GetRequest{
		AppName: "app", UserID: "u", SessionID: "preset",
	})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	wrapped := &BranchInjectingService{Inner: h.Service, Branch: "research"}
	// A nested subagent at "research.deep" — the wrapper must not
	// overwrite the deeper branch label with its own.
	ev := makeTestEvent("ev-2", "research.deep")
	if err := wrapped.AppendEvent(ctx, resp.Session, ev); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	if ev.Branch != "research.deep" {
		t.Errorf("preset Branch should not be overwritten; got %q", ev.Branch)
	}
}

func TestBranchInjectingService_DelegatesCRUD(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestEventLog(t)
	defer cleanup()
	ctx := context.Background()
	wrapped := &BranchInjectingService{Inner: h.Service, Branch: "research"}
	if _, err := wrapped.Create(ctx, &session.CreateRequest{
		AppName: "app", UserID: "u", SessionID: "delegated",
	}); err != nil {
		t.Fatalf("Create through wrapper: %v", err)
	}
	resp, err := wrapped.Get(ctx, &session.GetRequest{
		AppName: "app", UserID: "u", SessionID: "delegated",
	})
	if err != nil {
		t.Fatalf("Get through wrapper: %v", err)
	}
	if resp == nil || resp.Session == nil || resp.Session.ID() != "delegated" {
		t.Errorf("Get returned %+v, want session with ID=delegated", resp)
	}
	listResp, err := wrapped.List(ctx, &session.ListRequest{AppName: "app", UserID: "u"})
	if err != nil {
		t.Fatalf("List through wrapper: %v", err)
	}
	if listResp == nil || len(listResp.Sessions) == 0 {
		t.Errorf("List returned no sessions: %+v", listResp)
	}
	if err := wrapped.Delete(ctx, &session.DeleteRequest{
		AppName: "app", UserID: "u", SessionID: "delegated",
	}); err != nil {
		t.Fatalf("Delete through wrapper: %v", err)
	}
}
