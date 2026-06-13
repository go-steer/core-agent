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
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/go-steer/core-agent/pkg/auth"
)

func newTestACLStore(t *testing.T) SessionACLStore {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "acl.db")
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	store, err := NewSessionACLStore(context.Background(), db)
	if err != nil {
		t.Fatalf("NewSessionACLStore: %v", err)
	}
	return store
}

func TestSessionACLStore_PutGet_RoundTrip(t *testing.T) {
	t.Parallel()
	store := newTestACLStore(t)
	ctx := context.Background()

	row := SessionACLRow{
		AppName:      "core-agent",
		UserID:       "alice@example.com",
		SessionID:    "sess-abc-1",
		Owner:        "alice@example.com",
		Viewers:      []string{"viewer1@example.com"},
		Contributors: []string{"contrib1@example.com", "contrib2@example.com"},
	}
	if err := store.Put(ctx, row); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := store.Get(ctx, "core-agent", "alice@example.com", "sess-abc-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Owner != "alice@example.com" {
		t.Errorf("Owner: got %q, want %q", got.Owner, "alice@example.com")
	}
	if len(got.Viewers) != 1 || got.Viewers[0] != "viewer1@example.com" {
		t.Errorf("Viewers: got %v", got.Viewers)
	}
	if len(got.Contributors) != 2 {
		t.Errorf("Contributors: got %v (want 2)", got.Contributors)
	}
	if got.CreatedAt.IsZero() {
		t.Error("CreatedAt should be auto-populated")
	}
	if got.LastTouchedAt.IsZero() {
		t.Error("LastTouchedAt should be auto-populated")
	}
}

func TestSessionACLStore_Get_NotFound(t *testing.T) {
	t.Parallel()
	store := newTestACLStore(t)
	_, err := store.Get(context.Background(), "core-agent", "u", "missing-sid")
	if !errors.Is(err, ErrSessionACLNotFound) {
		t.Errorf("expected ErrSessionACLNotFound, got %v", err)
	}
}

func TestSessionACLStore_Put_RejectsEmptyOwner(t *testing.T) {
	t.Parallel()
	store := newTestACLStore(t)
	err := store.Put(context.Background(), SessionACLRow{
		AppName:   "core-agent",
		SessionID: "s1",
		Owner:     "", // intentional
	})
	if err == nil {
		t.Fatal("expected error for empty owner")
	}
}

func TestSessionACLStore_Put_UpsertSameTriple(t *testing.T) {
	t.Parallel()
	store := newTestACLStore(t)
	ctx := context.Background()
	row := SessionACLRow{
		AppName: "a", UserID: "u", SessionID: "s",
		Owner: "alice@example.com",
	}
	if err := store.Put(ctx, row); err != nil {
		t.Fatalf("first Put: %v", err)
	}
	row.Viewers = []string{"new-viewer@example.com"}
	if err := store.Put(ctx, row); err != nil {
		t.Fatalf("second Put (upsert): %v", err)
	}
	got, _ := store.Get(ctx, "a", "u", "s")
	if len(got.Viewers) != 1 || got.Viewers[0] != "new-viewer@example.com" {
		t.Errorf("upsert didn't overwrite viewers: got %v", got.Viewers)
	}
}

