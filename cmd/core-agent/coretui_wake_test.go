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

//go:build !no_tui

package main

import (
	"context"
	"sync"
	"testing"

	adkmodel "google.golang.org/adk/model"

	coretui "github.com/go-steer/core-tui/tui"

	"github.com/go-steer/core-agent/v2/pkg/agent"
)

// wakeProvider hands back a fresh stand-in LLM for any model ID so
// SwitchModel's real body — including buildAttachedAgent — runs end to
// end without a provider credential.
type wakeProvider struct{}

func (wakeProvider) Name() string { return "fake" }

func (wakeProvider) Model(context.Context, string) (adkmodel.LLM, error) {
	return newBlockingLLM(), nil
}

// wakeAdapter builds the local `--tui` adapter over a real
// *agent.Agent, wired the way launchTUIv2 wires it (own release
// group). No LLM turn is driven here — the wake signal is independent
// of the model — so the same blocking fake the interrupt tests use is
// reused purely as a stand-in LLM.
func wakeAdapter(t *testing.T) (*coreAgentAdapter, *agent.Agent) {
	t.Helper()
	inner, err := agent.New(newBlockingLLM(),
		agent.WithName("test"), agent.WithSession("u-813", "s-"+t.Name()))
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	return &coreAgentAdapter{
		inner:   inner,
		deps:    tuiDeps{Provider: wakeProvider{}},
		wakeRel: &wakeReleases{},
	}, inner
}

// wakeRequester reaches the adapter's wake channel the only way
// core-tui ever does: through coretui.Agent, type-asserted to the
// optional capability (tui/agentcmd.go's wakeListener).
func wakeRequester(t *testing.T, a coretui.Agent) coretui.WakeRequester {
	t.Helper()
	wr, ok := a.(coretui.WakeRequester)
	if !ok {
		t.Fatal("*coreAgentAdapter does not satisfy coretui.WakeRequester; local --tui mode would show no wake toast at all")
	}
	return wr
}

