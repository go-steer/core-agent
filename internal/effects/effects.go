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

// Package effects implements the recorded-effect outbox
// (docs/durable-write-gate-design.md): the runtime guard that makes
// mutating-tool re-execution ambiguity visible and blocking instead of
// silent.
//
// The session event log IS the outbox — a durable FunctionCall event is
// the intent record, its paired FunctionResponse the completion record,
// both keyed by (session, invocation, function-call ID). This package
// adds no storage; it only reads history and refuses or replays calls.
package effects

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/plugin"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/toolconfirmation"
	"google.golang.org/genai"
)

// SyntheticRepairMetadataKey marks synthetic FunctionResponse events
// appended by pkg/agent/tail_repair.go so pairScan never mistakes
// LLM transcript repair for a completed mutating tool call (#1400).
const SyntheticRepairMetadataKey = "core_agent_synthetic_repair"

// Class is a tool's effect classification under the mutation predicate.
type Class int

const (
	// ClassReadOnly tools never pay the outbox check.
	ClassReadOnly Class = iota

	// ClassMutating tools get intent/completion records in the log and
	// are refused in ambiguous-effect mode. It is the default for an
	// unknown tool (default-deny-unknown).
	ClassMutating

	// ClassSpawning tools start sub-runs whose inner tool calls are
	// governed by the sub-run's own outbox or inspected via
	// SubSessionScanner.
	ClassSpawning
)

// controlCalls are engine control-flow function calls, never tool
// effects: dangling ones are the normal wire shape of a paused or
// mid-flow session and must not trip ambiguous-effect mode.
var controlCalls = map[string]bool{
	"adk_request_input":               true,
	"adk_request_credential":          true,
	toolconfirmation.FunctionCallName: true, // "adk_request_confirmation"
	"finish_task":                     true,
	"mark_task_done":                  true,
	"transfer_to_agent":               true,
	"task_completed":                  true,
	"exit_loop":                       true,
	"pause_session":                   true,
}

// builtinClasses classifies core-agent's and mast's registered tools.
var builtinClasses = map[string]Class{
	// Spawning tools.
	"task":                     ClassSpawning,
	"single_turn":              ClassSpawning,
	"spawn_subagent":           ClassSpawning,
	"spawn_agent":              ClassSpawning,
	"invoke_specialist":        ClassSpawning,
	"run_shape_llm_router":     ClassSpawning,
	"run_shape_fan_out_fan_in": ClassSpawning,

	// Mutating tools.
	"write_file":          ClassMutating,
	"edit_file":           ClassMutating,
	"delete_file":         ClassMutating,
	"bash":                ClassMutating,
	"fetch_url":           ClassMutating,
	"alert":               ClassMutating,
	"spawn_remote_agent":  ClassMutating,
	"invoke_remote_agent": ClassMutating,
	"stop_subagent":       ClassMutating,
	"stop_agent":          ClassMutating,
	"send_message":        ClassMutating,

	// Read-only tools.
	"read_file":              ClassReadOnly,
	"read_many_files":        ClassReadOnly,
	"view_file_outline":      ClassReadOnly,
	"grep":                   ClassReadOnly,
	"glob":                   ClassReadOnly,
	"stat":                   ClassReadOnly,
	"list_dir":               ClassReadOnly,
	"json_query":             ClassReadOnly,
	"todo":                   ClassReadOnly,
	"record_plan":            ClassReadOnly,
	"wait_and_verify":        ClassReadOnly,
	"ask_user":               ClassReadOnly,
	"retrieve_raw":           ClassReadOnly,
	"list_skills":            ClassReadOnly,
	"load_skill":             ClassReadOnly,
	"load_skill_resource":    ClassReadOnly,
	"read_skill_resource":    ClassReadOnly,
	"schedule_next_turn":     ClassReadOnly,
	"report_done":            ClassReadOnly,
	"return_result":          ClassReadOnly,
	"check_subagent":         ClassReadOnly,
	"list_subagents":         ClassReadOnly,
	"read_subagent_logs":     ClassReadOnly,
	"check_agent":            ClassReadOnly,
	"list_agents":            ClassReadOnly,
	"read_agent_logs":        ClassReadOnly,
	"request_operator_input": ClassReadOnly,
}

