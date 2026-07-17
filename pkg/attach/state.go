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
	"context"
	"errors"
	"time"
)

// ErrCapabilityNotRegistered is returned by mutation-capability
// methods on OperatorView when the corresponding func field is nil.
// Handlers check for this with errors.Is and convert to HTTP 501 so
// operators see "capability not registered" instead of a stack trace.
//
// Reads use the empty-result convention instead (200 with zero data
// when the func is nil) — operators who hit a POST need to know if it
// took effect, while readers can accept "nothing here" silently.
var ErrCapabilityNotRegistered = errors.New("attach: capability not registered on this OperatorView")

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
// the remote TUI's /stats slash. PerModel is empty when only one model
// has been used (no breakdown needed). PerTurn is one entry per model
// call in submission order and is always populated when the tracker
// recorded any turns — see issue #222 for the motivating operator
// use case (per-turn cost + cache attribution).
//
// DigestMethods is the digest wrapper's per-method call count (issue
// #130 / task #84). Present when at least one digest.Process call has
// fired process-wide. Feeds "which pruner path is dominating" without
// operators needing to scrape per-event metadata.
type UsageInfo struct {
	Overall       UsageTotals            `json:"overall"`
	PerModel      map[string]UsageTotals `json:"per_model,omitempty"`
	PerTurn       []UsageTurn            `json:"per_turn,omitempty"`
	DigestMethods *DigestMethodsInfo     `json:"digest_methods,omitempty"`
}

// DigestMethodsInfo carries the pkg/digest telemetry snapshot in the
// /usage response. Counts is calls-per-method; BytesSaved is the
// cumulative byte reduction (raw - digest) accrued per method.
// Passthrough always contributes 0 to BytesSaved by definition.
type DigestMethodsInfo struct {
	Counts     map[string]int64 `json:"counts,omitempty"`
	BytesSaved map[string]int64 `json:"bytes_saved,omitempty"`
}

// UsageTotals mirrors usage.Totals in a JSON-friendly shape.
//
// InputTokens is the total effective prompt size and already includes
// InputTokensCached (Gemini semantics — see usage.Turn docstring).
// InputTokensUncached = InputTokens - InputTokensCached and is emitted
// as a convenience so operators don't have to do the subtraction.
//
// CostUSD is the daemon's own cost estimate with the cached-vs-uncached
// rate split applied. CostUSDUncachedReference is what CostUSD would
// have been with zero cache hits — the delta between the two is the
// caching win, which the demo drive on 2026-07-13 confirmed operators
// have no other way to see.
//
// Fields default to zero and use omitempty so a session that never
// touched the prompt cache still renders cleanly.
type UsageTotals struct {
	InputTokens              int64   `json:"input_tokens"`
	InputTokensCached        int64   `json:"input_tokens_cached,omitempty"`
	InputTokensUncached      int64   `json:"input_tokens_uncached,omitempty"`
	OutputTokens             int64   `json:"output_tokens"`
	ThoughtsTokens           int64   `json:"thoughts_tokens,omitempty"`
	Turns                    int     `json:"turns"`
	CostUSD                  float64 `json:"cost_usd"`
	CostUSDUncachedReference float64 `json:"cost_usd_uncached_reference,omitempty"`
}

// UsageTurn is one entry in UsageInfo.PerTurn — the per-model-call
// breakdown behind the aggregate Overall totals. Turn is 1-based in
// submission order; TotalTokens follows the genai convention
// (prompt + candidates + tool-use + thoughts).
type UsageTurn struct {
	Turn                     int       `json:"turn"`
	At                       time.Time `json:"ts"`
	Model                    string    `json:"model,omitempty"`
	InputTokens              int64     `json:"input_tokens"`
	InputTokensCached        int64     `json:"input_tokens_cached,omitempty"`
	InputTokensUncached      int64     `json:"input_tokens_uncached,omitempty"`
	OutputTokens             int64     `json:"output_tokens"`
	ThoughtsTokens           int64     `json:"thoughts_tokens,omitempty"`
	ToolUseTokens            int64     `json:"tool_use_tokens,omitempty"`
	TotalTokens              int64     `json:"total_tokens"`
	CostUSD                  float64   `json:"cost_usd"`
	CostUSDUncachedReference float64   `json:"cost_usd_uncached_reference,omitempty"`
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

	// DigestSavings surfaces the MCP wrap's cumulative effect (#223).
	// Zero-valued when the wrap layer never fired this session (no
	// MCP servers, wrap disabled, or every response was under the
	// threshold). Structural + agentic counts break out separately
	// because their cost math differs — see agent.ContextStats /
	// usage.DigestSavingsTotals for details.
	DigestSavings *DigestSavingsInfo `json:"digest_savings,omitempty"`
}

