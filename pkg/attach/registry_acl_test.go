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
	"errors"
	"sync"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/auth"
)

func TestRegister_DefaultsToEmptyACL(t *testing.T) {
	t.Parallel()
	// Legacy Register path must NOT stamp an owner — the result is an
	// admin-only-accessible session per the design doc's migration
	// story for sessions registered before multi-session shipped.
	reg := NewSessionRegistry()
	ag := &stubRegistrant{app: "core-agent", user: "u", sid: "s1"}
	entry, err := reg.Register(ag)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if entry.CurrentACL().Owner != "" {
		t.Errorf("Register stamped Owner %q; legacy path must leave it empty", entry.CurrentACL().Owner)
	}
}

func TestRegisterOwned_StampsOwner(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	ag := &stubRegistrant{app: "core-agent", user: "u", sid: "s1"}
	entry, err := reg.RegisterOwned(ag, "alice@example.com")
	if err != nil {
		t.Fatalf("RegisterOwned: %v", err)
	}
	if entry.CurrentACL().Owner != "alice@example.com" {
		t.Errorf("ACL.Owner: got %q, want %q", entry.CurrentACL().Owner, "alice@example.com")
	}
}

func TestRegisterOwned_RejectsEmptyOwner(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	ag := &stubRegistrant{app: "core-agent", user: "u", sid: "s1"}
	_, err := reg.RegisterOwned(ag, "")
	if err == nil {
		t.Fatal("RegisterOwned with empty owner must return an error (use Register for unowned)")
	}
}

// replaceViewers is the mutator shape the HTTP PATCH handler passes:
// overlay one list, leave everything else as the live ACL has it.
func replaceViewers(viewers ...string) func(auth.SessionACL) auth.SessionACL {
	return func(cur auth.SessionACL) auth.SessionACL {
		cur.Viewers = viewers
		return cur
	}
}

// TestAmendACL_UnknownSession — the caller gets the registry's own
// sentinel so the HTTP layer can map it to 404 rather than 500. This
// is reachable in production: the route looks the entry up, and the
// session can be evicted before AmendACL takes the lock.
func TestAmendACL_UnknownSession(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	_, err := reg.AmendACL(context.Background(), "a", "u", "nope", replaceViewers("v@example.com"))
	if !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("AmendACL on an unknown session: err = %v, want ErrSessionNotFound", err)
	}
}

// TestAmendACL_OwnerIsNotTransferable — the persisted owner index is
// what drives which sessions an operator can see when they're idle, so
// a transfer that went through would take the session away from the
// losing side with no way back.
func TestAmendACL_OwnerIsNotTransferable(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	entry, err := reg.RegisterOwned(&stubRegistrant{app: "a", user: "u", sid: "s1"}, "alice@example.com")
	if err != nil {
		t.Fatalf("RegisterOwned: %v", err)
	}
	steal := func(cur auth.SessionACL) auth.SessionACL {
		cur.Owner = "bob@example.com"
		return cur
	}
	_, err = reg.AmendACL(context.Background(), "a", "u", "s1", steal)
	if !errors.Is(err, ErrACLOwnerNotTransferable) {
		t.Fatalf("AmendACL changing Owner: err = %v, want ErrACLOwnerNotTransferable", err)
	}
	if got := entry.CurrentACL().Owner; got != "alice@example.com" {
		t.Errorf("Owner = %q after the refused AmendACL, want unchanged", got)
	}
}

// TestAmendACL_ZeroOwnerMeansUnchanged — the mutator signature has no
// way to say "don't touch the owner" other than leaving the zero value,
// which is exactly what a PATCH body that omits `owner` produces. If
// this were read as a transfer-to-nobody, every ordinary PATCH would be
// refused.
func TestAmendACL_ZeroOwnerMeansUnchanged(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	if _, err := reg.RegisterOwned(&stubRegistrant{app: "a", user: "u", sid: "s1"}, "alice@example.com"); err != nil {
		t.Fatalf("RegisterOwned: %v", err)
	}
	drop := func(auth.SessionACL) auth.SessionACL {
		return auth.SessionACL{Viewers: []string{"v@example.com"}}
	}
	got, err := reg.AmendACL(context.Background(), "a", "u", "s1", drop)
	if err != nil {
		t.Fatalf("AmendACL: %v", err)
	}
	if got.Owner != "alice@example.com" {
		t.Errorf("Owner = %q, want the untouched alice@example.com", got.Owner)
	}
}

