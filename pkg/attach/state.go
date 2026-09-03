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
	"strings"
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
	ToolSourceBuiltin  = "builtin"
	ToolSourceMCP      = "mcp"
	ToolSourceSkill    = "skill"
	ToolSourceSubagent = "subagent"
	ToolSourceOther    = "other"
)

// Agent run-states surfaced via GET /sessions/.../status. "running"
// covers any active turn; "deferred" means the scheduler is sleeping
// the agent until NextWakeAt; "paused" means the loop was explicitly
// parked by an operator (POST /pause, or POST /interrupt, which holds
// by default as of v1.5.0) and will start no new turn until a resume;
// "idle" means the agent is alive but not currently turning.
//
// These are mutually exclusive because State is one field, and
// "paused" outranks "running" when both apply. Read
// StatusInfo.TurnInFlight, not State, to answer "is a turn executing
// right now" — see the note on that field.
//
// "deferred" is declared but not yet produced: nothing on the agent
// exposes a scheduled wake time for the adapter to read.
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
	Source      string `json:"source"`           // builtin | mcp | skill | subagent | other
	Server      string `json:"server,omitempty"` // MCP server attribution when Source=mcp
	GateState   string `json:"gate_state,omitempty"`
}

// SUBagent statuses carried on AgentInfo.Status — the string form of
// background.Status. Distinct from the AgentState* set above, which
// describes the SESSION's loop rather than one spawned subagent.
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
type AgentInfo struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	Status          string    `json:"status"` // AgentStatus*: running | completed | failed | stopped | deferred
	StartedAt       time.Time `json:"started_at"`
	ParentSessionID string    `json:"parent_session_id,omitempty"`
	LastReport      string    `json:"last_report,omitempty"` // most recent report body, truncated
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

