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
	"context"
	"errors"

	"github.com/go-steer/core-agent/v2/pkg/agent"
	"github.com/go-steer/core-agent/v2/pkg/attach"
	"github.com/go-steer/core-agent/v2/pkg/digest"
	corebuiltins "github.com/go-steer/core-agent/v2/pkg/tools"
	"github.com/go-steer/core-agent/v2/pkg/usage"
)

// builtinToolNameSet caches the canonical built-in names for source
// classification in AttachTools.
var builtinToolNameSet = func() map[string]struct{} {
	out := map[string]struct{}{}
	for _, n := range corebuiltins.BuiltinToolNames() {
		out[n] = struct{}{}
	}
	return out
}()

// AttachTools implements attach.ToolsProvider. Returns the agent's
// full tool catalog as ToolInfo entries with source classification
// (builtin / subagent / mcp / skill / other), MCP server attribution,
// and the gate's pre-flight state per tool when a gate was wired via
// agent.WithGate.
//
// The catalog comes from three places because the agent holds it in
// three places (#767). agent.Tools() carries built-ins and the
// synchronous subagent tools. MCP tools and skill tools reach the
// agent as TOOLSETS — agent.WithToolsets, never agent.WithTools — so
// they are not in agent.Tools() at all and were previously missing
// from this endpoint entirely, not merely misclassified. They are
// folded in from the snapshot providers instead of by enumerating the
// live toolsets, which is a deliberate trade: the MCP snapshot is
// materialized once at startup (mcp.Server.ToolInfos), so /tools stays
// a pure in-memory read rather than fanning out a tools/list round-trip
// per server on an operator keystroke — and it cannot disagree with
// what /mcp reports, since both read the same snapshot.
//
// Unwired providers omit their section rather than erroring: an
// embedder with no MCP wiring has no MCP tools to report.
func (ad *Adapter) AttachTools() []attach.ToolInfo {
	a := ad.Agent()
	if a == nil {
		return nil
	}
	tools := a.Tools()
	gate := a.Gate()
	// Declarative subagents (#599) are registered as synchronous parent
	// tools named after the subagent; classify them as "subagent" so
	// operators can tell a `cluster` subagent apart from an ordinary
	// built-in (#627). Sourced from the agent, not the background
	// manager, so the classification holds even under
	// --no-background-agents (which nils the manager but keeps the sync
	// subagent tools).
	subagentSet := map[string]struct{}{}
	for _, n := range a.SubagentNames() {
		subagentSet[n] = struct{}{}
	}
	out := make([]attach.ToolInfo, 0, len(tools))
	seen := make(map[string]struct{}, len(tools))
	// gateKey is the name the POLICY matches on, which is not always
	// the tool's own name. Toolset tools go through
	// tools.GateToolset, whose Run calls CheckToolCall with the
	// NAMESPACE ("mcp" / "skill") as the tool name — the per-tool half
	// only keys session grants. Projecting ToolGateState off the
	// underlying name would therefore report a state the gate will
	// never apply: an `mcp:*` deny rule would render as "prompted" on
	// every MCP row.
	add := func(info attach.ToolInfo, gateKey string) {
		// First writer wins. agent.Tools() goes first, so a built-in
		// keeps its classification against an identically-named tool
		// from a provider. MCP tools are server-namespaced on the way
		// in (pkg/mcp wraps each server with its own prefix), so a
		// genuine collision means two providers claiming one name —
		// listing it twice would make the endpoint's own name column
		// ambiguous, which is worse than picking the agent's view.
		if _, dup := seen[info.Name]; dup {
			return
		}
		seen[info.Name] = struct{}{}
		if gate != nil {
			info.GateState = gate.ToolGateState(gateKey)
		}
		out = append(out, info)
	}

	for _, t := range tools {
		name := t.Name()
		info := attach.ToolInfo{
			Name:        name,
			Description: t.Description(),
			Source:      attach.ToolSourceOther,
		}
		if _, ok := subagentSet[name]; ok {
			info.Source = attach.ToolSourceSubagent
		} else if _, ok := builtinToolNameSet[name]; ok {
			info.Source = attach.ToolSourceBuiltin
		}
		add(info, name)
	}

	if ad.mcpFn != nil {
		for _, srv := range ad.mcpFn().Servers {
			for _, t := range srv.Tools {
				add(attach.ToolInfo{
					Name:        t.Name,
					Description: t.Description,
					Source:      attach.ToolSourceMCP,
					Server:      srv.Name,
				}, mcpGateNamespace)
			}
		}
	}

	if ad.skillToolsFn != nil {
		for _, t := range ad.skillToolsFn() {
			add(attach.ToolInfo{
				Name:        t.Name,
				Description: t.Description,
				Source:      attach.ToolSourceSkill,
			}, skillGateNamespace)
		}
	}
	return out
}

