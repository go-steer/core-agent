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

// Package conformance is the contract every eventlog.Transcript backend
// has to meet, as a reusable test suite. A backend's own test file
// calls Run with a Backend that opens it; the suite then checks
// ordering, filters, seq continuity, partial handling, the metadata
// sidecar, in-process wake-ups, and a round-trip of every event field
// resume depends on. docs/external-state-design.md decision 10 is why
// this exists: two ADK services once differed on stale writes and two
// projects shipped the same bug (#553).
package conformance

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"testing"
	"time"

	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v3/pkg/eventlog"
)

// Backend tells the suite how to get a Transcript.
type Backend struct {
	// Open returns a fresh, empty Transcript. The suite closes it.
	Open func(t *testing.T) eventlog.Transcript
	// Reopen, when set, returns a Transcript over the same store prev
	// used, after prev has been closed: the handover case. Backends
	// with no persistence leave it nil and the handover test is skipped.
	Reopen func(t *testing.T, prev eventlog.Transcript) eventlog.Transcript
	// WakeTimeout bounds how long the suite waits for a Subscribe
	// wake-up. Zero means 5s.
	WakeTimeout time.Duration
}

// Run runs the suite against b.
func Run(t *testing.T, b Backend) {
	t.Helper()
	if b.Open == nil {
		t.Fatal("conformance: Backend.Open is required")
	}
	if b.WakeTimeout == 0 {
		b.WakeTimeout = 5 * time.Second
	}
	t.Run("AppendAssignsIncreasingSeqPerTree", func(t *testing.T) { testOrder(t, b) })
	t.Run("PartialEventsAreNotStored", func(t *testing.T) { testPartial(t, b) })
	t.Run("FiltersMatchTheSQLSemantics", func(t *testing.T) { testFilters(t, b) })
	t.Run("UnscopedQueryIsServedOrRefusedCleanly", func(t *testing.T) { testUnscoped(t, b) })
	t.Run("HighestSeq", func(t *testing.T) { testHighestSeq(t, b) })
	t.Run("MetadataSidecarRoundTrips", func(t *testing.T) { testMetadata(t, b) })
	t.Run("SubscribeWakesOnAppend", func(t *testing.T) { testSubscribe(t, b) })
	t.Run("GetOfMissingSessionIsNotFound", func(t *testing.T) { testNotFound(t, b) })
	t.Run("DeleteRemovesTreeEntries", func(t *testing.T) { testDelete(t, b) })
	t.Run("EventFieldsSurviveTheRoundTrip", func(t *testing.T) { testFidelity(t, b) })
	t.Run("SeqContinuesAcrossHandover", func(t *testing.T) { testHandover(t, b) })
}

func open(t *testing.T, b Backend) eventlog.Transcript {
	t.Helper()
	tr := b.Open(t)
	if tr == nil {
		t.Fatal("Backend.Open returned nil")
	}
	t.Cleanup(func() { _ = tr.Close() })
	return tr
}

// app and user are the one (app, user) pair the suite creates sessions
// under; every session key differs by session ID instead. Queries
// against another user (testFilters) read, never create.
const (
	app  = "app"
	user = "user"
)

func create(t *testing.T, tr eventlog.Transcript, id string) session.Session {
	t.Helper()
	resp, err := tr.Service().Create(context.Background(), &session.CreateRequest{AppName: app, UserID: user, SessionID: id})
	if err != nil {
		t.Fatalf("Create %s/%s/%s: %v", app, user, id, err)
	}
	if got := resp.Session.ID(); got != id {
		t.Fatalf("Create returned session ID %q, want the requested %q", got, id)
	}
	return resp.Session
}

func event(id, author, branch, text string) *session.Event {
	ev := &session.Event{ID: id, Author: author, Branch: branch, InvocationID: "inv-" + id, Timestamp: time.Now().UTC().Truncate(time.Microsecond)}
	ev.Content = genai.NewContentFromText(text, genai.RoleModel)
	return ev
}

func appendEv(t *testing.T, ctx context.Context, tr eventlog.Transcript, sess session.Session, ev *session.Event) {
	t.Helper()
	if err := tr.Service().AppendEvent(ctx, sess, ev); err != nil {
		t.Fatalf("AppendEvent %s: %v", ev.ID, err)
	}
}