// Predicate classifies a tool by name. See NewPredicate.
type Predicate func(toolName string) Class

// ReadOnlyPredicate is an alias for Predicate used by auto-resume and
// auto-continue configuration structs.
type ReadOnlyPredicate = Predicate

// ReadOnlyHint reports the `readOnlyHint` an MCP server published for
// toolName. annotated is false when nothing has published annotations
// for that name.
type ReadOnlyHint func(toolName string) (readOnly, annotated bool)

// NewPredicate builds the mutation predicate with no annotation source:
// every MCP tool is unclassified and takes the default-deny-unknown
// answer. See NewPredicateWithHints for the wired form.
func NewPredicate(overrides map[string]bool) Predicate {
	return NewPredicateWithHints(overrides, nil)
}

// NewPredicateWithHints builds the mutation predicate. In order:
// control surfaces are read-only; per-tool overrides decide next;
// builtins use their registered class; an MCP tool its server annotated
// is classified by that annotation; and everything left over defaults
// to mutating (default-deny-unknown).
func NewPredicateWithHints(overrides map[string]bool, hint ReadOnlyHint) Predicate {
	return func(name string) Class {
		if controlCalls[name] {
			return ClassReadOnly
		}
		if mutating, ok := overrides[name]; ok {
			if mutating {
				return ClassMutating
			}
			return ClassReadOnly
		}
		if c, ok := builtinClasses[name]; ok {
			return c
		}
		if hint != nil {
			if readOnly, annotated := hint(name); annotated && readOnly {
				return ClassReadOnly
			}
		}
		return ClassMutating
	}
}

// DanglingIntent is a durable mutating (or spawning) FunctionCall from
// a prior attempt with no completion in the log.
type DanglingIntent struct {
	ToolName     string
	CallID       string
	InvocationID string
	Timestamp    time.Time
	EventIndex   int
}

// SubSessionScanner inspects a deterministic child session for a
// dangling ClassSpawning call and returns any dangling mutating calls
// inside that child session. When inspected is true, the parent's
// dangling spawn is replaced by the returned child intents; when false,
// the parent spawn fails closed as dangling itself.
type SubSessionScanner func(ctx context.Context, parentSessionID string, spawn DanglingIntent) (dangling []DanglingIntent, inspected bool)

// Config configures the outbox plugin.
type Config struct {
	// Predicate classifies tools; required.
	Predicate Predicate

	// SubAgentNames is the set of agent names composed under the
	// runner's root (see SubAgentNames).
	SubAgentNames map[string]bool

	// ExternalDangling returns intents this process recorded for the
	// session outside its event log. Optional.
	ExternalDangling func(ctx context.Context, sessionID string) []DanglingIntent

	// SubSessionScanner inspects deterministic child sessions for
	// dangling ClassSpawning calls. Optional; nil fails closed on any
	// dangling spawn.
	SubSessionScanner SubSessionScanner

	// AckedAt returns the operator's effects-acknowledgement watermark
	// for a session, if one exists. Optional; nil means no acks.
	AckedAt func(ctx context.Context, sessionID string) (time.Time, bool)

	// Logger for refusals, replays, and mode transitions. Optional.
	Logger *slog.Logger
}

// SubAgentNames walks the agent tree from root and returns every
// composed agent's name — the delegation-call exclusion set for
// Config.SubAgentNames.
func SubAgentNames(root agent.Agent) map[string]bool {
	out := map[string]bool{}
	var walk func(a agent.Agent)
	walk = func(a agent.Agent) {
		if a == nil || out[a.Name()] {
			return
		}
		out[a.Name()] = true
		for _, sub := range a.SubAgents() {
			walk(sub)
		}
	}
	walk(root)
	return out
}

// CheckNameCollisions reports composed sub-agent names that also name a
// mutating- or spawning-class tool.
func CheckNameCollisions(subAgents map[string]bool, pred Predicate, policies []ToolPolicy) []string {
	if len(subAgents) == 0 || pred == nil {
		return nil
	}
	candidates := make(map[string]bool, len(builtinClasses)+len(policies))
	for name := range builtinClasses {
		candidates[name] = true
	}
	for _, p := range policies {
		if name := strings.TrimSpace(p.Name); name != "" {
			candidates[name] = true
		}
	}
	var hits []string
	for name := range candidates {
		if subAgents[name] && pred(name) != ClassReadOnly {
			hits = append(hits, name)
		}
	}
	sort.Strings(hits)
	return hits
}

