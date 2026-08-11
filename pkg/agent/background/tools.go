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

package background

import (
	"context"
	"strings"
	"time"

	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
)

// budgetsFromArgs reads the per-spawn budget caps off the tool args.
// Zero values mean "no override" (inherit the spec/default cap); the
// reference paths treat them as tighten-only.
func budgetsFromArgs(args spawnAgentArgs) Budgets {
	return Budgets{
		MaxTurns:     args.MaxTurns,
		MaxCost:      args.MaxCostUSD,
		MaxWallclock: time.Duration(args.MaxWallclockSeconds) * time.Second,
	}
}

// spawnResult shapes a launch outcome into the tool's result. A nil
// error yields the running handle's identity; ErrNoParent propagates as
// a Go error (a developer wiring bug, not model-adaptable); any other
// error is surfaced as an "error: ..." status under fallbackName so the
// model can adjust and retry.
func spawnResult(h *Handle, fallbackName string, err error) (spawnAgentResult, error) {
	if err != nil {
		if err == ErrNoParent {
			return spawnAgentResult{}, err
		}
		return spawnAgentResult{Name: fallbackName, Status: "error: " + err.Error()}, nil
	}
	return spawnAgentResult{
		Name:   h.Name,
		Branch: h.Branch,
		Status: h.Status().String(),
	}, nil
}

// awaitResult blocks the parent turn until the subagent finishes, the
// sync-wait timeout elapses, or the parent context is canceled — the
// synchronous (wait: true) delegation path (#626/D5). On completion it
// returns the terminal status plus the subagent's final output; on timeout
// or cancellation the subagent keeps running in the background (its result
// arrives later as a [Background reports] line) and the tool returns a
// "running" status so the model knows the work is still in flight.
//
// Note: a subagent that completes always pushes its completion alert to the
// parent inbox (the goroutine's pushAlert runs before it closes done, so a
// waiter can't retroactively suppress it). A successful wait therefore
// surfaces the result twice — inline here, and again as a redundant
// [Background reports] line on the next turn. That's mildly noisy but not
// incorrect; deduping consumed-synchronously completions is a follow-up
// (see docs/unified-subagent-invocation-design.md, "Still open").
func (m *Manager) awaitResult(ctx context.Context, h *Handle) spawnAgentResult {
	var timeout <-chan time.Time
	if m.syncWaitTimeout > 0 {
		timer := time.NewTimer(m.syncWaitTimeout)
		defer timer.Stop()
		timeout = timer.C
	}
	select {
	case <-h.Done():
		res := spawnAgentResult{Name: h.Name, Branch: h.Branch, Status: h.Status().String()}
		if err := h.Err(); err != nil {
			res.Output = err.Error()
			return res
		}
		if r := h.Result(); r != nil {
			// Prefer the completion report (report_done's detail); fall
			// back to the last assistant text for runs that ended without
			// signalling completion (budget cap, deferral).
			if r.DoneDetail != "" {
				res.Output = r.DoneDetail
			} else {
				res.Output = r.FinalText
			}
		}
		return res
	case <-timeout:
		return spawnAgentResult{
			Name:   h.Name,
			Branch: h.Branch,
			Status: "running: wait timed out after " + m.syncWaitTimeout.String() + "; still running in background, result will be pushed",
		}
	case <-ctx.Done():
		return spawnAgentResult{
			Name:   h.Name,
			Branch: h.Branch,
			Status: "running: wait canceled; still running in background",
		}
	}
}