func collect(t *testing.T, tr eventlog.Transcript, from int64, opts ...eventlog.QueryOption) []eventlog.Entry {
	t.Helper()
	var out []eventlog.Entry
	for e, err := range tr.Since(context.Background(), from, opts...) {
		if err != nil {
			t.Fatalf("Since: %v", err)
		}
		out = append(out, e)
	}
	return out
}

func ids(entries []eventlog.Entry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Event.ID)
	}
	return out
}

func testOrder(t *testing.T, b Backend) {
	tr := open(t, b)
	ctx := context.Background()
	sess := create(t, tr, "root-a")
	for i := 1; i <= 5; i++ {
		appendEv(t, ctx, tr, sess, event(fmt.Sprintf("e%d", i), "model", "", "hi"))
	}
	got := collect(t, tr, 0, eventlog.ForSession("app", "user", "root-a"))
	if want := []string{"e1", "e2", "e3", "e4", "e5"}; !slices.Equal(ids(got), want) {
		t.Fatalf("Since(0) ids = %v, want %v", ids(got), want)
	}
	for i := 1; i < len(got); i++ {
		if got[i].Seq <= got[i-1].Seq {
			t.Fatalf("seq not strictly increasing: %d then %d", got[i-1].Seq, got[i].Seq)
		}
	}
	// A cursor resumes after the entry it names, not at it.
	rest := collect(t, tr, got[2].Seq, eventlog.ForSession("app", "user", "root-a"))
	if want := []string{"e4", "e5"}; !slices.Equal(ids(rest), want) {
		t.Fatalf("Since(seq of e3) ids = %v, want %v", ids(rest), want)
	}
	// Another tree starts its own sequence and is invisible here.
	other := create(t, tr, "root-b")
	appendEv(t, ctx, tr, other, event("o1", "model", "", "x"))
	if again := collect(t, tr, 0, eventlog.ForSession("app", "user", "root-a")); len(again) != 5 {
		t.Fatalf("tree a sees %d entries after a write to tree b, want 5", len(again))
	}
}

