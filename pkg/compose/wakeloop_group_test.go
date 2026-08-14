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
	"context"
	"testing"
	"time"

	"github.com/go-steer/core-agent/v2/pkg/auth"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

// #751: buildSession started every per-session wake loop with a bare
// `go runner.WakeLoop(...)` and kept no handle, so cancelling the
// daemon context only ever *signalled* them. Nothing downstream could
// tell a signalled loop from a returned one — and a signalled loop can
// still be mid-query against the eventlog, which is what the race
// detector caught when a test's `t.Cleanup` closed the handle under it.
//
// These two are the contract that replaces "probably finished by now":
// a live loop keeps WaitFor blocked, and a cancelled one releases it.
// The first assertion is the one that fails on pre-fix code, where the
// group is never told a loop exists and WaitFor returns true against a
// goroutine that is still running.

// grpWaitTimeout is the positive-path bound. Generous because a false
// failure here would read as a flaky test in a PR about a flaky test;
// a broken join point never returns at all, so the only cost of being
// generous is how long the failure takes to print.
const grpWaitTimeout = 30 * time.Second

func TestWakeLoopGroup_FactoryRegistersEverySessionsLoop(t *testing.T) {
	t.Parallel()

	ctx, cancelDaemon := context.WithCancel(context.Background())
	t.Cleanup(cancelDaemon)

	group := &WakeLoopGroup{}
	factory := BuildSessionFactory(SessionFactoryDeps{
		DaemonCtx: ctx,
		WakeLoops: group,
		Model:     stubLLM{},
		Template:  permissions.New(permissions.Options{}),
	})
	if factory == nil {
		t.Fatal("BuildSessionFactory returned nil")
	}
	caller := auth.Caller{Identity: "sre@example.com"}
	for i := 0; i < 3; i++ {
		if _, cancelSession, err := factory(ctx, caller); err != nil {
			t.Fatalf("factory (session %d): %v", i, err)
		} else {
			t.Cleanup(cancelSession)
		}
	}

	// Three loops are running and nothing has been cancelled, so a
	// drain must NOT report success. Pre-fix this returns immediately:
	// the group was never told the goroutines exist.
	if group.WaitFor(100 * time.Millisecond) {
		t.Fatal("WaitFor reported every wake loop returned while three are still running — the factory never registered them, so the join point is reporting an empty group as a completed drain")
	}

	cancelDaemon()
	if !group.WaitFor(grpWaitTimeout) {
		t.Errorf("wake loops did not return within %s of daemon-context cancel", grpWaitTimeout)
	}
}

// Eviction cancels one session without touching the daemon context.
// The group has to see that loop return too, or a daemon that evicts
// and then shuts down would wait out the full drain window for a
// goroutine that finished minutes ago.
func TestWakeLoopGroup_EvictionReleasesItsOwnLoop(t *testing.T) {
	t.Parallel()

	ctx, cancelDaemon := context.WithCancel(context.Background())
	t.Cleanup(cancelDaemon)

	group := &WakeLoopGroup{}
	factory := BuildSessionFactory(SessionFactoryDeps{
		DaemonCtx: ctx,
		WakeLoops: group,
		Model:     stubLLM{},
		Template:  permissions.New(permissions.Options{}),
	})
	caller := auth.Caller{Identity: "sre@example.com"}
	_, evict, err := factory(ctx, caller)
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	if group.WaitFor(100 * time.Millisecond) {
		t.Fatal("WaitFor succeeded before the session was evicted")
	}
	evict()
	if !group.WaitFor(grpWaitTimeout) {
		t.Errorf("evicted session's wake loop did not return within %s", grpWaitTimeout)
	}
}

func TestWakeLoopGroup_NilAndEmptyDrainImmediately(t *testing.T) {
	t.Parallel()

	// A caller that wants no join point leaves the field nil, and the
	// factory calls add/done on it unconditionally — so nil has to be
	// inert rather than a panic at session construction.
	var nilGroup *WakeLoopGroup
	nilGroup.add()
	nilGroup.done()
	// grpWaitTimeout, not a millisecond: WaitFor observes an empty
	// group through a goroutine, and on a loaded machine that goroutine
	// can miss a 1ms deadline it would otherwise beat by orders of
	// magnitude. A PR about a flaky test should not ship one.
	if !nilGroup.WaitFor(grpWaitTimeout) {
		t.Error("nil group must report a completed drain")
	}
	if !(&WakeLoopGroup{}).WaitFor(grpWaitTimeout) {
		t.Error("empty group must report a completed drain")
	}
}

// The daemon closes its attach listener before it drains, but that is
// an ordering, not a barrier: a POST /sessions already inside the
// factory can register a loop while the drain is parked. Two things
// must hold. The drain stays open — that session has a live loop, and
// reporting a completed drain over it is the bug this type exists to
// prevent. And the registration must not blow up the process: a
// sync.WaitGroup going 0 → 1 with a waiter parked panics, which would
// turn a tidy-shutdown feature into a crash at SIGTERM.
func TestWakeLoopGroup_RegistrationDuringADrainKeepsItOpen(t *testing.T) {
	t.Parallel()

	group := &WakeLoopGroup{}
	first := make(chan struct{})
	group.add()
	go func() {
		defer group.done()
		<-first
	}()

	// Park a waiter, then drop the count to zero and immediately bring
	// it back — the interleaving the listener-close ordering doesn't
	// exclude.
	parked := make(chan bool, 1)
	go func() { parked <- group.WaitFor(grpWaitTimeout) }()
	second := make(chan struct{})
	group.add()
	go func() {
		defer group.done()
		<-second
	}()
	close(first)

	select {
	case got := <-parked:
		t.Fatalf("WaitFor returned %v while a loop registered during the drain is still running", got)
	case <-time.After(100 * time.Millisecond):
	}

	close(second)
	select {
	case got := <-parked:
		if !got {
			t.Error("WaitFor reported a failed drain after every loop returned")
		}
	case <-time.After(grpWaitTimeout):
		t.Errorf("WaitFor did not observe the drain completing within %s", grpWaitTimeout)
	}

	// And the group is reusable: a registration after the count has
	// reached zero — the interleaving that panics a WaitGroup — opens a
	// new drain window rather than reporting the old one's result.
	third := make(chan struct{})
	group.add()
	go func() {
		defer group.done()
		<-third
	}()
	if group.WaitFor(100 * time.Millisecond) {
		t.Error("WaitFor reported a completed drain for a loop registered after the previous drain finished")
	}
	close(third)
	if !group.WaitFor(grpWaitTimeout) {
		t.Errorf("WaitFor did not observe the second drain completing within %s", grpWaitTimeout)
	}
}

func TestWakeLoopGroup_WaitForTimesOutOnAStuckLoop(t *testing.T) {
	t.Parallel()

	group := &WakeLoopGroup{}
	release := make(chan struct{})
	group.add()
	go func() {
		defer group.done()
		<-release
	}()
	if group.WaitFor(50 * time.Millisecond) {
		t.Error("WaitFor reported success while a registered loop is still running")
	}
	close(release)
	if !group.WaitFor(grpWaitTimeout) {
		t.Errorf("WaitFor did not observe the loop returning within %s", grpWaitTimeout)
	}
}