// TestAmendACL_MutatorSeesLiveACL is the lost-update regression. The
// mutator has to run under the registry lock against the ACL as it is
// at that moment; if the handler read the ACL first and handed over a
// pre-computed value, two concurrent PATCHes — one setting viewers, one
// setting contributors — would each carry the other's field forward
// from a stale snapshot and one edit would vanish. Silently, and on an
// authorization decision.
func TestAmendACL_MutatorSeesLiveACL(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	if _, err := reg.RegisterOwned(&stubRegistrant{app: "a", user: "u", sid: "s1"}, "alice@example.com"); err != nil {
		t.Fatalf("RegisterOwned: %v", err)
	}
	setViewers := replaceViewers("v@example.com")
	setContributors := func(cur auth.SessionACL) auth.SessionACL {
		cur.Contributors = []string{"c@example.com"}
		return cur
	}
	var wg sync.WaitGroup
	for _, amend := range []func(auth.SessionACL) auth.SessionACL{setViewers, setContributors} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := reg.AmendACL(context.Background(), "a", "u", "s1", amend); err != nil {
				t.Errorf("AmendACL: %v", err)
			}
		}()
	}
	wg.Wait()
	got, err := reg.AmendACL(context.Background(), "a", "u", "s1", func(cur auth.SessionACL) auth.SessionACL { return cur })
	if err != nil {
		t.Fatalf("AmendACL readback: %v", err)
	}
	if len(got.Viewers) != 1 || len(got.Contributors) != 1 {
		t.Errorf("both amendments must survive: viewers = %v, contributors = %v", got.Viewers, got.Contributors)
	}
}

// TestAmendACL_UnownedSessionStaysInMemory keeps the design's resolved
// OQ #7 invariant intact: "ACL row exists ⟺ session is resumable".
// Writing a row for a legacy unowned session would quietly make it
// resumable, which is a different session lifecycle than the operator
// signed up for.
func TestAmendACL_UnownedSessionStaysInMemory(t *testing.T) {
	t.Parallel()
	store := newTestACLStore(t)
	reg := NewSessionRegistryWithStore(store)
	if _, err := reg.Register(&stubRegistrant{app: "a", user: "u", sid: "legacy"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	got, err := reg.AmendACL(context.Background(), "a", "u", "legacy", replaceViewers("v@example.com"))
	if err != nil {
		t.Fatalf("AmendACL: %v", err)
	}
	if len(got.Viewers) != 1 {
		t.Errorf("in-memory ACL should still be amended; got %v", got.Viewers)
	}
	if _, err := store.Get(context.Background(), "a", "u", "legacy"); !errors.Is(err, ErrSessionACLNotFound) {
		t.Errorf("an unowned session must not gain a store row; Get err = %v", err)
	}
}

func TestListAuthorized_FiltersByCaller(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()

	// Two owned sessions + one legacy unowned.
	if _, err := reg.RegisterOwned(
		&stubRegistrant{app: "a", user: "u", sid: "alice-1"},
		"alice@example.com",
	); err != nil {
		t.Fatalf("Register alice: %v", err)
	}
	if _, err := reg.RegisterOwned(
		&stubRegistrant{app: "a", user: "u", sid: "bob-1"},
		"bob@example.com",
	); err != nil {
		t.Fatalf("Register bob: %v", err)
	}
	if _, err := reg.Register(
		&stubRegistrant{app: "a", user: "u", sid: "legacy"},
	); err != nil {
		t.Fatalf("Register legacy: %v", err)
	}

	// Alice sees only her own session.
	got := reg.ListAuthorized(auth.Caller{Identity: "alice@example.com"})
	if len(got) != 1 || got[0].SessionID != "alice-1" {
		var ids []string
		for _, e := range got {
			ids = append(ids, e.SessionID)
		}
		t.Errorf("alice sees %v, want [alice-1]", ids)
	}

	// Bob sees only his own session.
	got = reg.ListAuthorized(auth.Caller{Identity: "bob@example.com"})
	if len(got) != 1 || got[0].SessionID != "bob-1" {
		var ids []string
		for _, e := range got {
			ids = append(ids, e.SessionID)
		}
		t.Errorf("bob sees %v, want [bob-1]", ids)
	}

	// Stranger sees nothing.
	got = reg.ListAuthorized(auth.Caller{Identity: "stranger@example.com"})
	if len(got) != 0 {
		t.Errorf("stranger should see no sessions; got %d", len(got))
	}

	// Admin sees everything (legacy unowned included).
	got = reg.ListAuthorized(auth.Caller{Identity: "ops@example.com", Admin: true})
	if len(got) != 3 {
		t.Errorf("admin should see all 3 sessions; got %d", len(got))
	}

	// Anonymous (no identity) sees nothing — even the unowned legacy
	// entry (which has Owner="" — but the empty-identity check in
	// Authorize defends against that exact case).
	got = reg.ListAuthorized(auth.Caller{})
	if len(got) != 0 {
		t.Errorf("empty-identity Caller should see no sessions; got %d", len(got))
	}
}