// buildSpawnSpec turns spawn_agent args into a concrete Spec plus the
// name to report on error. It routes to the reference path when
// args.Agent names a preconfigured (catalog) subagent (applying
// narrowing-only overrides), or the ad-hoc inline-persona path otherwise
// — the latter gated by allow_adhoc (#626). Declarative-subagent
// templates are handled earlier, in the tool handler, via SpawnTemplate.
func buildSpawnSpec(mgr *Manager, args spawnAgentArgs) (Spec, string, error) {
	budgets := budgetsFromArgs(args)
	if ref := strings.TrimSpace(args.Agent); ref != "" {
		spec, err := mgr.resolvePredefinedSpec(ref, RefOverrides{
			Goal:    args.Goal,
			Model:   args.Model,
			Tools:   args.Tools,
			Budgets: budgets,
		})
		if err != nil {
			return Spec{}, ref, err
		}
		spec.Name = mgr.nextInstanceName(ref, args.Name)
		return spec, spec.Name, nil
	}
	// Ad-hoc (inline-persona) path — off unless the operator enabled it.
	if !mgr.AllowAdhoc() {
		return Spec{}, args.Name, ErrAdhocDisabled
	}
	// Ad-hoc model resolution is looser than the referenced path (D2/§1):
	// ad-hoc is already parent-authored, so naming a specific model is no
	// additional escalation. inherit/"" and "small" behave as elsewhere.
	modelID, err := mgr.resolveAdhocModel(args.Model)
	if err != nil {
		return Spec{}, args.Name, err
	}
	return Spec{
		Name:         args.Name,
		SystemPrompt: args.SystemPrompt,
		Goal:         args.Goal,
		Tools:        args.Tools,
		Extras:       args.Extras,
		Budgets:      budgets,
		ModelID:      modelID,
		Scheduler:    args.Scheduler,
	}, args.Name, nil
}

// spawnAgentArgs is the JSON shape the parent's model sees when it
// calls spawn_agent. The catalog-known tool names available today
// are listed in the description (read_file, write_file, edit_file,
// list_dir, glob, grep, bash, todo) — the model may also list any
// MCP-namespaced tool or skill name in extras.
type spawnAgentArgs struct {
	Agent               string   `json:"agent,omitempty" jsonschema:"name of a preconfigured subagent to spawn (the preferred form). Its persona, tools, model, and budgets are already set; you supply the goal and may only narrow the rest (drop tools, tighten budgets, downshift model to 'small'). Leave empty only to author an ad-hoc subagent inline, which requires system_prompt and is disabled unless the operator enabled ad-hoc spawns."`
	Name                string   `json:"name,omitempty" jsonschema:"optional instance name. For a preconfigured 'agent', omit to let the runtime auto-name the instance (e.g. cluster-1). Required for an ad-hoc subagent. No spaces, dots or slashes."`
	SystemPrompt        string   `json:"system_prompt,omitempty" jsonschema:"ad-hoc only: the subagent's system instruction. Ignored when 'agent' references a preconfigured subagent (its persona is fixed)."`
	Goal                string   `json:"goal" jsonschema:"the task the subagent should accomplish, written as a single instruction"`
	Model               string   `json:"model,omitempty" jsonschema:"optional model override: omit to inherit (the preconfigured subagent's model, or the parent's), or 'small' to downshift to the small tier. A specific model is not selectable here — configure a dedicated subagent for that."`
	Tools               []string `json:"tools,omitempty" jsonschema:"tool names to grant. For a preconfigured 'agent' this may only NARROW its grant (a subset — unlisted tools are dropped). For an ad-hoc subagent these are the built-in tools it may use (e.g. read_file, list_dir, glob, grep, bash, todo, write_file, edit_file). Unknown names error at spawn time."`
	Extras              []string `json:"extras,omitempty" jsonschema:"ad-hoc only: additional tool names beyond the built-ins (e.g. MCP tools like kubectl_get, or skill names). Looked up in the same catalog as tools."`
	MaxTurns            int      `json:"max_turns,omitempty" jsonschema:"tighten the per-subagent turn cap (may only lower a preconfigured subagent's cap)"`
	MaxCostUSD          float64  `json:"max_cost_usd,omitempty" jsonschema:"tighten the per-subagent dollar cap (may only lower a preconfigured subagent's cap)"`
	MaxWallclockSeconds int      `json:"max_wallclock_seconds,omitempty" jsonschema:"tighten the per-subagent wall-clock cap (may only lower a preconfigured subagent's cap)"`
	Scheduler           string   `json:"scheduler,omitempty" jsonschema:"ad-hoc only: between-turn scheduler for this subagent. Values: 'default' (use the manager's default — typical), 'sleep' (in-process goroutine sleep — long-lived daemon shape), 'exit_on_defer' (exit cleanly so an orchestrator like k8s CronJob restarts at the wake-time), 'none' (no scheduler — schedule_next_turn unavailable, useful for one-shot triage subagents). Default: 'default'."`
	Wait                bool     `json:"wait,omitempty" jsonschema:"set true to run the subagent synchronously: block this turn until it finishes and return its final output inline (like a direct delegation). Omit (the default) to fire-and-continue — the subagent runs in the background and its result is pushed to a later turn. A synchronous wait is capped by a tighter wall-clock; on timeout the subagent keeps running in the background and its result is pushed later."`
}

