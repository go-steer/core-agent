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

package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/core-agent/v2/pkg/auth"
	"github.com/go-steer/core-agent/v2/pkg/models/mock"
)

// newPauseTestAgent builds a minimal agent over the echo provider.
func newPauseTestAgent(t *testing.T) *Agent {
	t.Helper()
	m, err := mock.NewEcho().Model(context.Background(), "echo")
	if err != nil {
		t.Fatalf("model: %v", err)
	}
	a, err := New(m)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	return a
}

func TestAgent_Pause_IsIdempotentAndProjects(t *testing.T) {
	t.Parallel()

	a := newPauseTestAgent(t)
	if a.Paused() {
		t.Fatalf("fresh agent reports paused")
	}
	if got := (a.PauseState()); got.Paused {
		t.Errorf("PauseState on a fresh agent = %+v, want zero", got)
	}

	if !a.Pause("") {
		t.Fatalf("first Pause returned false, want true (gate transitioned)")
	}
	if a.Pause("") {
		t.Errorf("second Pause returned true, want false (already paused)")
	}

	st := a.PauseState()
	if !st.Paused {
		t.Errorf("PauseState.Paused = false after Pause")
	}
	if st.Reason != PauseReasonOperatorPause {
		t.Errorf("PauseState.Reason = %q, want %q", st.Reason, PauseReasonOperatorPause)
	}
	if st.Interrupted {
		t.Errorf("PauseState.Interrupted = true for a plain Pause; want false (no turn was cancelled)")
	}
	if st.Since.IsZero() {
		t.Errorf("PauseState.Since is zero")
	}

	if !a.Resume() {
		t.Errorf("Resume returned false, want true")
	}
	if a.Resume() {
		t.Errorf("second Resume returned true, want false (not paused)")
	}
	if a.Paused() {
		t.Errorf("still paused after Resume")
	}
}

// TestAgent_Run_BlocksWhilePaused is the core of the state machine: a
// parked agent starts no turn until someone resumes it.
func TestAgent_Run_BlocksWhilePaused(t *testing.T) {
	t.Parallel()

	a := newPauseTestAgent(t)
	a.Pause("")

	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		var runErr error
		for _, err := range a.Run(context.Background(), "hi") {
			if err != nil {
				runErr = err
			}
		}
		done <- runErr
	}()

	<-started
	select {
	case err := <-done:
		t.Fatalf("Run completed while the agent was paused (err=%v); the gate did not hold", err)
	case <-time.After(150 * time.Millisecond):
		// Expected: still parked.
	}

	a.Resume()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run after Resume: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("Run did not complete within 5s of Resume")
	}
}

// TestAgent_Run_PausedReleasesOnContextCancel proves the gate can't
// wedge a driver at shutdown: daemon teardown and per-session eviction
// both cancel the loop ctx, and awaitResume has to honor it.
func TestAgent_Run_PausedReleasesOnContextCancel(t *testing.T) {
	t.Parallel()

	a := newPauseTestAgent(t)
	a.Pause("")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		var runErr error
		for _, err := range a.Run(ctx, "hi") {
			if err != nil {
				runErr = err
			}
		}
		done <- runErr
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("Run did not return within 5s of ctx cancellation while paused")
	}
}

// TestAgent_PausedDoesNotDrainInbox is why awaitResume runs FIRST in
// Run. If the gate sat anywhere after the drain, the steer an operator
// typed while parked would be consumed by a turn that never ran.
func TestAgent_PausedDoesNotDrainInbox(t *testing.T) {
	t.Parallel()

	a := newPauseTestAgent(t)
	a.Pause("")

	// Inject as auto-continue so the implicit operator resume doesn't
	// fire — we want to observe the paused-with-queued-message state.
	if err := a.InjectAs("queued while parked", auth.Caller{Identity: AutoContinueOriginator}); err != nil {
		t.Fatalf("InjectAs: %v", err)
	}

	go func() {
		for range a.Run(context.Background(), "") { //nolint:revive // draining
		}
	}()

	time.Sleep(150 * time.Millisecond)
	if got := a.PendingInboxCount(); got != 1 {
		t.Errorf("PendingInboxCount while paused = %d, want 1 (a parked agent must not drain its inbox)", got)
	}
}

// TestAgent_InterruptAndHold_CancelsAndParks covers the default
// /interrupt gesture end to end at the library level.
func TestAgent_InterruptAndHold_CancelsAndParks(t *testing.T) {
	t.Parallel()

	a := newPauseTestAgent(t)
	turnCtx, turnCancel := context.WithCancel(context.Background())
	defer turnCancel()
	a.setCancelInFlight(turnCancel)

	interrupted, paused := a.InterruptAndHold("")
	if !interrupted {
		t.Errorf("InterruptAndHold.interrupted = false with a turn in flight")
	}
	if !paused {
		t.Errorf("InterruptAndHold.paused = false")
	}
	select {
	case <-turnCtx.Done():
	case <-time.After(100 * time.Millisecond):
		t.Errorf("in-flight turn ctx not cancelled within 100ms")
	}

	st := a.PauseState()
	if !st.Paused || st.Reason != PauseReasonOperatorInterrupt || !st.Interrupted {
		t.Errorf("PauseState = %+v, want paused with reason=%q interrupted=true", st, PauseReasonOperatorInterrupt)
	}
	// The audit flag must be armed BEFORE the cancel fires so the
	// interrupted turn's own cleanup always sees it (#565 ordering).
	if !a.pendingInterruptAudit.Load() {
		t.Errorf("pendingInterruptAudit not armed by InterruptAndHold; the audit row can land a turn late and let auto-continue re-drive the killed work")
	}
}

