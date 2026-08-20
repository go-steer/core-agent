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
	"time"

	"github.com/go-steer/core-agent/v2/pkg/attach"
	"github.com/go-steer/core-agent/v2/pkg/auth"
)

// Pause reasons stamped on PauseState.Reason. Bare strings (not a Go
// enum) because they cross the attach wire into the TUI and mast-web,
// which shouldn't need a Go type to render them.
const (
	// PauseReasonOperatorInterrupt is set by InterruptAndHold — an
	// operator stopped a turn and the loop is holding for their next
	// instruction.
	PauseReasonOperatorInterrupt = "operator-interrupt"
	// PauseReasonOperatorPause is set by a plain Pause — no turn was
	// cancelled; the loop just isn't allowed to start another one.
	PauseReasonOperatorPause = "operator-pause"
)

// PauseState is the projection every operator surface renders from —
// the TUI banner, GET /sessions/.../status, and the `pause` SSE event.
// Zero value means "not paused".
type PauseState struct {
	Paused bool
	// Since is when the gate closed (UTC). Zero when not paused.
	Since time.Time
	// Reason is one of the PauseReason* constants. Empty when not
	// paused.
	Reason string
	// Interrupted reports whether a turn was actually cancelled on the
	// way into this pause. False for a plain Pause, and false for an
	// InterruptAndHold that landed while the agent was idle — the
	// distinction the operator needs to know whether work was lost.
	Interrupted bool
}

// Pause closes the pause gate: no NEW turn starts until Resume (or a
// non-auto-continue Inject, which resumes implicitly — see InjectAs).
// A turn already in flight is deliberately left alone and runs to
// completion; there is no safe suspend point inside a model call, and
// reporting "paused" while tokens keep burning would be a lie. Use
// InterruptAndHold when the in-flight turn should die too.
//
// Idempotent: reports whether this call actually closed the gate.
// Empty reason defaults to PauseReasonOperatorPause.
//
// See docs/operator-interrupt-design.md for the full state machine.
func (a *Agent) Pause(reason string) bool {
	if a == nil {
		return false
	}
	if reason == "" {
		reason = PauseReasonOperatorPause
	}
	a.mu.Lock()
	transitioned := a.pauseLocked(reason, false)
	a.mu.Unlock()
	if transitioned {
		a.emitPause(reason, false)
	}
	return transitioned
}

// pauseLocked is the gate-closing core. Caller holds a.mu.
//
// When already paused it keeps the ORIGINAL reason and timestamp —
// first cause wins, so a plain Pause landing on top of an
// operator-interrupt doesn't erase the fact that work was cancelled.
// The one field it will upgrade is Interrupted: an interrupt arriving
// while already paused (a turn was somehow in flight anyway) is new
// information the operator needs.
func (a *Agent) pauseLocked(reason string, interrupted bool) bool {
	if a.pauseCh != nil {
		if interrupted && !a.pauseInterrupted {
			a.pauseInterrupted = true
			a.pauseReason = reason
		}
		return false
	}
	a.pauseCh = make(chan struct{})
	a.pauseSince = time.Now().UTC()
	a.pauseReason = reason
	a.pauseInterrupted = interrupted
	return true
}

// Resume opens the pause gate so the next turn can start. Idempotent:
// reports whether this call actually opened it (false when the agent
// wasn't paused, so a double-click from two operator surfaces isn't an
// error).
//
// Resume does NOT itself drive a turn. Callers that want the agent to
// pick work back up pair it with an Inject (steer text or a continue
// note) and RequestWake — see the resume dispositions in
// docs/operator-interrupt-design.md.
func (a *Agent) Resume() bool { return a.ResumeWithMode("") }

// ResumeWithMode is Resume carrying the operator's disposition
// ("steer" / "continue" / "abandon") for the `pause` SSE event, so a
// second client watching the same session can render what the operator
// chose rather than just that something changed. The mode is purely
// observational — the injecting is the caller's job either way.
func (a *Agent) ResumeWithMode(mode string) bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	ch := a.pauseCh
	if ch == nil {
		a.mu.Unlock()
		return false
	}
	a.pauseCh = nil
	a.pauseSince = time.Time{}
	a.pauseReason = ""
	a.pauseInterrupted = false
	a.mu.Unlock()
	// Closed outside the lock: awaitResume re-takes a.mu on wake, and
	// closing under the lock would make every blocked driver contend
	// with the resumer for no reason.
	close(ch)
	a.Emit(attach.EventPause, attach.PauseEvent{
		State: attach.PauseStateResumed,
		Mode:  mode,
		At:    time.Now().UTC(),
	})
	return true
}

