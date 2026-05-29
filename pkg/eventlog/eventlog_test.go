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

package eventlog

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	adkmodel "google.golang.org/adk/model"
	"google.golang.org/adk/session"
	"google.golang.org/genai"
)

// openTestHandle returns a Handle backed by a fresh on-disk SQLite
// database in t.TempDir(). On-disk (vs ":memory:") because Open
// creates two separate gorm.DB connections and an in-memory DB is
// not shared between them by default.
func openTestHandle(t *testing.T) (*Handle, func()) {
	t.Helper()
	dir := t.TempDir()
	dsn := filepath.Join(dir, "eventlog.db")
	h, err := Open(context.Background(), sqlite.Open(dsn))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return h, func() {
		if err := h.Close(); err != nil {
			t.Logf("Handle.Close: %v", err)
		}
	}
}

// mustCreateSession creates a session via the handle's Service and
// returns the live session.Session. Tests that want to exercise the
// AppendEvent path go through this.
func mustCreateSession(t *testing.T, h *Handle, app, user, sess string) session.Session {
	t.Helper()
	resp, err := h.Service.Create(context.Background(), &session.CreateRequest{
		AppName:   app,
		UserID:    user,
		SessionID: sess,
	})
	if err != nil {
		t.Fatalf("Service.Create: %v", err)
	}
	if resp == nil || resp.Session == nil {
		t.Fatalf("Service.Create returned nil session")
	}
	return resp.Session
}

// makeEvent constructs a minimal session.Event with a stable, unique
// ID. The author + branch + content fields are populated so query
// filters in the tests have something meaningful to assert on.
func makeEvent(id, author, branch, text string) *session.Event {
	return &session.Event{
		ID:        id,
		Author:    author,
		Branch:    branch,
		Timestamp: time.Now(),
		LLMResponse: adkmodel.LLMResponse{
			Content: &genai.Content{
				Role:  genai.RoleModel,
				Parts: []*genai.Part{{Text: text}},
			},
		},
	}
}

// drain collects every (Entry, error) the iterator emits, with a
// hard limit so a stuck iterator can't hang the test forever.
func drain(t *testing.T, it func(yield func(Entry, error) bool)) []Entry {
	t.Helper()
	const cap = 1000
	var out []Entry
	for e, err := range it {
		if err != nil {
			t.Fatalf("iterator error: %v", err)
		}
		out = append(out, e)
		if len(out) > cap {
			t.Fatalf("iterator exceeded %d entries; suspected runaway", cap)
		}
	}
	return out
}

func TestOpen_Idempotent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	dsn := filepath.Join(dir, "eventlog.db")
	h1, err := Open(context.Background(), sqlite.Open(dsn))
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if err := h1.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	// Re-open the same file. AutoMigrate must be a no-op the second
	// time; the existing schema must round-trip cleanly.
	h2, err := Open(context.Background(), sqlite.Open(dsn))
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	if err := h2.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestAppend_AssignsMonotonicSeq(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestHandle(t)
	defer cleanup()
	sess := mustCreateSession(t, h, "app", "user", "sess1")
	ctx := context.Background()

	var prev int64
	for i := 0; i < 5; i++ {
		ev := makeEvent(fmt.Sprintf("ev-%d", i), "author", "", fmt.Sprintf("text %d", i))
		if err := h.Service.AppendEvent(ctx, sess, ev); err != nil {
			t.Fatalf("AppendEvent %d: %v", i, err)
		}
		// Read seq via Since(prev).
		entries := drain(t, h.Stream.Since(ctx, prev, ForSession("app", "user", "sess1")))
		if len(entries) != 1 {
			t.Fatalf("expected 1 new entry after append %d, got %d", i, len(entries))
		}
		if entries[0].Seq <= prev {
			t.Errorf("seq did not advance: prev=%d new=%d", prev, entries[0].Seq)
		}
		if entries[0].Event == nil || entries[0].Event.ID != ev.ID {
			t.Errorf("loaded event mismatch: %+v", entries[0].Event)
		}
		prev = entries[0].Seq
	}
}

