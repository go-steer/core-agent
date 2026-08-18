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

package attach

import (
	"time"

	"github.com/go-steer/core-agent/v2/pkg/auth"
)

// Wire shapes for the operator interrupt/steer surface introduced in
// protocol v1.5.0 — POST /sessions/.../interrupt (extended), plus the
// new /pause and /resume. See docs/operator-interrupt-design.md for
// the state machine these serialize.
//
// The target UX is Claude Code's ESC: stop the loop, hold it, ask the
// operator what to do instead, then carry on either with the new
// instruction or unchanged. "Hold" is the piece that didn't exist
// before v1.5.0 — /interrupt was a one-way cancel and the agent went
// straight back to idle with no way to resume the work deliberately.

// The ResumeMode* constants these bodies carry live next to PauseEvent
// in events.go — the agent-layer gate speaks them too, and they landed
// with it.

// InterruptRequest is the (optional) POST body for
// /sessions/.../interrupt. An empty body is valid and means
// {"hold": true} — the v1.5.0 default.
type InterruptRequest struct {
	// Hold parks the loop after the cancel so no new turn starts until
	// a resume (or an operator inject, which resumes implicitly).
	// Pointer so "field omitted" is distinguishable from an explicit
	// false: omitted means hold, which is the Claude-Code-shaped
	// default and the safer failure mode for an explicit operator stop.
	// Send {"hold": false} for the pre-v1.5.0 cancel-and-carry-on
	// behavior.
	Hold *bool `json:"hold,omitempty"`
	// StopSubagents additionally stops every running background
	// subagent. Off by default: subagent runs are not resumable, so
	// killing them is an explicit operator choice rather than a side
	// effect of stopping the parent. The response lists what's still
	// running either way.
	StopSubagents bool `json:"stop_subagents,omitempty"`
}

// InterruptResponse is the /interrupt response body.
type InterruptResponse struct {
	Session string `json:"session"`
	// Interrupted reports whether there was an in-flight turn to
	// cancel. Repeatable within one turn: while the cancelled turn is
	// still unwinding this stays true, so an operator pressing again
	// because nothing visibly stopped yet is told the interrupt landed
	// rather than "nothing in flight".
	Interrupted bool `json:"interrupted"`
	// Paused is the post-condition gate state.
	Paused bool `json:"paused"`
	// RunningSubagents lists background subagents still executing after
	// the interrupt. Parking the parent doesn't touch them, and an
	// operator who thinks they stopped everything needs to see that.
	RunningSubagents []RunningSubagent `json:"running_subagents,omitempty"`
	// StoppedSubagents lists the ones this call stopped (only ever
	// non-empty when StopSubagents was set).
	StoppedSubagents []RunningSubagent `json:"stopped_subagents,omitempty"`
}

// RunningSubagent is the minimal identity of a live background
// subagent, as carried on InterruptResponse. The full record is
// available from GET /sessions/.../agents.
type RunningSubagent struct {
	Name string `json:"name"`
	ID   string `json:"id,omitempty"`
}

// PauseRequest is the (optional) POST body for /sessions/.../pause.
type PauseRequest struct {
	// Reason is stamped on the pause state and echoed to every operator
	// surface. Empty defaults to the operator-pause reason.
	Reason string `json:"reason,omitempty"`
}

// PauseResponse is the /pause response body. Paused is the
// post-condition state (always true on success); Transitioned reports
// whether THIS call is the one that closed the gate, so a client can
// stay quiet on a redundant press.
type PauseResponse struct {
	Session      string    `json:"session"`
	Paused       bool      `json:"paused"`
	Transitioned bool      `json:"transitioned"`
	State        string    `json:"state"`
	Since        time.Time `json:"paused_since,omitempty"`
	Reason       string    `json:"pause_reason,omitempty"`
}

// ResumeRequest is the POST body for /sessions/.../resume.
//
// Mode is optional: it defaults to "steer" when Steer is non-empty and
// "continue" otherwise, so the common client can send just
// {"steer": "..."} or an empty body.
type ResumeRequest struct {
	Mode  string `json:"mode,omitempty"`
	Steer string `json:"steer,omitempty"`
}

// ResumeResponse is the /resume response body. Resumed is false (with
// a 200, not an error) when the agent wasn't paused — resume is
// idempotent so two operator surfaces racing the same click don't
// produce a spurious failure.
type ResumeResponse struct {
	Session string `json:"session"`
	Resumed bool   `json:"resumed"`
	Mode    string `json:"mode"`
	State   string `json:"state"`
}

// PauseInfo is the registrant's projection of its pause gate, used by
// the /pause + /resume handlers and folded into GET /status.
type PauseInfo struct {
	Paused      bool      `json:"paused"`
	Since       time.Time `json:"paused_since,omitempty"`
	Reason      string    `json:"pause_reason,omitempty"`
	Interrupted bool      `json:"interrupted,omitempty"`
}

// PauseController is the optional capability behind POST
// /sessions/.../pause and /resume (v1.5.0). Registrants that don't
// implement it get a 501 from both, matching the convention for
// mutation endpoints: an operator who POSTs expecting an effect has to
// know when there wasn't one.
//
// AttachInterruptHold is the hold-aware sibling of
// InterruptProvider.AttachInterrupt. It exists as its own method
// (rather than a parameter on the old one) so pre-v1.5.0 registrants
// keep compiling and keep their cancel-only semantics.
type PauseController interface {
	// AttachPause closes the gate. Returns whether this call
	// transitioned it.
	AttachPause(reason string) bool
	// AttachInterruptHold cancels any in-flight turn AND closes the
	// gate, atomically with respect to each other. Returns
	// (interrupted, paused).
	AttachInterruptHold(reason string) (bool, bool)
	// AttachResume applies a resume disposition: inject the steer text
	// or a continue note (or neither, for abandon), open the gate, and
	// wake the loop. Returns the mode actually applied after defaulting.
	//
	// caller is the resolved operator identity; it becomes the turn
	// originator for the injected steer exactly as it would on a plain
	// /inject, so a resumed turn is attributed to the human who resumed
	// it rather than to whoever last drove the session.
	AttachResume(req ResumeRequest, caller auth.Caller) (ResumeResponse, error)
	// AttachPauseState projects the gate for /status and the handlers.
	AttachPauseState() PauseInfo
}

// AgentStopper is the optional capability behind POST
// /sessions/.../agents/{name}/stop, and behind the `stop_subagents`
// flag on /interrupt. Exposes the BackgroundAgentManager's existing
// Stop, which until v1.5.0 had no operator-facing route at all — a
// runaway loop inside a subagent survived every /interrupt an operator
// could send.
type AgentStopper interface {
	// AttachStopAgent stops the named background subagent. Returns
	// whether a running subagent by that name was found.
	AttachStopAgent(name string) (bool, error)
}
