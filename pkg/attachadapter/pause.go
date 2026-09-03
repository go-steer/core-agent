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

package attachadapter

import (
	"github.com/go-steer/core-agent/v2/pkg/agent"
	"github.com/go-steer/core-agent/v2/pkg/attach"
	"github.com/go-steer/core-agent/v2/pkg/auth"
)

// Pause/resume capability wiring (protocol v1.5.0). The gate itself
// lives on *agent.Agent (pkg/agent/pause.go) so every driver — the
// wake loop, the autonomous scheduler, the REPL, the embedded TUI —
// honours it; this file is just the attach-side projection of it.
//
// See docs/operator-interrupt-design.md.

// Compile-time checks that the adapter carries the v1.5.0 surface.
var (
	_ attach.PauseController   = (*Adapter)(nil)
	_ attach.AgentStopper      = (*Adapter)(nil)
	_ attach.AgentStopReporter = (*Adapter)(nil)
)

// AttachPause implements attach.PauseController. Parks the loop
// without touching an in-flight turn.
func (ad *Adapter) AttachPause(reason string) bool {
	return ad.Agent().Pause(reason)
}

// AttachInterruptHold implements attach.PauseController. Cancels the
// in-flight turn and parks the loop atomically.
func (ad *Adapter) AttachInterruptHold(reason string) (bool, bool) {
	return ad.Agent().InterruptAndHold(reason)
}

// AttachPauseState implements attach.PauseController.
func (ad *Adapter) AttachPauseState() attach.PauseInfo {
	st := ad.Agent().PauseState()
	return attach.PauseInfo{
		Paused:      st.Paused,
		Since:       st.Since,
		Reason:      st.Reason,
		Interrupted: st.Interrupted,
	}
}

// AttachResume implements attach.PauseController: resolve the
// disposition, frame the operator's text, and hand the whole thing to
// agent.ResumeWith (which queues before it opens the gate, so the
// released turn can't start ahead of the instruction).
//
// Mode defaults by content — steer when there's text, continue when
// there isn't — so the common client can send just {"steer": "..."}
// or an empty body.
func (ad *Adapter) AttachResume(req attach.ResumeRequest, caller auth.Caller) (attach.ResumeResponse, error) {
	a := ad.Agent()
	mode := req.Mode
	if mode == "" {
		if req.Steer != "" {
			mode = attach.ResumeModeSteer
		} else {
			mode = attach.ResumeModeContinue
		}
	}
	var message string
	switch mode {
	case attach.ResumeModeSteer:
		// Framed, not raw: the model has to be told that the silence
		// in its transcript was an operator kill rather than a tool
		// that returned nothing, or it re-runs the interrupted work.
		message = agent.FormatInterruptSteer(req.Steer)
	case attach.ResumeModeContinue:
		message = agent.FormatInterruptContinue()
	case attach.ResumeModeAbandon:
		// Nothing injected, nothing woken: the gate opens and the
		// agent goes quiet until something else drives it.
	}
	resumed, err := a.ResumeWith(mode, message, caller)
	if err != nil {
		return attach.ResumeResponse{}, err
	}
	state := attach.AgentStateIdle
	if a.Paused() {
		// Someone re-parked between our resume and this read. Report
		// what's true now rather than what we intended.
		state = attach.AgentStatePaused
	}
	return attach.ResumeResponse{
		Resumed: resumed,
		Mode:    mode,
		State:   state,
	}, nil
}

// AttachStopAgentOutcome implements attach.AgentStopReporter. Stops
// one background subagent by name and reports what the attempt did:
// Found=false only when the manager has never registered that name
// (which the handler turns into a 404), Stopped=false with a terminal
// Status when the subagent had already finished on its own.
func (ad *Adapter) AttachStopAgentOutcome(name string) (attach.StopAgentOutcome, error) {
	a := ad.Agent()
	if a == nil {
		return attach.StopAgentOutcome{}, nil
	}
	mgr := a.BackgroundManager()
	if mgr == nil {
		return attach.StopAgentOutcome{}, nil
	}
	return mgr.StopSubagent(name)
}

// AttachStopAgent implements attach.AgentStopper, the pre-1.12.0
// spelling. Kept so a client or embedder holding the older interface
// still resolves; the handler prefers AttachStopAgentOutcome and only
// falls back here for registrants that don't have it.
func (ad *Adapter) AttachStopAgent(name string) (bool, error) {
	out, err := ad.AttachStopAgentOutcome(name)
	return out.Found, err
}
