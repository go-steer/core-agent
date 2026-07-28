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

package agent

import (
	"context"
	"testing"

	"google.golang.org/adk/session"
	"google.golang.org/genai"
)

// These tests pin the compactingService CRUD pass-through against a
// REAL session.InMemoryService (#396). The wrapper sits between the
// runner and the session store — a bug in Create/List/Delete
// delegation would be silent data loss (sessions created into the
// void, deletes that don't delete), so each method is exercised
// end-to-end through the wrapper with reads verified on BOTH sides
// (through the wrapper and directly against the inner service).

func TestCompactingService_CreateRoundTripsThroughWrapper(t *testing.T) {
	t.Parallel()
	inner := session.InMemoryService()
	wrapped := &compactingService{inner: inner}
	ctx := context.Background()

	const appName, userID, sessionID = "app", "user", "sess-create"
	created, err := wrapped.Create(ctx, &session.CreateRequest{
		AppName: appName, UserID: userID, SessionID: sessionID,
	})
	if err != nil {
		t.Fatalf("wrapped.Create: %v", err)
	}
	if created == nil || created.Session == nil {
		t.Fatal("wrapped.Create returned nil session")
	}
	if created.Session.ID() != sessionID {
		t.Errorf("created session ID = %q, want %q", created.Session.ID(), sessionID)
	}

	// The session must exist in the REAL underlying store, not in
	// some wrapper-local shadow.
	got, err := inner.Get(ctx, &session.GetRequest{
		AppName: appName, UserID: userID, SessionID: sessionID,
	})
	if err != nil {
		t.Fatalf("inner.Get after wrapped.Create: %v", err)
	}
	if got.Session.ID() != sessionID {
		t.Errorf("inner store session ID = %q, want %q", got.Session.ID(), sessionID)
	}

	// Events appended through the wrapper survive the round-trip and
	// are readable back through BOTH the wrapper and the inner service.
	if err := wrapped.AppendEvent(ctx, created.Session, mkEvent(genai.RoleUser, "first prompt")); err != nil {
		t.Fatalf("wrapped.AppendEvent: %v", err)
	}
	for name, svc := range map[string]session.Service{"wrapped": wrapped, "inner": inner} {
		resp, err := svc.Get(ctx, &session.GetRequest{
			AppName: appName, UserID: userID, SessionID: sessionID,
		})
		if err != nil {
			t.Fatalf("%s.Get: %v", name, err)
		}
		var texts []string
		for ev := range resp.Session.Events().All() {
			texts = append(texts, contentText(ev.Content))
		}
		if len(texts) != 1 || texts[0] != "first prompt" {
			t.Errorf("%s view events = %v, want [first prompt]", name, texts)
		}
	}
}

func TestCompactingService_ListReturnsWhatCreateStored(t *testing.T) {
	t.Parallel()
	inner := session.InMemoryService()
	wrapped := &compactingService{inner: inner}
	ctx := context.Background()

	const appName, userID = "app", "user"
	want := map[string]bool{"s-a": true, "s-b": true, "s-c": true}
	for sid := range want {
		if _, err := wrapped.Create(ctx, &session.CreateRequest{
			AppName: appName, UserID: userID, SessionID: sid,
		}); err != nil {
			t.Fatalf("wrapped.Create(%s): %v", sid, err)
		}
	}

	resp, err := wrapped.List(ctx, &session.ListRequest{AppName: appName, UserID: userID})
	if err != nil {
		t.Fatalf("wrapped.List: %v", err)
	}
	got := map[string]bool{}
	for _, s := range resp.Sessions {
		got[s.ID()] = true
	}
	if len(got) != len(want) {
		t.Fatalf("List returned %d sessions %v, want %d", len(got), got, len(want))
	}
	for sid := range want {
		if !got[sid] {
			t.Errorf("List missing session %q (created through the wrapper)", sid)
		}
	}
}

func TestCompactingService_DeleteRemovesFromUnderlyingStore(t *testing.T) {
	t.Parallel()
	inner := session.InMemoryService()
	wrapped := &compactingService{inner: inner}
	ctx := context.Background()

	const appName, userID = "app", "user"
	for _, sid := range []string{"keep-me", "delete-me"} {
		if _, err := wrapped.Create(ctx, &session.CreateRequest{
			AppName: appName, UserID: userID, SessionID: sid,
		}); err != nil {
			t.Fatalf("Create(%s): %v", sid, err)
		}
	}

	if err := wrapped.Delete(ctx, &session.DeleteRequest{
		AppName: appName, UserID: userID, SessionID: "delete-me",
	}); err != nil {
		t.Fatalf("wrapped.Delete: %v", err)
	}

	// Gone from the real store...
	resp, err := inner.List(ctx, &session.ListRequest{AppName: appName, UserID: userID})
	if err != nil {
		t.Fatalf("inner.List: %v", err)
	}
	for _, s := range resp.Sessions {
		if s.ID() == "delete-me" {
			t.Error("deleted session still present in the underlying store")
		}
	}
	// ...while the untouched session survives.
	if len(resp.Sessions) != 1 || resp.Sessions[0].ID() != "keep-me" {
		var ids []string
		for _, s := range resp.Sessions {
			ids = append(ids, s.ID())
		}
		t.Errorf("post-delete sessions = %v, want [keep-me]", ids)
	}
}
