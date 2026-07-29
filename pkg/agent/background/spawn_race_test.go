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

import "testing"

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