// Gate namespaces the toolset wrappers register under — the literals
// passed to tools.GateToolset by pkg/mcp and pkg/skills. Duplicated
// rather than imported because pulling pkg/mcp into pkg/attachadapter
// for two strings would drag an MCP client dependency into every
// embedder that wraps an agent.
const (
	mcpGateNamespace   = "mcp"
	skillGateNamespace = "skill"
)

// AttachAgents implements attach.AgentsProvider. Returns the live
// background subagents from the agent's SubagentManager, or an
// empty slice when no manager was wired.
func (ad *Adapter) AttachAgents() []attach.AgentInfo {
	a := ad.Agent()
	if a == nil {
		return nil
	}
	mgr := a.BackgroundManager()
	if mgr == nil {
		return nil
	}
	return mgr.ListSubagents()
}

// AttachSubagentCatalog implements attach.SubagentCatalogProvider (#627).
// Returns the CONFIGURED subagent roster — declarative templates +
// predefined catalog specs the manager was wired with — distinct from
// AttachAgents (live/spawned instances). Empty when no manager is wired
// (e.g. --no-background-agents): nothing is spawnable by reference, so
// the roster is empty; the sync subagent tools still appear in
// AttachTools with source="subagent".
func (ad *Adapter) AttachSubagentCatalog() []attach.SubagentCatalogInfo {
	a := ad.Agent()
	if a == nil {
		return nil
	}
	mgr := a.BackgroundManager()
	if mgr == nil {
		return nil
	}
	return mgr.ListSubagentCatalog()
}

// AttachStatus implements attach.StatusProvider. Returns the agent's
// model name plus its coarse state: "paused" when an operator has
// parked the loop (v1.5.0), "idle" otherwise. "running" / "deferred"
// still need run-loop instrumentation that hasn't been wired.
func (ad *Adapter) AttachStatus() attach.StatusInfo {
	a := ad.Agent()
	if a == nil {
		return attach.StatusInfo{}
	}
	out := attach.StatusInfo{
		State:     attach.AgentStateIdle,
		ModelName: a.ModelName(),
	}
	// Reported from the adapter as well as centrally in the /status
	// handler: in-process consumers (the embedded TUI holds the adapter
	// directly, no HTTP hop) would otherwise be told "idle" about a
	// parked loop.
	if st := a.PauseState(); st.Paused {
		out.State = attach.AgentStatePaused
		out.PausedSince = st.Since
		out.PauseReason = st.Reason
		out.Interrupted = st.Interrupted
	}
	return out
}