// NewPlugin is an alias for New.
func NewPlugin(cfg Config) (*plugin.Plugin, error) {
	return New(cfg)
}

// New builds the outbox as an ADK runner plugin. Attach it via
// runner.Config.PluginConfig at every runner construction site.
func New(cfg Config) (*plugin.Plugin, error) {
	if cfg.Predicate == nil {
		return nil, fmt.Errorf("effects: Config.Predicate is required")
	}
	o := &outbox{
		cfg:   cfg,
		turns: make(map[string]*turnState),
	}
	if o.cfg.Logger == nil {
		o.cfg.Logger = slog.Default()
	}
	return plugin.New(plugin.Config{
		Name:               "mast-effects-outbox",
		BeforeRunCallback:  o.beforeRun,
		BeforeToolCallback: o.beforeTool,
		AfterRunCallback:   o.afterRun,
	})
}

type turnState struct {
	dangling    []DanglingIntent
	completions map[string]map[string]any
}

type outbox struct {
	cfg   Config
	mu    sync.Mutex
	turns map[string]*turnState
}

func (o *outbox) beforeRun(ictx agent.InvocationContext) (*genai.Content, error) {
	sess := ictx.Session()
	if sess == nil {
		return nil, nil
	}
	st := scanHistoryForRun(sess.Events(), o.cfg.Predicate, o.cfg.SubAgentNames)
	if o.cfg.SubSessionScanner != nil && len(st.dangling) > 0 {
		st.dangling = o.resolveSpawningDangling(ictx, sess.ID(), st.dangling)
	}
	if o.cfg.ExternalDangling != nil {
		if ext := o.cfg.ExternalDangling(ictx, sess.ID()); len(ext) > 0 {
			st.dangling = append(st.dangling, ext...)
			sortByTimestamp(st.dangling)
		}
	}
	if len(st.dangling) > 0 && o.cfg.AckedAt != nil {
		if ack, ok := o.cfg.AckedAt(ictx, sess.ID()); ok {
			kept := st.dangling[:0]
			for _, d := range st.dangling {
				if d.Timestamp.After(ack) {
					kept = append(kept, d)
				}
			}
			st.dangling = kept
		}
	}
	if len(st.dangling) == 0 && len(st.completions) == 0 {
		return nil, nil
	}
	o.mu.Lock()
	o.turns[ictx.InvocationID()] = st
	o.mu.Unlock()
	if len(st.dangling) > 0 {
		o.cfg.Logger.Warn("ambiguous prior effect: session has unresolved mutating tool calls from an interrupted turn; refusing mutating calls this turn",
			"session", sess.ID(), "invocation", ictx.InvocationID(),
			"dangling", describe(st.dangling))
	}
	return nil, nil
}

func (o *outbox) resolveSpawningDangling(ctx context.Context, sessionID string, dangling []DanglingIntent) []DanglingIntent {
	var resolved []DanglingIntent
	for _, d := range dangling {
		if o.cfg.Predicate(d.ToolName) == ClassSpawning {
			if childDangling, ok := o.cfg.SubSessionScanner(ctx, sessionID, d); ok {
				resolved = append(resolved, childDangling...)
				continue
			}
		}
		resolved = append(resolved, d)
	}
	sortByTimestamp(resolved)
	return resolved
}