func TestSince_ReturnsTailInOrder(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestHandle(t)
	defer cleanup()
	sess := mustCreateSession(t, h, "app", "user", "sess1")
	ctx := context.Background()

	// Append 5 events.
	for i := 0; i < 5; i++ {
		ev := makeEvent(fmt.Sprintf("ev-%d", i), "author", "", fmt.Sprintf("text %d", i))
		if err := h.Service.AppendEvent(ctx, sess, ev); err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
	}

	all := drain(t, h.Stream.Since(ctx, 0, ForSession("app", "user", "sess1")))
	if len(all) != 5 {
		t.Fatalf("want 5 entries, got %d", len(all))
	}
	for i := 1; i < len(all); i++ {
		if all[i].Seq <= all[i-1].Seq {
			t.Errorf("seq order broken at %d: %d then %d", i, all[i-1].Seq, all[i].Seq)
		}
	}

	tail := drain(t, h.Stream.Since(ctx, all[2].Seq, ForSession("app", "user", "sess1")))
	if len(tail) != 2 {
		t.Errorf("Since(K) returned %d entries; want 2 (the last two)", len(tail))
	}
}

func TestSince_ForSessionFiltersOtherSessions(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestHandle(t)
	defer cleanup()
	a := mustCreateSession(t, h, "app", "user", "A")
	b := mustCreateSession(t, h, "app", "user", "B")
	ctx := context.Background()

	if err := h.Service.AppendEvent(ctx, a, makeEvent("a-1", "x", "", "hi-A")); err != nil {
		t.Fatalf("AppendEvent A: %v", err)
	}
	if err := h.Service.AppendEvent(ctx, b, makeEvent("b-1", "x", "", "hi-B")); err != nil {
		t.Fatalf("AppendEvent B: %v", err)
	}

	onlyA := drain(t, h.Stream.Since(ctx, 0, ForSession("app", "user", "A")))
	if len(onlyA) != 1 || onlyA[0].Event.ID != "a-1" {
		t.Errorf("ForSession(A) leaked: got %+v", onlyA)
	}
}

func TestSince_WithBranchPrefixFilters(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestHandle(t)
	defer cleanup()
	sess := mustCreateSession(t, h, "app", "user", "sess1")
	ctx := context.Background()

	cases := []struct {
		id, branch string
	}{
		{"e-root", ""},
		{"e-parent-only", "parent"},
		{"e-parent-child-dot", "parent.child"},
		{"e-parent-child-slash", "parent/child"},
		{"e-other", "other"},
	}
	for _, c := range cases {
		if err := h.Service.AppendEvent(ctx, sess, makeEvent(c.id, "x", c.branch, c.id)); err != nil {
			t.Fatalf("AppendEvent %s: %v", c.id, err)
		}
	}

	matches := drain(t, h.Stream.Since(ctx, 0,
		ForSession("app", "user", "sess1"),
		WithBranchPrefix("parent"),
	))
	gotIDs := map[string]bool{}
	for _, e := range matches {
		gotIDs[e.Event.ID] = true
	}
	want := []string{"e-parent-only", "e-parent-child-dot", "e-parent-child-slash"}
	for _, id := range want {
		if !gotIDs[id] {
			t.Errorf("WithBranchPrefix(parent) missed %q", id)
		}
	}
	if gotIDs["e-other"] {
		t.Errorf("WithBranchPrefix(parent) leaked %q", "e-other")
	}
	if gotIDs["e-root"] {
		t.Errorf("WithBranchPrefix(parent) leaked %q", "e-root")
	}
}

func TestSince_WithAuthorAndLimit(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestHandle(t)
	defer cleanup()
	sess := mustCreateSession(t, h, "app", "user", "sess1")
	ctx := context.Background()

	for i := 0; i < 4; i++ {
		ev := makeEvent(fmt.Sprintf("u-%d", i), "user", "", "")
		if err := h.Service.AppendEvent(ctx, sess, ev); err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
	}
	for i := 0; i < 4; i++ {
		ev := makeEvent(fmt.Sprintf("a-%d", i), "assistant", "", "")
		if err := h.Service.AppendEvent(ctx, sess, ev); err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
	}

	users := drain(t, h.Stream.Since(ctx, 0,
		ForSession("app", "user", "sess1"),
		WithAuthor("user"),
	))
	if len(users) != 4 {
		t.Errorf("WithAuthor(user) returned %d, want 4", len(users))
	}

	first2 := drain(t, h.Stream.Since(ctx, 0,
		ForSession("app", "user", "sess1"),
		WithLimit(2),
	))
	if len(first2) != 2 {
		t.Errorf("WithLimit(2) returned %d, want 2", len(first2))
	}
}

