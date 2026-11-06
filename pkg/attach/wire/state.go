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

package wire

import (
	"context"
	"time"
)

// Tool source classifications surfaced via GET /sessions/.../tools.
// Bare strings (not a typed enum) so JSON clients downstream — the
// TUI, an eventual WebUI, operator scripts — don't have to know a
// Go type to reason about them.
const (
	ToolSourceBuiltin  = "builtin"
	ToolSourceMCP      = "mcp"
	ToolSourceSkill    = "skill"
	ToolSourceSubagent = "subagent"
	ToolSourceOther    = "other"
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
	Source      string `json:"source"`           // builtin | mcp | skill | subagent | other
	Server      string `json:"server,omitempty"` // MCP server attribution when Source=mcp
	GateState   string `json:"gate_state,omitempty"`
}

// SUBagent statuses carried on AgentInfo.Status — the string form of
// background.Status. Distinct from the AgentState* set in pkg/attach,
// which describes the SESSION's loop rather than one spawned subagent.
const (
	AgentStatusRunning   = "running"
	AgentStatusCompleted = "completed"
	AgentStatusFailed    = "failed"
	AgentStatusStopped   = "stopped"
	AgentStatusDeferred  = "deferred"
)

// AgentInfo is one background subagent the parent agent knows about,
// surfaced via GET /sessions/.../agents. Populated from the
// BackgroundAgentManager when one is wired; empty list otherwise.
//
// LastReport is the returned result, else the final text, once the
// subagent has finished; while it runs, its latest report_alert text,
// or failing that its latest model message, cut to 1 KiB (protocol
// 1.20.0, #1283).
//
// NextWakeAt and WakeDetail (protocol 1.20.0, #1283) are set only while
// the subagent is sleeping between turns on a schedule_next_turn wake:
// the time it will wake, and the one-line reason it gave. Status stays
// "running" while it sleeps, because a new status word would read as
// finished to clients that predate it. Both are absent otherwise, and
// always absent from a pre-1.20.0 daemon.
type AgentInfo struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	Status          string    `json:"status"` // AgentStatus*: running | completed | failed | stopped | deferred
	StartedAt       time.Time `json:"started_at"`
	ParentSessionID string    `json:"parent_session_id,omitempty"`
	LastReport      string    `json:"last_report,omitempty"` // see the type comment
	NextWakeAt      time.Time `json:"next_wake_at,omitzero"`
	WakeDetail      string    `json:"wake_detail,omitempty"`
}

// SubagentCatalogInfo is one CONFIGURED subagent in the roster surfaced
// via GET /sessions/.../subagents (#627) — what the daemon LOADED, as
// opposed to what it has spawned (AgentInfo / GET .../agents, "what's
// running"). Populated from the BackgroundAgentManager's catalog when one
// is wired; empty list otherwise (e.g. --no-background-agents, where
// nothing is spawnable by reference).
type SubagentCatalogInfo struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Model       string `json:"model,omitempty"` // resolved model id; empty only for a predefined spec that inherits the parent's
	Root        string `json:"root,omitempty"`  // own content root, when rooted
	// Modes is how this subagent can be invoked ON THIS SESSION: always
	// "async" (spawn_agent by reference), plus "sync" only when this
	// session's agent also carries it as a parent tool. A session created
	// via POST /sessions gets the background manager but not the
	// synchronous subagent tools, so its entries are async-only (#741).
	Modes []string `json:"modes"`
	// Tools is the subagent's CONFIGURED tool grant, sorted by name and
	// classified the same way GET .../tools classifies the parent's
	// (#767): builtin / mcp / skill / other, with MCP server attribution.
	// It answers "can this specialist actually reach kubectl?" — the
	// question an operator asks immediately after "what specialists
	// exist?" (#768).
	//
	// Configured, not effective: the loop-control tools every spawned
	// subagent gets wired regardless (return_result, report_alert,
	// schedule_next_turn) are absent, because they are a property of the
	// runtime rather than of this subagent's configuration. Empty for a
	// subagent granted no tools at all.
	Tools []ToolInfo `json:"tools,omitempty"`
}

