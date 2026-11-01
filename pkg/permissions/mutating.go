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

package permissions

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ToolClass classifies a tool's side-effect posture for the durable
// write gate and recorded-effect outbox (#1400).
//
// Zero value is ClassMutating (DefaultClass) so any unclassified tool
// fails closed as mutating.
type ToolClass int

const (
	// ClassMutating tools get intent/completion records in the session
	// outbox, require operator approval under require_approval, and are
	// refused when the session has an unacknowledged dangling effect.
	ClassMutating ToolClass = iota

	// ClassReadOnly tools do not mutate external state and never pay the
	// write-gate park or outbox replay check.
	ClassReadOnly

	// ClassSpawning tools start synchronous sub-runs whose inner tool
	// calls are governed by the sub-run's own outbox and write gate.
	ClassSpawning

	// DefaultClass is the fail-closed default for any unknown tool.
	DefaultClass = ClassMutating
)

// String renders ToolClass for diagnostics and logs.
func (c ToolClass) String() string {
	switch c {
	case ClassReadOnly:
		return "read_only"
	case ClassSpawning:
		return "spawning"
	default:
		return "mutating"
	}
}

// MutatingMode configures how the write gate handles mutating tool calls
// (hitl_policy.on_mutation).
type MutatingMode string

const (
	// MutatingModeRequireApproval parks mutating calls for operator approval.
	MutatingModeRequireApproval MutatingMode = "require_approval"
	// MutatingModeApply executes mutating calls without operator approval
	// after policy/plan-first checks pass.
	MutatingModeApply MutatingMode = "apply"
	// MutatingModeDryRun refuses mutating execution and reports what would
	// have been called.
	MutatingModeDryRun MutatingMode = "dry_run"
)

// Valid reports whether m is a recognized MutatingMode.
func (m MutatingMode) Valid() bool {
	switch m {
	case MutatingModeRequireApproval, MutatingModeApply, MutatingModeDryRun:
		return true
	default:
		return false
	}
}

// MutatingDecision is the outcome of a mutating tool policy check.
type MutatingDecision int

const (
	// MutatingDeny refuses the call outright.
	MutatingDeny MutatingDecision = iota
	// MutatingPark requires durable operator approval before execution.
	MutatingPark
	// MutatingAllow permits execution without parking.
	MutatingAllow
)

// ErrApprovalRequired reports that policy has no objection to a mutating
// tool call but that the call may not proceed until an operator approves
// it. It is the "ask a human" answer, not a denial, and it is the only
// non-denial answer CheckMutatingToolCall gives under require_approval.
var ErrApprovalRequired = errors.New("permissions: mutating tool call requires operator approval")

// ErrGrantScopeRefused reports that an operator's verdict on a mutating
// tool call came back with a grant scope broader than "this one call".
// See RecordMutationVerdict.
var ErrGrantScopeRefused = errors.New("permissions: grant scope not admissible for a mutating tool")

// engineControlCalls are ADK and runtime control-flow function calls,
// never external tool effects. They are unconditionally ClassReadOnly
// and cannot be overridden by configuration.
var engineControlCalls = map[string]bool{
	"adk_request_input":        true,
	"adk_request_credential":   true,
	"adk_request_confirmation": true,
	"finish_task":              true,
	"mark_task_done":           true,
	"transfer_to_agent":        true,
	"task_completed":           true,
	"exit_loop":                true,
	"pause_session":            true,
	"request_operator_input":   true,
}

// IsEngineControlCall reports whether name is an engine or harness
// control-flow function call rather than an external tool effect.
func IsEngineControlCall(name string) bool {
	return engineControlCalls[strings.TrimSpace(name)]
}

// builtinToolClasses classifies core-agent's built-in tools and mast's
// orchestration tools. Anything absent from this table defaults to
// ClassMutating.
var builtinToolClasses = map[string]ToolClass{
	// Read-only built-in tools.
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

	// Spawning tools (synchronous or background sub-runs whose inner
	// mutating calls are governed by the sub-run's own gate/outbox).
	"task":                     ClassSpawning,
	"single_turn":              ClassSpawning,
	"spawn_subagent":           ClassSpawning,
	"spawn_agent":              ClassSpawning,
	"invoke_specialist":        ClassSpawning,
	"run_shape_llm_router":     ClassSpawning,
	"run_shape_fan_out_fan_in": ClassSpawning,

	// Mutating built-in and remote tools.
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
}

// DefaultToolClass returns the built-in ToolClass for name. Engine
// control calls are always ClassReadOnly; known built-ins return their
// registered class; unknown tools return DefaultClass (ClassMutating).
func DefaultToolClass(name string) ToolClass {
	name = strings.TrimSpace(name)
	if IsEngineControlCall(name) {
		return ClassReadOnly
	}
	if c, ok := builtinToolClasses[name]; ok {
		return c
	}
	return DefaultClass
}

