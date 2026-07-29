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

package background

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/model"
)

// The #488 spawn-race fixes, distilled. The hazardous interleaving:
// Spawn A reserves a name and drops the lock for tool/scheduler/
// model resolution (which can do network I/O); meanwhile Stop marks
// A's handle terminal and a same-name Spawn B evicts it and
// registers a NEW handle. A's resolution then fails — and the old
// unconditional delete removed B's handle: B kept running,
// unreachable by name, with the name freed for a duplicate. The
// same window makes A's terminal alert readable as B finishing.

func TestUnreserve_SparesRespawnedHandle(t *testing.T) {
	t.Parallel()
	mgr, _ := newFakeManager(t)

	h1 := &Handle{Name: "n", status: StatusStopped, done: make(chan struct{})}
	h2 := &Handle{Name: "n", status: StatusRunning, done: make(chan struct{})}

	// Spawn A's reservation…
	mgr.mu.Lock()
	mgr.agents["n"] = h1
	mgr.mu.Unlock()
	// …overtaken by the Stop → terminal-evict → re-spawn of B.
	mgr.mu.Lock()
	mgr.agents["n"] = h2
	mgr.mu.Unlock()

	// A's failure-path rollback must NOT remove B's handle.
	mgr.unreserve("n", h1)
	if got, ok := mgr.Get("n"); !ok || got != h2 {
		t.Fatalf("after stale unreserve, Get(n) = (%v, %v); want B's handle intact — the old unconditional delete orphaned the new subagent", got, ok)
	}

	// The matching handle does unreserve.
	mgr.unreserve("n", h2)
	if _, ok := mgr.Get("n"); ok {
		t.Fatal("matching unreserve left the reservation in place")
	}
}

func TestShouldAlert_SuppressedOnlyWhenNameReSpawned(t *testing.T) {
	t.Parallel()
	mgr, _ := newFakeManager(t)

	h1 := &Handle{Name: "n", status: StatusCompleted, done: make(chan struct{})}
	h2 := &Handle{Name: "n", status: StatusRunning, done: make(chan struct{})}

	// Name owned by the same handle: alert delivers (the normal
	// completion path — terminal handles stay registered until the
	// next same-name Spawn evicts them).
	mgr.mu.Lock()
	mgr.agents["n"] = h1
	mgr.mu.Unlock()
	if !mgr.shouldAlert("n", h1) {
		t.Error("alert for the current handle must deliver")
	}

	// Name re-spawned: the old incarnation's alert would read as the
	// new subagent finishing — suppress.
	mgr.mu.Lock()
	mgr.agents["n"] = h2
	mgr.mu.Unlock()
	if mgr.shouldAlert("n", h1) {
		t.Error("stale incarnation's alert must be suppressed after a same-name re-spawn")
	}
	if !mgr.shouldAlert("n", h2) {
		t.Error("the new incarnation's own alert must deliver")
	}

	// Name gone entirely (evicted, never re-spawned): still deliver —
	// the alert is the only record the parent gets.
	mgr.mu.Lock()
	delete(mgr.agents, "n")
	mgr.mu.Unlock()
	if !mgr.shouldAlert("n", h1) {
		t.Error("alert for an evicted-but-not-replaced name must still deliver")
	}
}

// blockingProvider parks Model() until released, then fails — the
// injectable seam that makes the #502 interleaving deterministic:
// Spawn is provably inside its pre-launch resolution window when
// Manager.Close snapshots the reservation.
type blockingProvider struct {
	entered  chan struct{} // closed when Model() is first entered
	release  chan struct{} // closing lets Model() return its error
	enterOne sync.Once
}

func (p *blockingProvider) Name() string { return "blocking" }
func (p *blockingProvider) Model(ctx context.Context, id string) (adkmodel.LLM, error) {
	p.enterOne.Do(func() { close(p.entered) })
	<-p.release
	return nil, errors.New("blocking provider always fails")
}

// TestClose_DoesNotHangOnPreLaunchFailingSpawn pins the #502 fix: a
// Spawn whose pre-launch resolution fails never launches the
// goroutine that closes handle.done — so a Manager.Close that
// snapshotted the reservation mid-window blocked on <-h.done
// forever. abortSpawn now closes done on every failure path.
func TestClose_DoesNotHangOnPreLaunchFailingSpawn(t *testing.T) {
	t.Parallel()

	prov := &blockingProvider{entered: make(chan struct{}), release: make(chan struct{})}
	mgr, err := NewManager(WithProvider(prov, "blocked"), WithMaxConcurrent(4))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	newTestParent(t, mgr)

	spawnDone := make(chan struct{})
	go func() {
		defer close(spawnDone)
		_, _ = mgr.Spawn(context.Background(), "", Spec{Name: "stuck", SystemPrompt: "p", Goal: "g"})
	}()

	// Spawn is now provably inside provider.Model — reservation in
	// the map, goroutine never to launch.
	select {
	case <-prov.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("Spawn never reached provider.Model")
	}
	if _, ok := mgr.Get("stuck"); !ok {
		t.Fatal("reservation not visible mid-resolution")
	}

	closeDone := make(chan struct{})
	go func() {
		_ = mgr.Close()
		close(closeDone)
	}()
	// Give Close time to snapshot the reservation and start waiting.
	time.Sleep(100 * time.Millisecond)

	// Release the provider: the failure path must close handle.done
	// and unblock Close. Pre-fix this hung forever.
	close(prov.release)
	select {
	case <-closeDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Manager.Close hung on the pre-launch-failing spawn's never-closed done channel (#502)")
	}
	<-spawnDone
}