// AgentsProvider is the optional capability for GET /sessions/.../agents.
// Returns the background subagents tracked by the registrant's
// BackgroundAgentManager (if any).
type AgentsProvider interface {
	AttachAgents() []AgentInfo
}

// SubagentCatalogProvider is the optional capability for
// GET /sessions/.../subagents (#627). Returns the CONFIGURED subagent
// roster (declarative templates + predefined catalog specs) — distinct
// from AgentsProvider, which returns live/spawned instances. Absence (or
// a nil manager) reports an empty list rather than 501, matching the
// other read-only projections.
type SubagentCatalogProvider interface {
	AttachSubagentCatalog() []SubagentCatalogInfo
}

// DescriptionProvider is the optional capability the agent-card
// handler consults when AgentCardConfig.Description is empty. Returns
// a one-line summary of what the agent does — fed by the same source
// as ADK's llmagent.Config.Description, so the operator writes it
// once and it flows to both the LLM's system prompt and the public
// discovery card.
type DescriptionProvider interface {
	Description() string
}

// SubagentSpec is the POST body for /slash/subagent. Mirrors
// agent.BackgroundSpec in JSON-friendly form.
type SubagentSpec struct {
	Name         string         `json:"name"`
	SystemPrompt string         `json:"system_prompt,omitempty"`
	Goal         string         `json:"goal"`
	Tools        []string       `json:"tools,omitempty"`
	Extras       []string       `json:"extras,omitempty"`
	Budgets      SubagentBudget `json:"budgets,omitempty"`
	Scheduler    string         `json:"scheduler,omitempty"`
}

// SubagentBudget mirrors agent.BackgroundBudgets. Zero values mean
// "use the manager's default for that field".
type SubagentBudget struct {
	MaxTurns      int     `json:"max_turns,omitempty"`
	MaxCostUSD    float64 `json:"max_cost_usd,omitempty"`
	MaxWallClockS int     `json:"max_wall_clock_seconds,omitempty"`
}

// SubagentSpawnResponse confirms the spawn. StartedAt is the
// manager's record of when the subagent's first turn dispatched.
type SubagentSpawnResponse struct {
	Name      string    `json:"name"`
	StartedAt time.Time `json:"started_at"`
}

// SubagentSpawner is the optional capability for
// POST /sessions/.../slash/subagent.
type SubagentSpawner interface {
	AttachSpawnSubagent(ctx context.Context, spec SubagentSpec) (SubagentSpawnResponse, error)
}

// StopAgentOutcome is what a stop attempt actually did, as opposed to
// what the operator asked for. The two differ whenever the subagent
// beat the operator to it, which is the ordinary case for a subagent
// an operator only noticed because it had been running a while.
type StopAgentOutcome struct {
	// Found reports whether the manager has a subagent registered
	// under this name — live or finished. False is the only thing that
	// justifies a 404: the operator aimed at a name that does not
	// exist, so they missed, and a retry against the same name will
	// miss too.
	Found bool
	// Stopped reports whether THIS call halted a live subagent. False
	// with Found=true means it had already reached a terminal status
	// before the call arrived.
	Stopped bool
	// Status is the subagent's status after the call — "stopped" when
	// Stopped is true, otherwise whatever it terminated as
	// ("completed", "failed", "deferred"). Empty when the registrant
	// only implements AgentStopper and cannot say.
	Status string
}

// AgentStopReporter is the 1.12.0 refinement of AgentStopper: it
// separates "this call stopped a running subagent" from "the subagent
// had already finished", which a single bool cannot carry and which
// the route consequently reported wrong for its whole life (#897).
//
// A separate interface rather than a changed AgentStopper signature,
// following IdentifyingInjector (#840): these capability interfaces are
// implemented outside this repository, and the handler degrades to the
// old answer when only the old method is present.
type AgentStopReporter interface {
	// AttachStopAgentOutcome stops the named background subagent and
	// reports what the attempt actually did.
	AttachStopAgentOutcome(name string) (StopAgentOutcome, error)
}
