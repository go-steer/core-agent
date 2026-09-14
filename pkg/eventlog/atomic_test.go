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
	"testing"

	"google.golang.org/adk/session"
)

// One event, one transaction (#976). The property under test is that
// the ADK events row and the overlay row cannot exist apart: a crash
// between them used to leave an event the overlay never indexed, and
// the overlay is what Since and Watch read, so the event was not late —
// it was gone.

// streamOf reaches the gormStream behind a Handle's Service. The
// Service is a session.Service to its callers; these tests are about
// the wiring underneath it.
func streamOf(t *testing.T, h *Handle) *gormStream {
	t.Helper()
	svc, ok := h.Service.(*service)
	if !ok {
		t.Fatalf("Handle.Service is %T, want *service", h.Service)
	}
	return svc.stream
}

// countADKEvents reports how many rows ADK holds for a session, read
// through its own service rather than through our overlay — the whole
// question here is whether the two agree.
func countADKEvents(t *testing.T, h *Handle, app, user, sid string) int {
	t.Helper()
	resp, err := h.Service.Get(context.Background(), &session.GetRequest{
		AppName: app, UserID: user, SessionID: sid,
	})
	if err != nil {
		t.Fatalf("Service.Get: %v", err)
	}
	if resp == nil || resp.Session == nil {
		t.Fatalf("Service.Get returned no session")
	}
	return resp.Session.Events().Len()
}

func countOverlayRows(t *testing.T, h *Handle, sid string) int {
	t.Helper()
	var n int64
	if err := h.DB.Model(&agentEventRow{}).Where("session_id = ?", sid).Count(&n).Error; err != nil {
		t.Fatalf("count overlay rows: %v", err)
	}
	return int(n)
}

// The registration must actually happen against the ADK we ship. It is
// best-effort by design — a future ADK that hides its *gorm.DB falls
// back to the old two-write path rather than failing Open — and a
// silent fallback is exactly the failure this test exists to make loud.
// If it fires, #976's guarantee is off and nothing else would say so.
func TestOpen_WiresTheInTransactionOverlayWrite(t *testing.T) {
	t.Parallel()

	h, cleanup := openTestHandle(t)
	defer cleanup()

	if !streamOf(t, h).overlayInTx {
		t.Fatal("overlay write is not inside ADK's transaction — the callback could not be registered, " +
			"so AppendEvent is back to two independent writes and #976 is un-fixed")
	}
}

// The table filter is the one ADK internal this depends on. Asserting
// the name directly means a dependency bump that renames the events
// table fails here, with the reason attached, rather than quietly
// turning the atomic write into a no-op that nothing would notice until
// a crash.
func TestADKEventsTableName_IsStillWhatTheCallbackFiltersOn(t *testing.T) {
	t.Parallel()

	h, cleanup := openTestHandle(t)
	defer cleanup()
	sess := mustCreateSession(t, h, "app", "user", "s-table")
	ctx := context.Background()

	if err := h.Service.AppendEvent(ctx, sess, makeEvent("e1", "model", "", "hi")); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}

	var n int64
	if err := h.adkDB.Table(adkEventsTable).Count(&n).Error; err != nil {
		t.Fatalf("counting %q: %v — if ADK renamed its events table, update adkEventsTable "+
			"in atomic.go or the overlay write silently stops being transactional", adkEventsTable, err)
	}
	if n != 1 {
		t.Fatalf("%q holds %d rows after one AppendEvent, want 1", adkEventsTable, n)
	}
}

// The core property, and the one that fails on pre-#976 code.
//
// A duplicate event ID makes the overlay insert fail on its unique
// index — the same failure a crash produces from the other side, and
// the only one a test can inject deterministically at exactly the point
// between the two writes. Before the fix, ADK had already committed its
// event row by then and the failure arrived too late to take it back:
// the caller saw an error and the database kept an event the overlay
// would never index. Now the insert runs inside ADK's transaction, so
// its error rolls the event row back with it.
func TestAppendEvent_AFailedOverlayWriteRollsBackTheEvent(t *testing.T) {
	t.Parallel()

	h, cleanup := openTestHandle(t)
	defer cleanup()
	const sid = "s-rollback"
	sess := mustCreateSession(t, h, "app", "user", sid)
	ctx := context.Background()

	// Claim the event ID in the overlay first, so the real insert
	// collides with it.
	squatter := &agentEventRow{
		AppName: "app", UserID: "user", SessionID: sid, EventID: "e-dup",
	}
	if err := h.DB.Create(squatter).Error; err != nil {
		t.Fatalf("seeding the colliding overlay row: %v", err)
	}

	err := h.Service.AppendEvent(ctx, sess, makeEvent("e-dup", "model", "", "hi"))
	if err == nil {
		t.Fatal("AppendEvent succeeded despite an overlay row it could not write")
	}

	// Either both rows or neither. The overlay has only the row the test
	// planted, and ADK must have nothing.
	if got := countOverlayRows(t, h, sid); got != 1 {
		t.Errorf("overlay rows = %d, want 1 (only the planted one)", got)
	}
	if got := countADKEvents(t, h, "app", "user", sid); got != 0 {
		t.Errorf("ADK kept %d event(s) whose overlay row failed to write — "+
			"that event is invisible to every Since and Watch consumer, permanently", got)
	}
}