func testPartial(t *testing.T, b Backend) {
	tr := open(t, b)
	ctx := context.Background()
	sess := create(t, tr, "root-p")
	p := event("partial", "model", "", "half")
	p.Partial = true
	appendEv(t, ctx, tr, sess, p)
	appendEv(t, ctx, tr, sess, event("final", "model", "", "whole"))
	got := collect(t, tr, 0, eventlog.ForSession("app", "user", "root-p"))
	if want := []string{"final"}; !slices.Equal(ids(got), want) {
		t.Fatalf("partial event stored: ids = %v, want %v", ids(got), want)
	}
	resp, err := tr.Service().Get(ctx, &session.GetRequest{AppName: "app", UserID: "user", SessionID: "root-p"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if n := resp.Session.Events().Len(); n != 1 {
		t.Fatalf("Get returned %d events, want 1 (the partial must not be persisted)", n)
	}
}

func testFilters(t *testing.T, b Backend) {
	tr := open(t, b)
	ctx := context.Background()
	root := create(t, tr, "root-f")
	child := create(t, tr, "root-f:sub:research:inv1")
	appendEv(t, ctx, tr, root, event("r1", "user", "", "ask"))
	appendEv(t, ctx, tr, root, event("r2", "model", "", "answer"))
	appendEv(t, ctx, tr, child, event("c1", "research", "research", "look"))
	appendEv(t, ctx, tr, child, event("c2", "research/helper", "research.helper", "dig"))
	appendEv(t, ctx, tr, child, event("c3", "researcher", "researcher", "not a child branch"))

	cases := []struct {
		name string
		opts []eventlog.QueryOption
		want []string
	}{
		{"root session only", []eventlog.QueryOption{eventlog.ForSession("app", "user", "root-f")}, []string{"r1", "r2"}},
		{"child session only", []eventlog.QueryOption{eventlog.ForSession("app", "user", "root-f:sub:research:inv1")}, []string{"c1", "c2", "c3"}},
		{"whole tree", []eventlog.QueryOption{eventlog.WithSessionTree("app", "user", "root-f")}, []string{"r1", "r2", "c1", "c2", "c3"}},
		{"tree + author", []eventlog.QueryOption{eventlog.WithSessionTree("app", "user", "root-f"), eventlog.WithAuthor("model")}, []string{"r2"}},
		{"tree + author suffix", []eventlog.QueryOption{eventlog.WithSessionTree("app", "user", "root-f"), eventlog.WithAuthorSuffix("helper")}, []string{"c2"}},
		{"tree + branch prefix: exact and dotted descendant, not a longer name", []eventlog.QueryOption{eventlog.WithSessionTree("app", "user", "root-f"), eventlog.WithBranchPrefix("research")}, []string{"c1", "c2"}},
		{"tree + any branch prefix", []eventlog.QueryOption{eventlog.WithSessionTree("app", "user", "root-f"), eventlog.WithAnyBranchPrefix("researcher", "nothing")}, []string{"c3"}},
		{"tree + limit", []eventlog.QueryOption{eventlog.WithSessionTree("app", "user", "root-f"), eventlog.WithLimit(2)}, []string{"r1", "r2"}},
		{"tree of a different user is empty", []eventlog.QueryOption{eventlog.WithSessionTree("app", "other", "root-f")}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ids(collect(t, tr, 0, tc.opts...))
			if !slices.Equal(got, tc.want) {
				t.Fatalf("ids = %v, want %v", got, tc.want)
			}
		})
	}
}

func testUnscoped(t *testing.T, b Backend) {
	tr := open(t, b)
	ctx := context.Background()
	sess := create(t, tr, "root-u")
	appendEv(t, ctx, tr, sess, event("u1", "model", "", "x"))
	var n int
	for _, err := range tr.Since(ctx, 0) {
		if err != nil {
			if !errors.Is(err, eventlog.ErrUnscopedQuery) {
				t.Fatalf("unscoped Since failed with %v; the only error the contract allows wraps ErrUnscopedQuery", err)
			}
			return
		}
		n++
	}
	if n != 1 {
		t.Fatalf("unscoped Since served %d entries, want 1 (or a clean ErrUnscopedQuery)", n)
	}
}

func testHighestSeq(t *testing.T, b Backend) {
	tr := open(t, b)
	ctx := context.Background()
	if got, err := tr.HighestSeq(ctx, "app", "user", "root-h"); err != nil || got != 0 {
		t.Fatalf("HighestSeq on an empty tree = %d, %v; want 0, nil", got, err)
	}
	sess := create(t, tr, "root-h")
	child := create(t, tr, "root-h:sub:x:1")
	appendEv(t, ctx, tr, sess, event("h1", "model", "", "a"))
	appendEv(t, ctx, tr, child, event("h2", "x", "x", "b"))
	all := collect(t, tr, 0, eventlog.WithSessionTree("app", "user", "root-h"))
	got, err := tr.HighestSeq(ctx, "app", "user", "root-h")
	if err != nil {
		t.Fatalf("HighestSeq: %v", err)
	}
	if want := all[len(all)-1].Seq; got != want {
		t.Fatalf("HighestSeq = %d, want %d (the child's event counts for the tree)", got, want)
	}
}

func testMetadata(t *testing.T, b Backend) {
	tr := open(t, b)
	sess := create(t, tr, "root-m")
	md := map[string]string{"caller": "alice", "proxy_by": "gateway"}
	ctx := eventlog.ContextWithEntryMetadata(context.Background(), md)
	appendEv(t, ctx, tr, sess, event("m1", "model", "", "x"))
	appendEv(t, context.Background(), tr, sess, event("m2", "model", "", "y"))
	got := collect(t, tr, 0, eventlog.ForSession("app", "user", "root-m"))
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2", len(got))
	}
	if !maps.Equal(got[0].Metadata, md) {
		t.Fatalf("entry m1 metadata = %v, want %v", got[0].Metadata, md)
	}
	if len(got[1].Metadata) != 0 {
		t.Fatalf("entry m2 metadata = %v, want none", got[1].Metadata)
	}
}