// ResumeWith is the operator's full resume disposition in one call:
// queue the message (if any), open the gate, and wake the loop — in
// that order.
//
// The order is the point. Injecting first means the steer is already
// on the queue when the gate opens, so the turn the resume releases
// picks it up; opening the gate first leaves a window where a wake
// loop blocked in awaitResume starts an un-steered turn and the
// operator's instruction lands a turn late, against work they'd just
// redirected.
//
// message is the already-framed text (see FormatInterruptSteer /
// FormatInterruptContinue) — empty for an abandon, which opens the
// gate and wakes nothing. mode is carried on the `pause` SSE event.
//
// Returns whether the gate was actually open()ed by this call; false
// (with no error) when the agent wasn't paused, which is the
// idempotent case, not a failure. A queued message is still queued and
// woken in that case: an operator whose resume raced someone else's
// still gets their instruction delivered.
//
// ResumeWith takes no context, so a resume-steer message carries no
// trace context and contributes no link to the turn it shapes (unlike
// InjectAsContext). Deliberate for now: changing this exported
// signature would break every caller, and the resume path is not the
// one the cross-process watcher→daemon story runs through.
func (a *Agent) ResumeWith(mode, message string, caller auth.Caller) (bool, error) {
	if a == nil {
		return false, errors.New("agent: nil receiver")
	}
	if message != "" {
		// The prompt_id is dropped here on purpose: ResumeWith's
		// exported signature already returns (bool, error), and a
		// resume-steer is answered on the /resume response, which has
		// no field for it. A caller that needs the id can inject the
		// framed text itself with InjectAsContextWithID and then
		// resume.
		if _, err := a.injectAs(context.Background(), message, caller, injectMode{wake: true}); err != nil {
			return false, err
		}
	}
	resumed := a.ResumeWithMode(mode)
	if message != "" {
		// injectAs already requested a wake; this covers the abandon-
		// adjacent case where the caller wants the loop moving again
		// without new text. Cheap and idempotent either way.
		return resumed, nil
	}
	if mode != attach.ResumeModeAbandon {
		a.RequestWake()
	}
	return resumed, nil
}

// emitPause publishes a gate-closed event. Emitted from the agent
// rather than the attach handler so EVERY path that parks the loop —
// the HTTP endpoints, autonomous.Handle.Pause, a library caller —
// reaches connected operators identically.
func (a *Agent) emitPause(reason string, interrupted bool) {
	a.Emit(attach.EventPause, attach.PauseEvent{
		State:       attach.PauseStatePaused,
		Reason:      reason,
		Interrupted: interrupted,
		At:          time.Now().UTC(),
	})
}

// Paused reports whether the gate is currently closed. Cheap enough
// for a status poll.
func (a *Agent) Paused() bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.pauseCh != nil
}

// PauseState snapshots the gate for operator surfaces.
func (a *Agent) PauseState() PauseState {
	if a == nil {
		return PauseState{}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.pauseCh == nil {
		return PauseState{}
	}
	return PauseState{
		Paused:      true,
		Since:       a.pauseSince,
		Reason:      a.pauseReason,
		Interrupted: a.pauseInterrupted,
	}
}

// InterruptAndHold cancels the in-flight turn (if any) AND closes the
// pause gate, atomically with respect to each other: the gate is
// closed under the same a.mu acquisition that reads the cancel func,
// so a wake racing the interrupt can't slip a fresh turn in between
// the cancel and the hold.
//
// Returns (interrupted, paused): interrupted is Interrupt's "there was
// something to cancel"; paused is the post-condition gate state, which
// is always true — an operator who hits interrupt while the agent
// happens to be idle still meant "stop", and holding is what makes
// that stick.
//
// Empty reason defaults to PauseReasonOperatorInterrupt.
//
// The interrupt audit row (#565) is armed BEFORE the cancel fires,
// which closes a real race: the attach handler used to call
// MarkInterruptPending only after AttachInterrupt returned, while
// drainInterruptAudit runs from the interrupted turn's own cleanup. If
// cleanup won, the row landed a turn late — and auto-continue, which
// reads that row as "deliberate kill, don't resume", could re-drive
// exactly the work the operator had just killed. Arming first makes
// the ordering deterministic.
func (a *Agent) InterruptAndHold(reason string) (interrupted bool, paused bool) {
	if a == nil {
		return false, false
	}
	if reason == "" {
		reason = PauseReasonOperatorInterrupt
	}
	a.mu.Lock()
	cancel := a.cancelInFlight
	interrupted = cancel != nil
	transitioned := a.pauseLocked(reason, interrupted)
	a.mu.Unlock()
	if cancel != nil {
		a.pendingInterruptAudit.Store(true)
		cancel()
	}
	if transitioned {
		a.emitPause(reason, interrupted)
	}
	return interrupted, true
}

// awaitResume blocks until the pause gate is open or ctx dies. Called
// as the very FIRST thing in Run — before the guardrail restore, the
// cost-ceiling preflight, and (critically) the inbox drain: a paused
// agent must leave queued messages queued, so the steer an operator
// types while parked is still there for the turn that resume starts.
//
// Never holds a.mu while blocking, matching
// autonomous.Handle.beforeTurn. Loops rather than waiting once because
// a Pause can re-close the gate between the close and our re-check.
func (a *Agent) awaitResume(ctx context.Context) error {
	if a == nil {
		return nil
	}
	for {
		a.mu.Lock()
		ch := a.pauseCh
		a.mu.Unlock()
		if ch == nil {
			return nil
		}
		select {
		case <-ch:
			// Gate opened; re-check in case it closed again.
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
