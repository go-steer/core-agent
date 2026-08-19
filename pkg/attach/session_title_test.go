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

package attach

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-steer/core-agent/v2/pkg/auth"
)

// titledRegistrant is a stubRegistrant that also implements the
// optional SessionTitleProvider — the shape a #808-aware host has.
type titledRegistrant struct {
	*stubRegistrant
	title string
}

func (t *titledRegistrant) SessionTitle() string { return t.title }

// A pre-#808 registrant doesn't implement the capability at all, and
// the row must degrade to no title rather than to a panic — this is the
// feature-detection-by-type-assertion pattern, so the negative case is
// the one worth pinning.
func TestEntryTitle_UnimplementedCapabilityIsEmpty(t *testing.T) {
	t.Parallel()
	e := &Entry{Agent: &stubRegistrant{app: "core-agent", user: "u", sid: "s"}}
	if got := entryTitle(e); got != "" {
		t.Errorf("entryTitle on a plain Registrant = %q, want \"\"", got)
	}
	if got := entryTitle(nil); got != "" {
		t.Errorf("entryTitle(nil) = %q, want \"\"", got)
	}
}

func TestEntryTitle_ReadsTheCapability(t *testing.T) {
	t.Parallel()
	e := &Entry{Agent: &titledRegistrant{
		stubRegistrant: &stubRegistrant{app: "core-agent", user: "u", sid: "s"},
		title:          "Rotate the staging certs",
	}}
	if got, want := entryTitle(e), "Rotate the staging certs"; got != want {
		t.Errorf("entryTitle = %q, want %q", got, want)
	}
}