// DigestSavingsInfo is the wire-format view of one session's
// cumulative MCP digest-wrap savings. Nil on ContextInfo when the
// session has recorded no digest-wrap activity. Broken out so remote
// TUI renderers pick out structural vs. agentic without recomputing.
type DigestSavingsInfo struct {
	StructuralCalls          int     `json:"structural_calls"`
	StructuralTokensSaved    int64   `json:"structural_tokens_saved"`
	AgenticCalls             int     `json:"agentic_calls"`
	AgenticTokensSaved       int64   `json:"agentic_tokens_saved"`
	AgenticSubagentInTokens  int64   `json:"agentic_subagent_input_tokens"`
	AgenticSubagentOutTokens int64   `json:"agentic_subagent_output_tokens"`
	AgenticSubagentCostUSD   float64 `json:"agentic_subagent_cost_usd"`
	PassthroughCalls         int     `json:"passthrough_calls"`
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
	// Source names the catalog layer that served CurrentModel's rate.
	// Values are the pricing.SourceX constants:
	//   "cfg-override" | "project-file" | "user-manual" |
	//   "user-external" | "builtin"
	// Empty when no rate resolved for CurrentModel (renders as "$—"
	// downstream).
	Source       string        `json:"source"`
	LastRefresh  time.Time     `json:"last_refresh,omitempty"`
	KnownModels  int           `json:"known_models"`
	CurrentModel string        `json:"current_model,omitempty"`
	Current      *ModelPricing `json:"current,omitempty"`
}

