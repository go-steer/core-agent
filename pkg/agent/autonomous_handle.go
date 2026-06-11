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
	"sync"
	"sync/atomic"

	"google.golang.org/adk/tool"

	"github.com/go-steer/core-agent/pkg/auth"
)

// AutonomousStatus describes the lifecycle state of a run started
// via StartAutonomous. Read via AutonomousHandle.Status; transitions
// are driven by Pause / Resume / Stop and by the run goroutine's
// terminal handoff.
type AutonomousStatus int

const (
	// AutonomousRunning — goroutine is alive and not paused.
	AutonomousRunning AutonomousStatus = iota
	// AutonomousPaused — Pause was called; loop is blocked at the
	// next pre-turn checkpoint until Resume fires.
	AutonomousPaused
	// AutonomousStopped — Stop was called; goroutine has unwound or
	// is about to (the ctx cancel propagates through the current
	// turn's LLM/tool calls).
	AutonomousStopped
	// AutonomousCompleted — RunAutonomous returned with
	// Reason==Completed.
	AutonomousCompleted
	// AutonomousFailed — RunAutonomous returned with a non-Completed
	// terminal reason (budget exceeded, retry aborted, etc.) or a
	// Go error from the loop machinery.
	AutonomousFailed
)

// String renders the status for diagnostics and tool results.
func (s AutonomousStatus) String() string {
	switch s {
	case AutonomousRunning:
		return "running"
	case AutonomousPaused:
		return "paused"
	case AutonomousStopped:
		return "stopped"
	case AutonomousCompleted:
		return "completed"
	case AutonomousFailed:
		return "failed"
	default:
		return "?"
	}
}

// BuildFunc has the same shape RunAutonomous expects: the driver
// hands it the extra tools it injected (today: just the done tool)
// and the consumer returns a configured *Agent. The Agent's
// session.Service must be wired (durable or in-memory).
type BuildFunc func(extraTools []tool.Tool) (*Agent, error)

// AutonomousHandle is the programmatic-control surface returned by
// StartAutonomous. The autonomous loop runs in its own goroutine;
// methods on the handle are safe for concurrent callers.
//
// Typical usage from a harness:
//
//	h, _ := agent.StartAutonomous(ctx, build, "monitor cluster X",
//	    agent.WithMaxTurns(0), agent.WithMaxWallclock(time.Hour))
//	defer h.Stop()
//	// Inject new instructions as they arrive from outside:
//	h.Inject("priority changed: focus on Q4 review")
//	// Or pause briefly:
//	h.Pause(); ...; h.Resume()
//	// Block until terminal:
//	result, err := h.Wait()
type AutonomousHandle struct {
	// runCtx is the context the autonomous loop runs under. Stop
	// cancels via runCancel.
	runCtx    context.Context
	runCancel context.CancelFunc

	mu         sync.Mutex
	status     AutonomousStatus
	result     *RunResult
	runErr     error
	agent      *Agent // captured once build() returns; used for Inject + emitNoteEvent
	stopCalled atomic.Bool

	// pauseCh is nil when running, a channel when paused. Resume
	// closes it and nils out the field so the next Pause can
	// replace it. The BeforeTurn hook selects on pauseCh and
	// runCtx.Done.
	pauseCh chan struct{}

	done  chan struct{} // closed when the run goroutine exits
	ready chan struct{} // closed when h.agent has been captured (see wrappedBuild in StartAutonomous)
}