// The two halves of GET /sessions read the title from different places
// — the live registrant for active rows, the persisted row for idle
// ones — and a client can't tell which half produced a row. So both
// have to answer, or a session's name blinks out the moment it is
// evicted.
func TestListSessions_TitleFromBothHalves(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := newTestACLStore(t)
	reg := NewSessionRegistryWithStore(store)

	if _, err := reg.RegisterOwned(&titledRegistrant{
		stubRegistrant: &stubRegistrant{app: "core-agent", user: "alice@example.com", sid: "s-active"},
		title:          "Trace the flaky deploy",
	}, "alice@example.com"); err != nil {
		t.Fatalf("RegisterOwned: %v", err)
	}
	// Idle: a row on disk with no live entry behind it.
	if err := store.Put(ctx, SessionACLRow{
		AppName:   "core-agent",
		UserID:    "alice@example.com",
		SessionID: "s-idle",
		Owner:     "alice@example.com",
		Title:     "Write the migration notes",
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	h := &handlers{reg: reg, pool: newBroadcasterPool(), enforceACL: true}
	r := httptest.NewRequest(http.MethodGet, "/sessions", nil)
	r = r.WithContext(auth.WithCaller(r.Context(), auth.Caller{Identity: "alice@example.com"}))
	rr := httptest.NewRecorder()
	h.listSessions(rr, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("listSessions status = %d, body: %s", rr.Code, rr.Body.String())
	}

	var envelope struct {
		Sessions []sessionDescriptor `json:"sessions"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := map[string]string{
		"s-active": "Trace the flaky deploy",
		"s-idle":   "Write the migration notes",
	}
	got := make(map[string]string, len(envelope.Sessions))
	for _, d := range envelope.Sessions {
		got[d.SessionID] = d.Title
	}
	for sid, title := range want {
		if got[sid] != title {
			t.Errorf("row %s title = %q, want %q (all rows: %v)", sid, got[sid], title, got)
		}
	}
}

// GORM's Save is a whole-row upsert, so a Put that doesn't carry a
// Title would blank one that's already there. Sessions are re-Put on
// ownership changes long after their title was written, and losing the
// name on an ACL edit would look like the feature randomly failing.
func TestSessionACLStore_PutWithoutTitlePreservesIt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := newTestACLStore(t)
	base := SessionACLRow{
		AppName:   "core-agent",
		UserID:    "alice@example.com",
		SessionID: "s-1",
		Owner:     "alice@example.com",
		Title:     "Audit the IAM bindings",
	}
	if err := store.Put(ctx, base); err != nil {
		t.Fatalf("Put: %v", err)
	}

	untitled := base
	untitled.Title = ""
	untitled.Viewers = []string{"bob@example.com"}
	if err := store.Put(ctx, untitled); err != nil {
		t.Fatalf("Put (untitled): %v", err)
	}

	got, err := store.Get(ctx, "core-agent", "alice@example.com", "s-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Title != "Audit the IAM bindings" {
		t.Errorf("Title after an untitled Put = %q, want it preserved", got.Title)
	}
	if len(got.Viewers) != 1 || got.Viewers[0] != "bob@example.com" {
		t.Errorf("Viewers = %v, want the untitled Put's edit to have landed", got.Viewers)
	}
}

func TestSessionACLStore_SetTitle(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := newTestACLStore(t)
	if err := store.Put(ctx, SessionACLRow{
		AppName:   "core-agent",
		UserID:    "alice@example.com",
		SessionID: "s-1",
		Owner:     "alice@example.com",
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := store.SetTitle(ctx, "core-agent", "alice@example.com", "s-1", "Bisect the latency spike"); err != nil {
		t.Fatalf("SetTitle: %v", err)
	}
	got, err := store.Get(ctx, "core-agent", "alice@example.com", "s-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Title != "Bisect the latency spike" {
		t.Errorf("Title = %q, want the SetTitle value", got.Title)
	}
	if got.Owner != "alice@example.com" {
		t.Errorf("Owner = %q — SetTitle must touch one column, not the row", got.Owner)
	}
}

// A session with no ACL row (registered without an owner) is the
// ordinary case for a single-session daemon, not an error worth
// surfacing — but SetTitle still has to say so, because eviction
// swallows the answer and nothing else would.
func TestSessionACLStore_SetTitle_NoRow(t *testing.T) {
	t.Parallel()
	store := newTestACLStore(t)
	err := store.SetTitle(context.Background(), "core-agent", "alice@example.com", "nope", "x")
	if !errors.Is(err, ErrSessionACLNotFound) {
		t.Errorf("SetTitle on a missing row = %v, want ErrSessionACLNotFound", err)
	}
}

// Eviction is the handover: the entry leaves memory, and from then on
// GET /sessions can only answer from the row. If the title doesn't make
// that trip, a session's name disappears the first time it goes idle.
func TestEvictBefore_PersistsTheTitle(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := newTestACLStore(t)
	reg := NewSessionRegistryWithStore(store)
	if _, err := reg.RegisterOwned(&titledRegistrant{
		stubRegistrant: &stubRegistrant{app: "core-agent", user: "alice@example.com", sid: "s-1"},
		title:          "Reconcile the billing export",
	}, "alice@example.com"); err != nil {
		t.Fatalf("RegisterOwned: %v", err)
	}

	if n := reg.EvictBefore(time.Now().Add(time.Hour)); n != 1 {
		t.Fatalf("EvictBefore evicted %d, want 1", n)
	}
	got, err := store.Get(ctx, "core-agent", "alice@example.com", "s-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Title != "Reconcile the billing export" {
		t.Errorf("persisted title = %q, want the evicted entry's", got.Title)
	}
}

// An untitled eviction must not clear a title the row already carries:
// SetTitle("") would, so EvictBefore has to skip the write entirely.
// The case is real — an operator renames a session by hand, the agent
// itself never generated one, and then the session goes idle.
func TestEvictBefore_UntitledEntryLeavesTheRowAlone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := newTestACLStore(t)
	reg := NewSessionRegistryWithStore(store)
	if _, err := reg.RegisterOwned(&stubRegistrant{app: "core-agent", user: "alice@example.com", sid: "s-1"}, "alice@example.com"); err != nil {
		t.Fatalf("RegisterOwned: %v", err)
	}
	if err := store.SetTitle(ctx, "core-agent", "alice@example.com", "s-1", "Named by hand"); err != nil {
		t.Fatalf("SetTitle: %v", err)
	}

	if n := reg.EvictBefore(time.Now().Add(time.Hour)); n != 1 {
		t.Fatalf("EvictBefore evicted %d, want 1", n)
	}
	got, err := store.Get(ctx, "core-agent", "alice@example.com", "s-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Title != "Named by hand" {
		t.Errorf("title after an untitled eviction = %q, want it untouched", got.Title)
	}
}
