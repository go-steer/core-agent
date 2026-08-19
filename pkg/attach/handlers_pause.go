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
	"fmt"
	"net/http"
	"strings"

	"github.com/go-steer/core-agent/v2/pkg/auth"
)

// POST /sessions/.../pause, /resume, and /agents/{name}/stop — the
// v1.5.0 operator hold surface. See docs/operator-interrupt-design.md.
//
// The shape these serve is Claude Code's ESC: /interrupt (with the
// default hold) stops the turn and PARKS the loop, the operator is
// asked what to do instead, and /resume carries their answer — new
// instruction, plain carry-on, or abandon. /pause is the same park
// without killing an in-flight turn, for "stop after this one".

// pauseMaxBytes caps POST /pause and /interrupt bodies. Both carry
// only flags and a short reason string.
const pauseMaxBytes = 4 * 1024

// resumeMaxBytes caps the POST /resume body. Sized like inject
// (injectMaxBytes): the steer text IS an injected message, and an
// operator who can /inject 8 KiB should be able to say the same thing
// as a resume instruction.
const resumeMaxBytes = injectMaxBytes

// registerPause wires the hold surface onto the mux.
//
// /pause is NOT drain-gated: parking a loop during shutdown is
// harmless and, if anything, what a draining daemon wants. /resume IS
// drain-gated — it injects and wakes, and a 200 that promises a turn
// the dying wake loop will never run is exactly the acknowledged-then-
// lost failure #564 closed for /inject and /wake.
func (h *handlers) registerPause(mux *http.ServeMux) {
	h.routeSession(mux, "POST", "pause", auth.ActionSessionWrite, h.doPause)
	h.routeSessionDrainGated(mux, "POST", "resume", auth.ActionSessionWrite, h.doResume)
	h.routeSession(mux, "POST", "agents/{name}/stop", auth.ActionSessionWrite, h.doStopAgent)
}

func (h *handlers) doPause(w http.ResponseWriter, r *http.Request, entry *Entry) {
	pc, ok := entry.Agent.(PauseController)
	if !ok {
		http.Error(w, "pause: this agent does not implement PauseController (older runtime?)", http.StatusNotImplemented)
		return
	}
	var req PauseRequest
	// Body is optional — a bare POST is the common case.
	if r.ContentLength > 0 {
		if err := readJSON(r, &req, pauseMaxBytes); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	transitioned := pc.AttachPause(strings.TrimSpace(req.Reason))
	state := pc.AttachPauseState()
	// State mirrors the gate we just read, not the one we asked for: a
	// resume racing this call leaves the agent idle, and saying "paused"
	// alongside `"paused": false` would make the two fields contradict
	// each other in the same body.
	agentState := AgentStateIdle
	if state.Paused {
		agentState = AgentStatePaused
	}
	writeJSON(w, http.StatusOK, PauseResponse{
		Session:      entry.SessionID,
		Paused:       state.Paused,
		Transitioned: transitioned,
		State:        agentState,
		Since:        state.Since,
		Reason:       state.Reason,
	})
}

func (h *handlers) doResume(w http.ResponseWriter, r *http.Request, entry *Entry) {
	pc, ok := entry.Agent.(PauseController)
	if !ok {
		http.Error(w, "resume: this agent does not implement PauseController (older runtime?)", http.StatusNotImplemented)
		return
	}
	var req ResumeRequest
	// Body is optional: an empty POST is "continue", the single most
	// common disposition (the operator looked, decided nothing needed
	// to change, and let it carry on).
	if r.ContentLength > 0 {
		if err := readJSON(r, &req, resumeMaxBytes); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	req.Steer = strings.TrimSpace(req.Steer)
	req.Mode = strings.TrimSpace(strings.ToLower(req.Mode))
	switch req.Mode {
	case "", ResumeModeSteer, ResumeModeContinue, ResumeModeAbandon:
	default:
		http.Error(w, fmt.Sprintf("resume: unknown mode %q (want steer, continue, or abandon)", req.Mode), http.StatusBadRequest)
		return
	}
	if req.Mode == ResumeModeSteer && req.Steer == "" {
		http.Error(w, "resume: mode=steer requires non-empty 'steer' text", http.StatusBadRequest)
		return
	}
	// Second drain check after the body read — same TOCTOU closure as
	// doInject (#564 review P1).
	if h.rejectDraining(w) {
		return
	}
	caller, _ := auth.CallerFromContext(r.Context())
	resp, err := pc.AttachResume(req, caller)
	if err != nil {
		http.Error(w, fmt.Sprintf("resume: %v", err), http.StatusInternalServerError)
		return
	}
	resp.Session = entry.SessionID
	if resp.State == "" {
		resp.State = AgentStateIdle
	}
	writeJSON(w, http.StatusOK, resp)
}

// doStopAgent stops one running background subagent. Until v1.5.0 the
// manager's Stop had no operator-facing route at all: a runaway loop
// inside a subagent survived every /interrupt an operator could send,
// because interrupting the parent only cancels the parent's turn.
//
// 404 when no running subagent by that name is found — an operator
// aiming at a specific runaway needs to know they missed, and a 200
// here would read as "stopped".
func (h *handlers) doStopAgent(w http.ResponseWriter, r *http.Request, entry *Entry) {
	st, ok := entry.Agent.(AgentStopper)
	if !ok {
		http.Error(w, "stop: this agent does not implement AgentStopper (older runtime?)", http.StatusNotImplemented)
		return
	}
	name := strings.TrimSpace(r.PathValue("name"))
	if name == "" {
		http.Error(w, "stop: subagent name is required", http.StatusBadRequest)
		return
	}
	stopped, err := st.AttachStopAgent(name)
	if err != nil {
		http.Error(w, fmt.Sprintf("stop: %v", err), http.StatusInternalServerError)
		return
	}
	if !stopped {
		http.Error(w, fmt.Sprintf("stop: no running subagent named %q", name), http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"session": entry.SessionID,
		"agent":   name,
		"stopped": true,
	})
}

// runningSubagents projects the live background subagents still
// executing, for the /interrupt response. Parking the parent doesn't
// touch them, and an operator who believes they stopped everything
// needs to see what kept running.
func runningSubagents(entry *Entry) []RunningSubagent {
	p, ok := entry.Agent.(AgentsProvider)
	if !ok {
		return nil
	}
	var out []RunningSubagent
	for _, a := range p.AttachAgents() {
		if a.Status != AgentStatusRunning {
			continue
		}
		out = append(out, RunningSubagent{Name: a.Name, ID: a.ID})
	}
	return out
}