// pending reports whether ch holds a wake right now. No timer: every
// call below happens after RequestWake has returned, and the agent's
// fan-out completes synchronously before it does, so a subscriber
// that has not been delivered to by then never will be. A timeout
// here would let a regression that reaches only one subscriber pass
// on a lucky schedule, which is worse than no test.
func pending(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// TestCoreAgentAdapter_WakeDoesNotStealFromTheDriver is the #813
// regression.
//
// Pre-fix, WakeRequested was `return a.inner.WakeRequested()` — the
// agent's own buffered-1 channel, which delivers each value to
// exactly one receiver. core-tui's listener and the agent's driver
// (pkg/runner's REPL/wake loops, or the SleepScheduler the autonomous
// driver parks in) then took turns: a wake either pierced the sleep
// or raised the toast, never reliably both, and neither side could
// tell it had lost one.
//
// Fails-first evidence: restore the aliasing body and the second
// assertion fails deterministically — the single RequestWake lands in
// the one shared buffer, `driver` drains it, and the adapter's
// channel is empty. Not a timing question; there is only one token.
func TestCoreAgentAdapter_WakeDoesNotStealFromTheDriver(t *testing.T) {
	t.Parallel()
	ad, inner := wakeAdapter(t)
	defer ad.closeWake()

	// The driver's subscription, taken the way pkg/runner and the
	// autonomous scheduler take it.
	driver := inner.WakeRequested()
	tui := wakeRequester(t, ad).WakeRequested()
	if tui == nil {
		t.Fatal("adapter WakeRequested() returned nil; core-tui declines the subscription on nil")
	}
	// Error, not Fatal, so the behavioral assertions below still run
	// and report the consequence rather than only the cause.
	if tui == driver {
		t.Error("adapter handed core-tui the agent's own wake channel; the TUI and the driver will take turns consuming wakes")
	}

	inner.RequestWake()

	if !pending(driver) {
		t.Error("the driver saw no wake — a sleeping scheduler would run to term instead of being interrupted")
	}
	if !pending(tui) {
		t.Error("the TUI saw no wake — the operator toast never fires")
	}
}

// TestCoreAgentAdapter_WakeRequestedIsStableAcrossCalls pins the
// property that makes a per-subscriber fan-out safe to put behind
// this method at all. core-tui re-invokes WakeRequested to re-arm its
// listener after every single wake, so a body that minted a fresh
// subscription per call would register one more channel on the agent
// per wake and never release any of them.
func TestCoreAgentAdapter_WakeRequestedIsStableAcrossCalls(t *testing.T) {
	t.Parallel()
	ad, inner := wakeAdapter(t)
	defer ad.closeWake()

	first := ad.WakeRequested()
	inner.RequestWake()
	if !pending(first) {
		t.Fatal("first subscription saw no wake")
	}
	second := ad.WakeRequested() // core-tui's re-arm
	if second != first {
		t.Fatal("WakeRequested returned a different channel on re-arm; core-tui would leak one subscription per wake")
	}

	inner.RequestWake()
	if !pending(second) {
		t.Error("the re-armed listener saw no wake")
	}
}

// TestCoreAgentAdapter_CloseWakeReleasesTheSubscription is the
// teardown half. A fan-out with no unsubscribe leaks a channel per
// TUI session; launchTUIv2 defers this call so the agent stops
// fanning out to a listener that has gone away.
func TestCoreAgentAdapter_CloseWakeReleasesTheSubscription(t *testing.T) {
	t.Parallel()
	ad, inner := wakeAdapter(t)
	driver := inner.WakeRequested()
	tui := ad.WakeRequested()

	ad.closeWake()
	ad.closeWake() // idempotent

	inner.RequestWake()

	if pending(tui) {
		t.Error("a released subscription still received a wake")
	}
	if !pending(driver) {
		t.Error("releasing the TUI subscription stopped delivery to the driver")
	}
}

// TestCoreAgentAdapter_WakeRequestedIsGoroutineSafe is the -race
// guard on the lazy subscribe. core-tui re-arms from its Update
// goroutine while the returned tea.Cmd runs on another, and
// launchTUIv2's deferred closeWake can land during either — so the
// once-guarded field writes have to be visible across all three.
func TestCoreAgentAdapter_WakeRequestedIsGoroutineSafe(t *testing.T) {
	t.Parallel()
	ad, inner := wakeAdapter(t)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ch := ad.WakeRequested(); ch != nil {
				select {
				case <-ch:
				default:
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		inner.RequestWake()
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		ad.closeWake()
	}()
	wg.Wait()

	// Whichever order they landed in, the subscription is released:
	// closeWake either unsubscribed a real one or claimed the once so
	// no later call could take one.
	ad.closeWake()
	ch := ad.WakeRequested()
	if ch == nil {
		return // never subscribed; nothing more to assert
	}
	// Drop anything the racing RequestWake left buffered before it was
	// released — the assertion is about NEW deliveries.
	pending(ch)
	inner.RequestWake()
	if pending(ch) {
		t.Error("a wake reached the adapter after closeWake")
	}
}

// TestCoreAgentAdapter_SwitchModelSubscriptionIsReleasedToo is the
// teardown hole a `/model` swap opens.
//
// core-tui re-arms its wake listener against whatever is in
// m.opts.Agent at the time of the re-arm (tui/update.go re-issues
// wakeListener after every wakeMsg; tui/agentcmd.go reads
// m.opts.Agent fresh), and applyModelSwitch has by then replaced it
// with the adapter SwitchModel returned. So the subscription can land
// on an adapter launchTUIv2 never sees, and its own
// `defer wrapped.closeWake()` covers only the first one. The shared
// wakeReleases group is what closes that gap.
//
// Fails-first evidence: drop `wakeRel: a.wakeRel` from SwitchModel and
// the successor's subscription survives closeWake — the final
// assertion sees a wake on a channel that should have gone quiet.
func TestCoreAgentAdapter_SwitchModelSubscriptionIsReleasedToo(t *testing.T) {
	t.Parallel()
	ad, _ := wakeAdapter(t)
	_ = ad.WakeRequested() // the pre-swap listener

	next, err := ad.SwitchModel("gemini-3.5-pro")
	if err != nil {
		t.Fatalf("SwitchModel: %v", err)
	}
	child, ok := next.(*coreAgentAdapter)
	if !ok {
		t.Fatalf("SwitchModel returned %T, want *coreAgentAdapter", next)
	}
	// The re-arm core-tui performs after the swap.
	ch := wakeRequester(t, next).WakeRequested()
	if ch == nil {
		t.Fatal("the post-swap adapter handed core-tui a nil wake channel")
	}

	ad.closeWake() // the only teardown launchTUIv2 has

	child.inner.RequestWake()
	if pending(ch) {
		t.Error("the /model successor's wake subscription outlived the TUI session; nothing ever releases it")
	}
}

// TestCoreAgentAdapter_SwitchModelSubscriptionAfterTeardown covers the
// same path in the other order: a re-arm that lands while the TUI is
// already unwinding. The group is closed by then, so the subscription
// is released as soon as it is registered rather than retained for the
// life of the swapped-in agent.
func TestCoreAgentAdapter_SwitchModelSubscriptionAfterTeardown(t *testing.T) {
	t.Parallel()
	ad, _ := wakeAdapter(t)
	next, err := ad.SwitchModel("gemini-3.5-pro")
	if err != nil {
		t.Fatalf("SwitchModel: %v", err)
	}
	child := next.(*coreAgentAdapter)

	ad.closeWake()

	ch := child.WakeRequested()
	child.inner.RequestWake()
	if ch != nil && pending(ch) {
		t.Error("a subscription taken after teardown stayed live")
	}
}

// TestCoreAgentAdapter_CloseWakeWithoutSubscribing covers an adapter
// nothing ever asked for a channel — closeWake on it must be a no-op
// rather than a panic on a nil unsubscribe, and it must not leave the
// door open for a later subscription that nobody will release.
func TestCoreAgentAdapter_CloseWakeWithoutSubscribing(t *testing.T) {
	t.Parallel()
	ad, inner := wakeAdapter(t)

	ad.closeWake() // must not panic

	// The driver is unaffected, and the once is spent, so a late
	// WakeRequested cannot register a subscription after teardown.
	inner.RequestWake()
	if !pending(inner.WakeRequested()) {
		t.Error("the driver stopped receiving wakes after an unused adapter was closed")
	}
	if ch := ad.WakeRequested(); ch != nil {
		t.Error("WakeRequested subscribed after closeWake; a post-teardown subscription is never released")
	}
}