// AttachUsage implements attach.UsageProvider. Returns the agent's
// usage tracker totals plus a per-model breakdown when more than one
// model has been used in this session (typical pattern: parent on a
// frontier model, subtasks on a cheap flash-tier model via
// --agentic-small-model), plus a per-turn array so operators can
// answer per-turn cost/cache questions without hand-scraping the
// eventlog (issue #222). Returns a zero UsageInfo if no usage tracker
// was wired (agent.WithUsageTracker).
//
// cost_usd_uncached_reference is computed per-turn using the resolved
// pricing for that turn's model — sessions that mix models (parent +
// subtask on a flash-tier via --agentic-small-model) get accurate
// reference numbers instead of averaging one model's rates over the
// other. Rolled up into Overall / PerModel by summing per-turn
// contributions.
func (ad *Adapter) AttachUsage() attach.UsageInfo {
	a := ad.Agent()
	if a == nil || a.Tracker() == nil {
		return attach.UsageInfo{}
	}
	tracker := a.Tracker()
	turns := tracker.All()
	out := attach.UsageInfo{
		Overall: usageTotalsToAttach(tracker.Totals()),
	}
	byModel := tracker.TotalsByModel()
	if len(byModel) > 1 {
		out.PerModel = make(map[string]attach.UsageTotals, len(byModel))
		for name, t := range byModel {
			out.PerModel[name] = usageTotalsToAttach(t)
		}
	}
	if len(turns) == 0 {
		return out
	}
	perTurn := make([]attach.UsageTurn, 0, len(turns))
	var overallUncachedRef float64
	perModelUncachedRef := make(map[string]float64, len(byModel))
	for i, t := range turns {
		p := usage.PriceFor(t.Model, nil)
		uncachedRef := p.CostUSD(t.InputTokens, t.OutputTokens)
		perTurn = append(perTurn, turnToAttach(i+1, t, uncachedRef))
		overallUncachedRef += uncachedRef
		perModelUncachedRef[t.Model] += uncachedRef
	}
	out.Overall.CostUSDUncachedReference = overallUncachedRef
	if out.PerModel != nil {
		for name, ref := range perModelUncachedRef {
			totals := out.PerModel[name]
			totals.CostUSDUncachedReference = ref
			out.PerModel[name] = totals
		}
	}
	out.PerTurn = perTurn
	// Digest telemetry is a package-level counter (see pkg/digest/telemetry.go).
	// Populate the wire field only when at least one Process call has
	// fired — an empty snapshot on a session that never touched
	// pkg/digest would confuse operators into thinking the wrap is
	// wired when it isn't.
	if snap := digest.Telemetry(); len(snap.MethodCounts) > 0 {
		out.DigestMethods = &attach.DigestMethodsInfo{
			Counts:     snap.MethodCounts,
			BytesSaved: snap.BytesSaved,
		}
	}
	return out
}

// AttachContext implements attach.ContextProvider. Projects the
// agent's ContextStats (compaction / checkpoint / subtask shape) into
// the attach wire format. Same cost as ContextStats (one
// session.Service.Get() + O(events) scan) — operator-driven,
// infrequent.
func (ad *Adapter) AttachContext() attach.ContextInfo {
	a := ad.Agent()
	if a == nil {
		return attach.ContextInfo{}
	}
	s := a.ContextStats()
	out := attach.ContextInfo{
		Compactions:          s.CompactionCount,
		Checkpoints:          s.CheckpointCount,
		LastTaskNote:         s.LastCheckpointNote,
		TotalCharsSummarized: s.TotalSummaryChars,
		SubtaskTurns:         s.SubtaskCount,
		SubtaskInputTokens:   int64(s.SubtaskInputTokens),
		SubtaskOutputTokens:  int64(s.SubtaskOutputTokens),
		SubtaskCostUSD:       s.SubtaskCostUSD,
	}
	// Digest savings (#223): nil out on a fresh session so remote
	// renderers can distinguish "wrap layer never fired" from "fired
	// with zero savings."
	ds := s.DigestSavings
	if ds.StructuralCalls+ds.AgenticCalls+ds.PassthroughCalls > 0 {
		out.DigestSavings = &attach.DigestSavingsInfo{
			StructuralCalls:          ds.StructuralCalls,
			StructuralTokensSaved:    int64(ds.StructuralTokensSaved),
			AgenticCalls:             ds.AgenticCalls,
			AgenticTokensSaved:       int64(ds.AgenticTokensSaved),
			AgenticSubagentInTokens:  int64(ds.AgenticSubagentInTokens),
			AgenticSubagentOutTokens: int64(ds.AgenticSubagentOutTokens),
			AgenticSubagentCostUSD:   ds.AgenticSubagentCostUSD,
			PassthroughCalls:         ds.PassthroughCalls,
		}
	}
	return out
}

