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

package eventlog_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v3/pkg/eventlog"
	"github.com/go-steer/core-agent/v3/pkg/statestore/memory"
)

// openMemoryHandle builds a Handle over the memory transcript with a
// fresh SQLite file for the local stores — the shape OpenTranscript
// exists for, with the one non-SQL backend that needs no credentials.
func openMemoryHandle(t *testing.T, opts ...eventlog.Option) (*eventlog.Handle, *memory.Transcript) {
	t.Helper()
	tr := memory.New()
	h, err := eventlog.OpenTranscript(context.Background(), tr, sqlite.Open(filepath.Join(t.TempDir(), "local.db")), opts...)
	if err != nil {
		t.Fatalf("OpenTranscript: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h, tr
}

func textEvent(id, text string) *session.Event {
	ev := &session.Event{ID: id, Author: "model", InvocationID: "inv-" + id, Timestamp: time.Now()}
	ev.Content = genai.NewContentFromText(text, genai.RoleModel)
	return ev
}

func TestOpenTranscript_RequiresTranscript(t *testing.T) {
	t.Parallel()
	if _, err := eventlog.OpenTranscript(context.Background(), nil, sqlite.Open(filepath.Join(t.TempDir(), "x.db"))); err == nil {
		t.Fatal("OpenTranscript(nil) succeeded")
	}
}

// The Handle's Service appends through the transcript (which assigns
// seq) and applies guardrail state locally; its Stream reads the
// transcript. Nothing goes through an overlay table.
func TestOpenTranscript_AppendLandsInTranscriptAndLocalGuardrail(t *testing.T) {
	t.Parallel()
	h, tr := openMemoryHandle(t)
	ctx := context.Background()
	resp, err := h.Service.Create(ctx, &session.CreateRequest{AppName: "app", UserID: "u", SessionID: "s1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := h.Service.AppendEvent(ctx, resp.Session, textEvent("e1", "hello")); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	var seen []int64
	for e, err := range h.Stream.Since(ctx, 0, eventlog.ForSession("app", "u", "s1")) {
		if err != nil {
			t.Fatalf("Since: %v", err)
		}
		seen = append(seen, e.Seq)
	}
	if len(seen) != 1 || seen[0] != 1 {
		t.Fatalf("Since seqs = %v, want [1]", seen)
	}
	if high, err := tr.HighestSeq(ctx, "app", "u", "s1"); err != nil || high != 1 {
		t.Fatalf("transcript HighestSeq = %d, %v; want 1", high, err)
	}
	// Guardrail state is a local-store concern and must still be kept:
	// Create seeds the row (Load alone cannot tell a seeded row from a
	// missing one, so count it).
	var gr int64
	if err := h.DB.Table("agent_guardrail_state").Where("session_id = ?", "s1").Count(&gr).Error; err != nil || gr != 1 {
		t.Fatalf("guardrail rows after Create+Append on a transcript Handle = %d, %v; want 1", gr, err)
	}
	// The overlay table exists (Ping reads it) but holds nothing.
	var n int64
	if err := h.DB.Table("agent_eventlog").Count(&n).Error; err != nil || n != 0 {
		t.Fatalf("overlay rows = %d, %v; want 0 on a transcript Handle", n, err)
	}
	if err := h.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

func TestOpenTranscript_StreamAppendIsNotTheWriteSeam(t *testing.T) {
	t.Parallel()
	h, _ := openMemoryHandle(t)
	ctx := context.Background()
	resp, err := h.Service.Create(ctx, &session.CreateRequest{AppName: "app", UserID: "u", SessionID: "s1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := h.Stream.Append(ctx, resp.Session, textEvent("e1", "x")); !errors.Is(err, eventlog.ErrAppendViaService) {
		t.Fatalf("Stream.Append error = %v, want ErrAppendViaService", err)
	}
}

// Watch over a transcript Handle is wake-up driven: an event appended
// after Watch started arrives without a poll interval elapsing.
func TestOpenTranscript_WatchIsLiveWithoutPolling(t *testing.T) {
	t.Parallel()
	h, _ := openMemoryHandle(t, eventlog.WithWatchInterval(time.Hour))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := h.Service.Create(ctx, &session.CreateRequest{AppName: "app", UserID: "u", SessionID: "s1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := h.Service.AppendEvent(ctx, resp.Session, textEvent("e1", "before")); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	got := make(chan string, 4)
	go func() {
		for e, err := range h.Stream.Watch(ctx, 0, eventlog.ForSession("app", "u", "s1")) {
			if err != nil {
				got <- "err:" + err.Error()
				return
			}
			got <- e.Event.ID
		}
	}()
	if id := <-got; id != "e1" {
		t.Fatalf("first watched id = %q, want e1 (replay)", id)
	}
	if err := h.Service.AppendEvent(ctx, resp.Session, textEvent("e2", "after")); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	select {
	case id := <-got:
		if id != "e2" {
			t.Fatalf("live watched id = %q, want e2", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Watch did not deliver the live event (the poll interval is an hour, so it must come from a wake-up)")
	}
}

func TestOpenTranscript_WatchRefusesUnscopedQuery(t *testing.T) {
	t.Parallel()
	h, _ := openMemoryHandle(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for _, err := range h.Stream.Watch(ctx, 0) {
		if !errors.Is(err, eventlog.ErrUnscopedQuery) {
			t.Fatalf("unscoped Watch yielded %v, want ErrUnscopedQuery", err)
		}
		return
	}
	t.Fatal("unscoped Watch returned without an error")
}

func TestOpenTranscript_MetadataExtractorReachesTheBackend(t *testing.T) {
	t.Parallel()
	h, _ := openMemoryHandle(t, eventlog.WithMetadataExtractor(func(context.Context) map[string]string {
		return map[string]string{"caller": "alice"}
	}))
	ctx := context.Background()
	resp, err := h.Service.Create(ctx, &session.CreateRequest{AppName: "app", UserID: "u", SessionID: "s1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := h.Service.AppendEvent(ctx, resp.Session, textEvent("e1", "x")); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	for e, err := range h.Stream.Since(ctx, 0, eventlog.ForSession("app", "u", "s1")) {
		if err != nil {
			t.Fatalf("Since: %v", err)
		}
		if e.Metadata["caller"] != "alice" {
			t.Fatalf("Entry.Metadata = %v, want caller=alice", e.Metadata)
		}
	}
}

func TestOpenTranscript_DeleteAndPartialTakeTheNoOverlayPath(t *testing.T) {
	t.Parallel()
	h, _ := openMemoryHandle(t)
	ctx := context.Background()
	resp, err := h.Service.Create(ctx, &session.CreateRequest{AppName: "app", UserID: "u", SessionID: "s1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	p := textEvent("p", "half")
	p.Partial = true
	if err := h.Service.AppendEvent(ctx, resp.Session, p); err != nil {
		t.Fatalf("AppendEvent(partial): %v", err)
	}
	if err := h.Service.AppendEvent(ctx, resp.Session, textEvent("e1", "x")); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	// Delete used to dereference the overlay stream unconditionally; a
	// transcript Handle has none.
	if err := h.Service.Delete(ctx, &session.DeleteRequest{AppName: "app", UserID: "u", SessionID: "s1"}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := h.Service.Get(ctx, &session.GetRequest{AppName: "app", UserID: "u", SessionID: "s1"}); !eventlog.IsSessionNotFound(err) {
		t.Fatalf("Get after Delete = %v, want not-found", err)
	}
	var rows int64
	if err := h.DB.Table("agent_guardrail_state").Where("session_id = ?", "s1").Count(&rows).Error; err != nil || rows != 0 {
		t.Fatalf("guardrail rows after Delete = %d, %v; want 0", rows, err)
	}
}

func TestOpenTranscript_CloseClosesBoth(t *testing.T) {
	t.Parallel()
	h, tr := openMemoryHandle(t)
	if err := h.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := tr.HighestSeq(context.Background(), "app", "u", "s"); !errors.Is(err, eventlog.ErrClosed) {
		t.Fatalf("transcript still open after Handle.Close: %v", err)
	}
	if err := h.Ping(context.Background()); err == nil {
		t.Fatal("Ping succeeded after Close")
	}
}
