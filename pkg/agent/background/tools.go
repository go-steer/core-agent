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

	"github.com/go-steer/core-agent/v2/pkg/agent/autonomous"
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

// refusedSpawn shapes a launch that never happened.
//
// The refusal goes into the result twice, on purpose. Status carries
// the "error: ..." prose the model reads and adapts to — that half is
// the errors-as-results contract this tool has always had. Error
// carries the same text under ADK's reserved key, which is what every
// consumer downstream of the model reads to tell a failed call from a
// successful one: the flow traces the call as failed, the watchdog
// counts it against the failure streak, and both TUIs draw ✗ with the
// reason instead of rendering a refusal exactly like a launch (#746).
func refusedSpawn(name string, err error) spawnAgentResult {
	return spawnAgentResult{
		Name:   name,
		Status: "error: " + err.Error(),
		Error:  err.Error(),
	}
}

// spawnResult shapes a launch outcome into the tool's result. A nil
// error yields the running handle's identity; ErrNoParent propagates as
// a Go error (a developer wiring bug, not model-adaptable); any other
// error is surfaced as a refusal under fallbackName so the model can
// adjust and retry.
func spawnResult(h *Handle, fallbackName string, err error) (spawnAgentResult, error) {
	if err != nil {
		if err == ErrNoParent {
			return spawnAgentResult{}, err
		}
		return refusedSpawn(fallbackName, err), nil
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
// A completion consumed here is delivered exactly once (#646): the waiter
// claims the handle before blocking, and the completion goroutine skips its
// terminal [Background reports] alert when the claim is outstanding. A wait
// that gives up (timeout or cancellation) releases the claim so the alert is
// pushed as it would be for a fire-and-continue spawn.
func (m *Manager) awaitResult(ctx context.Context, h *Handle) spawnAgentResult {
	// Claim before blocking. A false return means the subagent was already
	// terminal — its alert is already in flight and can't be suppressed, so
	// the result is delivered inline AND as a report. That window is the
	// microseconds between Spawn returning a handle and this call.
	claimed := h.claimSync()
	var timeout <-chan time.Time
	if m.syncWaitTimeout > 0 {
		timer := time.NewTimer(m.syncWaitTimeout)
		defer timer.Stop()
		timeout = timer.C
	}
	select {
	case <-h.Done():
		return completionResult(h)
	case <-timeout:
		if res, ok := m.abandonWait(h, claimed); !ok {
			return res
		}
		return spawnAgentResult{
			Name:   h.Name,
			Branch: h.Branch,
			// "into your next turn" names the delivery rather than
			// just promising one: the result arrives as a
			// "[Background reports]" line, and pushAlert wakes this
			// agent so that turn happens even if this one was going
			// to be the last (#780). Saying where it lands is what
			// lets the model decide to end the turn and read it,
			// instead of padding the turn out to stay alive.
			Status: "running: wait timed out after " + m.syncWaitTimeout.String() + "; still running in background, its result will be pushed into your next turn",
		}
	case <-ctx.Done():
		if res, ok := m.abandonWait(h, claimed); !ok {
			return res
		}
		return spawnAgentResult{
			Name:   h.Name,
			Branch: h.Branch,
			Status: "running: wait canceled; still running in background",
		}
	}
}

// abandonWait resolves the race between a waiter giving up and the
// completion goroutine finishing. ok=true means the claim was released
// cleanly and the caller should report "still running". ok=false means
// the goroutine consumed the claim first and suppressed its alert on
// this waiter's behalf, so the returned result must be delivered inline
// or the outcome is lost entirely. The blocking receive is bounded: the
// goroutine closes done immediately after the suppression check.
func (m *Manager) abandonWait(h *Handle, claimed bool) (spawnAgentResult, bool) {
	// The subagent may have finished in the very instant the wait gave up;
	// select picks arbitrarily when two cases are ready at once. Prefer the
	// result, which is already in hand. This also covers the completions
	// that never alert at all (shouldAlert false — e.g. a stopped
	// subagent), where "still running, result will be pushed" would be a
	// promise nothing keeps.
	select {
	case <-h.Done():
		return completionResult(h), false
	default:
	}
	if !claimed || h.releaseSync() {
		return spawnAgentResult{}, true
	}
	<-h.Done()
	return completionResult(h), false
}

// completionResult renders a terminal handle as the synchronous spawn's
// tool result.
//
// Output keeps the completion report as the primary deliverable — the
// done-tool prose spawned subagents get (subagentDoneToolDescription)
// tells them to put the findings there, which is the durable half of
// #641. FinalText is carried alongside it whenever it adds something,
// so a persona that answers in prose and then reports a one-line status
// doesn't leave the parent with nothing but the status.
func completionResult(h *Handle) spawnAgentResult {
	status := h.Status()
	res := spawnAgentResult{Name: h.Name, Branch: h.Branch, Status: status.String()}
	runErr := h.Err()
	r := h.Result()
	if r != nil || runErr != nil {
		var reason autonomous.StopReason
		if r != nil {
			reason = r.Reason
		}
		res.StopReason = stopClass(status, reason, runErr)
	}
	if runErr != nil {
		res.Output = runErr.Error()
		return res
	}
	if r == nil {
		return res
	}
	res.Output = r.DoneDetail
	if res.Output == "" {
		// Ended without signalling completion (budget cap, deferral) —
		// the last assistant text is all there is.
		res.Output = r.FinalText
	} else if r.FinalText != "" && r.FinalText != res.Output {
		res.FinalText = r.FinalText
	}
	return res
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
// calls spawn_agent. The `tools` description names no tools itself:
// rosterTool.Declaration appends this build's real grantable catalog
// (see withGrantableTools), because a hard-coded example lists names a
// distroless build never registered. The model may also list any
// MCP-namespaced tool or skill name in extras.
type spawnAgentArgs struct {
	Agent               string   `json:"agent,omitempty" jsonschema:"name of a preconfigured subagent to spawn (the preferred form). Its persona, tools, model, and budgets are already set; you supply the goal and may only narrow the rest (drop tools, tighten budgets, downshift model to 'small'). Leave empty only to author an ad-hoc subagent inline, which requires system_prompt and is disabled unless the operator enabled ad-hoc spawns."`
	Name                string   `json:"name,omitempty" jsonschema:"optional instance name. For a preconfigured 'agent', omit to let the runtime auto-name the instance (e.g. cluster-1). Required for an ad-hoc subagent. No spaces, dots or slashes."`
	SystemPrompt        string   `json:"system_prompt,omitempty" jsonschema:"ad-hoc only: the subagent's system instruction. Ignored when 'agent' references a preconfigured subagent (its persona is fixed)."`
	Goal                string   `json:"goal" jsonschema:"the task the subagent should accomplish, written as a single instruction"`
	Model               string   `json:"model,omitempty" jsonschema:"optional model override: omit to inherit (the preconfigured subagent's model, or the parent's), or 'small' to downshift to the small tier. A specific model is not selectable here — configure a dedicated subagent for that."`
	Tools               []string `json:"tools,omitempty" jsonschema:"tool names to grant. For a preconfigured 'agent' this may only NARROW its grant (a subset — unlisted tools are dropped). For an ad-hoc subagent these are the built-in tools it may use. Unknown names error at spawn time."`
	Extras              []string `json:"extras,omitempty" jsonschema:"ad-hoc only: additional tool names beyond the built-ins (e.g. MCP tools like kubectl_get, or skill names). Looked up in the same catalog as tools."`
	MaxTurns            int      `json:"max_turns,omitempty" jsonschema:"tighten the per-subagent turn cap (may only lower a preconfigured subagent's cap)"`
	MaxCostUSD          float64  `json:"max_cost_usd,omitempty" jsonschema:"tighten the per-subagent dollar cap (may only lower a preconfigured subagent's cap)"`
	MaxWallclockSeconds int      `json:"max_wallclock_seconds,omitempty" jsonschema:"tighten the per-subagent wall-clock cap (may only lower a preconfigured subagent's cap)"`
	Scheduler           string   `json:"scheduler,omitempty" jsonschema:"ad-hoc only: between-turn scheduler for this subagent. Values: 'default' (use the manager's default — typical), 'sleep' (in-process goroutine sleep — long-lived daemon shape), 'exit_on_defer' (exit cleanly so an orchestrator like k8s CronJob restarts at the wake-time), 'none' (no scheduler — schedule_next_turn unavailable, useful for one-shot triage subagents). Default: 'default'. This also picks how the subagent stops: with a scheduler it is a standing worker that keeps looping until a budget or an explicit return; without one it is a bounded delegation that finishes as soon as it stops calling tools."`
	Wait                bool     `json:"wait,omitempty" jsonschema:"set true to run the subagent synchronously: block this turn until it finishes and return its final output inline (like a direct delegation). Omit (the default) to fire-and-continue — the subagent runs in the background and its result is pushed to a later turn. A synchronous wait is capped by a tighter wall-clock; on timeout the subagent keeps running in the background and its result is pushed later."`
}

type spawnAgentResult struct {
	Name   string `json:"name"`
	Branch string `json:"branch"`
	Status string `json:"status"`
	// Error is the refusal text for a spawn that never launched —
	// invalid spec, depth or concurrency cap, unknown reference, a
	// self-spawn (#742). Empty for every launch that happened,
	// including one whose subagent later failed: this field is about
	// the CALL, not the run's outcome (that is StopReason).
	//
	// It exists because the errors-as-results contract above hides the
	// failure from everything except the model: a refusal and a launch
	// were the same shape, so the TUI drew "↳ branch, name, status" for
	// both and an operator watching the 2026-08-14 UAT read a refused
	// self-spawn as a subagent that had started (#746). "error" is
	// ADK's reserved key for exactly this, so populating it lights up
	// the failure affordance every consumer already has.
	Error string `json:"error,omitempty"`
	// Output is the subagent's deliverable on a synchronous spawn
	// (wait: true) that ran to completion: its completion report, or its
	// final text for a run that ended without signalling completion
	// (budget cap, deferral). Empty for fire-and-continue spawns and for
	// a wait that timed out (the result is pushed later).
	Output string `json:"output,omitempty"`
	// FinalText is the subagent's last assistant text, carried alongside
	// Output when it says something Output doesn't (#641).
	//
	// Which of the two holds the substance depends on how the subagent's
	// persona writes: some put the findings in the completion report,
	// some answer in prose and then report a one-line status. Returning
	// only the report is what made a parent re-derive an analysis it had
	// just delegated, so both channels are surfaced.
	FinalText string `json:"final_text,omitempty"`
	// StopReason says whether Output is a finished result or a partial:
	// "natural" (the subagent finished), "max_steps" / "budget" (it ran
	// out of room — re-ask with what is missing), "deferred" (it will
	// resume on its own), "stopped", or "error". Empty for a
	// fire-and-continue spawn and for a wait that timed out, neither of
	// which has an outcome yet. See StopClass.
	StopReason StopClass `json:"stop_reason,omitempty"`
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
				return refusedSpawn(name, err), nil
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
		Name:        SpawnAgentToolName,
		Description: spawnAgentDescription,
	}, handler)
	if err != nil {
		panic("background: NewSpawnAgentTool: " + err.Error())
	}
	// The roster wrapper folds the configured subagents into the schema
	// the model sees (#640). It must wrap rather than be baked into the
	// description above: the roster isn't known until
	// SetSubagentTemplates runs, which is after construction.
	return rosterTool{inner: t, mgr: mgr}
}

// spawnAgentDescription is the static half of spawn_agent's model-facing
// description; rosterTool appends the live roster to it (#640).
const spawnAgentDescription = "Spawn an in-process background subagent that runs in parallel with you. " +
	"Prefer referencing a preconfigured subagent by name via the 'agent' field and supplying its 'goal' (optionally a quick plan) — its persona, tools, model, and budgets are already set by the operator, and you may only narrow them. " +
	"When a configured subagent covers the task, delegate to it instead of doing the work yourself: it exists because the operator scoped it for exactly this. " +
	"Authoring an ad-hoc subagent inline (system_prompt + tools) is only possible when the operator enabled ad-hoc spawns. " +
	"The subagent runs autonomously; you'll receive its updates as '[Background reports]' lines prepended to your next turn when it calls report_alert or finishes. " +
	"Use this for tasks that should run continuously (monitoring) or in parallel (independent fan-out work). " +
	"Do NOT list 'schedule_next_turn', 'report_alert', 'return_result', or its aliases 'report_done' / 'report_completed' / 'mark_task_done' in the tools field — those are auto-wired into every subagent by the runtime; listing them is a no-op (silently skipped)."

type stopAgentArgs struct {
	Name string `json:"name" jsonschema:"the name of the subagent to stop"`
}

type stopAgentResult struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	// Error mirrors spawnAgentResult.Error: a stop that didn't happen
	// (unknown name) is a failed call, and saying so under the reserved
	// key is what keeps it from rendering as a stop that did (#746).
	Error string `json:"error,omitempty"`
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
				Error:  err.Error(),
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
		Name:        StopAgentToolName,
		Description: "Stop a running background subagent. The subagent's goroutine exits at its next checkpoint; its terminal status becomes 'stopped'.",
	}, handler)
	if err != nil {
		panic("background: NewStopAgentTool: " + err.Error())
	}
	return t
}