// usageTotalsToAttach projects usage.Totals into attach.UsageTotals.
// Tokens widen from int to int64 since the wire format reserves the
// larger range for forward compatibility. CostUSDUncachedReference is
// filled in by AttachUsage after this projection because it depends
// on per-turn rate lookups the Totals shape doesn't carry.
func usageTotalsToAttach(t usage.Totals) attach.UsageTotals {
	return attach.UsageTotals{
		InputTokens:           int64(t.InputTokens),
		InputTokensCached:     int64(t.CachedInputTokens),
		InputTokensCacheWrite: int64(t.CacheCreationInputTokens),
		InputTokensUncached:   int64(t.UncachedInputTokens()),
		OutputTokens:          int64(t.OutputTokens),
		ThoughtsTokens:        int64(t.ThoughtsTokens),
		Turns:                 t.Turns,
		CostUSD:               t.CostUSD,
	}
}

// turnToAttach projects one usage.Turn into attach.UsageTurn. turnIdx
// is 1-based (submission order). uncachedRef is the counterfactual
// cost for this turn if nothing had been cached — computed by the
// caller against the per-turn model's pricing.
func turnToAttach(turnIdx int, t usage.Turn, uncachedRef float64) attach.UsageTurn {
	return attach.UsageTurn{
		Turn:                     turnIdx,
		At:                       t.At,
		Model:                    t.Model,
		InputTokens:              int64(t.InputTokens),
		InputTokensCached:        int64(t.CachedInputTokens),
		InputTokensCacheWrite:    int64(t.CacheCreationInputTokens),
		InputTokensUncached:      int64(t.UncachedInputTokens()),
		OutputTokens:             int64(t.OutputTokens),
		ThoughtsTokens:           int64(t.ThoughtsTokens),
		ToolUseTokens:            int64(t.ToolUseTokens),
		TotalTokens:              int64(t.InputTokens + t.OutputTokens + t.ThoughtsTokens + t.ToolUseTokens),
		CostUSD:                  t.CostUSD,
		CostUSDUncachedReference: uncachedRef,
	}
}

// AttachPerms implements attach.PermsProvider. Returns the gate's
// current Snapshot (mode + allow + deny pattern lists) projected
// into the attach wire format, plus the per-session approval log
// so the remote TUI's /permissions slash can render what was
// approved this session. Returns zero PermsInfo if no gate was
// wired via agent.WithGate.
func (ad *Adapter) AttachPerms() attach.PermsInfo {
	a := ad.Agent()
	if a == nil || a.Gate() == nil {
		return attach.PermsInfo{}
	}
	gate := a.Gate()
	s := gate.Snapshot()
	out := attach.PermsInfo{
		Mode:  string(s.Mode),
		Allow: s.Allow,
		Deny:  s.Deny,
	}
	for _, ap := range gate.Approvals() {
		out.Approvals = append(out.Approvals, attach.ApprovalInfo{
			Tool:     ap.Tool,
			Key:      ap.Key,
			Decision: ap.Decision.String(),
			At:       ap.At,
			By:       ap.By,
		})
	}
	return out
}

// AttachAddAllow implements attach.PermsController. Delegates to
// permissions.Gate.AddAllowPatterns. Returns nil if no gate was
// wired (no-op rather than error — operators shouldn't see an error
// for an absent gate). Surfaces validation errors from the gate so
// the operator sees malformed-pattern feedback.
func (ad *Adapter) AttachAddAllow(patterns []string) error {
	a := ad.Agent()
	if a == nil || a.Gate() == nil {
		return nil
	}
	return a.Gate().AddAllowPatterns(patterns)
}

// AttachAddDeny implements attach.PermsController. Delegates to
// permissions.Gate.AddDenyPatterns.
func (ad *Adapter) AttachAddDeny(patterns []string) error {
	a := ad.Agent()
	if a == nil || a.Gate() == nil {
		return nil
	}
	return a.Gate().AddDenyPatterns(patterns)
}

// AttachMemory implements attach.MemoryProvider. Returns nil when
// no provider was wired — the handler emits 200 with an empty
// `{"sources": []}`.
func (ad *Adapter) AttachMemory() []attach.MemorySource {
	if ad == nil || ad.memoryFn == nil {
		return nil
	}
	return ad.memoryFn()
}

