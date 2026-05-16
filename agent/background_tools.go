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

package agent

import (
	"context"
	"time"

	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
)

// spawnAgentArgs is the JSON shape the parent's model sees when it
// calls spawn_agent. The catalog-known tool names available today
// are listed in the description (read_file, write_file, edit_file,
// list_dir, glob, grep, bash, todo) — the model may also list any
// MCP-namespaced tool or skill name in extras.
type spawnAgentArgs struct {
	Name                string   `json:"name" jsonschema:"unique short identifier for this background subagent (no spaces, dots or slashes)"`
	SystemPrompt        string   `json:"system_prompt" jsonschema:"the subagent's system instruction — describes its role, constraints, and how it should behave"`
	Goal                string   `json:"goal" jsonschema:"the task the subagent should accomplish, written as a single instruction"`
	Tools               []string `json:"tools,omitempty" jsonschema:"built-in tool names to grant the subagent (e.g. read_file, list_dir, glob, grep, bash, todo, write_file, edit_file). Unknown names error at spawn time."`
	Extras              []string `json:"extras,omitempty" jsonschema:"additional tool names beyond the built-ins (e.g. MCP tools like kubectl_get, or skill names). Looked up in the same catalog as tools."`
	MaxTurns            int      `json:"max_turns,omitempty" jsonschema:"override the default per-subagent turn cap (default: manager's WithBackgroundDefaultBudgets)"`
	MaxCostUSD          float64  `json:"max_cost_usd,omitempty" jsonschema:"override the default per-subagent dollar cap"`
	MaxWallclockSeconds int      `json:"max_wallclock_seconds,omitempty" jsonschema:"override the default per-subagent wall-clock cap"`
}

type spawnAgentResult struct {
	Name   string `json:"name"`
	Branch string `json:"branch"`
	Status string `json:"status"`
}

// NewSpawnAgentTool returns a tool the parent's model can call to
// launch a new in-process background subagent. The tool's name in
// the model's view is "spawn_agent"; the registered handler defers
// to mgr.Spawn after reading the calling tool.Context's branch so
// the new subagent's events land in the right hierarchical branch.
//
// Spawn errors (invalid spec, depth/concurrency cap, unknown tool)
// are returned as the tool's result text rather than as Go errors,
// so the model sees them in conversation context and can adapt
// (e.g. by stopping a sibling first). Provider/model construction
// errors propagate normally since those are typically caller-fixable
// configuration problems.
func NewSpawnAgentTool(mgr *BackgroundAgentManager) tool.Tool {
	handler := func(toolCtx tool.Context, args spawnAgentArgs) (spawnAgentResult, error) {
		parentBranch := toolCtx.Branch()
		spec := BackgroundSpec{
			Name:         args.Name,
			SystemPrompt: args.SystemPrompt,
			Goal:         args.Goal,
			Tools:        args.Tools,
			Extras:       args.Extras,
			Budgets: BackgroundBudgets{
				MaxTurns:     args.MaxTurns,
				MaxCost:      args.MaxCostUSD,
				MaxWallclock: time.Duration(args.MaxWallclockSeconds) * time.Second,
			},
		}
		h, err := mgr.Spawn(toolCtx, parentBranch, spec)
		if err != nil {
			// Surface as a result so the model can adjust, except for
			// the "no parent" case which is a developer error.
			if err == ErrNoParent {
				return spawnAgentResult{}, err
			}
			return spawnAgentResult{
				Name:   args.Name,
				Status: "error: " + err.Error(),
			}, nil
		}
		return spawnAgentResult{
			Name:   h.Name,
			Branch: h.Branch,
			Status: h.Status().String(),
		}, nil
	}
	t, err := functiontool.New(functiontool.Config{
		Name:        "spawn_agent",
		Description: "Spawn an in-process background subagent that runs in parallel with you. You provide its name, system prompt, goal, and the tools it may use. The subagent runs autonomously; you'll receive its updates as '[Background reports]' lines prepended to your next turn when it calls report_alert or finishes. Use this for tasks that should run continuously (monitoring) or in parallel (independent fan-out work).",
	}, handler)
	if err != nil {
		panic("agent: NewSpawnAgentTool: " + err.Error())
	}
	return t
}

// listAgentsResult is returned by list_agents: one row per registered
// subagent regardless of state.
type listAgentsResult struct {
	Agents []agentSummary `json:"agents"`
}

type agentSummary struct {
	Name      string `json:"name"`
	Branch    string `json:"branch"`
	Status    string `json:"status"`
	StartedAt string `json:"started_at"`
}