func (o *outbox) beforeTool(ctx agent.Context, t tool.Tool, args map[string]any) (map[string]any, error) {
	class := o.cfg.Predicate(t.Name())
	if class == ClassReadOnly {
		return nil, nil
	}

	o.mu.Lock()
	st := o.turns[ctx.InvocationID()]
	o.mu.Unlock()
	if st == nil {
		return nil, nil
	}

	if class == ClassMutating {
		if resp, ok := st.completions[ctx.FunctionCallID()]; ok {
			if resp == nil {
				resp = map[string]any{"mast_replayed_effect": true, "note": "effect already recorded as completed; original response was empty"}
			}
			o.cfg.Logger.Info("replaying recorded effect instead of re-executing",
				"tool", t.Name(), "function_call_id", ctx.FunctionCallID(),
				"invocation", ctx.InvocationID())
			return resp, nil
		}
	}

	if len(st.dangling) == 0 {
		return nil, nil
	}
	o.cfg.Logger.Warn("refusing tool call in ambiguous-effect mode",
		"tool", t.Name(), "function_call_id", ctx.FunctionCallID(),
		"invocation", ctx.InvocationID())
	return map[string]any{
		"error": "ambiguous_prior_effect",
		"detail": "this session has mutating tool calls from an interrupted prior turn whose outcome is unknown; " +
			"mutating and sub-run-spawning calls are refused until an operator acknowledges " +
			"(mast sessions ack-effects, or resume --ack-effects, or abort the session). " +
			"Do not retry this call; finish any read-only work and report the situation.",
		"dangling_calls": describe(st.dangling),
	}, nil
}

func (o *outbox) afterRun(ictx agent.InvocationContext) {
	o.mu.Lock()
	delete(o.turns, ictx.InvocationID())
	o.mu.Unlock()
}

type scannedCall struct {
	intent                DanglingIntent
	class                 Class
	excluded              bool
	confirmedAtEventIndex int
}

type pairedScan struct {
	unpaired           []scannedCall
	completions        map[string]map[string]any
	lastCallEventIndex int
	lastEventIndex     int
}

type scanAccumulator struct {
	ps                *pairedScan
	pred              Predicate
	subAgents         map[string]bool
	open              map[string]scannedCall
	callName          map[string]string
	confirmTargetByID map[string]string
	confirmedCount    map[string]int
	order             []string
}

func pairScan(events session.Events, pred Predicate, subAgents map[string]bool) *pairedScan {
	if pred == nil {
		pred = NewPredicate(nil)
	}
	acc := &scanAccumulator{
		ps:                &pairedScan{completions: map[string]map[string]any{}, lastCallEventIndex: -1, lastEventIndex: -1},
		pred:              pred,
		subAgents:         subAgents,
		open:              map[string]scannedCall{},
		callName:          map[string]string{},
		confirmTargetByID: map[string]string{},
		confirmedCount:    map[string]int{},
	}
	evIdx := -1
	for ev := range events.All() {
		if ev == nil || ev.Content == nil || isSyntheticRepairEvent(ev) {
			continue
		}
		evIdx++
		acc.ps.lastEventIndex = evIdx
		longRunning := map[string]bool{}
		for _, id := range ev.LongRunningToolIDs {
			longRunning[id] = true
		}
		for _, part := range ev.Content.Parts {
			if part == nil {
				continue
			}
			if fc := part.FunctionCall; fc != nil {
				acc.onFunctionCall(ev, evIdx, fc, longRunning)
			}
			if fr := part.FunctionResponse; fr != nil {
				acc.onFunctionResponse(ev, evIdx, fr)
			}
		}
	}
	for _, id := range acc.order {
		if c, ok := acc.open[id]; ok {
			acc.ps.unpaired = append(acc.ps.unpaired, c)
		}
	}
	return acc.ps
}

func (acc *scanAccumulator) onFunctionCall(ev *session.Event, evIdx int, fc *genai.FunctionCall, longRunning map[string]bool) {
	if fc.Name == toolconfirmation.FunctionCallName {
		if id := confirmationGatedCallID(fc.Args); id != "" {
			delete(acc.ps.completions, id)
			if fc.ID != "" {
				acc.confirmTargetByID[fc.ID] = id
			}
			if orig, ok := acc.open[id]; ok {
				orig.excluded = true
				acc.open[id] = orig
			}
		}
	}
	if fc.ID == "" {
		return
	}
	acc.ps.lastCallEventIndex = evIdx
	acc.callName[fc.ID] = fc.Name
	if _, seen := acc.open[fc.ID]; !seen {
		acc.order = append(acc.order, fc.ID)
	}
	acc.open[fc.ID] = scannedCall{
		intent: DanglingIntent{
			ToolName:     fc.Name,
			CallID:       fc.ID,
			InvocationID: ev.InvocationID,
			Timestamp:    ev.Timestamp,
			EventIndex:   evIdx,
		},
		class:                 acc.pred(fc.Name),
		excluded:              longRunning[fc.ID] || controlCalls[fc.Name] || acc.subAgents[fc.Name],
		confirmedAtEventIndex: -1,
	}
}