// AttachSkills implements attach.SkillsProvider.
func (ad *Adapter) AttachSkills() []attach.SkillInfo {
	if ad == nil || ad.skillsFn == nil {
		return nil
	}
	return ad.skillsFn()
}

// AttachMCP implements attach.MCPProvider.
func (ad *Adapter) AttachMCP() attach.MCPInfo {
	if ad == nil || ad.mcpFn == nil {
		return attach.MCPInfo{}
	}
	return ad.mcpFn()
}

// SessionTitle implements attach.SessionTitleProvider. Unprefixed,
// unlike its neighbours, because it forwards a method of the same name
// on the agent rather than projecting adapter-local state — and because
// the attach interface names the method for what a session has, not for
// where it is read from.
//
// Empty until the first turn's generation lands (or an operator renames
// the session); GET /sessions omits the field rather than sending "".
func (ad *Adapter) SessionTitle() string {
	return ad.Agent().SessionTitle()
}

// SetSessionTitle implements attach.SessionTitleSetter — the write half
// of the same capability, backing POST /sessions/{sid}/title.
//
// It has to be here, not only on *agent.Agent: this adapter is what
// gets registered with the attach registry, so the handler's type
// assertion runs against the Adapter. A read half that forwards and a
// write half that doesn't would answer every rename with 501 while
// every direct-agent test still passed.
//
// Setting "" clears the title and re-arms automatic generation; the
// agent normalizes, which is why the handler reads the title back
// rather than echoing the request.
func (ad *Adapter) SetSessionTitle(title string) {
	ad.Agent().SetSessionTitle(title)
}

// AttachPricing implements attach.PricingProvider.
func (ad *Adapter) AttachPricing() attach.PricingInfo {
	if ad == nil || ad.pricingFn == nil {
		return attach.PricingInfo{}
	}
	return ad.pricingFn()
}

// AttachRefreshPricing implements attach.PricingController. Returns
// attach.ErrCapabilityNotRegistered when no func was wired — the
// handler maps that to HTTP 501.
func (ad *Adapter) AttachRefreshPricing(ctx context.Context) (attach.PricingRefreshResponse, error) {
	if ad == nil || ad.refreshFn == nil {
		return attach.PricingRefreshResponse{}, attach.ErrCapabilityNotRegistered
	}
	return ad.refreshFn(ctx)
}

// AttachSetManualPricing implements attach.PricingController.
func (ad *Adapter) AttachSetManualPricing(req attach.PricingSetRequest) error {
	if ad == nil || ad.setPricingFn == nil {
		return attach.ErrCapabilityNotRegistered
	}
	return ad.setPricingFn(req)
}

// AttachReload implements attach.Reloader. Returns a response with
// Errors populated by ErrCapabilityNotRegistered when no func was
// wired so the handler emits the same 501 the other unwired
// controllers do.
func (ad *Adapter) AttachReload(ctx context.Context) attach.ReloadResponse {
	if ad == nil || ad.reloadFn == nil {
		return attach.ReloadResponse{Errors: []string{attach.ErrCapabilityNotRegistered.Error()}}
	}
	return ad.reloadFn(ctx)
}

// AttachReplan implements attach.ReplanProvider. Routes to the
// closure wired by WithReplanner; returns
// ErrCapabilityNotRegistered when no func was wired.
func (ad *Adapter) AttachReplan(ctx context.Context, req attach.ReplanRequest) (attach.ReplanResponse, error) {
	if ad == nil || ad.replanFn == nil {
		return attach.ReplanResponse{}, attach.ErrCapabilityNotRegistered
	}
	return ad.replanFn(ctx, req)
}

// AttachPromptBroker implements attach.PromptBrokerProvider.
func (ad *Adapter) AttachPromptBroker() *attach.PromptBroker {
	if ad == nil {
		return nil
	}
	return ad.promptBroker
}

