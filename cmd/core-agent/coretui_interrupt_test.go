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
	"iter"
	"sync"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/model"

	coretui "github.com/go-steer/core-tui/tui"

	"github.com/go-steer/core-agent/v2/pkg/agent"
)

// blockingLLM parks inside GenerateContent until its context is
// cancelled, so a turn driven by it stays in flight for as long as the
// test needs. `entered` is closed on the first call, which is the
// signal that the agent has registered its per-turn cancel and
// Interrupt has something real to cancel.
type blockingLLM struct {
	once    sync.Once
	entered chan struct{}
}

func newBlockingLLM() *blockingLLM {
	return &blockingLLM{entered: make(chan struct{})}
}

func (l *blockingLLM) Name() string { return "gemini-3.5-flash" }

func (l *blockingLLM) GenerateContent(ctx context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		l.once.Do(func() { close(l.entered) })
		<-ctx.Done()
		// Error-only yield: the run loop branches on the error and
		// discards the response, so attaching a payload here would
		// imply a contract this fake never exercises.
		yield(nil, ctx.Err())
	}
}

func interruptAdapter(t *testing.T, llm adkmodel.LLM) *coreAgentAdapter {
	t.Helper()
	// Session ID is per-test so parallel tests can't share one.
	inner, err := agent.New(llm, agent.WithName("test"), agent.WithSession("u-803", "s-"+t.Name()))
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	// No attachAd: Interrupt goes straight to inner, and wiring the
	// attach adapter in would suggest attach is on this path.
	return &coreAgentAdapter{inner: inner}
}

// remoteInterrupter reaches the adapter's Interrupt the only way
// core-tui ever does: through coretui.Agent, type-asserted to the
// optional capability. Every test below dispatches this way on
// purpose — calling a.Interrupt directly would make the tests
// uncompilable against the pre-fix `Interrupt() bool`, which turns
// the fails-first evidence into a build error instead of a failing
// assertion with a message that explains the bug.
func remoteInterrupter(t *testing.T, a coretui.Agent) coretui.RemoteInterrupter {
	t.Helper()
	ri, ok := a.(coretui.RemoteInterrupter)
	if !ok {
		t.Fatal("*coreAgentAdapter does not satisfy coretui.RemoteInterrupter; " +
			"core-tui's /interrupt falls through to \"no turn in flight\" in local --tui mode (#803)")
	}
	return ri
}

// TestCoreAgentAdapter_ImplementsRemoteInterrupter is the #803
// regression at the type level: core-tui discovers /interrupt support
// by type-asserting the host agent to coretui.RemoteInterrupter, so a
// method with the wrong signature is indistinguishable from no method
// at all. The pre-fix adapter had `Interrupt() bool` (and a doc
// comment naming a coretui.Interruptible that never existed), so this
// assertion declined and local --tui mode silently fell through to
// "no turn in flight".
//
// Fails-first proof: revert Interrupt to `Interrupt() bool` and this
// test fails at the type assertion — as do the two behavioral tests
// below, which reach Interrupt through the same helper and so cannot
// get far enough to exercise their own claims. That is the honest
// shape of the evidence: there is one pre-fix defect and it is the
// assertion; the other two tests exist to keep the *post*-fix wiring
// and error semantics from rotting. The compile guard in
// coretui_enabled.go fires on the same drift at build time; the point
// of having both is that the guard stops it while this test says what
// it costs.
func TestCoreAgentAdapter_ImplementsRemoteInterrupter(t *testing.T) {
	t.Parallel()

	a := interruptAdapter(t, newBlockingLLM())
	remoteInterrupter(t, a)
}

// TestCoreAgentAdapter_InterruptCancelsInFlightTurn proves the wiring
// is real and not just shaped right: the call must reach
// agent.Agent.Interrupt and cancel the live turn. Dispatched through
// the coretui.RemoteInterrupter interface value so the test exercises
// the same path core-tui's remoteInterruptCmd does.
func TestCoreAgentAdapter_InterruptCancelsInFlightTurn(t *testing.T) {
	t.Parallel()

	llm := newBlockingLLM()
	a := interruptAdapter(t, llm)

	// The turn runs on its own cancellable context purely as a test
	// backstop: if the fix regresses and Interrupt never reaches
	// inner, this releases the parked model call at test exit instead
	// of leaking a goroutine into the rest of the parallel run. The
	// assertions below never rely on it.
	turnCtx, abandonTurn := context.WithCancel(context.Background())
	defer abandonTurn()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range a.Run(turnCtx, "start a long turn") { //nolint:revive // drain
		}
	}()

	// Wait for the model call to park, which is when the agent has a
	// cancel registered.
	select {
	case <-llm.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("model was never invoked; no turn to interrupt")
	}

	if err := remoteInterrupter(t, a).Interrupt(context.Background()); err != nil {
		t.Fatalf("Interrupt on an in-flight turn = %v, want nil", err)
	}

	select {
	case <-done:
		// The cancel propagated: the parked model call returned and
		// the run loop unwound.
	case <-time.After(10 * time.Second):
		t.Fatal("turn still running 10s after Interrupt; the cancel never reached inner")
	}
}

// TestCoreAgentAdapter_InterruptIdleReturnsError locks in the error
// semantics chosen in the fix. core-tui's remoteInterruptDoneMsg
// handler reads a nil error as "the cancel landed": it prints
// "/interrupt: remote turn cancelled" and calls endLiveStretch() to
// stop the spinner. Returning nil for an idle agent would therefore
// claim a cancel that never happened. A non-nil error renders as an
// inline RoleError row instead.
func TestCoreAgentAdapter_InterruptIdleReturnsError(t *testing.T) {
	t.Parallel()

	a := interruptAdapter(t, newBlockingLLM())

	err := remoteInterrupter(t, a).Interrupt(context.Background())
	if err == nil {
		t.Fatal("Interrupt with no turn in flight = nil, want an error; " +
			"nil makes core-tui report a successful cancel and kill the spinner")
	}
	if got, want := err.Error(), "no turn in flight"; got != want {
		t.Errorf("Interrupt error = %q, want %q", got, want)
	}
}

// TestCoreAgentAdapter_InterruptIgnoresContext pins the other half of
// the decision documented on the method: the ctx parameter exists
// because coretui.RemoteInterrupter requires it, and this
// implementation deliberately does not consult it — the in-process
// cancel is a local function-pointer call with no I/O for core-tui's
// 5s deadline to guard. An already-cancelled ctx must therefore still
// cancel the turn. Without this, a well-meaning future edit could add
// a ctx.Err() precheck and turn every interrupt fired near a shutdown
// into a silent no-op, and only the doc comment would object.
func TestCoreAgentAdapter_InterruptIgnoresContext(t *testing.T) {
	t.Parallel()

	llm := newBlockingLLM()
	a := interruptAdapter(t, llm)

	turnCtx, abandonTurn := context.WithCancel(context.Background())
	defer abandonTurn()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range a.Run(turnCtx, "start a long turn") { //nolint:revive // drain
		}
	}()

	select {
	case <-llm.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("model was never invoked; no turn to interrupt")
	}

	dead, cancel := context.WithCancel(context.Background())
	cancel()
	if err := remoteInterrupter(t, a).Interrupt(dead); err != nil {
		t.Fatalf("Interrupt with an already-cancelled ctx = %v, want nil (ctx must not gate the cancel)", err)
	}

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("turn still running 10s after Interrupt with a dead ctx; the cancel was gated on ctx")
	}
}