// MCPToolClass returns the ToolClass for an MCP tool given its server's
// readOnlyHint annotation and whether the server is configured to trust
// readOnlyHint annotations (mast #573). Untrusted servers always
// classify as ClassMutating unless overridden by operator config.
func MCPToolClass(readOnlyHint, trustServerHint bool) ToolClass {
	if trustServerHint && readOnlyHint {
		return ClassReadOnly
	}
	return ClassMutating
}

// SetToolClasses configures per-tool classification overrides on the
// gate. Engine control calls remain ClassReadOnly regardless of
// overrides. Safe for concurrent use.
func (g *Gate) SetToolClasses(classes map[string]ToolClass) {
	if g == nil {
		return
	}
	copied := make(map[string]ToolClass, len(classes))
	for k, v := range classes {
		k = strings.TrimSpace(k)
		if k == "" || IsEngineControlCall(k) {
			continue
		}
		copied[k] = v
	}
	g.mu.Lock()
	g.toolClasses = copied
	g.mu.Unlock()
}

// ToolClass returns the resolved ToolClass for toolName on this gate.
// Precedence: engine control calls (always ClassReadOnly) > explicit
// per-gate overrides > DefaultToolClass (built-in table, then
// ClassMutating).
func (g *Gate) ToolClass(toolName string) ToolClass {
	toolName = strings.TrimSpace(toolName)
	if IsEngineControlCall(toolName) {
		return ClassReadOnly
	}
	if g != nil {
		g.mu.Lock()
		classes := g.toolClasses
		g.mu.Unlock()
		if c, ok := classes[toolName]; ok {
			return c
		}
	}
	return DefaultToolClass(toolName)
}

// SetMutatingMode sets the gate's mutating-tool policy mode.
func (g *Gate) SetMutatingMode(m MutatingMode) {
	if g == nil {
		return
	}
	g.mu.Lock()
	g.mutatingMode = m
	g.mu.Unlock()
}

// MutatingMode returns the gate's mutating-tool policy mode, defaulting
// to MutatingModeRequireApproval when unset.
func (g *Gate) MutatingMode() MutatingMode {
	if g == nil {
		return MutatingModeRequireApproval
	}
	g.mu.Lock()
	m := g.mutatingMode
	g.mu.Unlock()
	if m == "" {
		return MutatingModeRequireApproval
	}
	return m
}

// CheckMutatingToolCall applies policy to a call the workload's mutation
// predicate classifies as mutating.
//
// Under the default MutatingModeRequireApproval it never returns nil:
// either the call is refused outright — the error says why — or the
// answer is ErrApprovalRequired, meaning "policy is satisfied, now go
// ask a human". No mutation is auto-approved by ModeYolo, ModeAllow,
// ModeAcceptEdits, session grants, or policy allowlists.
//
// What does apply, in order: the plan-first pre-check, the deny policy,
// and ModePlan, under which nothing executes at all.
func (g *Gate) CheckMutatingToolCall(ctx context.Context, toolName, key string) error {
	g = g.resolveSessionGate(ctx)
	if err := g.planFirstDenial(toolName, false); err != nil {
		return err
	}
	if g.matchPolicy(toolName, key) == OutcomeDeny {
		return fmt.Errorf("%s denied by config policy: %q", toolName, key)
	}
	if g.Mode() == ModePlan {
		return fmt.Errorf("%s denied: tool execution disabled in 'plan' mode (%q)", toolName, key)
	}
	switch g.MutatingMode() {
	case MutatingModeApply:
		return nil
	case MutatingModeDryRun:
		return fmt.Errorf("%s denied: mutating tool execution disabled in 'dry_run' mode (%q)", toolName, key)
	}
	return fmt.Errorf("%w (tool=%s detail=%q)", ErrApprovalRequired, toolName, key)
}

// RecordMutationVerdict closes the loop CheckMutatingToolCall opened: an
// operator has answered, and this decides whether that answer authorizes
// the call.
//
// The admissible answers are exactly DecisionAllowOnce and
// DecisionDeny. Anything broader is refused with ErrGrantScopeRefused
// rather than silently narrowed to allow-once, so a UI that offers the
// wrong buttons fails loudly instead of granting more than the operator
// was told they were granting.
//
// Nothing is remembered on any path. An allow-once is recorded in the
// approval audit log and nowhere else, so the next mutating call asks
// again even for a byte-identical request.
func (g *Gate) RecordMutationVerdict(ctx context.Context, toolName, key string, d Decision) error {
	g = g.resolveSessionGate(ctx)
	switch d {
	case DecisionDeny:
		return fmt.Errorf("%s denied by operator: %q", toolName, key)
	case DecisionAllowOnce:
		g.recordApproval(toolName, key, d, "")
		return nil
	default:
		return fmt.Errorf("%w: %s for mutating tool %s (%q); only allow-once is admissible for a mutation", ErrGrantScopeRefused, d, toolName, key)
	}
}