// ModelPricing describes one model's rate breakdown.
//
// UpdatedAt records when the rate was last verified against its
// provider — LiteLLM refresh time for external entries, generator
// run time for builtin entries, operator edit time for manual
// overrides. Zero when unknown. Surfaced via GET /sessions/.../pricing
// so operators can spot stale rates. The catalog-layer attribution
// ("which source served this rate") lives on the enclosing
// PricingInfo.Source, not here — one field per snapshot avoids
// implying the ModelPricing block would carry per-entry sources if
// PricingInfo ever grows to return multiple models.
type ModelPricing struct {
	InputUSDPerMTok  float64   `json:"input_usd_per_mtok"`
	OutputUSDPerMTok float64   `json:"output_usd_per_mtok"`
	CachedUSDPerMTok float64   `json:"cached_usd_per_mtok,omitempty"`
	UpdatedAt        time.Time `json:"updated_at,omitempty"`
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

// DescriptionProvider is the optional capability the agent-card
// handler consults when AgentCardConfig.Description is empty. Returns
// a one-line summary of what the agent does — fed by the same source
// as ADK's llmagent.Config.Description, so the operator writes it
// once and it flows to both the LLM's system prompt and the public
// discovery card.
type DescriptionProvider interface {
	Description() string
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

	// PR A2 (mutation endpoints) func fields. nil means the
	// corresponding POST returns 501 (capability not registered).
	RefreshPricing func(ctx context.Context) (PricingRefreshResponse, error)
	SetPricing     func(req PricingSetRequest) error
	Reload         func(ctx context.Context) ReloadResponse
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

// AttachRefreshPricing satisfies PricingController. Returns
// ErrCapabilityNotRegistered when RefreshPricing is nil so the
// handler emits 501.
func (o *OperatorView) AttachRefreshPricing(ctx context.Context) (PricingRefreshResponse, error) {
	if o.RefreshPricing == nil {
		return PricingRefreshResponse{}, ErrCapabilityNotRegistered
	}
	return o.RefreshPricing(ctx)
}

// AttachSetManualPricing satisfies PricingController.
func (o *OperatorView) AttachSetManualPricing(req PricingSetRequest) error {
	if o.SetPricing == nil {
		return ErrCapabilityNotRegistered
	}
	return o.SetPricing(req)
}

// AttachReload satisfies Reloader. Returns a ReloadResponse with
// Errors populated by the sentinel string when Reload is nil so the
// handler emits 501.
func (o *OperatorView) AttachReload(ctx context.Context) ReloadResponse {
	if o.Reload == nil {
		return ReloadResponse{Errors: []string{ErrCapabilityNotRegistered.Error()}}
	}
	return o.Reload(ctx)
}

// PermsInfo is the response shape of GET /sessions/.../perms — backs
// the remote TUI's /permissions slash. Mirrors permissions.Snapshot
// plus the per-session approval log so the operator can review
// what was approved this session.
type PermsInfo struct {
	Mode      string         `json:"mode"`
	Allow     []string       `json:"allow,omitempty"`
	Deny      []string       `json:"deny,omitempty"`
	Approvals []ApprovalInfo `json:"approvals,omitempty"`
}

// ApprovalInfo is one row in the per-session approval log. Mirrors
// permissions.ApprovalLog in a JSON-friendly shape.
type ApprovalInfo struct {
	Tool     string    `json:"tool"`
	Key      string    `json:"key,omitempty"`
	Decision string    `json:"decision"` // "allow-once" | "allow-session" | etc.
	At       time.Time `json:"at"`
}

// PatternsRequest is the POST body for /perms/allow + /perms/deny.
// Lets the operator add one or more patterns in a single call.
type PatternsRequest struct {
	Patterns []string `json:"patterns"`
}

// PricingSetRequest is the POST body for /pricing/set.
type PricingSetRequest struct {
	Model            string  `json:"model"`
	InputUSDPerMTok  float64 `json:"input_usd_per_mtok"`
	OutputUSDPerMTok float64 `json:"output_usd_per_mtok"`
}

// PricingRefreshResponse is the response shape of POST
// /pricing/refresh — reports whether the upstream fetch produced new
// data, the model count post-refresh, and the refreshed-at timestamp
// so the client can update its display.
type PricingRefreshResponse struct {
	Updated     bool      `json:"updated"`
	KnownModels int       `json:"known_models"`
	LastRefresh time.Time `json:"last_refresh"`
	Detail      string    `json:"detail,omitempty"` // human-readable note when Updated=false
}

// ReloadResponse is the response shape of POST /reload — reports
// per-surface success so the operator sees which parts (memory /
// skills / mcp) succeeded and which failed without parsing logs.
type ReloadResponse struct {
	Memory bool     `json:"memory"`
	Skills bool     `json:"skills"`
	MCP    bool     `json:"mcp"`
	Errors []string `json:"errors,omitempty"`
}

// PermsProvider is the optional capability for GET /sessions/.../perms.
type PermsProvider interface {
	AttachPerms() PermsInfo
}

// PermsController is the optional capability for POST
// /sessions/.../perms/allow + /perms/deny. Mutates the gate's
// pattern list; the new patterns take effect for future tool calls
// without restarting the agent. Each method returns an error so the
// gate's own pattern-validation errors surface to the operator.
type PermsController interface {
	AttachAddAllow(patterns []string) error
	AttachAddDeny(patterns []string) error
}

// PricingController is the optional capability for POST
// /sessions/.../pricing/refresh + /pricing/set. Implementations
// typically delegate to the binary's pricing layer (internal/pricing
// in cmd/core-agent) rather than reimplementing it.
type PricingController interface {
	AttachRefreshPricing(ctx context.Context) (PricingRefreshResponse, error)
	AttachSetManualPricing(req PricingSetRequest) error
}

// Reloader is the optional capability for POST /sessions/.../reload.
// Re-walks the agent's project dependencies (memory / skills / MCP)
// and reports per-surface success. The implementation decides what
// "reload" means — e.g., re-load AGENTS.md, reload skills, restart
// MCP servers. Hot-swap semantics are the binary's concern.
type Reloader interface {
	AttachReload(ctx context.Context) ReloadResponse
}

// CompactRequest is the POST body for /slash/compact. Focus is the
// optional steer text the operator typed after `/compact <focus>`
// (e.g. "preserve the test failures"). Empty for a default-focus run.
type CompactRequest struct {
	Focus string `json:"focus,omitempty"`
}

// CompactResponse is the response shape of POST /slash/compact.
// Mirrors the agent.CompactionResult fields the remote TUI needs to
// render the post-compaction confirmation row.
type CompactResponse struct {
	SummaryEventID string `json:"summary_event_id,omitempty"`
	SummaryText    string `json:"summary_text,omitempty"`
	DurationMS     int64  `json:"duration_ms"`
	Skipped        bool   `json:"skipped,omitempty"`
}

// CheckpointRequest is the POST body for /slash/done. Note is the
// optional task-note the operator typed after `/done <note>`. Empty
// when the operator didn't supply one (the checkpointer can derive
// a default).
type CheckpointRequest struct {
	Note string `json:"note,omitempty"`
}

// CheckpointResponse is the response shape of POST /slash/done.
type CheckpointResponse struct {
	CheckpointEventID string `json:"checkpoint_event_id,omitempty"`
	SummaryText       string `json:"summary_text,omitempty"`
	TaskNote          string `json:"task_note,omitempty"`
	DurationMS        int64  `json:"duration_ms"`
	Skipped           bool   `json:"skipped,omitempty"`
}

// SideQueryRequest is the POST body for /slash/btw — the operator's
// side question. The agent answers using its session history but
// doesn't persist the round-trip; results render as a dismissible
// overlay rather than a turn boundary.
type SideQueryRequest struct {
	Question string `json:"question"`
}

// ReplanRequest is the POST body for /slash/replan. Today there's
// no body — operator clicks /replan and the agent revokes the
// latest plan, clears the gate flag, and waits for the next
// model turn. Future versions may add an optional Reason field for
// a system-note that primes the model's redraft.
type ReplanRequest struct {
	Reason string `json:"reason,omitempty"`
}

// ReplanResponse is the response shape of POST /slash/replan.
// Mirrors what `tools.RevokeLatestPlan` returned plus a status
// flag for the no-plan-to-revoke case (which is not an error —
// /replan can be called defensively to ensure the gate is clear).
type ReplanResponse struct {
	// ArchivedPath is the full path of the file that was renamed
	// from plan-<N>.md to plan-<N>-revoked.md. Empty if there was
	// no active plan to revoke.
	ArchivedPath string `json:"archived_path,omitempty"`
	// PlanWasActive reports whether a plan was active before this
	// call. False means the gate flag was clear and no file got
	// renamed (still safe to call).
	PlanWasActive bool `json:"plan_was_active"`
	// Message is the operator-facing one-liner the TUI renders.
	Message string `json:"message,omitempty"`
}

// SideQueryResponse carries the agent's answer text.
type SideQueryResponse struct {
	Answer string `json:"answer"`
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

// CompactSlashProvider is the optional capability for
// POST /sessions/.../slash/compact.
type CompactSlashProvider interface {
	AttachCompact(ctx context.Context, focus string) (CompactResponse, error)
}

// CheckpointSlashProvider is the optional capability for
// POST /sessions/.../slash/done.
type CheckpointSlashProvider interface {
	AttachCheckpoint(ctx context.Context, note string) (CheckpointResponse, error)
}

// SideQueryProvider is the optional capability for
// POST /sessions/.../slash/btw.
type SideQueryProvider interface {
	AttachAskSideQuestion(ctx context.Context, question string) (string, error)
}

// SubagentSpawner is the optional capability for
// POST /sessions/.../slash/subagent.
type SubagentSpawner interface {
	AttachSpawnSubagent(ctx context.Context, spec SubagentSpec) (SubagentSpawnResponse, error)
}

// ReplanProvider is the optional capability for
// POST /sessions/.../slash/replan. Implementations clear the gate's
// plan-recorded flag and archive the latest plan artifact to
// plan-<N>-revoked.md. Wired by binaries that set up plan-first
// gating (require_plan_artifact: true); binaries without plan-first
// support don't register this capability and the route 501s.
type ReplanProvider interface {
	AttachReplan(ctx context.Context, req ReplanRequest) (ReplanResponse, error)
}

// OperatorView additions for PR A2 (mutation endpoints): three
// func fields surface caller-held implementations of the pricing /
// reload capabilities. PermsController is implemented directly on
// *agent.Agent (the gate is held by the agent), so OperatorView
// doesn't need a Perms field — embedded Registrant carries it.
//
// Set these only for the binary-specific operations you want
// exposed. nil means the corresponding POST returns 501 (capability
// not registered) — different from the read endpoints' "200 with
// empty data" convention because operators who hit a POST expecting
// it to take effect must know if it didn't.
//
// Wire-up example:
//
//	view := &attach.OperatorView{
//	    Registrant:     ag,
//	    RefreshPricing: func(ctx context.Context) (attach.PricingRefreshResponse, error) {
//	        outcome, err := pricing.Refresh(ctx, coreHome, refreshOpts)
//	        ...
//	    },
//	    SetPricing: func(req attach.PricingSetRequest) error { ... },
//	    Reload:     func(ctx context.Context) attach.ReloadResponse { ... },
//	}