func testSubscribe(t *testing.T, b Backend) {
	tr := open(t, b)
	ctx := context.Background()
	sess := create(t, tr, "root-s")
	wake, cancel := tr.Subscribe("app", "user", "root-s")
	defer cancel()
	select {
	case <-wake:
		t.Fatal("wake-up before any append")
	default:
	}
	appendEv(t, ctx, tr, sess, event("s1", "model", "", "a"))
	select {
	case <-wake:
	case <-time.After(b.WakeTimeout):
		t.Fatal("no wake-up after AppendEvent")
	}
	// Coalescing: several appends before the reader looks are one
	// wake-up at most, and Since from the cursor catches all of them.
	for i := 2; i <= 4; i++ {
		appendEv(t, ctx, tr, sess, event(fmt.Sprintf("s%d", i), "model", "", "b"))
	}
	select {
	case <-wake:
	case <-time.After(b.WakeTimeout):
		t.Fatal("no wake-up after a burst of appends")
	}
	if got := collect(t, tr, 0, eventlog.ForSession("app", "user", "root-s")); len(got) != 4 {
		t.Fatalf("Since after burst = %d entries, want 4", len(got))
	}
	// A different tree's append does not wake this subscriber.
	other := create(t, tr, "root-s2")
	select {
	case <-wake:
	default:
	}
	appendEv(t, ctx, tr, other, event("z1", "model", "", "c"))
	select {
	case <-wake:
		t.Fatal("woken by an append to another tree")
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	appendEv(t, ctx, tr, sess, event("s5", "model", "", "d"))
	select {
	case _, ok := <-wake:
		if ok {
			t.Fatal("woken after cancel")
		}
	case <-time.After(50 * time.Millisecond):
	}
}

func testNotFound(t *testing.T, b Backend) {
	tr := open(t, b)
	_, err := tr.Service().Get(context.Background(), &session.GetRequest{AppName: "app", UserID: "user", SessionID: "never"})
	if err == nil {
		t.Fatal("Get of a missing session returned no error")
	}
	if !eventlog.IsSessionNotFound(err) {
		t.Fatalf("Get of a missing session returned %v; IsSessionNotFound must classify it", err)
	}
}

func testDelete(t *testing.T, b Backend) {
	tr := open(t, b)
	ctx := context.Background()
	root := create(t, tr, "root-d")
	child := create(t, tr, "root-d:sub:k:1")
	keep := create(t, tr, "root-keep")
	appendEv(t, ctx, tr, root, event("d1", "model", "", "a"))
	appendEv(t, ctx, tr, child, event("d2", "k", "k", "b"))
	appendEv(t, ctx, tr, keep, event("k1", "model", "", "c"))
	if err := tr.Service().Delete(ctx, &session.DeleteRequest{AppName: "app", UserID: "user", SessionID: "root-d"}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got := collect(t, tr, 0, eventlog.WithSessionTree("app", "user", "root-d")); len(got) != 0 {
		t.Fatalf("tree still has %d entries after Delete of its root: %v", len(got), ids(got))
	}
	if _, err := tr.Service().Get(ctx, &session.GetRequest{AppName: "app", UserID: "user", SessionID: "root-d"}); !eventlog.IsSessionNotFound(err) {
		t.Fatalf("Get after Delete = %v, want not-found", err)
	}
	if got := collect(t, tr, 0, eventlog.ForSession("app", "user", "root-keep")); len(got) != 1 {
		t.Fatalf("unrelated tree lost entries: %d, want 1", len(got))
	}
	// A child that was created but never appended to goes with its root.
	create(t, tr, "root-d2")
	create(t, tr, "root-d2:sub:quiet:1")
	if err := tr.Service().Delete(ctx, &session.DeleteRequest{AppName: app, UserID: "user", SessionID: "root-d2"}); err != nil {
		t.Fatalf("Delete root-d2: %v", err)
	}
	if _, err := tr.Service().Get(ctx, &session.GetRequest{AppName: app, UserID: "user", SessionID: "root-d2:sub:quiet:1"}); !eventlog.IsSessionNotFound(err) {
		t.Fatalf("quiet child after root Delete = %v, want not-found", err)
	}
	// Re-creating a deleted root continues the tree's sequence rather
	// than restarting it: a reader's cursor from before the delete must
	// not skip the new session's first events.
	high, err := tr.HighestSeq(ctx, app, "user", "root-d")
	if err != nil {
		t.Fatalf("HighestSeq after Delete: %v", err)
	}
	again := create(t, tr, "root-d")
	appendEv(t, ctx, tr, again, event("d3", "model", "", "again"))
	got := collect(t, tr, 0, eventlog.ForSession(app, "user", "root-d"))
	if len(got) != 1 || got[0].Seq <= high || got[0].Seq <= 2 {
		t.Fatalf("after delete+recreate: entries=%d seq=%d; want 1 entry with seq past the deleted ones (>2, >%d)", len(got), got[0].Seq, high)
	}
}

// testFidelity appends one event carrying every field that resume,
// the usage tracker, the auto-continue classifier and Anthropic
// tool_use pairing read back, and compares what Get and Since return.
// FinishReason and ModelVersion are deliberately absent: no ADK store
// persists them, which is why eventlog.service stamps them into
// CustomMetadata before the backend sees the event.
func testFidelity(t *testing.T, b Backend) {
	tr := open(t, b)
	ctx := context.Background()
	sess := create(t, tr, "root-x")

	ev := &session.Event{
		ID:                 "fx1",
		Author:             "model",
		Branch:             "parent.child",
		InvocationID:       "inv-fx1",
		Timestamp:          time.Date(2026, 11, 6, 12, 0, 0, 123456000, time.UTC),
		LongRunningToolIDs: []string{"lr-1"},
	}
	ev.Content = &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{
		genai.NewPartFromText("calling"),
		{FunctionCall: &genai.FunctionCall{ID: "call-1", Name: "read_file", Args: map[string]any{"path": "/etc/hosts"}}},
		{FunctionResponse: &genai.FunctionResponse{ID: "call-1", Name: "read_file", Response: map[string]any{"ok": true}}},
	}}
	ev.UsageMetadata = &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 120, CandidatesTokenCount: 30, TotalTokenCount: 150, CachedContentTokenCount: 100}
	ev.CustomMetadata = map[string]any{"finish_reason": "MAX_TOKENS", "model_version": "gemini-x"}
	ev.GroundingMetadata = &genai.GroundingMetadata{WebSearchQueries: []string{"what is seq"}}
	ev.ErrorCode = "E42"
	ev.ErrorMessage = "boom"
	ev.Actions = session.EventActions{
		StateDelta: map[string]any{"k": "v"},
		Compaction: &session.EventCompaction{
			StartTimestamp:   time.Date(2026, 11, 6, 11, 0, 0, 0, time.UTC),
			EndTimestamp:     time.Date(2026, 11, 6, 11, 30, 0, 0, time.UTC),
			CompactedContent: genai.NewContentFromText("summary", genai.RoleModel),
		},
	}
	appendEv(t, ctx, tr, sess, ev)

	resp, err := tr.Service().Get(ctx, &session.GetRequest{AppName: "app", UserID: "user", SessionID: "root-x"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if n := resp.Session.Events().Len(); n != 1 {
		t.Fatalf("Get returned %d events, want 1", n)
	}
	checkFidelity(t, "Get", ev, resp.Session.Events().At(0))
	if v, err := resp.Session.State().Get("k"); err != nil || v != "v" {
		t.Errorf("session state after StateDelta: k = %v, %v; want \"v\"", v, err)
	}
	got := collect(t, tr, 0, eventlog.ForSession("app", "user", "root-x"))
	if len(got) != 1 {
		t.Fatalf("Since returned %d entries, want 1", len(got))
	}
	checkFidelity(t, "Since", ev, got[0].Event)
}

func checkFidelity(t *testing.T, via string, want, got *session.Event) {
	t.Helper()
	check := func(field string, w, g any) {
		t.Helper()
		if !reflect.DeepEqual(w, g) {
			t.Errorf("%s: %s = %#v, want %#v", via, field, g, w)
		}
	}
	check("ID", want.ID, got.ID)
	check("Author", want.Author, got.Author)
	check("Branch", want.Branch, got.Branch)
	check("InvocationID", want.InvocationID, got.InvocationID)
	if !want.Timestamp.Equal(got.Timestamp.Truncate(time.Microsecond)) {
		t.Errorf("%s: Timestamp = %v, want %v (to the microsecond)", via, got.Timestamp, want.Timestamp)
	}
	check("LongRunningToolIDs", want.LongRunningToolIDs, got.LongRunningToolIDs)
	if got.Content == nil || len(got.Content.Parts) != 3 {
		t.Fatalf("%s: Content = %#v, want 3 parts", via, got.Content)
	}
	check("Content.Parts[0].Text", want.Content.Parts[0].Text, got.Content.Parts[0].Text)
	check("FunctionCall.ID", want.Content.Parts[1].FunctionCall.ID, got.Content.Parts[1].FunctionCall.ID)
	check("FunctionCall.Name", want.Content.Parts[1].FunctionCall.Name, got.Content.Parts[1].FunctionCall.Name)
	check("FunctionCall.Args", want.Content.Parts[1].FunctionCall.Args, got.Content.Parts[1].FunctionCall.Args)
	check("FunctionResponse.ID", want.Content.Parts[2].FunctionResponse.ID, got.Content.Parts[2].FunctionResponse.ID)
	check("FunctionResponse.Response", want.Content.Parts[2].FunctionResponse.Response, got.Content.Parts[2].FunctionResponse.Response)
	if got.UsageMetadata == nil {
		t.Errorf("%s: UsageMetadata dropped", via)
	} else {
		check("UsageMetadata.PromptTokenCount", want.UsageMetadata.PromptTokenCount, got.UsageMetadata.PromptTokenCount)
		check("UsageMetadata.CandidatesTokenCount", want.UsageMetadata.CandidatesTokenCount, got.UsageMetadata.CandidatesTokenCount)
		check("UsageMetadata.CachedContentTokenCount", want.UsageMetadata.CachedContentTokenCount, got.UsageMetadata.CachedContentTokenCount)
	}
	check("CustomMetadata", want.CustomMetadata, got.CustomMetadata)
	if got.GroundingMetadata == nil {
		t.Errorf("%s: GroundingMetadata dropped", via)
	} else {
		check("GroundingMetadata.WebSearchQueries", want.GroundingMetadata.WebSearchQueries, got.GroundingMetadata.WebSearchQueries)
	}
	check("ErrorCode", want.ErrorCode, got.ErrorCode)
	check("ErrorMessage", want.ErrorMessage, got.ErrorMessage)
	check("Actions.StateDelta", want.Actions.StateDelta, got.Actions.StateDelta)
	if got.Actions.Compaction == nil {
		t.Errorf("%s: Actions.Compaction dropped", via)
	} else {
		check("Compaction.CompactedContent", want.Actions.Compaction.CompactedContent, got.Actions.Compaction.CompactedContent)
		if !want.Actions.Compaction.StartTimestamp.Equal(got.Actions.Compaction.StartTimestamp) ||
			!want.Actions.Compaction.EndTimestamp.Equal(got.Actions.Compaction.EndTimestamp) {
			t.Errorf("%s: Compaction window = %v..%v, want %v..%v", via,
				got.Actions.Compaction.StartTimestamp, got.Actions.Compaction.EndTimestamp,
				want.Actions.Compaction.StartTimestamp, want.Actions.Compaction.EndTimestamp)
		}
	}
}

func testHandover(t *testing.T, b Backend) {
	if b.Reopen == nil {
		t.Skip("backend has no persistence to hand over")
	}
	first := b.Open(t)
	t.Cleanup(func() { _ = first.Close() })
	ctx := context.Background()
	sess := create(t, first, "root-ho")
	appendEv(t, ctx, first, sess, event("ho1", "model", "", "a"))
	appendEv(t, ctx, first, sess, event("ho2", "model", "", "b"))
	before := collect(t, first, 0, eventlog.ForSession("app", "user", "root-ho"))
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	second := b.Reopen(t, first)
	t.Cleanup(func() { _ = second.Close() })
	high, err := second.HighestSeq(ctx, "app", "user", "root-ho")
	if err != nil {
		t.Fatalf("HighestSeq after reopen: %v", err)
	}
	if want := before[1].Seq; high != want {
		t.Fatalf("HighestSeq after reopen = %d, want %d", high, want)
	}
	resp, err := second.Service().Get(ctx, &session.GetRequest{AppName: "app", UserID: "user", SessionID: "root-ho"})
	if err != nil {
		t.Fatalf("Get after reopen: %v", err)
	}
	appendEv(t, ctx, second, resp.Session, event("ho3", "model", "", "c"))
	after := collect(t, second, 0, eventlog.ForSession("app", "user", "root-ho"))
	if want := []string{"ho1", "ho2", "ho3"}; !slices.Equal(ids(after), want) {
		t.Fatalf("after handover ids = %v, want %v", ids(after), want)
	}
	if after[2].Seq <= before[1].Seq {
		t.Fatalf("seq after handover %d does not continue past %d", after[2].Seq, before[1].Seq)
	}
	if after[0].Seq != before[0].Seq || after[1].Seq != before[1].Seq {
		t.Fatalf("existing seqs changed across handover: %v -> %v", []int64{before[0].Seq, before[1].Seq}, []int64{after[0].Seq, after[1].Seq})
	}
}
