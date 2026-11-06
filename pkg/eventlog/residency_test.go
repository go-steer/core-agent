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
	"sync"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"google.golang.org/adk/v2/session"

	"github.com/go-steer/core-agent/v3/pkg/eventlog"
	"github.com/go-steer/core-agent/v3/pkg/statestore/memory"
)

// twoInstances opens two lease-holding Handles that share one transcript
// (the external store) and one control database — two daemons pointed
// at the same backends.
func twoInstances(t *testing.T, opts ...eventlog.Option) (a, b *eventlog.Handle) {
	t.Helper()
	tr := memory.New()
	dsn := filepath.Join(t.TempDir(), "control.db")
	open := func() *eventlog.Handle {
		h, err := eventlog.OpenTranscript(context.Background(), tr, sqlite.Open(dsn),
			append([]eventlog.Option{eventlog.WithResidencyLease()}, opts...)...)
		if err != nil {
			t.Fatalf("OpenTranscript: %v", err)
		}
		t.Cleanup(func() { _ = h.Close() })
		return h
	}
	return open(), open()
}

func create(t *testing.T, h *eventlog.Handle, id string) session.Session {
	t.Helper()
	resp, err := h.Service.Create(context.Background(), &session.CreateRequest{AppName: "app", UserID: "u", SessionID: id})
	if err != nil {
		t.Fatalf("Create %s: %v", id, err)
	}
	return resp.Session
}

// Decision 1: the instance that created a tree holds it; another
// instance can read it but can neither create in it nor write to it.
func TestResidency_OneInstanceHoldsATree(t *testing.T) {
	t.Parallel()
	a, b := twoInstances(t)
	ctx := context.Background()
	create(t, a, "s1")
	if !a.HoldsResidency("app", "u", "s1") {
		t.Fatal("creator does not hold the tree")
	}
	// Reading is free and takes nothing.
	got, err := b.Service.Get(ctx, &session.GetRequest{AppName: "app", UserID: "u", SessionID: "s1"})
	if err != nil {
		t.Fatalf("Get from the other instance: %v", err)
	}
	if b.HoldsResidency("app", "u", "s1") {
		t.Fatal("a read took the lease")
	}
	if _, err := b.Service.Create(ctx, &session.CreateRequest{AppName: "app", UserID: "u", SessionID: "s1:sub:k:1"}); !errors.Is(err, eventlog.ErrSessionLocked) {
		t.Fatalf("child Create from the other instance = %v, want ErrSessionLocked", err)
	}
	if err := b.Service.AppendEvent(ctx, got.Session, textEvent("x", "intruder")); !errors.Is(err, eventlog.ErrNotResident) || !errors.Is(err, eventlog.ErrSessionLocked) {
		t.Fatalf("append from the other instance = %v, want ErrNotResident wrapping ErrSessionLocked", err)
	}
	if err := b.AcquireResidency(ctx, "app", "u", "s1"); !errors.Is(err, eventlog.ErrSessionLocked) {
		t.Fatalf("explicit resume on the other instance = %v, want ErrSessionLocked", err)
	}
	// The holder writes freely: children and derived trees share the lease.
	child := create(t, a, "s1:sub:k:1")
	if err := a.Service.AppendEvent(ctx, child, textEvent("c1", "ok")); err != nil {
		t.Fatalf("holder append to child: %v", err)
	}
	digest := create(t, a, "s1:digest")
	if err := a.Service.AppendEvent(ctx, digest, textEvent("d1", "ok")); err != nil {
		t.Fatalf("holder append to derived tree: %v", err)
	}
	var rows int64
	if err := a.DB.Table("agent_run_lock").Where("session_id LIKE ?", "residency:%").Count(&rows).Error; err != nil || rows != 1 {
		t.Fatalf("residency rows = %d, %v; want 1 (one lease for the whole base tree)", rows, err)
	}
	// Another tree is unaffected.
	create(t, b, "s2")
}