// NewListAgentsTool returns a tool the parent's model can call to see
// every subagent the manager has tracked (running + terminal). Empty
// list when none have been spawned.
func NewListAgentsTool(mgr *BackgroundAgentManager) tool.Tool {
	handler := func(_ tool.Context, _ struct{}) (listAgentsResult, error) {
		all := mgr.List()
		out := listAgentsResult{Agents: make([]agentSummary, 0, len(all))}
		for _, h := range all {
			out.Agents = append(out.Agents, agentSummary{
				Name:      h.Name,
				Branch:    h.Branch,
				Status:    h.Status().String(),
				StartedAt: h.StartedAt.Format(time.RFC3339),
			})
		}
		return out, nil
	}
	t, err := functiontool.New(functiontool.Config{
		Name:        "list_agents",
		Description: "List every background subagent you've spawned, with current status. Use to introspect what's running before deciding whether to spawn more or stop existing ones.",
	}, handler)
	if err != nil {
		panic("agent: NewListAgentsTool: " + err.Error())
	}
	return t
}

type checkAgentArgs struct {
	Name string `json:"name" jsonschema:"the name of the subagent you spawned earlier (from spawn_agent's result)"`
}

type checkAgentResult struct {
	Name           string  `json:"name"`
	Branch         string  `json:"branch"`
	Status         string  `json:"status"`
	StartedAt      string  `json:"started_at"`
	FinalText      string  `json:"final_text,omitempty"`
	StopReason     string  `json:"stop_reason,omitempty"`
	Error          string  `json:"error,omitempty"`
	Turns          int     `json:"turns,omitempty"`
	InputTokens    int     `json:"input_tokens,omitempty"`
	OutputTokens   int     `json:"output_tokens,omitempty"`
	CostUSD        float64 `json:"cost_usd,omitempty"`
	DurationSecond float64 `json:"duration_seconds,omitempty"`
}

// NewCheckAgentTool returns a tool the parent's model can call to
// inspect one subagent's detailed status — including its terminal
// result (final text, stop reason, totals) once it's finished.
func NewCheckAgentTool(mgr *BackgroundAgentManager) tool.Tool {
	handler := func(_ tool.Context, args checkAgentArgs) (checkAgentResult, error) {
		h, ok := mgr.Get(args.Name)
		if !ok {
			return checkAgentResult{
				Name:   args.Name,
				Status: "not_found",
			}, nil
		}
		res := checkAgentResult{
			Name:      h.Name,
			Branch:    h.Branch,
			Status:    h.Status().String(),
			StartedAt: h.StartedAt.Format(time.RFC3339),
		}
		if r := h.Result(); r != nil {
			res.FinalText = r.FinalText
			res.StopReason = string(r.Reason)
			res.Turns = r.Turns
			res.InputTokens = r.InputTokens
			res.OutputTokens = r.OutputTokens
			res.CostUSD = r.CostUSD
			res.DurationSecond = r.Duration.Seconds()
		}
		if err := h.Err(); err != nil {
			res.Error = err.Error()
		}
		return res, nil
	}
	t, err := functiontool.New(functiontool.Config{
		Name:        "check_agent",
		Description: "Get detailed status for one background subagent. Returns final result + totals once the subagent has finished, or the running status otherwise.",
	}, handler)
	if err != nil {
		panic("agent: NewCheckAgentTool: " + err.Error())
	}
	return t
}

type stopAgentArgs struct {
	Name string `json:"name" jsonschema:"the name of the subagent to stop"`
}

type stopAgentResult struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

// NewStopAgentTool returns a tool the parent's model can call to
// cancel a running subagent. No-op if the subagent already terminal.
// Returns an error result (not a tool failure) when the name is
// unknown so the model can adapt.
func NewStopAgentTool(mgr *BackgroundAgentManager) tool.Tool {
	handler := func(_ tool.Context, args stopAgentArgs) (stopAgentResult, error) {
		if err := mgr.Stop(args.Name); err != nil {
			return stopAgentResult{
				Name:   args.Name,
				Status: "error: " + err.Error(),
			}, nil
		}
		h, _ := mgr.Get(args.Name)
		st := "stopping"
		if h != nil {
			st = h.Status().String()
		}
		return stopAgentResult{Name: args.Name, Status: st}, nil
	}
	t, err := functiontool.New(functiontool.Config{
		Name:        "stop_agent",
		Description: "Stop a running background subagent. The subagent's goroutine exits at its next checkpoint; its terminal status becomes 'stopped'.",
	}, handler)
	if err != nil {
		panic("agent: NewStopAgentTool: " + err.Error())
	}
	return t
}

// NewBackgroundSpawnTools is a convenience that returns all four
// model-facing background-agent tools in one slice, ready to pass
// through agent.WithTools. The bundled CLI uses this to wire the
// full suite atomically.
func NewBackgroundSpawnTools(mgr *BackgroundAgentManager) []tool.Tool {
	return []tool.Tool{
		NewSpawnAgentTool(mgr),
		NewListAgentsTool(mgr),
		NewCheckAgentTool(mgr),
		NewStopAgentTool(mgr),
	}
}

// ensure imports stay live when handler bodies don't reference them
// directly in future edits.
var _ = context.Background