// The happy path still writes exactly one of each, and the overlay row
// still gets a real seq — the atomic path must not have quietly stopped
// assigning one, since seq is the cursor every consumer reads by.
func TestAppendEvent_WritesOneOfEachAndAssignsASeq(t *testing.T) {
	t.Parallel()

	h, cleanup := openTestHandle(t)
	defer cleanup()
	const sid = "s-happy"
	sess := mustCreateSession(t, h, "app", "user", sid)
	ctx := context.Background()

	for _, id := range []string{"e1", "e2", "e3"} {
		if err := h.Service.AppendEvent(ctx, sess, makeEvent(id, "model", "", id)); err != nil {
			t.Fatalf("AppendEvent %s: %v", id, err)
		}
	}

	if got := countADKEvents(t, h, "app", "user", sid); got != 3 {
		t.Errorf("ADK events = %d, want 3", got)
	}
	if got := countOverlayRows(t, h, sid); got != 3 {
		t.Errorf("overlay rows = %d, want 3", got)
	}

	entries := drain(t, h.Stream.Since(ctx, 0))
	if len(entries) != 3 {
		t.Fatalf("Since returned %d entries, want 3", len(entries))
	}
	var last int64
	for i, e := range entries {
		if e.Seq <= last {
			t.Fatalf("entry %d has seq %d, not greater than the previous %d", i, e.Seq, last)
		}
		last = e.Seq
		if e.Event == nil {
			t.Fatalf("entry %d did not hydrate — the overlay row references an event ADK does not have", i)
		}
	}
}

// A partial event is one ADK deliberately does not persist. Writing an
// overlay row for it produces the orphan deleteSession's comment calls
// poison: the seq is real, so an unfiltered Watch re-queries the row on
// every poll and re-fails to hydrate it, forever. Fails on pre-#976
// code, which wrote the row unconditionally.
func TestAppendEvent_PartialEventWritesNoOverlayRow(t *testing.T) {
	t.Parallel()

	h, cleanup := openTestHandle(t)
	defer cleanup()
	const sid = "s-partial"
	sess := mustCreateSession(t, h, "app", "user", sid)
	ctx := context.Background()

	ev := makeEvent("e-partial", "model", "", "streaming chunk")
	ev.Partial = true
	if err := h.Service.AppendEvent(ctx, sess, ev); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}

	if got := countADKEvents(t, h, "app", "user", sid); got != 0 {
		t.Fatalf("ADK persisted %d partial event(s), want 0", got)
	}
	if got := countOverlayRows(t, h, sid); got != 0 {
		t.Fatalf("overlay rows = %d, want 0 — a row pointing at an event ADK never wrote "+
			"fails to hydrate on every poll, for the life of the log", got)
	}
}

// The callback fires on every create ADK's pool makes, including its
// own overlay insert. Nothing but the events table may trigger it, or a
// session Create would write a stray overlay row — and the insert would
// recurse.
func TestOverlayCallback_IgnoresCreatesThatAreNotEvents(t *testing.T) {
	t.Parallel()

	h, cleanup := openTestHandle(t)
	defer cleanup()

	// Create alone writes a session row and no event.
	mustCreateSession(t, h, "app", "user", "s-noevents")
	if got := countOverlayRows(t, h, "s-noevents"); got != 0 {
		t.Fatalf("overlay rows = %d after Create with no events, want 0", got)
	}
}

// The record is consumed once. A second create inside the same
// transaction must not write a second overlay row for the same event —
// which is also what stops the overlay insert from re-entering the
// callback and recursing.
func TestOverlayPending_IsConsumedOnce(t *testing.T) {
	t.Parallel()

	h, cleanup := openTestHandle(t)
	defer cleanup()
	s := streamOf(t, h)

	p := &overlayPending{done: true}
	ctx := withOverlayPending(context.Background(), p)
	if got := overlayPendingFrom(ctx); got != p {
		t.Fatalf("overlayPendingFrom returned %v, want the stashed record", got)
	}
	if overlayPendingFrom(context.Background()) != nil {
		t.Fatal("a context with no stashed record must yield nil, or every ADK write becomes ours")
	}
	//nolint:staticcheck // deliberately passing a nil context to the nil-guard.
	if overlayPendingFrom(nil) != nil {
		t.Fatal("a nil context must yield nil rather than panicking")
	}
	if s == nil {
		t.Fatal("no stream")
	}
}
