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

import "time"

// Tool source classifications surfaced via GET /sessions/.../tools.
// Bare strings (not a typed enum) so JSON clients downstream — the
// TUI, an eventual WebUI, operator scripts — don't have to know a
// Go type to reason about them.
const (
	ToolSourceBuiltin = "builtin"
	ToolSourceMCP     = "mcp"
	ToolSourceSkill   = "skill"
	ToolSourceOther   = "other"
)

// Agent run-states surfaced via GET /sessions/.../status. "running"
// covers any active turn; "deferred" means the scheduler is sleeping
// the agent until NextWakeAt; "paused" means the autonomous loop was
// explicitly paused (future, via /pause); "idle" means the agent is
// alive but not currently turning.
const (
	AgentStateRunning  = "running"
	AgentStateDeferred = "deferred"
	AgentStatePaused   = "paused"
	AgentStateIdle     = "idle"
)

// ToolInfo is one entry in the GET /sessions/.../tools response.
//
// GateState carries the pre-flight projection from
// permissions.Gate.ToolGateState — empty when no gate is wired
// (library callers with no permission policy). The TUI v1 fetches the
// field but doesn't surface it; v1.1 adds the column in the /tools
// modal.
type ToolInfo struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Source      string `json:"source"`           // builtin | mcp | skill | other
	Server      string `json:"server,omitempty"` // MCP server attribution when Source=mcp
	GateState   string `json:"gate_state,omitempty"`
}

// AgentInfo is one background subagent the parent agent knows about,
// surfaced via GET /sessions/.../agents. Populated from the
// BackgroundAgentManager when one is wired; empty list otherwise.
type AgentInfo struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	Status          string    `json:"status"` // running | done | failed | paused
	StartedAt       time.Time `json:"started_at"`
	ParentSessionID string    `json:"parent_session_id,omitempty"`
	LastReport      string    `json:"last_report,omitempty"` // most recent report body, truncated
}

// StatusInfo is the response shape of GET /sessions/.../status.
// ModelName is what the TUI's usage panel labels with; the rest is
// agent-loop introspection useful for the /status slash command.
type StatusInfo struct {
	State       string    `json:"state"` // running | deferred | paused | idle
	ModelName   string    `json:"model_name,omitempty"`
	NextWakeAt  time.Time `json:"next_wake_at,omitempty"` // populated when State=deferred
	CurrentTool string    `json:"current_tool,omitempty"` // populated when State=running and a tool is in flight
}

// ToolsProvider is the optional capability a Registrant can implement
// to surface its tool catalog over GET /sessions/.../tools. The handler
// type-asserts at request time; absence reports an empty list rather
// than 501 so old Registrant impls keep working.
//
// Method is named AttachTools (not Tools) to avoid colliding with
// *agent.Agent.Tools() which already returns []tool.Tool. Agents that
// implement the attach surface define a distinct method with this
// shape that does the conversion internally.
type ToolsProvider interface {
	AttachTools() []ToolInfo
}

// AgentsProvider is the optional capability for GET /sessions/.../agents.
// Returns the background subagents tracked by the registrant's
// BackgroundAgentManager (if any).
type AgentsProvider interface {
	AttachAgents() []AgentInfo
}

// StatusProvider is the optional capability for GET /sessions/.../status.
// Returns the agent's current run-state + model identity.
type StatusProvider interface {
	AttachStatus() StatusInfo
}

// InterruptProvider is the optional capability for
// POST /sessions/.../interrupt. Returns true if there was an
// in-flight turn to cancel, false if the agent was idle (no-op).
// Agents that don't implement it get an HTTP 412 from the
// /interrupt handler — interrupt is a write to agent state, and
// silently no-op'ing would mislead operators about whether their
// intent took effect.
type InterruptProvider interface {
	AttachInterrupt() bool
}