// Reads by scanners never claim a session, so another process can
// still resume it.
func TestResidency_ReadsNeverLease(t *testing.T) {
	t.Parallel()
	a, b := twoInstances(t)
	ctx := context.Background()
	create(t, a, "r1")
	a.ReleaseResidency("app", "u", "r1")
	for i := 0; i < 3; i++ {
		if _, err := a.Service.Get(ctx, &session.GetRequest{AppName: "app", UserID: "u", SessionID: "r1"}); err != nil {
			t.Fatalf("Get: %v", err)
		}
	}
	if err := b.AcquireResidency(ctx, "app", "u", "r1"); err != nil {
		t.Fatalf("resume elsewhere after reads: %v", err)
	}
}

// Releasing (eviction) or closing hands the tree over at once, without
// waiting out the staleness window.
func TestResidency_ReleaseAndCloseHandOver(t *testing.T) {
	t.Parallel()
	a, b := twoInstances(t)
	ctx := context.Background()
	create(t, a, "s1")
	create(t, a, "s1:digest")
	a.ReleaseResidency("app", "u", "s1")
	if a.HoldsResidency("app", "u", "s1") || a.HoldsResidency("app", "u", "s1:digest") {
		t.Fatal("release left a lease held")
	}
	// The row is deleted in the background; the other instance gets
	// it as soon as that lands.
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := b.AcquireResidency(ctx, "app", "u", "s1")
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("other instance never got the released tree: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	create(t, b, "s3")
	// Close releases synchronously: the row is gone at once. (The two
	// instances share one transcript here, so check the control store.)
	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	var n int64
	if err := a.DB.Table("agent_run_lock").Where("session_id = ?", "residency:s3").Count(&n).Error; err != nil || n != 0 {
		t.Fatalf("residency rows for s3 after Close = %d, %v; want 0", n, err)
	}
}

// An evicted tree is tombstoned: writes still in flight from it are
// refused instead of taking the lease back, until it is resumed.
func TestResidency_EvictedTreeRefusesWritesUntilResumed(t *testing.T) {
	t.Parallel()
	a, _ := twoInstances(t)
	ctx := context.Background()
	sess := create(t, a, "ev")
	a.ReleaseResidency("app", "u", "ev")
	if err := a.Service.AppendEvent(ctx, sess, textEvent("late", "in flight")); !errors.Is(err, eventlog.ErrNotResident) {
		t.Fatalf("write after eviction = %v, want ErrNotResident", err)
	}
	if _, err := a.Service.Create(ctx, &session.CreateRequest{AppName: "app", UserID: "u", SessionID: "ev:sub:late:1"}); !errors.Is(err, eventlog.ErrNotResident) {
		t.Fatalf("subagent Create after eviction = %v, want ErrNotResident", err)
	}
	if a.HoldsResidency("app", "u", "ev") {
		t.Fatal("a late write took the lease back")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := a.AcquireResidency(ctx, "app", "u", "ev")
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("resume after eviction: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := a.Service.AppendEvent(ctx, sess, textEvent("e2", "resumed")); err != nil {
		t.Fatalf("write after resume: %v", err)
	}
}

// A write's context carries the fence deadline: the point a successor
// could steal the lease.
func TestResidency_WriteDeadlineBoundsTheWrite(t *testing.T) {
	t.Parallel()
	slow := &slowTranscript{Transcript: memory.New()}
	h, err := eventlog.OpenTranscript(context.Background(), slow, sqlite.Open(filepath.Join(t.TempDir(), "c.db")),
		eventlog.WithResidencyLease(), eventlog.WithLockHeartbeat(time.Hour, 2*time.Second))
	if err != nil {
		t.Fatalf("OpenTranscript: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })
	sess := create(t, h, "dl")
	if err := h.Service.AppendEvent(context.Background(), sess, textEvent("e1", "x")); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	if slow.deadline.IsZero() {
		t.Fatal("the write's context had no deadline")
	}
	// staleAfter 2s from a heartbeat at create: the deadline is within
	// ~2s of now, never later.
	if until := time.Until(slow.deadline); until <= 0 || until > 2*time.Second {
		t.Fatalf("write deadline is %s away, want within the 2s staleness window", until)
	}
}

// slowTranscript records the deadline its Service's AppendEvent sees.
type slowTranscript struct {
	*memory.Transcript
	deadline time.Time
}

func (s *slowTranscript) Service() session.Service {
	return &deadlineService{Service: s.Transcript.Service(), rec: s}
}

type deadlineService struct {
	session.Service
	rec *slowTranscript
}

func (d *deadlineService) AppendEvent(ctx context.Context, sess session.Session, ev *session.Event) error {
	if dl, ok := ctx.Deadline(); ok {
		d.rec.deadline = dl
	}
	return d.Service.AppendEvent(ctx, sess, ev)
}

// Taking a lease makes the transcript forget its cached view of the tree.
func TestResidency_AcquireInvalidatesTheTranscriptCache(t *testing.T) {
	t.Parallel()
	inv := &invalidatingTranscript{Transcript: memory.New()}
	h, err := eventlog.OpenTranscript(context.Background(), inv, sqlite.Open(filepath.Join(t.TempDir(), "c.db")), eventlog.WithResidencyLease())
	if err != nil {
		t.Fatalf("OpenTranscript: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })
	create(t, h, "inv")
	inv.mu.Lock()
	got := append([]string(nil), inv.roots...)
	inv.mu.Unlock()
	if len(got) != 1 || got[0] != "inv" {
		t.Fatalf("Invalidate calls = %v, want [inv]", got)
	}
}

type invalidatingTranscript struct {
	*memory.Transcript
	mu    sync.Mutex
	roots []string
}

func (i *invalidatingTranscript) Invalidate(_, _, root string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.roots = append(i.roots, root)
}

// Decisions 2 and 3: a stolen lease fences the tree at once and runs
// the per-tree handler, or the fallback when none is registered.
func TestResidency_StolenLeaseFencesAndNotifies(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var fallback []string
	a, _ := twoInstances(t,
		eventlog.WithLockHeartbeat(10*time.Millisecond, time.Second),
		eventlog.WithResidencyLost(func(app, user, root string, stolen bool) {
			mu.Lock()
			defer mu.Unlock()
			if stolen {
				fallback = append(fallback, root)
			}
		}))
	ctx := context.Background()
	watched := create(t, a, "w")
	unwatched := create(t, a, "f")
	got := make(chan bool, 1)
	defer a.WatchResidency("app", "u", "w", func(stolen bool) { got <- stolen })()

	// Another holder takes both rows (what a successor does after a
	// stale heartbeat); our next heartbeat matches nothing.
	if err := a.DB.Exec(`UPDATE agent_run_lock SET holder = 'successor' WHERE session_id IN ('residency:w', 'residency:f')`).Error; err != nil {
		t.Fatalf("steal: %v", err)
	}
	select {
	case stolen := <-got:
		if !stolen {
			t.Fatal("per-tree handler reported a stall, want stolen")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("per-tree handler never ran")
	}
	if err := a.Service.AppendEvent(ctx, watched, textEvent("x", "after loss")); !errors.Is(err, eventlog.ErrLeaseLost) {
		t.Fatalf("append after loss = %v, want ErrLeaseLost", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(fallback)
		mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("fallback ran for %d trees, want exactly the unwatched one", n)
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	if fallback[0] != "f" {
		t.Fatalf("fallback ran for %q, want f", fallback[0])
	}
	mu.Unlock()
	if err := a.Service.AppendEvent(ctx, unwatched, textEvent("y", "after loss")); !errors.Is(err, eventlog.ErrLeaseLost) {
		t.Fatalf("append to the unwatched tree after loss = %v, want ErrLeaseLost", err)
	}
}

// Decision 3's self-fence: with no confirmed heartbeat inside the loss
// window the holder stops writing on its own clock, before Lost() has
// had a chance to fire.
func TestResidency_FenceOnOwnDeadline(t *testing.T) {
	t.Parallel()
	// One heartbeat an hour against a 2s staleness window: the loss
	// window is 1s (the staleAfter/2 floor) and no beat will ever extend
	// it. Generous so a loaded -race run stays inside it for e1.
	a, _ := twoInstances(t, eventlog.WithLockHeartbeat(time.Hour, 2*time.Second))
	ctx := context.Background()
	sess := create(t, a, "d")
	if err := a.Service.AppendEvent(ctx, sess, textEvent("e1", "inside the window")); err != nil {
		t.Fatalf("append inside the window: %v", err)
	}
	time.Sleep(1200 * time.Millisecond)
	if err := a.Service.AppendEvent(ctx, sess, textEvent("e2", "past the deadline")); !errors.Is(err, eventlog.ErrLeaseLost) {
		t.Fatalf("append past the deadline = %v, want ErrLeaseLost", err)
	}
}

// Deleting a root releases its tree.
func TestResidency_DeleteReleases(t *testing.T) {
	t.Parallel()
	a, b := twoInstances(t)
	ctx := context.Background()
	create(t, a, "gone")
	if err := a.Service.Delete(ctx, &session.DeleteRequest{AppName: "app", UserID: "u", SessionID: "gone"}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if a.HoldsResidency("app", "u", "gone") {
		t.Fatal("lease survived Delete of the root")
	}
	create(t, b, "gone")
}

// Without the option nothing changes: no lease, no fence.
func TestResidency_OffByDefault(t *testing.T) {
	t.Parallel()
	h, _ := openMemoryHandle(t)
	create(t, h, "plain")
	if h.HoldsResidency("app", "u", "plain") {
		t.Fatal("a Handle without WithResidencyLease took a lease")
	}
	h.ReleaseResidency("app", "u", "plain") // no-op, no panic
	h.WatchResidency("app", "u", "plain", func(bool) {})()
}

// Caller-chosen IDs that contain ":" are distinct trees; only this
// runtime's own derived suffixes share a session's lease.
func TestResidency_ColonInCallerIDsDoesNotMergeTrees(t *testing.T) {
	t.Parallel()
	a, b := twoInstances(t)
	create(t, a, "proj:1")
	create(t, b, "proj:2") // would be ErrSessionLocked if both keyed to "proj"
	a.ReleaseResidency("app", "u", "proj:1")
	if !b.HoldsResidency("app", "u", "proj:2") {
		t.Fatal("releasing proj:1 released proj:2")
	}
}

// Release then an immediate re-acquire on the same Handle succeeds: the
// acquire waits for our own row to be deleted instead of being refused
// by it.
func TestResidency_ImmediateReacquireAfterRelease(t *testing.T) {
	t.Parallel()
	a, _ := twoInstances(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		create(t, a, "again") // first pass creates; later passes re-create after Delete
		a.ReleaseResidency("app", "u", "again")
		if err := a.AcquireResidency(ctx, "app", "u", "again"); err != nil {
			t.Fatalf("pass %d: re-acquire right after release: %v", i, err)
		}
		if err := a.Service.Delete(ctx, &session.DeleteRequest{AppName: "app", UserID: "u", SessionID: "again"}); err != nil {
			t.Fatalf("pass %d: Delete: %v", i, err)
		}
	}
}

// Deleting a derived session releases nothing; deleting the session
// itself releases without a tombstone, so the ID can be reused freely.
func TestResidency_DeleteScope(t *testing.T) {
	t.Parallel()
	a, _ := twoInstances(t)
	ctx := context.Background()
	create(t, a, "base")
	create(t, a, "base:digest")
	if err := a.Service.Delete(ctx, &session.DeleteRequest{AppName: "app", UserID: "u", SessionID: "base:digest"}); err != nil {
		t.Fatalf("Delete derived: %v", err)
	}
	if !a.HoldsResidency("app", "u", "base") {
		t.Fatal("deleting base:digest released base")
	}
	if err := a.Service.Delete(ctx, &session.DeleteRequest{AppName: "app", UserID: "u", SessionID: "base"}); err != nil {
		t.Fatalf("Delete base: %v", err)
	}
	// No tombstone: an implicit first write to a new session under the
	// same ID is allowed, as for any never-resident tree.
	resp, err := a.Service.Create(ctx, &session.CreateRequest{AppName: "app", UserID: "u", SessionID: "base"})
	if err != nil {
		t.Fatalf("re-Create after Delete: %v", err)
	}
	if err := a.Service.AppendEvent(ctx, resp.Session, textEvent("n1", "new")); err != nil {
		t.Fatalf("write to the re-created session: %v", err)
	}
}
