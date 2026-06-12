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
	"testing"

	"github.com/go-steer/core-agent/pkg/auth"
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
	if entry.ACL.Owner != "" {
		t.Errorf("Register stamped Owner %q; legacy path must leave it empty", entry.ACL.Owner)
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
	if entry.ACL.Owner != "alice@example.com" {
		t.Errorf("ACL.Owner: got %q, want %q", entry.ACL.Owner, "alice@example.com")
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