// AttachCompact implements attach.CompactSlashProvider. Wraps
// agent.Compact and projects the result into the JSON wire format.
// Errors propagate; the attach handler turns them into 500s.
func (ad *Adapter) AttachCompact(ctx context.Context, focus string) (attach.CompactResponse, error) {
	a := ad.Agent()
	if a == nil {
		return attach.CompactResponse{}, nil
	}
	res, err := a.Compact(ctx, focus)
	if err != nil {
		return attach.CompactResponse{}, err
	}
	return attach.CompactResponse{
		SummaryEventID: res.SummaryEventID,
		SummaryText:    res.SummaryText,
		DurationMS:     res.Duration.Milliseconds(),
		Skipped:        res.Skipped,
	}, nil
}

// AttachCheckpoint implements attach.CheckpointSlashProvider. Wraps
// agent.Checkpoint.
func (ad *Adapter) AttachCheckpoint(ctx context.Context, note string) (attach.CheckpointResponse, error) {
	a := ad.Agent()
	if a == nil {
		return attach.CheckpointResponse{}, nil
	}
	res, err := a.Checkpoint(ctx, note)
	if err != nil {
		return attach.CheckpointResponse{}, err
	}
	return attach.CheckpointResponse{
		CheckpointEventID: res.CheckpointEventID,
		SummaryText:       res.SummaryText,
		TaskNote:          res.TaskNote,
		DurationMS:        res.Duration.Milliseconds(),
		Skipped:           res.Skipped,
	}, nil
}

// AttachAskSideQuestion implements attach.SideQueryProvider. Wraps
// agent.AskSideQuestion (the /btw side-channel that doesn't persist
// to the event log).
//
// The agent's empty-answer error is translated into the wire package's
// equivalent so the handler can answer 200 + empty:true without
// pkg/attach importing pkg/agent (which imports it).
func (ad *Adapter) AttachAskSideQuestion(ctx context.Context, question string) (string, error) {
	a := ad.Agent()
	if a == nil {
		return "", nil
	}
	answer, err := a.AskSideQuestion(ctx, question)
	if err != nil {
		var empty *agent.SideQuestionEmptyError
		if errors.As(err, &empty) {
			return "", &attach.SideQueryEmptyError{Detail: empty.Detail}
		}
		return "", err
	}
	return answer, nil
}

// AttachSpawnSubagent implements attach.SubagentSpawner. Delegates
// to the agent's wired SubagentManager. Returns
// ErrSubagentSpawnerUnavailable when no manager is attached.
func (ad *Adapter) AttachSpawnSubagent(ctx context.Context, spec attach.SubagentSpec) (attach.SubagentSpawnResponse, error) {
	a := ad.Agent()
	if a == nil || a.BackgroundManager() == nil {
		return attach.SubagentSpawnResponse{}, ErrSubagentSpawnerUnavailable
	}
	return a.BackgroundManager().SpawnSubagent(ctx, spec)
}

// ErrSubagentSpawnerUnavailable is returned by AttachSpawnSubagent
// when the agent wasn't constructed with agent.WithBackgroundManager.
// The attach handler maps this to HTTP 501 so the operator sees
// "subagent spawn not registered" instead of a 500.
//
// The message string is load-bearing: pkg/attach matches it literally
// (it can't import this package's sentinel without knowing about the
// agent side), so change it only in lockstep with
// isSubagentSpawnerUnavailable in pkg/attach/handlers_slash.go. The
// prefix says "attachadapter" — the sentinel's home since the #443
// split (the stale "agent:" prefix from its old home was fixed under
// #492 item 5).
var ErrSubagentSpawnerUnavailable = errors.New("attachadapter: subagent spawner unavailable (no BackgroundAgentManager wired)")

// AttachInterrupt implements attach.InterruptProvider so the
// attach-mode POST /sessions/<sid>/interrupt handler can dispatch
// cancel intents from a remote operator. Forwards to agent.Interrupt.
func (ad *Adapter) AttachInterrupt() bool {
	return ad.Agent().Interrupt()
}

// MarkInterruptPending implements attach.InterruptSelfAuditor. The
// /interrupt handler calls it (instead of appending the audit row
// out-of-band) so the audit is written from the agent's own turn loop
// after the interrupted turn unwinds, dodging the OCC race that
// mislabeled operator cancels as stale-session errors (#565). Forwards
// to agent.MarkInterruptPending.
func (ad *Adapter) MarkInterruptPending() {
	ad.Agent().MarkInterruptPending()
}