func (acc *scanAccumulator) onFunctionResponse(ev *session.Event, evIdx int, fr *genai.FunctionResponse) {
	if fr.ID == "" {
		return
	}
	// mast #561 fix: a confirmation-gated call persists a placeholder
	// FunctionResponse with Actions.RequestedToolConfirmations[fr.ID]
	// when parking. Do NOT delete open[fr.ID] and do NOT record it as a
	// completion: keep it in open with excluded=true until the operator
	// answers adk_request_confirmation.
	if ev.Actions.RequestedToolConfirmations != nil {
		if _, isPlaceholder := ev.Actions.RequestedToolConfirmations[fr.ID]; isPlaceholder {
			if c, ok := acc.open[fr.ID]; ok {
				c.excluded = true
				acc.open[fr.ID] = c
			}
			return
		}
	}
	if origID, isConfirm := acc.confirmTargetByID[fr.ID]; isConfirm {
		delete(acc.open, fr.ID)
		if isConfirmationApproved(fr.Response) {
			acc.confirmedCount[origID]++
			if c, ok := acc.open[origID]; ok {
				c.excluded = false
				c.intent.Timestamp = ev.Timestamp
				if acc.confirmedCount[origID] == 1 {
					c.confirmedAtEventIndex = evIdx
				} else {
					c.confirmedAtEventIndex = -1
				}
				acc.open[origID] = c
			}
		} else {
			delete(acc.open, origID)
		}
		return
	}
	delete(acc.open, fr.ID)
	name := fr.Name
	if name == "" {
		name = acc.callName[fr.ID]
	}
	if acc.pred(name) == ClassMutating {
		acc.ps.completions[fr.ID] = fr.Response
	}
}

func isSyntheticRepairEvent(ev *session.Event) bool {
	if ev == nil || ev.CustomMetadata == nil {
		return false
	}
	b, _ := ev.CustomMetadata[SyntheticRepairMetadataKey].(bool)
	return b
}

// IsConfirmationApproved reports whether an adk_request_confirmation
// FunctionResponse.Response map represents an operator approval (accounting for
// both top-level {"confirmed": true, "payload": ...} and JSON-string wrapped
// {"response": "{\"confirmed\":true,\"payload\":...}"} shapes, and rejecting
// when payload.verdict == "reject").
func IsConfirmationApproved(resp map[string]any) bool {
	return isConfirmationApproved(resp)
}

func isConfirmationApproved(resp map[string]any) bool {
	if resp == nil {
		return false
	}
	unwrapped := unwrapConfirmationResponse(resp)
	confirmed, ok := unwrapped["confirmed"].(bool)
	if !ok || !confirmed {
		return false
	}
	if payload, hasPayload := unwrapped["payload"]; hasPayload && payload != nil {
		raw, err := json.Marshal(payload)
		if err == nil {
			var v struct {
				Verdict string `json:"verdict"`
			}
			if json.Unmarshal(raw, &v) == nil && v.Verdict == "reject" {
				return false
			}
		}
	}
	return true
}

func unwrapConfirmationResponse(resp map[string]any) map[string]any {
	if _, ok := resp["confirmed"].(bool); ok {
		return resp
	}
	raw, ok := resp["response"].(string)
	if !ok || raw == "" {
		return resp
	}
	var inner map[string]any
	if err := json.Unmarshal([]byte(raw), &inner); err != nil {
		return resp
	}
	return inner
}

func scanHistory(events session.Events, pred Predicate, subAgents map[string]bool) *turnState {
	return scanHistoryInternal(events, pred, subAgents, false)
}

func scanHistoryForRun(events session.Events, pred Predicate, subAgents map[string]bool) *turnState {
	return scanHistoryInternal(events, pred, subAgents, true)
}