type spawnAgentResult struct {
	Name   string `json:"name"`
	Branch string `json:"branch"`
	Status string `json:"status"`
	// Output carries the subagent's final text on a synchronous spawn
	// (wait: true) that ran to completion. Empty for fire-and-continue
	// spawns and for a wait that timed out (the result is pushed later).
	Output string `json:"output,omitempty"`
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
func NewSpawnAgentTool(mgr *Manager) tool.Tool {
	handler := func(toolCtx tool.Context, args spawnAgentArgs) (spawnAgentResult, error) {
		parentBranch := toolCtx.Branch()

		var (
			h    *Handle
			name string
			err  error
		)
		// Declarative-subagent template reference (rooted or inline
		// subagent registered via SetSubagentTemplates) takes the
		// pre-resolved async path — its persona/tools/model are already
		// built and can't be reconstructed from the catalog (#626).
		if ref := strings.TrimSpace(args.Agent); ref != "" && mgr.hasTemplate(ref) {
			name = ref
			h, err = mgr.SpawnTemplate(toolCtx, parentBranch, ref, RefOverrides{
				Goal:    args.Goal,
				Model:   args.Model,
				Tools:   args.Tools,
				Budgets: budgetsFromArgs(args),
			}, args.Name)
		} else {
			// Catalog reference or ad-hoc inline persona → concrete Spec.
			var spec Spec
			spec, name, err = buildSpawnSpec(mgr, args)
			if err != nil {
				// Resolution errors (unknown reference, ad-hoc disabled,
				// non-narrowing override) are surfaced as a result so the
				// model can adapt.
				return spawnAgentResult{Name: name, Status: "error: " + err.Error()}, nil
			}
			h, err = mgr.Spawn(toolCtx, parentBranch, spec)
		}

		// Fire-and-continue (the default) or a failed launch → report the
		// handle's identity/error immediately. A synchronous spawn (wait:
		// true) blocks the turn on completion and returns the final output
		// inline (#626/D5), capped by the sync-wait timeout.
		if err != nil || !args.Wait {
			return spawnResult(h, name, err)
		}
		return mgr.awaitResult(toolCtx, h), nil
	}
	t, err := functiontool.New(functiontool.Config{
		Name:        "spawn_agent",
		Description: "Spawn an in-process background subagent that runs in parallel with you. Prefer referencing a preconfigured subagent by name via the 'agent' field and supplying its 'goal' (optionally a quick plan) — its persona, tools, model, and budgets are already set by the operator, and you may only narrow them. Authoring an ad-hoc subagent inline (system_prompt + tools) is only possible when the operator enabled ad-hoc spawns. The subagent runs autonomously; you'll receive its updates as '[Background reports]' lines prepended to your next turn when it calls report_alert or finishes. Use this for tasks that should run continuously (monitoring) or in parallel (independent fan-out work). Do NOT list 'schedule_next_turn', 'report_done', 'report_alert', or 'report_completed' in the tools field — those are auto-wired into every subagent by the runtime; listing them is a no-op (silently skipped).",
	}, handler)
	if err != nil {
		panic("background: NewSpawnAgentTool: " + err.Error())
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
func NewListAgentsTool(mgr *Manager) tool.Tool {
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
		panic("background: NewListAgentsTool: " + err.Error())
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
func NewCheckAgentTool(mgr *Manager) tool.Tool {
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
		panic("background: NewCheckAgentTool: " + err.Error())
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
func NewStopAgentTool(mgr *Manager) tool.Tool {
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
		panic("background: NewStopAgentTool: " + err.Error())
	}
	return t
}

// NewSpawnTools is a convenience that returns all four
// model-facing background-agent tools in one slice, ready to pass
// through agent.WithTools. The bundled CLI uses this to wire the
// full suite atomically.
func NewSpawnTools(mgr *Manager) []tool.Tool {
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