// StartAutonomous launches a new autonomous run in a goroutine and
// returns a handle the caller uses to control / observe it.
// Otherwise identical surface to RunAutonomous — same BuildFunc,
// same options. Wait() returns the same RunResult shape.
//
// The goroutine context is derived from ctx with our own cancel
// function so Stop() can cancel independently of the caller's ctx.
// If the caller's ctx fires, the run is still cancelled (the
// derived ctx inherits cancellation).
func StartAutonomous(ctx context.Context, build BuildFunc, goal string, opts ...AutonomousOption) (*AutonomousHandle, error) {
	if build == nil {
		return nil, errors.New("agent: StartAutonomous: build is required")
	}
	runCtx, cancel := context.WithCancel(ctx)
	h := &AutonomousHandle{
		runCtx:    runCtx,
		runCancel: cancel,
		status:    AutonomousRunning,
		done:      make(chan struct{}),
		ready:     make(chan struct{}),
	}

	// Capture the constructed agent so Inject and emitNoteEvent can
	// reach it. Wrap the caller's build in a closure that records
	// the agent before returning it and signals readiness so an
	// out-of-band consumer (e.g. a stdin reader calling Inject) can
	// wait for the agent to be available before sending input that
	// would otherwise fail with "agent not yet constructed".
	wrappedBuild := func(extras []tool.Tool) (*Agent, error) {
		a, err := build(extras)
		if err != nil {
			return nil, err
		}
		h.mu.Lock()
		h.agent = a
		h.mu.Unlock()
		close(h.ready)
		return a, nil
	}

	// Append our BeforeTurn hook so options the caller passes can
	// override unrelated config but not nuke the pause/stop wiring.
	// If a caller wants their own beforeTurn behavior, they can
	// chain by calling our hook from inside theirs.
	allOpts := append([]AutonomousOption{}, opts...)
	allOpts = append(allOpts, WithBeforeTurn(h.beforeTurn))

	go func() {
		defer close(h.done)
		result, err := RunAutonomous(runCtx, wrappedBuild, goal, allOpts...)

		h.mu.Lock()
		h.result = &result
		h.runErr = err
		// Status precedence: Stop already set Stopped; otherwise
		// classify by outcome.
		if h.status != AutonomousStopped {
			switch {
			case err != nil:
				h.status = AutonomousFailed
			case result.Reason == StopReasonCompleted:
				h.status = AutonomousCompleted
			default:
				h.status = AutonomousFailed
			}
		}
		h.mu.Unlock()
	}()
	return h, nil
}

