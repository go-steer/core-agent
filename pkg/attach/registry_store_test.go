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
	"testing"
	"time"

	"github.com/go-steer/core-agent/pkg/auth"
)

func TestRegisterOwned_PersistsACLToStore(t *testing.T) {
	t.Parallel()
	store := newTestACLStore(t)
	reg := NewSessionRegistryWithStore(store)
	ag := &stubRegistrant{app: "core-agent", user: "u", sid: "sess-1"}

	if _, err := reg.RegisterOwned(ag, "alice@example.com"); err != nil {
		t.Fatalf("RegisterOwned: %v", err)
	}

	got, err := store.Get(context.Background(), "core-agent", "u", "sess-1")
	if err != nil {
		t.Fatalf("store should have the row after RegisterOwned: %v", err)
	}
	if got.Owner != "alice@example.com" {
		t.Errorf("persisted Owner: got %q, want %q", got.Owner, "alice@example.com")
	}
}

func TestRegister_DoesNotPersistACLToStore(t *testing.T) {
	t.Parallel()
	// Legacy Register (no Owner) intentionally stays in-memory only.
	// "ACL row exists ⟺ session is resumable" — the design's
	// resolved OQ #7.
	store := newTestACLStore(t)
	reg := NewSessionRegistryWithStore(store)
	ag := &stubRegistrant{app: "core-agent", user: "u", sid: "sess-legacy"}

	if _, err := reg.Register(ag); err != nil {
		t.Fatalf("Register: %v", err)
	}

	_, err := store.Get(context.Background(), "core-agent", "u", "sess-legacy")
	if !errors.Is(err, ErrSessionACLNotFound) {
		t.Errorf("legacy Register must NOT write to the store; got Get err = %v", err)
	}
}

func TestRegisterOwned_NilStoreIsBackwardCompat(t *testing.T) {
	t.Parallel()
	// NewSessionRegistry (no store) should still accept RegisterOwned —
	// the persistence is opt-in. The ACL still lives on the in-memory
	// entry; just doesn't survive restart.
	reg := NewSessionRegistry()
	ag := &stubRegistrant{app: "core-agent", user: "u", sid: "sess-x"}
	entry, err := reg.RegisterOwned(ag, "alice@example.com")
	if err != nil {
		t.Fatalf("RegisterOwned with nil store: %v", err)
	}
	if entry.ACL.Owner != "alice@example.com" {
		t.Errorf("in-memory ACL.Owner: got %q", entry.ACL.Owner)
	}
}

func TestRegisterOwned_RollsBackInMemoryOnStoreFailure(t *testing.T) {
	t.Parallel()
	// If the store rejects the Put, the in-memory entry must be
	// rolled back — otherwise we'd have an unresumable session that
	// looks active until daemon restart, which the operator would
	// reasonably consider durable.
	reg := NewSessionRegistryWithStore(&failingStore{})
	ag := &stubRegistrant{app: "core-agent", user: "u", sid: "sess-rollback"}

	_, err := reg.RegisterOwned(ag, "alice@example.com")
	if err == nil {
		t.Fatal("expected error from failing store")
	}
	// In-memory entry should not exist — verified via Lookup
	// returning ErrSessionNotFound (the public surface the
	// session-resolution handlers consult).
	if _, lookupErr := reg.Lookup("core-agent", "sess-rollback"); !errors.Is(lookupErr, ErrSessionNotFound) {
		t.Errorf("in-memory entry should be rolled back after store failure; got Lookup err = %v", lookupErr)
	}
}

// failingStore is a SessionACLStore that fails every Put. Used to
// verify the rollback path.
type failingStore struct{}

func (*failingStore) Put(context.Context, SessionACLRow) error {
	return errors.New("simulated store failure")
}

func (*failingStore) Get(context.Context, string, string, string) (SessionACLRow, error) {
	return SessionACLRow{}, ErrSessionACLNotFound
}

func (*failingStore) Delete(context.Context, string, string, string) error { return nil }

func (*failingStore) Touch(context.Context, string, string, string, time.Time) error { return nil }

func (*failingStore) ListByOwner(context.Context, string) ([]SessionACLRow, error) { return nil, nil }

func (*failingStore) ListVisibleTo(context.Context, auth.Caller) ([]SessionACLRow, error) {
	return nil, nil
}
