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

package digest_test

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"google.golang.org/adk/session"

	"github.com/go-steer/core-agent/pkg/digest"
	"github.com/go-steer/core-agent/pkg/eventlog"
)

// EventlogStore tests live in a _test package so we can import
// pkg/eventlog + gorm/sqlite without the package under test
// developing a hard dependency on either. Keeps pkg/digest's build
// closure lean for consumers who only want FilesystemStore.

const (
	testApp = "test-app"
	testUsr = "test-user"
	testSid = "test-session"
)

func newTestEventLog(t *testing.T) (*eventlog.Handle, func()) {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "session.db")
	h, err := eventlog.Open(context.Background(), sqlite.Open(dsn))
	if err != nil {
		t.Fatalf("eventlog.Open: %v", err)
	}
	// Seed the session so AppendEvent has a Session to write against.
	if _, err := h.Service.Create(context.Background(), &session.CreateRequest{
		AppName: testApp, UserID: testUsr, SessionID: testSid,
	}); err != nil {
		_ = h.Close()
		t.Fatalf("session create: %v", err)
	}
	return h, func() { _ = h.Close() }
}

func TestNewEventlogStore_RejectsNilHandle(t *testing.T) {
	t.Parallel()
	if _, err := digest.NewEventlogStore(nil, testApp, testUsr, testSid); err == nil {
		t.Error("expected error for nil handle")
	}
}

func TestNewEventlogStore_RejectsEmptySessionIdentity(t *testing.T) {
	t.Parallel()
	h, cleanup := newTestEventLog(t)
	defer cleanup()
	cases := []struct {
		name             string
		app, user, sid   string
		wantErrSubstring string
	}{
		{"empty app", "", testUsr, testSid, "empty session identity"},
		{"empty user", testApp, "", testSid, "empty session identity"},
		{"empty sid", testApp, testUsr, "", "empty session identity"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := digest.NewEventlogStore(h, tc.app, tc.user, tc.sid)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !contains(err.Error(), tc.wantErrSubstring) {
				t.Errorf("err = %v, want containing %q", err, tc.wantErrSubstring)
			}
		})
	}
}

func TestEventlogStore_PutGetRoundTrip(t *testing.T) {
	t.Parallel()
	h, cleanup := newTestEventLog(t)
	defer cleanup()
	store, err := digest.NewEventlogStore(h, testApp, testUsr, testSid)
	if err != nil {
		t.Fatalf("NewEventlogStore: %v", err)
	}

	payload := []byte(`{"raw":"tool response with binary \x00 and unicode ✓"}`)
	if err := store.Put(context.Background(), "call-abc", payload); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := store.Get(context.Background(), "call-abc")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("Get returned %q, want %q", got, payload)
	}
}