func scanHistoryInternal(events session.Events, pred Predicate, subAgents map[string]bool, exemptTrailingConfirmation bool) *turnState {
	ps := pairScan(events, pred, subAgents)
	st := &turnState{completions: ps.completions}
	for _, c := range ps.unpaired {
		if c.excluded {
			continue
		}
		// When beforeRun runs at the start of a confirmation turn, ADK has just
		// appended the user's adk_request_confirmation FunctionResponse as the
		// trailing event in the session log. Exempt only that trailing
		// confirmation so the approved call can dispatch now, while any earlier
		// confirmation that crashed mid-tool in a prior turn (even under the same
		// reused ADK InvocationID) surfaces as dangling.
		if exemptTrailingConfirmation && c.confirmedAtEventIndex >= 0 && c.confirmedAtEventIndex == ps.lastEventIndex {
			continue
		}
		if c.class == ClassMutating || c.class == ClassSpawning {
			st.dangling = append(st.dangling, c.intent)
		}
	}
	sort.Slice(st.dangling, func(i, j int) bool { return st.dangling[i].Timestamp.Before(st.dangling[j].Timestamp) })
	return st
}

// DanglingScan splits a session's unpaired FunctionCalls into the three
// buckets the boot-time auto-resume pass acts on.
type DanglingScan struct {
	Mutating           []DanglingIntent
	Repairable         []DanglingIntent
	Deferred           []DanglingIntent
	LastCallEventIndex int
}

// Unacked returns the Mutating intents whose Timestamp is strictly after ackedAt.
func (d DanglingScan) Unacked(ackedAt time.Time) []DanglingIntent {
	if ackedAt.IsZero() {
		return d.Mutating
	}
	var out []DanglingIntent
	for _, m := range d.Mutating {
		if m.Timestamp.After(ackedAt) {
			out = append(out, m)
		}
	}
	return out
}

// ScanDangling classifies a session's unpaired FunctionCalls for the
// auto-resume eligibility and repair decisions.
func ScanDangling(events session.Events, pred Predicate, subAgents map[string]bool) DanglingScan {
	ps := pairScan(events, pred, subAgents)
	out := DanglingScan{LastCallEventIndex: ps.lastCallEventIndex}
	for _, c := range ps.unpaired {
		switch {
		case c.excluded:
			out.Deferred = append(out.Deferred, c.intent)
		case c.class == ClassMutating || c.class == ClassSpawning:
			out.Mutating = append(out.Mutating, c.intent)
		default:
			out.Repairable = append(out.Repairable, c.intent)
		}
	}
	sortByTimestamp(out.Mutating)
	sortByTimestamp(out.Repairable)
	sortByTimestamp(out.Deferred)
	return out
}

func sortByTimestamp(ds []DanglingIntent) {
	sort.Slice(ds, func(i, j int) bool { return ds[i].Timestamp.Before(ds[j].Timestamp) })
}

func confirmationGatedCallID(args map[string]any) string {
	orig, ok := args["originalFunctionCall"]
	if !ok {
		return ""
	}
	switch v := orig.(type) {
	case *genai.FunctionCall:
		if v != nil {
			return v.ID
		}
	case map[string]any:
		if id, ok := v["id"].(string); ok {
			return id
		}
	}
	return ""
}

func describe(ds []DanglingIntent) []string {
	out := make([]string, 0, len(ds))
	for _, d := range ds {
		out = append(out, fmt.Sprintf("%s (call %s, invocation %s, %s)",
			d.ToolName, d.CallID, d.InvocationID, d.Timestamp.UTC().Format(time.RFC3339)))
	}
	return out
}

// Overrides flattens workload tool_catalog per-tool policies into the
// override map NewPredicate consumes, logging each applied override.
func Overrides(logger *slog.Logger, policies []ToolPolicy) map[string]bool {
	if len(policies) == 0 {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	out := make(map[string]bool, len(policies))
	for _, p := range policies {
		name := strings.TrimSpace(p.Name)
		if name == "" || p.Mutating == nil {
			continue
		}
		out[name] = *p.Mutating
		logger.Info("tool mutation-class override applied (workload tool_catalog)",
			"tool", name, "mutating", *p.Mutating)
	}
	return out
}

// ToolPolicy mirrors per-tool mutation policy overrides.
type ToolPolicy struct {
	Name     string
	Mutating *bool
}