func TestWatch_BlocksUntilAppendThenYields(t *testing.T) {
	t.Parallel()
	// Tighter watch interval so the test is fast.
	dir := t.TempDir()
	dsn := filepath.Join(dir, "eventlog.db")
	h, err := Open(context.Background(), sqlite.Open(dsn),
		WithWatchInterval(20*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer h.Close()
	sess := mustCreateSession(t, h, "app", "user", "watch")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	type recv struct {
		entries []Entry
		err     error
	}
	results := make(chan recv, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		var got []Entry
		for e, err := range h.Stream.Watch(ctx, 0, ForSession("app", "user", "watch")) {
			if err != nil {
				results <- recv{got, err}
				return
			}
			got = append(got, e)
			if len(got) == 2 {
				cancel() // trigger graceful Watch exit
			}
		}
		results <- recv{got, nil}
	}()

	// Sleep a touch so Watch is parked in its poll loop, then Append.
	time.Sleep(50 * time.Millisecond)
	if err := h.Service.AppendEvent(context.Background(), sess, makeEvent("w-1", "x", "", "first")); err != nil {
		t.Fatalf("Append 1: %v", err)
	}
	time.Sleep(60 * time.Millisecond)
	if err := h.Service.AppendEvent(context.Background(), sess, makeEvent("w-2", "x", "", "second")); err != nil {
		t.Fatalf("Append 2: %v", err)
	}

	wg.Wait()
	r := <-results
	if r.err != nil && !errors.Is(r.err, context.Canceled) && !errors.Is(r.err, context.DeadlineExceeded) {
		t.Errorf("Watch err = %v", r.err)
	}
	if len(r.entries) < 2 {
		t.Errorf("Watch yielded %d entries, want >= 2: %+v", len(r.entries), r.entries)
	}
	if len(r.entries) >= 2 && r.entries[0].Seq >= r.entries[1].Seq {
		t.Errorf("Watch returned out of order: %d then %d", r.entries[0].Seq, r.entries[1].Seq)
	}
}

func TestService_DelegatesCRUDToADK(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestHandle(t)
	defer cleanup()
	ctx := context.Background()

	// Create + Get + List + Delete all run through ADK; this just
	// pins that the wrapper isn't dropping calls.
	if _, err := h.Service.Create(ctx, &session.CreateRequest{
		AppName: "app", UserID: "user", SessionID: "crud",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	getResp, err := h.Service.Get(ctx, &session.GetRequest{
		AppName: "app", UserID: "user", SessionID: "crud",
	})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if getResp == nil || getResp.Session == nil || getResp.Session.ID() != "crud" {
		t.Errorf("Get returned %+v", getResp)
	}
	listResp, err := h.Service.List(ctx, &session.ListRequest{
		AppName: "app", UserID: "user",
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if listResp == nil || len(listResp.Sessions) == 0 {
		t.Errorf("List returned no sessions: %+v", listResp)
	}
	if err := h.Service.Delete(ctx, &session.DeleteRequest{
		AppName: "app", UserID: "user", SessionID: "crud",
	}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

func TestAppend_DuplicateEventIDRejected(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestHandle(t)
	defer cleanup()
	sess := mustCreateSession(t, h, "app", "user", "dup")
	ctx := context.Background()

	ev := makeEvent("only-id", "x", "", "first")
	if err := h.Service.AppendEvent(ctx, sess, ev); err != nil {
		t.Fatalf("first AppendEvent: %v", err)
	}
	// Re-using the same ID must fail at the overlay layer (unique
	// index on event_id). This is the property that makes retries
	// safe and surfaces accidental ID reuse promptly.
	dup := makeEvent("only-id", "x", "", "second")
	err := h.Service.AppendEvent(ctx, sess, dup)
	if err == nil {
		t.Fatalf("expected error from duplicate event ID, got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "unique") &&
		!strings.Contains(strings.ToLower(err.Error()), "constraint") {
		t.Logf("duplicate-id error message: %v", err)
	}
}

func TestWithSessionTree_ReturnsParentAndSubagent(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestHandle(t)
	defer cleanup()
	parent := mustCreateSession(t, h, "app", "u", "task-1")
	research := mustCreateSession(t, h, "app", "u", "task-1:sub:research")
	ctx := context.Background()

	if err := h.Service.AppendEvent(ctx, parent, makeEvent("p-1", "user", "", "go")); err != nil {
		t.Fatalf("AppendEvent parent: %v", err)
	}
	if err := h.Service.AppendEvent(ctx, research, makeEvent("r-1", "research", "research", "results")); err != nil {
		t.Fatalf("AppendEvent research: %v", err)
	}

	got := drain(t, h.Stream.Since(ctx, 0, WithSessionTree("app", "u", "task-1")))
	if len(got) != 2 {
		t.Fatalf("WithSessionTree returned %d entries, want 2: %+v", len(got), got)
	}
	ids := map[string]bool{}
	for _, e := range got {
		ids[e.Event.ID] = true
	}
	for _, want := range []string{"p-1", "r-1"} {
		if !ids[want] {
			t.Errorf("missing %q in tree result; got %v", want, ids)
		}
	}
}

func TestWithSessionTree_IgnoresUnrelatedSessions(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestHandle(t)
	defer cleanup()
	a := mustCreateSession(t, h, "app", "u", "task-A")
	aResearch := mustCreateSession(t, h, "app", "u", "task-A:sub:research")
	b := mustCreateSession(t, h, "app", "u", "task-B")
	ctx := context.Background()

	for sess, id := range map[session.Session]string{a: "a-1", aResearch: "ar-1", b: "b-1"} {
		if err := h.Service.AppendEvent(ctx, sess, makeEvent(id, "x", "", "")); err != nil {
			t.Fatalf("AppendEvent %s: %v", id, err)
		}
	}

	got := drain(t, h.Stream.Since(ctx, 0, WithSessionTree("app", "u", "task-A")))
	ids := map[string]bool{}
	for _, e := range got {
		ids[e.Event.ID] = true
	}
	for _, want := range []string{"a-1", "ar-1"} {
		if !ids[want] {
			t.Errorf("WithSessionTree missed %q; got %v", want, ids)
		}
	}
	if ids["b-1"] {
		t.Errorf("WithSessionTree leaked unrelated session: %v", ids)
	}
}

func TestWithSessionTree_DepthAgnostic(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestHandle(t)
	defer cleanup()
	// The current naming convention is "<parent>:sub:<branch>" so
	// nesting becomes "task:sub:a:sub:b". The LIKE clause matches
	// the prefix once and recursive descent works because every
	// descendant carries the parent prefix in its name.
	p := mustCreateSession(t, h, "app", "u", "task")
	a := mustCreateSession(t, h, "app", "u", "task:sub:a")
	b := mustCreateSession(t, h, "app", "u", "task:sub:a:sub:b")
	ctx := context.Background()
	for sess, id := range map[session.Session]string{p: "p", a: "a", b: "b"} {
		if err := h.Service.AppendEvent(ctx, sess, makeEvent(id, "x", "", "")); err != nil {
			t.Fatalf("AppendEvent %s: %v", id, err)
		}
	}
	got := drain(t, h.Stream.Since(ctx, 0, WithSessionTree("app", "u", "task")))
	if len(got) != 3 {
		t.Errorf("got %d entries, want 3 (parent + child + grandchild): %+v", len(got), got)
	}
}

func TestWithSessionTree_ComposesWithAuthor(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestHandle(t)
	defer cleanup()
	parent := mustCreateSession(t, h, "app", "u", "task")
	research := mustCreateSession(t, h, "app", "u", "task:sub:research")
	ctx := context.Background()

	if err := h.Service.AppendEvent(ctx, parent, makeEvent("p-user", "user", "", "")); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	if err := h.Service.AppendEvent(ctx, parent, makeEvent("p-asst", "assistant", "", "")); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	if err := h.Service.AppendEvent(ctx, research, makeEvent("r-asst", "assistant", "research", "")); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}

	// All assistant events under the tree.
	got := drain(t, h.Stream.Since(ctx, 0,
		WithSessionTree("app", "u", "task"),
		WithAuthor("assistant"),
	))
	ids := map[string]bool{}
	for _, e := range got {
		ids[e.Event.ID] = true
	}
	if !ids["p-asst"] || !ids["r-asst"] {
		t.Errorf("expected p-asst + r-asst, got %v", ids)
	}
	if ids["p-user"] {
		t.Errorf("user-author event leaked through assistant filter: %v", ids)
	}
}

func TestStream_ClosedRejectsOps(t *testing.T) {
	t.Parallel()
	h, _ := openTestHandle(t)
	if err := h.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	ctx := context.Background()
	_, err := h.Stream.Append(ctx, nil, nil)
	if !errors.Is(err, ErrClosed) {
		t.Errorf("Append after Close: err = %v, want ErrClosed", err)
	}
}