func TestEventlogStore_GetUnknownReturnsErrNotFound(t *testing.T) {
	t.Parallel()
	h, cleanup := newTestEventLog(t)
	defer cleanup()
	store, _ := digest.NewEventlogStore(h, testApp, testUsr, testSid)

	_, err := store.Get(context.Background(), "never-put")
	if !errors.Is(err, digest.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestEventlogStore_PutEmptyCallIDRejects(t *testing.T) {
	t.Parallel()
	h, cleanup := newTestEventLog(t)
	defer cleanup()
	store, _ := digest.NewEventlogStore(h, testApp, testUsr, testSid)

	if err := store.Put(context.Background(), "", []byte("x")); err == nil {
		t.Error("expected error for empty callID")
	}
}

func TestEventlogStore_PutOverwritesReturnsLatest(t *testing.T) {
	t.Parallel()
	// Multiple Puts under the same callID all land as separate events
	// (append-only log — we don't rewrite history). Get must return
	// the LATEST value so callers see the same "last write wins"
	// semantics FilesystemStore has.
	h, cleanup := newTestEventLog(t)
	defer cleanup()
	store, _ := digest.NewEventlogStore(h, testApp, testUsr, testSid)

	if err := store.Put(context.Background(), "call-1", []byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(context.Background(), "call-1", []byte("second")); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(context.Background(), "call-1")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "second" {
		t.Errorf("Get returned %q, want %q", got, "second")
	}
}

func TestEventlogStore_IsolatesAcrossCallIDs(t *testing.T) {
	t.Parallel()
	// Interleaved Puts under different callIDs must not cross-
	// contaminate — scan-based lookup is easy to get wrong.
	h, cleanup := newTestEventLog(t)
	defer cleanup()
	store, _ := digest.NewEventlogStore(h, testApp, testUsr, testSid)

	writes := map[string][]byte{
		"call-a": []byte("payload alpha"),
		"call-b": []byte("payload bravo"),
		"call-c": []byte("payload charlie"),
	}
	for id, p := range writes {
		if err := store.Put(context.Background(), id, p); err != nil {
			t.Fatalf("Put(%s): %v", id, err)
		}
	}
	for id, want := range writes {
		got, err := store.Get(context.Background(), id)
		if err != nil {
			t.Errorf("Get(%s): %v", id, err)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("Get(%s) = %q, want %q", id, got, want)
		}
	}
}

func TestEventlogStore_HandlesBinaryPayload(t *testing.T) {
	t.Parallel()
	// Base64 encoding is what makes this test important — a naive
	// text-only store would mangle high bits or NUL bytes. Prove
	// the round-trip is byte-exact.
	h, cleanup := newTestEventLog(t)
	defer cleanup()
	store, _ := digest.NewEventlogStore(h, testApp, testUsr, testSid)

	payload := make([]byte, 256)
	for i := range payload {
		payload[i] = byte(i)
	}
	if err := store.Put(context.Background(), "bin", payload); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(context.Background(), "bin")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("binary round-trip mismatch")
	}
}

func TestProcess_WithEventlogStore(t *testing.T) {
	t.Parallel()
	// End-to-end: Process wired with an EventlogStore + CallID
	// persists raw and the store can read it back. Same load-bearing
	// property retrieve_raw depends on, tested against the real
	// eventlog backend rather than the filesystem one.
	h, cleanup := newTestEventLog(t)
	defer cleanup()
	store, _ := digest.NewEventlogStore(h, testApp, testUsr, testSid)

	payload := []byte(`{"large":"tool response that would be pruned"}`)
	res, err := digest.Process(context.Background(), payload, digest.Options{
		Threshold: 0,
		Store:     store,
		CallID:    "toolcall-xyz",
	})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if res.CallID != "toolcall-xyz" {
		t.Errorf("CallID = %q, want toolcall-xyz", res.CallID)
	}
	got, err := store.Get(context.Background(), "toolcall-xyz")
	if err != nil {
		t.Fatalf("Store.Get: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("stored raw doesn't match original")
	}
}

// TestEventlogStore_PutDoesNotAdvanceParentSession is the regression
// gate for issue #273: EventlogStore.Put used to Get + AppendEvent
// against the parent session row, which bumped the ADK last_update_time
// and tripped optimistic-concurrency on the runner's mid-turn session
// snapshot ("stale session error"). After the fix Put writes to a
// derived <sid>:digest row and the parent row's update time is
// unchanged from Put — proven here by fetching the parent session
// before + after a Put and asserting the two snapshots have identical
// LastUpdateTime.
func TestEventlogStore_PutDoesNotAdvanceParentSession(t *testing.T) {
	t.Parallel()
	h, cleanup := newTestEventLog(t)
	defer cleanup()

	store, err := digest.NewEventlogStore(h, testApp, testUsr, testSid)
	if err != nil {
		t.Fatalf("NewEventlogStore: %v", err)
	}

	// Snapshot the parent session's LastUpdateTime BEFORE any Put.
	before, err := h.Service.Get(context.Background(), &session.GetRequest{
		AppName: testApp, UserID: testUsr, SessionID: testSid,
	})
	if err != nil {
		t.Fatalf("parent Get (before): %v", err)
	}
	beforeUpdated := before.Session.LastUpdateTime()

	if err := store.Put(context.Background(), "call-1", []byte("payload")); err != nil {
		t.Fatalf("Put: %v", err)
	}

	after, err := h.Service.Get(context.Background(), &session.GetRequest{
		AppName: testApp, UserID: testUsr, SessionID: testSid,
	})
	if err != nil {
		t.Fatalf("parent Get (after): %v", err)
	}
	afterUpdated := after.Session.LastUpdateTime()

	if !afterUpdated.Equal(beforeUpdated) {
		t.Errorf("parent session LastUpdateTime moved: before=%v after=%v — Put leaked onto the runner-visible row (issue #273 regressed)",
			beforeUpdated, afterUpdated)
	}
}

// TestEventlogStore_MultiplePutsShareDerivedSession is the sibling
// invariant: repeated Puts must not each try to Create the derived
// session — that would surface a duplicate-key error on the second
// Put. The sync.Once guard in ensureDerivedSession is what carries
// this invariant; regressing it (e.g. moving Create outside the Once)
// would fail this test.
func TestEventlogStore_MultiplePutsShareDerivedSession(t *testing.T) {
	t.Parallel()
	h, cleanup := newTestEventLog(t)
	defer cleanup()

	store, err := digest.NewEventlogStore(h, testApp, testUsr, testSid)
	if err != nil {
		t.Fatalf("NewEventlogStore: %v", err)
	}
	for i, id := range []string{"a", "b", "c"} {
		if err := store.Put(context.Background(), id, []byte("p")); err != nil {
			t.Fatalf("Put[%d]: %v", i, err)
		}
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (len(sub) == 0 || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
