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

package compose

import (
	"sync"
	"time"
)

// WakeLoopGroup is a join point for the per-session wake loops a
// SessionFactoryDeps starts. Wire one into
// SessionFactoryDeps.WakeLoops and the factory registers every loop it
// spawns; WaitFor then blocks until they have all returned.
//
// It exists because cancelling the daemon context only *signals* those
// goroutines. buildSession used to start them with a bare
// `go runner.WakeLoop(...)` and keep no handle, so nothing downstream
// could tell a cancelled loop from a returned one — and a loop that
// has been signalled may still be mid-query against the eventlog.
// Tearing that handle down underneath it is a use-after-close: the
// race detector caught it as a flaky test teardown (#751), but the
// same ordering is in the daemon, where `defer handle.Close()` is
// registered long before the loops start and therefore runs while they
// are still live.
//
// The zero value is ready to use, and every method is nil-safe so a
// caller that doesn't want a join point can leave the field unset.
//
// Deliberately NOT wired into the eviction path: the registry's
// eviction sweep runs across every tenant, and blocking it on one
// session's in-flight turn would be the trade the existing
// `go bgClose()` comment already rejects. Eviction signals; shutdown
// joins.
// A counter plus a broadcast channel rather than a sync.WaitGroup: a
// session can be created while a drain is in flight (the daemon closes
// its listener first, but "first" is an ordering, not a barrier — an
// in-flight POST /sessions can still be inside the factory), and a
// WaitGroup whose counter goes 0 → 1 with a waiter parked panics with
// "Add called concurrently with Wait". Crashing the process at SIGTERM
// to report that a goroutine was tidy would invert the point of the
// exercise. Here the same interleaving simply keeps the drain open,
// which is also the correct answer: that session has a live loop too.
type WakeLoopGroup struct {
	mu sync.Mutex
	n  int
	// zeroC is non-nil only while at least one WaitFor is parked, and
	// is closed (then dropped) the moment n reaches zero — so a later
	// drain gets a fresh channel and the group is reusable.
	zeroC chan struct{}
}

// add registers a wake loop that is about to start. Must be called on
// the spawning goroutine, before the `go` statement, so a WaitFor that
// races construction can't observe a count of zero and report a drain
// that hasn't happened.
func (g *WakeLoopGroup) add() {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.n++
}

// done marks a registered wake loop as returned.
func (g *WakeLoopGroup) done() {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.n--
	if g.n <= 0 && g.zeroC != nil {
		close(g.zeroC)
		g.zeroC = nil
	}
}

// WaitFor blocks until every wake loop registered with this group has
// returned, or until timeout elapses, and reports whether they all
// returned. A nil group and an empty group both report true
// immediately.
//
// The caller is responsible for cancelling the loops first — this
// waits, it does not signal. A WaitFor with nothing cancelled will
// simply burn the timeout, which is why the daemon derives its own
// cancel for the factory rather than relying on the outermost
// `defer stop()`.
//
// A false return is worth logging rather than swallowing: it means a
// turn is still running against resources the caller is about to tear
// down, and the operator's next symptom would otherwise be a SQLite
// error on a closing connection with no explanation attached.
func (g *WakeLoopGroup) WaitFor(timeout time.Duration) bool {
	if g == nil {
		return true
	}
	g.mu.Lock()
	if g.n <= 0 {
		g.mu.Unlock()
		return true
	}
	if g.zeroC == nil {
		g.zeroC = make(chan struct{})
	}
	zero := g.zeroC
	g.mu.Unlock()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-zero:
		return true
	case <-timer.C:
		return false
	}
}