// The model-facing names of the two delegation tools. Exported because
// the delegation surface is something a caller has to be able to reason
// about by name: the CLI carves these out of what a subagent inherits so
// a subagent doesn't get a delegation surface it never asked for (#748).
const (
	SpawnAgentToolName = "spawn_agent"
	StopAgentToolName  = "stop_agent"
)

// SpawnToolNames returns the names NewSpawnTools registers, for callers
// that need to filter a tool slice rather than build one — chiefly the
// CLI, which withholds the delegation surface from subagents that
// inherit the parent's registry wholesale (#748).
func SpawnToolNames() []string {
	return []string{SpawnAgentToolName, StopAgentToolName}
}

// NewSpawnTools is a convenience that returns both model-facing
// background-agent tools (spawn_agent + stop_agent) in one slice, ready
// to pass through agent.WithTools. The bundled CLI uses this to wire the
// suite atomically. Introspection (list/check) is intentionally NOT a
// model tool: completed subagents push their results back to the parent
// (the [Background reports] channel) and spawn_agent {wait:true} covers
// blocking needs, so a poll loop is redundant. Operators inspect live
// instances out-of-band via the attach hub / TUI.
func NewSpawnTools(mgr *Manager) []tool.Tool {
	return []tool.Tool{
		NewSpawnAgentTool(mgr),
		NewStopAgentTool(mgr),
	}
}