// StatusInfo is the response shape of GET /sessions/.../status.
// ModelName is what the TUI's usage panel labels with; the rest is
// agent-loop introspection useful for the /status slash command.
//
// The three pause fields (v1.5.0) are populated when State=paused.
// Interrupted is the one an operator actually reads first: it says
// whether a turn was killed on the way into this pause, or whether the
// loop simply isn't allowed to start one.
//
// TurnInFlight (v1.12.0) is separate from State on purpose. State is
// one field and pause wins it — both here and in the /status handler's
// central pause projection — so a session that is parked AND still
// finishing the turn the park interrupted reports "paused", and every
// client that renders a hold banner off State keeps working. But
// "parked and still running" is precisely the window an operator needs
// to see (#896, #799 F4: a hold banner while the turn ran on for 226
// more seconds), and it is unrepresentable in a single field. The bool
// carries it without overloading State.
type StatusInfo struct {
	State       string    `json:"state"` // running | deferred | paused | idle
	ModelName   string    `json:"model_name,omitempty"`
	NextWakeAt  time.Time `json:"next_wake_at,omitempty"` // populated when State=deferred
	CurrentTool string    `json:"current_tool,omitempty"` // populated when State=running and a tool is in flight

	// TurnInFlight reports whether a turn is executing right now,
	// independent of State. Always meaningful: true with State=running
	// is the ordinary case, true with State=paused is the parked-but-
	// still-finishing window above, and false with State=paused is a
	// quiet hold.
	TurnInFlight bool `json:"turn_in_flight,omitempty"`

	PausedSince time.Time `json:"paused_since,omitempty"` // populated when State=paused
	PauseReason string    `json:"pause_reason,omitempty"` // populated when State=paused
	Interrupted bool      `json:"interrupted,omitempty"`  // a turn was cancelled entering the pause
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

// SubagentCatalogProvider is the optional capability for
// GET /sessions/.../subagents (#627). Returns the CONFIGURED subagent
// roster (declarative templates + predefined catalog specs) — distinct
// from AgentsProvider, which returns live/spawned instances. Absence (or
// a nil manager) reports an empty list rather than 501, matching the
// other read-only projections.
type SubagentCatalogProvider interface {
	AttachSubagentCatalog() []SubagentCatalogInfo
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

// InterruptSelfAuditor is the optional capability a registrant
// implements when it records the operator-interrupt audit event itself,
// from inside its own serialized turn loop, instead of leaving the
// /interrupt handler to append it out-of-band.
//
// The handler's fallback (appendInterruptAudit) does a
// Get-then-AppendEvent on the live session row while the interrupted
// turn is still flushing its final events. That extra write bumps the
// row's last_update_time and can trip the runner's optimistic-
// concurrency check mid-flush, so the operator's clean cancel surfaces
// as an opaque "stale session error" instead of ctx.Canceled (#565).
//
// A registrant that implements this interface takes ownership of the
// audit: MarkInterruptPending records the intent, and the registrant
// appends the audit row once the interrupted turn unwinds — the one
// window with no live runner handle racing the write. When a registrant
// implements it, the /interrupt handler calls MarkInterruptPending and
// skips its own append.
type InterruptSelfAuditor interface {
	// MarkInterruptPending records that an operator interrupt fired and
	// an audit row should be appended once the current turn unwinds.
	// Called from the /interrupt handler only after AttachInterrupt
	// reported an in-flight turn was cancelled. Safe to call
	// concurrently with the registrant's turn loop.
	MarkInterruptPending()
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
// both InputTokensCached and InputTokensCacheWrite (Gemini semantics —
// see usage.Turn docstring). The three input buckets are disjoint:
// InputTokensUncached = InputTokens - InputTokensCached -
// InputTokensCacheWrite, emitted as a convenience so operators don't
// have to do the subtraction.
//
// InputTokensCacheWrite is the premium-rated bucket: tokens spent
// establishing a prompt-cache entry (Anthropic's
// cache_creation_input_tokens). Always zero for providers that don't
// bill cache writes per token, which is why it carries omitempty.
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
	InputTokensCacheWrite    int64   `json:"input_tokens_cache_write,omitempty"`
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
	InputTokensCacheWrite    int64     `json:"input_tokens_cache_write,omitempty"`
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
// CacheWriteUSDPerMTok is the premium charged for input tokens that
// WRITE a cache entry, as opposed to CachedUSDPerMTok's discount for
// reading one (#263). Omitted for providers that don't bill writes per
// token. It belongs on the rate card because it is a rate that does
// charging: without it an operator who sees an unexpected cost, runs
// /pricing to check, and finds only in/out/cache-read has no way to
// confirm a configured `cache_creation_input_per_mtok` took effect.
//
// CacheWrite1hUSDPerMTok is the same rate at the 1-hour breakpoint TTL
// — a distinct, dearer rate rather than a variant of the field above
// (Anthropic charges 2x base input against the 5-minute TTL's 1.25x),
// and it is separately configurable as
// `cache_creation_1h_input_per_mtok` (#770, #929). Omitted when unset,
// for the same reason as the others: a rendered $0.0000 reads as "free"
// rather than "this provider bills one TTL".
type ModelPricing struct {
	InputUSDPerMTok        float64   `json:"input_usd_per_mtok"`
	OutputUSDPerMTok       float64   `json:"output_usd_per_mtok"`
	CachedUSDPerMTok       float64   `json:"cached_usd_per_mtok,omitempty"`
	CacheWriteUSDPerMTok   float64   `json:"cache_write_usd_per_mtok,omitempty"`
	CacheWrite1hUSDPerMTok float64   `json:"cache_write_1h_usd_per_mtok,omitempty"`
	UpdatedAt              time.Time `json:"updated_at,omitempty"`
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

// SessionTitleProvider is the optional capability the GET /sessions
// handler consults for a session's short operator-facing label — the
// title line of a picker row, as against the ID that identifies it
// (#808). Returns "" when the session has no title, which is the
// correct answer for a registrant that never generated one; the field
// is omitted from the wire and clients fall back to the ID.
//
// Distinct from DescriptionProvider, which is per-AGENT ("what this
// agent does", fed to the discovery card). This is per-SESSION ("what
// this particular conversation is about") and changes with the work.
type SessionTitleProvider interface {
	SessionTitle() string
}

// SessionTitleSetter is the optional capability behind
// POST /sessions/.../title — the manual rename (#808). An inferred
// title with no override is a worse deal than no title at all: the
// operator can see the name is wrong and can do nothing about it.
//
// Embeds SessionTitleProvider rather than standing alone so the
// handler can report what was actually STORED rather than echoing
// what was asked for. Implementations normalize (trim, strip the
// model's decorative quoting, cap the length), so the two differ
// often enough that echoing the request would be a small lie in the
// one response whose entire content is the stored value.
//
// Setting "" clears the title. For *agent.Agent that also re-arms
// automatic generation, which is what an operator clearing a bad name
// would expect; a registrant with no generation to re-arm simply ends
// up untitled, and its rows fall back to the ID.
type SessionTitleSetter interface {
	SessionTitleProvider
	SetSessionTitle(title string)
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
	// By is the verified identity that approved, when the daemon could
	// attribute the answer (#830). Omitted — not "unknown", not the
	// anonymous placeholder — when it could not, so "who allowed this"
	// is answerable after the fact without the log ever guessing.
	By string `json:"by,omitempty"`
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
// typically delegate to the binary's pricing layer (pkg/pricing
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
	// Empty reports that the call SUCCEEDED and the model produced no
	// text — a thought-only response, a safety block, a bare STOP. It
	// is not a failure and doesn't come back as one (protocol 1.5.0):
	// a 500 here is reserved for the model call actually failing, so
	// surfaces can render "no answer" inline instead of an error
	// modal that says nothing about what happened.
	Empty bool `json:"empty,omitempty"`
	// Detail is the provider's explanation when it gave one —
	// "finish_reason=SAFETY", "error=RESOURCE_EXHAUSTED: ...". Free-
	// form and often absent. Only meaningful alongside Empty.
	Detail string `json:"detail,omitempty"`
}

// AnswerText is the operator-facing rendering of a side-question
// result: the answer when there is one, and otherwise an explicit
// "no answer" line carrying whatever reason the provider gave.
//
// Lives on the wire type so the in-process TUI and the remote TUI
// can't drift on the phrasing — an operator switching between
// `core-agent --tui` and an attached `core-agent-tui` should see the
// same words for the same outcome.
func (r SideQueryResponse) AnswerText() string {
	if answer := strings.TrimSpace(r.Answer); answer != "" {
		return answer
	}
	if r.Detail != "" {
		return "(no answer — " + r.Detail + ")"
	}
	return "(no answer — the model returned no text)"
}

// SideQueryEmptyError is what a SideQueryProvider returns when the
// model answered with no text. The handler turns it into a 200 +
// {"answer":"", "empty":true, "detail":...} rather than a 500.
//
// Declared here rather than matched on the agent package's own error
// so pkg/attach stays free of an import cycle and third-party
// registrants can produce the same wire shape.
type SideQueryEmptyError struct {
	// Detail is the provider's reason, if any. See
	// SideQueryResponse.Detail.
	Detail string
}

func (e *SideQueryEmptyError) Error() string {
	if e == nil || e.Detail == "" {
		return "side query: model returned no text"
	}
	return "side query: model returned no text (" + e.Detail + ")"
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
// plan-<N>-revoked.md. Binaries without plan-first support don't
// register this capability and the route 501s. Registration does not
// depend on permissions.plan_mode: under "advisory" (or off) there is
// no gate flag set, so the same closure archives whatever artifact
// exists and reports that nothing was blocked.
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
