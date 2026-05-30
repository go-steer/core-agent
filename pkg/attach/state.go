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

// UsageInfo is the response shape of GET /sessions/.../usage. Backs
// the remote TUI's /stats slash. PerModel is empty when only one
// model has been used (no breakdown needed).
type UsageInfo struct {
	Overall  UsageTotals            `json:"overall"`
	PerModel map[string]UsageTotals `json:"per_model,omitempty"`
}

// UsageTotals mirrors usage.Totals in a JSON-friendly shape. Cached
// input tokens omitted when zero (most providers don't break them out).
type UsageTotals struct {
	InputTokens       int64   `json:"input_tokens"`
	OutputTokens      int64   `json:"output_tokens"`
	CachedInputTokens int64   `json:"cached_input_tokens,omitempty"`
	Turns             int     `json:"turns"`
	CostUSD           float64 `json:"cost_usd"`
}

// ContextInfo is the response shape of GET /sessions/.../context.
// Backs the remote TUI's /context slash. Mirrors agent.ContextStats
// but with json tags + a fixed scalar shape so the wire format is
// stable across agent-package refactors.
type ContextInfo struct {
	Compactions          int     `json:"compactions"`
	Checkpoints          int     `json:"checkpoints"`
	LastTaskNote         string  `json:"last_task_note,omitempty"`
	TotalCharsSummarized int     `json:"total_chars_summarized"`
	SubtaskTurns         int     `json:"subtask_turns"`
	SubtaskInputTokens   int64   `json:"subtask_input_tokens"`
	SubtaskOutputTokens  int64   `json:"subtask_output_tokens"`
	SubtaskCostUSD       float64 `json:"subtask_cost_usd"`
}

// MemorySource is one row in GET /sessions/.../memory — backs the
// remote TUI's /memory slash. Mirrors instruction.Source.
type MemorySource struct {
	Scope string `json:"scope"` // "user-global" | "project"
	Path  string `json:"path"`
	Size  int    `json:"size"`
}

// SkillInfo is one row in GET /sessions/.../skills — backs the
// remote TUI's /skills slash. Description is what the model sees;
// the operator uses it to verify why a skill did or didn't trigger.
type SkillInfo struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// MCPInfo is the response shape of GET /sessions/.../mcp — backs
// the remote TUI's /mcp slash. Each Server carries its lifecycle
// status plus the tools it exposes.
type MCPInfo struct {
	Servers []MCPServerInfo `json:"servers"`
}

// MCPServerInfo describes one declared MCP server.
type MCPServerInfo struct {
	Name      string        `json:"name"`
	Status    string        `json:"status"`    // "running" | "starting" | "failed" | "stopped"
	Transport string        `json:"transport"` // "stdio" | "http"
	Tools     []MCPToolInfo `json:"tools,omitempty"`
}

// MCPToolInfo describes one tool exposed by an MCP server.
type MCPToolInfo struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// PricingInfo is the response shape of GET /sessions/.../pricing —
// backs the remote TUI's /pricing slash. Reports the layered-lookup
// state at request time: how many models have rates, which layer the
// current model resolved against, and the current model's rate
// breakdown.
type PricingInfo struct {
	Source       string        `json:"source"` // "config" | "project-file" | "user-file" | "compiled-in" | "litellm-cache" | "fallback"
	LastRefresh  time.Time     `json:"last_refresh,omitempty"`
	KnownModels  int           `json:"known_models"`
	CurrentModel string        `json:"current_model,omitempty"`
	Current      *ModelPricing `json:"current,omitempty"`
}

// ModelPricing describes one model's rate breakdown.
type ModelPricing struct {
	InputUSDPerMTok  float64 `json:"input_usd_per_mtok"`
	OutputUSDPerMTok float64 `json:"output_usd_per_mtok"`
	CachedUSDPerMTok float64 `json:"cached_usd_per_mtok,omitempty"`
	Source           string  `json:"source,omitempty"`
}

// UsageProvider is the optional capability for GET /sessions/.../usage.
type UsageProvider interface {
	AttachUsage() UsageInfo
}

// ContextProvider is the optional capability for GET /sessions/.../context.
type ContextProvider interface {
	AttachContext() ContextInfo
}

// MemoryProvider is the optional capability for GET /sessions/.../memory.
type MemoryProvider interface {
	AttachMemory() []MemorySource
}

// SkillsProvider is the optional capability for GET /sessions/.../skills.
type SkillsProvider interface {
	AttachSkills() []SkillInfo
}

// MCPProvider is the optional capability for GET /sessions/.../mcp.
type MCPProvider interface {
	AttachMCP() MCPInfo
}

// PricingProvider is the optional capability for GET /sessions/.../pricing.
type PricingProvider interface {
	AttachPricing() PricingInfo
}

// OperatorView wraps a base Registrant (typically *agent.Agent) with
// the caller-held operator-display state — instruction memory, skill
// bundles, MCP servers, pricing snapshot. Library callers construct
// one and register THAT instead of the bare agent, so the operator
// TUI sees /memory, /skills, /mcp, /pricing alongside /tools and
// /status.
//
// Each func field is optional. A nil func means the corresponding
// /sessions/.../<endpoint> returns 404 (capability not registered).
// Pass populated snapshot funcs only for the surfaces you want
// exposed.
//
// The funcs are called per-request so callers can return fresh
// snapshots (e.g., after /pricing refresh updates the in-memory
// rate table). The funcs should be cheap — they typically just
// project an existing in-memory snapshot into the wire shape.
//
// Typical wiring:
//
//	view := &attach.OperatorView{
//	    Registrant: ag,
//	    Memory:     func() []attach.MemorySource { return attach.SnapshotMemory(loadedMemory) },
//	    Skills:     func() []attach.SkillInfo    { return skillsToAttachInfos(loadedSkills) },
//	    MCP:        func() attach.MCPInfo        { return mcpToAttachInfo(mcpServers) },
//	    Pricing:    func() attach.PricingInfo    { return pricingSnapshot(cfg) },
//	}
//	reg.Register(view)
type OperatorView struct {
	Registrant

	Memory  func() []MemorySource
	Skills  func() []SkillInfo
	MCP     func() MCPInfo
	Pricing func() PricingInfo
}

// AttachMemory satisfies MemoryProvider when Memory is non-nil.
// Returns nil otherwise; the handler treats nil-result as "capability
// not registered" and returns 404.
func (o *OperatorView) AttachMemory() []MemorySource {
	if o.Memory == nil {
		return nil
	}
	return o.Memory()
}

// AttachSkills satisfies SkillsProvider when Skills is non-nil.
func (o *OperatorView) AttachSkills() []SkillInfo {
	if o.Skills == nil {
		return nil
	}
	return o.Skills()
}

// AttachMCP satisfies MCPProvider when MCP is non-nil.
func (o *OperatorView) AttachMCP() MCPInfo {
	if o.MCP == nil {
		return MCPInfo{}
	}
	return o.MCP()
}

// AttachPricing satisfies PricingProvider when Pricing is non-nil.
func (o *OperatorView) AttachPricing() PricingInfo {
	if o.Pricing == nil {
		return PricingInfo{}
	}
	return o.Pricing()
}