// TestAgent_InterruptAndHold_ParksWhenIdle: an operator who hits
// interrupt a beat after the turn ended still meant "stop".
func TestAgent_InterruptAndHold_ParksWhenIdle(t *testing.T) {
	t.Parallel()

	a := newPauseTestAgent(t)
	interrupted, paused := a.InterruptAndHold("")
	if interrupted {
		t.Errorf("interrupted = true with no turn in flight")
	}
	if !paused || !a.Paused() {
		t.Errorf("idle InterruptAndHold did not park the agent")
	}
	if a.PauseState().Interrupted {
		t.Errorf("PauseState.Interrupted = true, want false (nothing was cancelled)")
	}
	if a.pendingInterruptAudit.Load() {
		t.Errorf("pendingInterruptAudit armed for an idle interrupt; no turn was killed, so there is nothing to audit")
	}
}

// TestAgent_Pause_FirstCauseWins: a plain Pause landing on top of an
// operator-interrupt must not erase the fact that work was cancelled.
func TestAgent_Pause_FirstCauseWins(t *testing.T) {
	t.Parallel()

	a := newPauseTestAgent(t)
	_, turnCancel := context.WithCancel(context.Background())
	a.setCancelInFlight(turnCancel)
	a.InterruptAndHold("")

	a.Pause("")
	st := a.PauseState()
	if st.Reason != PauseReasonOperatorInterrupt {
		t.Errorf("Reason = %q after a Pause on top of an interrupt, want %q", st.Reason, PauseReasonOperatorInterrupt)
	}
	if !st.Interrupted {
		t.Errorf("Interrupted flag cleared by a subsequent Pause")
	}
}

// TestAgent_InjectResumesForOperatorNotAutoContinue is the
// compatibility hinge: interrupt-then-inject keeps working exactly as
// it did before pause existed, while auto-continue can't un-park a loop
// a human parked.
func TestAgent_InjectResumesForOperatorNotAutoContinue(t *testing.T) {
	t.Parallel()

	t.Run("auto-continue does not resume", func(t *testing.T) {
		t.Parallel()
		a := newPauseTestAgent(t)
		a.Pause("")
		if err := a.InjectAs("carry on", auth.Caller{Identity: AutoContinueOriginator}); err != nil {
			t.Fatalf("InjectAs: %v", err)
		}
		if !a.Paused() {
			t.Errorf("auto-continue inject un-parked the agent")
		}
	})

	t.Run("identified operator resumes", func(t *testing.T) {
		t.Parallel()
		a := newPauseTestAgent(t)
		a.Pause("")
		if err := a.InjectAs("do this instead", auth.Caller{Identity: "operator@example.com"}); err != nil {
			t.Fatalf("InjectAs: %v", err)
		}
		if a.Paused() {
			t.Errorf("operator inject did not resume the agent")
		}
	})

	t.Run("anonymous inject resumes", func(t *testing.T) {
		t.Parallel()
		a := newPauseTestAgent(t)
		a.Pause("")
		if err := a.Inject("do this instead"); err != nil {
			t.Fatalf("Inject: %v", err)
		}
		if a.Paused() {
			t.Errorf("legacy zero-identity inject did not resume the agent; single-user deployments would park forever")
		}
	})
}

func TestFormatInterruptSteer(t *testing.T) {
	t.Parallel()

	if got := FormatInterruptSteer("   "); got != "" {
		t.Errorf("FormatInterruptSteer(blank) = %q, want empty so callers fall back to the continue framing", got)
	}
	got := FormatInterruptSteer("check the logs first")
	if !strings.Contains(got, "check the logs first") {
		t.Errorf("steer text missing from framing:\n%s", got)
	}
	for _, want := range []string{"[The operator interrupted you]", "Do not silently resume"} {
		if !strings.Contains(got, want) {
			t.Errorf("framing missing %q:\n%s", want, got)
		}
	}
}

func TestFormatInterruptContinue(t *testing.T) {
	t.Parallel()

	got := FormatInterruptContinue()
	// The re-check line is the load-bearing one: a cancelled tool call
	// has an unknown real-world effect even after tail repair (#537)
	// makes the history well-formed again.
	if !strings.Contains(got, "re-check its state") {
		t.Errorf("continue framing does not warn about the cancelled tool call:\n%s", got)
	}
	if !strings.Contains(got, "carry on") {
		t.Errorf("continue framing missing the operator-said-carry-on context:\n%s", got)
	}
}
