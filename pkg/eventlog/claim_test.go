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
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
)

func openClaimDB(t *testing.T, path string) *Handle {
	t.Helper()
	h, err := Open(context.Background(), sqlite.Open(path))
	if err != nil {
		t.Fatalf("Open(%s): %v", path, err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
}

// The core rule: one interruption, one claim — and the second caller is
// a DIFFERENT handle on the same file, because the whole point is two
// daemons sharing a database rather than two goroutines sharing a
// process.
func TestClaimContinuation_OneInterruptionOneClaim(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "s.db")
	a, b := openClaimDB(t, path), openClaimDB(t, path)
	at := time.Now().Add(-time.Minute)

	got, err := a.ClaimContinuation(ctx, "core-agent", "alice", "sid", at, "daemon-a")
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if !got {
		t.Fatal("first claim = false, want the first caller to win")
	}

	got, err = b.ClaimContinuation(ctx, "core-agent", "alice", "sid", at, "daemon-b")
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if got {
		t.Error("second claim = true; two daemons would both continue the same interruption")
	}

	// Re-claiming your own claim is the same answer. The retry driver
	// re-runs the whole pass on a timer against a tail that has not
	// changed, so this is the common case, not an edge one.
	got, err = a.ClaimContinuation(ctx, "core-agent", "alice", "sid", at, "daemon-a")
	if err != nil {
		t.Fatalf("re-claim: %v", err)
	}
	if got {
		t.Error("re-claiming the same interruption = true; the retry driver would inject on every tick")
	}
}

// A claim is about one interruption, not about the session. A session
// interrupted again after a continuation ran must be continuable again,
// or the first claim silently disables auto-continue for that session
// forever.
func TestClaimContinuation_ALaterInterruptionIsANewFact(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "s.db")
	a, b := openClaimDB(t, path), openClaimDB(t, path)
	first := time.Now().Add(-10 * time.Minute)
	second := first.Add(5 * time.Minute)

	if got, err := a.ClaimContinuation(ctx, "core-agent", "alice", "sid", first, "daemon-a"); err != nil || !got {
		t.Fatalf("first claim = %v, %v; want true, nil", got, err)
	}
	if got, err := b.ClaimContinuation(ctx, "core-agent", "alice", "sid", second, "daemon-b"); err != nil || !got {
		t.Fatalf("later interruption = %v, %v; want true, nil — the session went quiet again", got, err)
	}
	// And the newer claim now stands, so the OLDER one is no longer the
	// row: a straggler re-reading the stale tail does not get to reclaim
	// it either, because the row moved on.
	if got, err := a.ClaimContinuation(ctx, "core-agent", "alice", "sid", second, "daemon-a"); err != nil || got {
		t.Errorf("re-claim of the newer interruption = %v, %v; want false, nil", got, err)
	}
}

// Claims are per session triple. A busy daemon continuing one session
// must not stand down on another.
func TestClaimContinuation_IsPerSession(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := openClaimDB(t, filepath.Join(t.TempDir(), "s.db"))
	at := time.Now()

	for _, sid := range []string{"sid-a", "sid-b"} {
		if got, err := h.ClaimContinuation(ctx, "core-agent", "alice", sid, at, "d"); err != nil || !got {
			t.Errorf("claim(%s) = %v, %v; want true, nil", sid, got, err)
		}
	}
	// Same session id, different user, is a different session.
	if got, err := h.ClaimContinuation(ctx, "core-agent", "bob", "sid-a", at, "d"); err != nil || !got {
		t.Errorf("claim for another user = %v, %v; want true, nil", got, err)
	}
}

// An injection that never happened must give the claim back, or one
// failed inject strands the session until it is interrupted again.
func TestReleaseContinuationClaim_MakesTheInterruptionClaimableAgain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "s.db")
	a, b := openClaimDB(t, path), openClaimDB(t, path)
	at := time.Now()

	if got, err := a.ClaimContinuation(ctx, "core-agent", "alice", "sid", at, "daemon-a"); err != nil || !got {
		t.Fatalf("claim = %v, %v; want true, nil", got, err)
	}
	if err := a.ReleaseContinuationClaim(ctx, "core-agent", "alice", "sid", at, "daemon-a"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if got, err := b.ClaimContinuation(ctx, "core-agent", "alice", "sid", at, "daemon-b"); err != nil || !got {
		t.Errorf("claim after release = %v, %v; want true, nil", got, err)
	}
}