func TestSessionACLStore_Delete(t *testing.T) {
	t.Parallel()
	store := newTestACLStore(t)
	ctx := context.Background()
	row := SessionACLRow{AppName: "a", UserID: "u", SessionID: "s", Owner: "alice@example.com"}
	_ = store.Put(ctx, row)
	if err := store.Delete(ctx, "a", "u", "s"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	_, err := store.Get(ctx, "a", "u", "s")
	if !errors.Is(err, ErrSessionACLNotFound) {
		t.Errorf("after Delete, Get should return ErrSessionACLNotFound; got %v", err)
	}
	// Delete is idempotent.
	if err := store.Delete(ctx, "a", "u", "s"); err != nil {
		t.Errorf("Delete should be idempotent; got %v", err)
	}
}

func TestSessionACLStore_Touch(t *testing.T) {
	t.Parallel()
	store := newTestACLStore(t)
	ctx := context.Background()
	row := SessionACLRow{AppName: "a", UserID: "u", SessionID: "s", Owner: "alice@example.com"}
	_ = store.Put(ctx, row)

	original, _ := store.Get(ctx, "a", "u", "s")
	when := original.LastTouchedAt.Add(1 * time.Hour)
	if err := store.Touch(ctx, "a", "u", "s", when); err != nil {
		t.Fatalf("Touch: %v", err)
	}
	got, _ := store.Get(ctx, "a", "u", "s")
	if !got.LastTouchedAt.Equal(when) {
		t.Errorf("Touch didn't update LastTouchedAt: got %v, want %v", got.LastTouchedAt, when)
	}
	if !got.CreatedAt.Equal(original.CreatedAt) {
		t.Errorf("Touch shouldn't change CreatedAt: got %v, want %v", got.CreatedAt, original.CreatedAt)
	}
}

func TestSessionACLStore_Touch_NoRowIsNoError(t *testing.T) {
	t.Parallel()
	// Sessions registered before the store was wired (legacy
	// Register, no ACL row) hit Touch when the registry refreshes
	// their LastTouchedAt. Should silently skip, not error.
	store := newTestACLStore(t)
	err := store.Touch(context.Background(), "a", "u", "ghost-sid", time.Now())
	if err != nil {
		t.Errorf("Touch on non-existent row should be silent; got %v", err)
	}
}

func TestSessionACLStore_ListByOwner(t *testing.T) {
	t.Parallel()
	store := newTestACLStore(t)
	ctx := context.Background()
	_ = store.Put(ctx, SessionACLRow{AppName: "a", UserID: "u", SessionID: "s1", Owner: "alice@example.com"})
	_ = store.Put(ctx, SessionACLRow{AppName: "a", UserID: "u", SessionID: "s2", Owner: "alice@example.com"})
	_ = store.Put(ctx, SessionACLRow{AppName: "a", UserID: "u", SessionID: "s3", Owner: "bob@example.com"})

	alices, err := store.ListByOwner(ctx, "alice@example.com")
	if err != nil {
		t.Fatalf("ListByOwner: %v", err)
	}
	if len(alices) != 2 {
		t.Errorf("alice should own 2 sessions; got %d", len(alices))
	}
}

func TestSessionACLStore_ListVisibleTo_Matrix(t *testing.T) {
	t.Parallel()
	store := newTestACLStore(t)
	ctx := context.Background()
	// alice owns one session; bob owns one; ops is admin.
	_ = store.Put(ctx, SessionACLRow{
		AppName: "a", UserID: "u", SessionID: "alice-1",
		Owner:        "alice@example.com",
		Contributors: []string{"contrib@example.com"},
	})
	_ = store.Put(ctx, SessionACLRow{
		AppName: "a", UserID: "u", SessionID: "bob-1",
		Owner:   "bob@example.com",
		Viewers: []string{"viewer@example.com"},
	})

	tests := []struct {
		name     string
		caller   auth.Caller
		wantSIDs []string
	}{
		{
			name:     "alice sees only alice's session",
			caller:   auth.Caller{Identity: "alice@example.com"},
			wantSIDs: []string{"alice-1"},
		},
		{
			name:     "bob sees only bob's session",
			caller:   auth.Caller{Identity: "bob@example.com"},
			wantSIDs: []string{"bob-1"},
		},
		{
			name:     "contrib sees alice's session (contributor)",
			caller:   auth.Caller{Identity: "contrib@example.com"},
			wantSIDs: []string{"alice-1"},
		},
		{
			name:     "viewer sees bob's session (viewer)",
			caller:   auth.Caller{Identity: "viewer@example.com"},
			wantSIDs: []string{"bob-1"},
		},
		{
			name:     "stranger sees nothing",
			caller:   auth.Caller{Identity: "stranger@example.com"},
			wantSIDs: nil,
		},
		{
			name:     "admin sees everything",
			caller:   auth.Caller{Identity: "ops@example.com", Admin: true},
			wantSIDs: []string{"alice-1", "bob-1"},
		},
		{
			name:     "zero-identity sees nothing",
			caller:   auth.Caller{},
			wantSIDs: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := store.ListVisibleTo(ctx, tt.caller)
			if err != nil {
				t.Fatalf("ListVisibleTo: %v", err)
			}
			gotSIDs := make([]string, 0, len(got))
			for _, r := range got {
				gotSIDs = append(gotSIDs, r.SessionID)
			}
			sort.Strings(gotSIDs)
			sort.Strings(tt.wantSIDs)
			if len(gotSIDs) != len(tt.wantSIDs) {
				t.Errorf("got %v, want %v", gotSIDs, tt.wantSIDs)
				return
			}
			for i := range gotSIDs {
				if gotSIDs[i] != tt.wantSIDs[i] {
					t.Errorf("got %v, want %v", gotSIDs, tt.wantSIDs)
					return
				}
			}
		})
	}
}

func TestSessionACLStore_ConcurrentPut(t *testing.T) {
	t.Parallel()
	// Many distinct triples Put concurrently — sanity check that
	// the GORM connection serializes correctly. Don't test
	// concurrent Put on the SAME triple (upsert semantics make
	// the "winner" non-deterministic, which is fine in practice
	// but not a useful test invariant).
	store := newTestACLStore(t)
	ctx := context.Background()
	const n = 50
	var wg sync.WaitGroup
	errCh := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := store.Put(ctx, SessionACLRow{
				AppName: "a", UserID: "u",
				SessionID: "sess-" + intToString(i),
				Owner:     "alice@example.com",
			})
			if err != nil {
				errCh <- err
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("concurrent Put: %v", err)
	}
	alices, _ := store.ListByOwner(ctx, "alice@example.com")
	if len(alices) != n {
		t.Errorf("ListByOwner after %d concurrent Puts: got %d, want %d", n, len(alices), n)
	}
}

func TestSessionACLRow_ACL_RoundTrip(t *testing.T) {
	t.Parallel()
	row := SessionACLRow{
		Owner:        "alice@example.com",
		Viewers:      []string{"v1", "v2"},
		Contributors: []string{"c1"},
	}
	acl := row.ACL()
	if acl.Owner != "alice@example.com" {
		t.Errorf("Owner: got %q", acl.Owner)
	}
	if len(acl.Viewers) != 2 {
		t.Errorf("Viewers: got %v", acl.Viewers)
	}
	if len(acl.Contributors) != 1 {
		t.Errorf("Contributors: got %v", acl.Contributors)
	}
	// Defensive copy: mutating ACL slices shouldn't affect the row.
	acl.Viewers[0] = "mutated"
	if row.Viewers[0] == "mutated" {
		t.Error("ACL() must return defensive copies of slice fields")
	}
}

// intToString is a tiny helper used by TestSessionACLStore_ConcurrentPut
// to keep that test free of strconv import noise. Mirrors strconv.Itoa
// without the import.
func intToString(i int) string {
	if i == 0 {
		return "0"
	}
	const digits = "0123456789"
	var buf [20]byte
	pos := len(buf)
	neg := i < 0
	if neg {
		i = -i
	}
	for i > 0 {
		pos--
		buf[pos] = digits[i%10]
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