// beforeTurn is the hook AutonomousHandle wires into the autonomous
// loop via WithBeforeTurn. Runs at the top of each iteration; blocks
// while paused (until Resume fires or the run context is cancelled).
func (h *AutonomousHandle) beforeTurn(ctx context.Context, _ int) error {
	h.mu.Lock()
	ch := h.pauseCh
	h.mu.Unlock()
	if ch == nil {
		return nil // not paused, proceed
	}
	select {
	case <-ch:
		// Resumed.
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Pause requests the loop to pause at the next per-turn checkpoint.
// The currently-running turn finishes normally; subsequent turns
// block until Resume fires or Stop / ctx cancellation tears the
// goroutine down.
//
// Idempotent: calling Pause while already paused is a no-op.
// Returns an error only when called after the run has terminated.
//
// Emits a synthetic "paused" event to the agent's eventlog
// (Author="<binary>/autonomous", CustomMetadata.kind="paused") for
// audit, when an eventlog is wired. No-op when not.
func (h *AutonomousHandle) Pause() error {
	h.mu.Lock()
	if h.status == AutonomousStopped || h.status == AutonomousCompleted || h.status == AutonomousFailed {
		h.mu.Unlock()
		return errors.New("agent: AutonomousHandle.Pause: run already terminated")
	}
	if h.pauseCh != nil {
		h.mu.Unlock()
		return nil // already paused
	}
	h.pauseCh = make(chan struct{})
	h.status = AutonomousPaused
	a := h.agent
	h.mu.Unlock()

	if a != nil {
		_ = emitNoteEvent(h.runCtx, a, "paused", "autonomous run paused")
	}
	return nil
}

// Resume unblocks the autonomous loop's BeforeTurn hook so the next
// turn can start. Idempotent: calling Resume while not paused is a
// no-op. Returns an error only when called after the run has
// terminated.
//
// Emits a synthetic "resumed" event to the agent's eventlog for
// audit, when an eventlog is wired.
func (h *AutonomousHandle) Resume() error {
	h.mu.Lock()
	if h.status == AutonomousStopped || h.status == AutonomousCompleted || h.status == AutonomousFailed {
		h.mu.Unlock()
		return errors.New("agent: AutonomousHandle.Resume: run already terminated")
	}
	if h.pauseCh == nil {
		h.mu.Unlock()
		return nil // not paused
	}
	close(h.pauseCh)
	h.pauseCh = nil
	h.status = AutonomousRunning
	a := h.agent
	h.mu.Unlock()

	if a != nil {
		_ = emitNoteEvent(h.runCtx, a, "resumed", "autonomous run resumed")
	}
	return nil
}

// Stop cancels the run's context. The currently-running LLM call
// returns context.Canceled; the loop exits; the goroutine cleans up.
// Idempotent: subsequent Stop calls are no-ops.
//
// If the loop is paused when Stop is called, the ctx cancellation
// unblocks the BeforeTurn hook (which selects on both pauseCh and
// ctx.Done) so the goroutine can exit.
func (h *AutonomousHandle) Stop() error {
	if !h.stopCalled.CompareAndSwap(false, true) {
		return nil
	}
	h.mu.Lock()
	// Mark stopped pre-cancel so the goroutine's terminal block
	// doesn't reclassify to Failed.
	if h.status == AutonomousRunning || h.status == AutonomousPaused {
		h.status = AutonomousStopped
	}
	h.mu.Unlock()
	h.runCancel()
	return nil
}

// Inject queues a message on the underlying agent's inbox. The next
// turn drains the inbox and prepends an "[Inbox]" block to the
// prompt the model sees. Returns an error when called before the
// goroutine has constructed the agent (typically a fraction of a
// second after StartAutonomous returns) or after the agent is
// inaccessible.
func (h *AutonomousHandle) Inject(message string) error {
	return h.InjectAs(message, auth.Caller{})
}

// InjectAs is Inject with a per-message originator identity (see
// Agent.InjectAs). Same lifecycle rules as Inject.
func (h *AutonomousHandle) InjectAs(message string, caller auth.Caller) error {
	h.mu.Lock()
	a := h.agent
	h.mu.Unlock()
	if a == nil {
		return errors.New("agent: AutonomousHandle.Inject: agent not yet constructed")
	}
	return a.InjectAs(message, caller)
}

// RequestWake fires the underlying agent's wake signal, interrupting
// any active scheduler sleep. Pairs with Inject for "operator nudged
// the loop, wake now" semantics; Inject already calls RequestWake
// internally, so this is for the alert-arrival case (or any other
// signal that doesn't carry a message). No-op when the agent hasn't
// been constructed yet.
func (h *AutonomousHandle) RequestWake() {
	h.mu.Lock()
	a := h.agent
	h.mu.Unlock()
	if a == nil {
		return
	}
	a.RequestWake()
}

// Status returns the current lifecycle state. Safe to call any time;
// the goroutine's terminal handoff is mutex-coordinated.
func (h *AutonomousHandle) Status() AutonomousStatus {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.status
}

// Wait blocks until the autonomous goroutine exits, then returns
// the same RunResult + error pair RunAutonomous returns. Safe to
// call from multiple goroutines; the result + err are set under the
// mutex once before the done channel closes.
func (h *AutonomousHandle) Wait() (RunResult, error) {
	<-h.done
	h.mu.Lock()
	defer h.mu.Unlock()
	var res RunResult
	if h.result != nil {
		res = *h.result
	}
	return res, h.runErr
}

// Done returns the channel that closes when the autonomous
// goroutine exits. Useful when a caller wants to combine the wait
// with other selects (e.g. ctx + Done).
func (h *AutonomousHandle) Done() <-chan struct{} { return h.done }

// Ready returns a channel that closes once the underlying agent has
// been constructed (i.e. the wrappedBuild closure inside the
// autonomous loop has run and captured the agent). Inject and
// RequestWake fail with "agent not yet constructed" when called
// before this fires; out-of-band consumers (stdin readers, alert
// watchers) should wait on Ready before issuing those calls. May
// already be closed by the time Ready returns — the select-on-Ready
// pattern handles both cases naturally.
func (h *AutonomousHandle) Ready() <-chan struct{} { return h.ready }