// A late unwind must not delete a successor's claim. Releasing is
// conditional on both the interruption AND the holder still matching.
func TestReleaseContinuationClaim_WillNotDeleteSomebodyElsesClaim(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := openClaimDB(t, filepath.Join(t.TempDir(), "s.db"))
	first := time.Now().Add(-time.Minute)
	second := first.Add(30 * time.Second)

	if _, err := h.ClaimContinuation(ctx, "core-agent", "alice", "sid", first, "daemon-a"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := h.ClaimContinuation(ctx, "core-agent", "alice", "sid", second, "daemon-b"); err != nil {
		t.Fatalf("re-claim: %v", err)
	}
	// daemon-a unwinds late and tries to hand back the claim it had.
	if err := h.ReleaseContinuationClaim(ctx, "core-agent", "alice", "sid", first, "daemon-a"); err != nil {
		t.Fatalf("stale release: %v", err)
	}
	// daemon-b's claim must still stand.
	if got, err := h.ClaimContinuation(ctx, "core-agent", "alice", "sid", second, "daemon-c"); err != nil || got {
		t.Errorf("claim after a stale release = %v, %v; want false, nil — the stale release deleted a live claim", got, err)
	}
	// And the same-holder-wrong-interruption case, the other way round.
	if err := h.ReleaseContinuationClaim(ctx, "core-agent", "alice", "sid", second, "daemon-a"); err != nil {
		t.Fatalf("wrong-holder release: %v", err)
	}
	if got, err := h.ClaimContinuation(ctx, "core-agent", "alice", "sid", second, "daemon-c"); err != nil || got {
		t.Errorf("claim after a wrong-holder release = %v, %v; want false, nil", got, err)
	}
}

// The strand case, and the reason the claim expires at all. A daemon
// injects a continuation and is killed before the wake loop drains it.
// The note lived in memory, so it is gone; the tail is byte for byte
// what it was; nobody is talking to this session, so no NEWER
// interruption will ever arrive. If the claim were permanent that
// session is done being autonomous, which is the exact failure the
// feature exists to prevent. It has to age out.
func TestClaimContinuation_AnAgedClaimIsReclaimable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "s.db")
	dead, reboot := openClaimDB(t, path), openClaimDB(t, path)
	at := time.Now()
	crashedAt := at.Add(time.Second)

	if got, err := dead.claimContinuationAt(ctx, "core-agent", "alice", "sid", at, "daemon-a", crashedAt); err != nil || !got {
		t.Fatalf("claim = %v, %v; want true, nil", got, err)
	}
	// Still inside the TTL: the note may simply not have committed yet,
	// and re-injecting here is the duplicate we are preventing.
	tooSoon := crashedAt.Add(continuationClaimTTL - time.Second)
	if got, err := reboot.claimContinuationAt(ctx, "core-agent", "alice", "sid", at, "daemon-b", tooSoon); err != nil || got {
		t.Errorf("claim %v after the first = %v, %v; want false, nil", continuationClaimTTL-time.Second, got, err)
	}
	// Past it: the note is never going to commit, so take the claim back.
	late := crashedAt.Add(continuationClaimTTL + time.Second)
	if got, err := reboot.claimContinuationAt(ctx, "core-agent", "alice", "sid", at, "daemon-b", late); err != nil || !got {
		t.Errorf("claim %v after the first = %v, %v; want true, nil — the session is stranded", continuationClaimTTL+time.Second, got, err)
	}
	// Re-claiming refreshes the clock, so the new holder gets a full TTL
	// rather than inheriting the dead daemon's expired one.
	if got, err := dead.claimContinuationAt(ctx, "core-agent", "alice", "sid", at, "daemon-a", late.Add(time.Second)); err != nil || got {
		t.Errorf("claim right after the reclaim = %v, %v; want false, nil — the TTL did not reset", got, err)
	}
}

// The in-tree callers all hold the run lock, so the compare-and-set is
// belt and braces — but it is a single statement precisely so it does
// not depend on that, and a test that proves it is cheap.
func TestClaimContinuation_ExactlyOneWinnerWithoutTheRunLock(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "s.db")
	at := time.Now()

	const racers = 8
	handles := make([]*Handle, racers)
	for i := range handles {
		handles[i] = openClaimDB(t, path)
	}
	// Migrate once up front: AutoMigrate is not the thing under test and
	// eight concurrent DDL statements only measure SQLite's busy timeout.
	if _, err := handles[0].ClaimContinuation(ctx, "core-agent", "alice", "sid", at.Add(-time.Hour), "warmup"); err != nil {
		t.Fatalf("warmup claim: %v", err)
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	won := 0
	for i := range handles {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := handles[i].ClaimContinuation(ctx, "core-agent", "alice", "sid", at, "racer")
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				t.Errorf("racer %d: %v", i, err)
				return
			}
			if got {
				won++
			}
		}()
	}
	wg.Wait()
	if won != 1 {
		t.Errorf("%d of %d racers claimed the same interruption, want exactly 1", won, racers)
	}
}

func TestClaimContinuation_NilHandleIsAnError(t *testing.T) {
	t.Parallel()
	var h *Handle
	if _, err := h.ClaimContinuation(context.Background(), "a", "u", "s", time.Now(), "x"); err == nil {
		t.Error("ClaimContinuation on a nil handle = nil error")
	}
	if err := h.ReleaseContinuationClaim(context.Background(), "a", "u", "s", time.Now(), "x"); err == nil {
		t.Error("ReleaseContinuationClaim on a nil handle = nil error")
	}
}
