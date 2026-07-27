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
// (builtin vs other) and the gate's pre-flight state per tool when
// a gate was wired via agent.WithGate. MCP / skill attribution is
// "other" in v1 — distinguishing them at the slice level needs an
// upstream metadata pass we haven't done yet.
func (ad *Adapter) AttachTools() []attach.ToolInfo {
	a := ad.Agent()
	if a == nil {
		return nil
	}
	tools := a.Tools()
	gate := a.Gate()
	out := make([]attach.ToolInfo, 0, len(tools))
	for _, t := range tools {
		name := t.Name()
		info := attach.ToolInfo{
			Name:        name,
			Description: t.Description(),
			Source:      attach.ToolSourceOther,
		}
		if _, ok := builtinToolNameSet[name]; ok {
			info.Source = attach.ToolSourceBuiltin
		}
		if gate != nil {
			info.GateState = gate.ToolGateState(name)
		}
		out = append(out, info)
	}
	return out
}

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

// AttachStatus implements attach.StatusProvider. V1 returns the agent's
// model name + a coarse "idle" state — finer-grained state (running /
// deferred / paused) would require run-loop instrumentation that
// hasn't been wired yet; the design doc captures pause/resume + state
// mutation as v3 work.
func (ad *Adapter) AttachStatus() attach.StatusInfo {
	a := ad.Agent()
	if a == nil {
		return attach.StatusInfo{}
	}
	return attach.StatusInfo{
		State:     attach.AgentStateIdle,
		ModelName: a.ModelName(),
	}
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
		InputTokens:         int64(t.InputTokens),
		InputTokensCached:   int64(t.CachedInputTokens),
		InputTokensUncached: int64(t.InputTokens - t.CachedInputTokens),
		OutputTokens:        int64(t.OutputTokens),
		ThoughtsTokens:      int64(t.ThoughtsTokens),
		Turns:               t.Turns,
		CostUSD:             t.CostUSD,
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
		InputTokensUncached:      int64(t.InputTokens - t.CachedInputTokens),
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
func (ad *Adapter) AttachAskSideQuestion(ctx context.Context, question string) (string, error) {
	a := ad.Agent()
	if a == nil {
		return "", nil
	}
	return a.AskSideQuestion(ctx, question)
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
// agent side), so it must not change. Formerly
// agent.ErrSubagentSpawnerUnavailable.
var ErrSubagentSpawnerUnavailable = errors.New("agent: subagent spawner unavailable (no BackgroundAgentManager wired)")

// AttachInterrupt implements attach.InterruptProvider so the
// attach-mode POST /sessions/<sid>/interrupt handler can dispatch
// cancel intents from a remote operator. Forwards to agent.Interrupt.
func (ad *Adapter) AttachInterrupt() bool {
	return ad.Agent().Interrupt()
}
