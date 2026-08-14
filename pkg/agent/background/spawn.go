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
	"errors"
	"fmt"
	"strings"
	"time"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/adk/tool"

	"github.com/go-steer/core-agent/v2/pkg/agent"
	"github.com/go-steer/core-agent/v2/pkg/agent/autonomous"
	"github.com/go-steer/core-agent/v2/pkg/agent/internal/subsession"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
	coretools "github.com/go-steer/core-agent/v2/pkg/tools"
	"github.com/go-steer/core-agent/v2/pkg/usage"
)

// subagentDoneToolDescription replaces the autonomous driver's default
// done-tool prose for spawned subagents (#641, restated for the
// result-style return tool in #728).
//
// The default asks for "a one-sentence detail explaining what you
// accomplished", which is right for a top-level autonomous run whose
// output a human reads directly. A subagent's caller is another MODEL:
// its completion report is handed back as the spawn_agent tool result,
// and a one-sentence status line leaves the parent with no findings, so
// it redoes the work it delegated. Stating that the report is the
// deliverable makes the useful content arrive by construction rather
// than depending on how the subagent's persona happens to write.
const subagentDoneToolDescription = "Return your result to the agent that delegated this task, and finish. " +
	"Put YOUR ACTUAL FINDINGS in the result argument — it is handed back to the delegating agent and " +
	"is the only thing it is guaranteed to receive. Write the answer, the root-cause analysis, the " +
	"proposed change, or the specific reason you stopped, with the evidence that supports it. Do not " +
	"write a status line like \"investigated the issue and found the cause\": the delegating agent " +
	"cannot see your work, so a summary of what you did forces it to redo it."

// subagentReturnToolAliases are the additional names wired to the same
// return signal as return_result (#728).
//
// Each one is a name a subagent's model has actually reached for:
//
//   - report_done — the autonomous driver's historical done tool, and
//     the name every existing subagent prompt in the wild names.
//   - report_completed — used to be a SEPARATE tool that pushed a
//     "completed" alert to the parent WITHOUT ending the loop, whose
//     own description told the model to "call report_done separately
//     to actually terminate". Calling it now returns, which is what a
//     model calling it always meant. The parent still gets a
//     "completed" alert: the terminal alert fired by the goroutine
//     wrapper in launch covers it.
//   - mark_task_done — the PARENT's checkpoint tool, deliberately not
//     registered on subagents (subtask.go). In the 2026-08-13 GKE UAT
//     the cluster subagent reached for it anyway, got tool-not-found,
//     and never recovered onto a real done tool: it had the answer and
//     no way to hand it back.
var subagentReturnToolAliases = []string{"report_done", "report_completed", "mark_task_done"}

// subagentReturnContract is the #727 instruction block, rendered for
// the async path — which does have a return tool, unlike a declarative
// subagent invoked synchronously as a parent tool. Both paths render
// from agent.SubagentReturnContract so the same declared subagent can't
// be told two different things depending on how it was reached.
var subagentReturnContract = agent.SubagentReturnContract(autonomous.DefaultReturnToolName)

// boundedReturnContract is the same block for a bounded delegation,
// which registers no return tool: it terminates when the model stops
// asking for tools, so the contract points at the last message
// instead of a tool call (#730).
var boundedReturnContract = agent.SubagentReturnContract("")

// resolvedSpawn is a fully-resolved launch recipe — everything the
// per-spawn goroutine needs after kind-specific resolution has run.
// Two callers build it: Spawn, from a catalog Spec (tools resolved by
// name against m.catalog, model built from m.provider); and
// SpawnTemplate, from a predefined SubagentTemplate (tools + toolsets
// pre-resolved by the declarative-subagent builder, model built from
// the template's own factory). Both then funnel through launch, which
// owns the shared reservation + goroutine machinery (#626).
type resolvedSpawn struct {
	// name is the per-instance name (Spec.Name, or a template's
	// auto-derived "<spec>-<n>"). goal is the task.
	name string
	goal string
	// specName is the DECLARED name behind the instance: the template
	// or predefined-spec this spawn references ("cluster" for instance
	// "cluster-2"), or the instance name itself for an ad-hoc spec,
	// which has no declaration to point back to. It is what the
	// self-spawn guard matches on — instance names are unique by
	// construction, so matching those would catch nothing (#732).
	specName string
	// instrOpts install the persona: WithExtraInstruction /
	// WithInstruction for a catalog Spec, WithUserInstruction for a
	// template (layer 4, matching the sync declarative path). Empty when
	// the subagent carries no persona layer.
	instrOpts []agent.Option
	// tools are the built-in tools (already resolved to instances);
	// toolsets are MCP + skills groups (nil for the catalog path, which
	// has no toolset dimension). Both are shared, stateless handles safe
	// to reuse across concurrent instances.
	tools    []tool.Tool
	toolsets []tool.Toolset
	// buildModel builds a fresh LLM for this spawn. Called after the
	// reservation so a Stop arriving during its (network) I/O still
	// cancels cleanly (#366). priceModelID labels the model for /usage.
	buildModel   func(context.Context) (adkmodel.LLM, error)
	priceModelID string
	// maxDepth caps the subagent's OWN nesting (0 = substrate default).
	maxDepth int
	// budgets + scheduler bound and pace the run.
	budgets   Budgets
	scheduler coretools.Scheduler
	// mode is the termination contract, already resolved past
	// ModeAuto by the time launch reads it.
	mode Mode
}

// Spawn launches a new background subagent under spec. parentBranch
// is the branch the calling tool's context carries (typically empty
// for the top-level parent, "bg.<name>" when nested); the subagent's
// own branch becomes "<parentBranch>.bg.<spec.Name>" via composeBranch
// so the eventlog audit trail remains hierarchical.
//
// Returns the handle immediately; the subagent's goroutine runs
// autonomous.Run against spec.Goal until budgets fire, the model
// signals done via report_completed, the parent calls Stop, or the
// goroutine's context is cancelled.
//
// Returned errors are pre-flight: invalid spec, depth or concurrency
// cap exceeded, unknown tool name, or manager not yet attached to a
// parent. Once the goroutine is running, terminal errors land on the
// handle (h.Err()) and a corresponding Alert is pushed.
func (m *Manager) Spawn(ctx context.Context, parentBranch string, spec Spec) (*Handle, error) {
	if err := validateSpec(spec); err != nil {
		return nil, err
	}
	// Resolve the catalog Spec's per-kind pieces: built-in tools by name,
	// scheduler choice, and the model id (the spec's own when set — a
	// predefined spec's configured model or a "small" downshift resolved
	// at reference time, #626 — else the manager's, typically the
	// parent's). These are fast + read-only, so they run before the
	// reservation; the slow model build happens inside launch, after.
	tools, err := m.resolveTools(append([]string{}, append(spec.Tools, spec.Extras...)...))
	if err != nil {
		return nil, err
	}
	sched, err := m.resolveScheduler(spec.Scheduler)
	if err != nil {
		return nil, err
	}
	resolvedModelID := m.modelID
	if id := strings.TrimSpace(spec.ModelID); id != "" {
		resolvedModelID = id
	}
	var instrOpts []agent.Option
	if strings.TrimSpace(spec.SystemPrompt) != "" {
		if spec.ReplaceSystemPrompt {
			// Pre-#459 escape hatch: bare prompt, no harness layers.
			instrOpts = []agent.Option{agent.WithInstruction(spec.SystemPrompt)}
		} else {
			instrOpts = []agent.Option{agent.WithExtraInstruction(spec.SystemPrompt)}
		}
	}
	// Ref is set by resolvePredefinedSpec and survives the caller's
	// rewrite of Name to a per-instance name; an ad-hoc spec has none,
	// and its own name is the closest thing to a declaration it has.
	specName := strings.TrimSpace(spec.Ref)
	if specName == "" {
		specName = spec.Name
	}
	return m.launch(ctx, parentBranch, resolvedSpawn{
		name:         spec.Name,
		specName:     specName,
		goal:         spec.Goal,
		instrOpts:    instrOpts,
		tools:        tools,
		buildModel:   func(c context.Context) (adkmodel.LLM, error) { return m.provider.Model(c, resolvedModelID) },
		priceModelID: resolvedModelID,
		budgets:      mergeBudgets(m.defaultBudgets, spec.Budgets),
		scheduler:    sched,
		mode:         spec.Mode,
	})
}

// launch reserves the subagent slot, builds a fresh LLM, and starts the
// goroutine that runs autonomous.Run against rs.goal. It is the single
// path both Spawn (catalog Spec) and SpawnTemplate (predefined template)
// funnel through, so all the concurrency-safety fixes live in one place:
// the depth + concurrency caps and same-name eviction under the lock, the
// cancel registered before the handle becomes visible (#366), the
// identity-checked rollback on a pre-launch model error (#488/#502), and
// the shouldAlert suppression of a stale incarnation's terminal alert.
func (m *Manager) launch(ctx context.Context, parentBranch string, rs resolvedSpawn) (*Handle, error) {
	// Validation + caps + parent presence are all checked under the
	// manager lock so a burst of concurrent launches can't all pass the
	// cap check before any registers a handle.
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, ErrManagerClosed
	}
	parent := m.parent
	if parent == nil {
		m.mu.Unlock()
		return nil, ErrNoParent
	}
	if err := validateSpawnName(rs.name); err != nil {
		m.mu.Unlock()
		return nil, err
	}
	if strings.TrimSpace(rs.goal) == "" {
		m.mu.Unlock()
		return nil, errors.New("background: goal is required")
	}
	if depth := subsession.CurrentDepth(ctx); depth >= m.maxDepth {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w (depth=%d, max=%d)", ErrDepthExceeded, depth, m.maxDepth)
	}
	// No depth cap can catch recursion that stays shallow: the observed
	// failure was a subagent spawning ITSELF at depth 1, under a cap of
	// 2, with a byte-identical goal — two agents investigating the same
	// incident and both billing the parent (#732). The lineage check is
	// structural and matches on the declared name, so "cluster-1"
	// spawning "cluster-2" is caught even though the instance names
	// differ.
	if subsession.InLineage(ctx, rs.specName) {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: %q is already running as an ancestor of this spawn — do the work in this run, or delegate to a different subagent", ErrSelfSpawn, rs.specName)
	}
	if existing, exists := m.agents[rs.name]; exists {
		if existing.Status() == StatusRunning {
			m.mu.Unlock()
			return nil, fmt.Errorf("%w: %q", ErrSubagentExists, rs.name)
		}
		// The previous holder of this name reached a terminal state
		// (completed / failed / stopped / deferred) — evict its handle
		// so the name is reusable. Without the eviction a name was
		// burned forever once its subagent finished, which broke
		// re-spawn-with-same-name workflows.
		delete(m.agents, rs.name)
	}
	if m.maxConcurrent > 0 && m.runningCount() >= m.maxConcurrent {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w (running=%d, max=%d)", ErrTooManyConcurrent, m.runningCount(), m.maxConcurrent)
	}
	// Reserve the slot before we drop the lock so a concurrent launch
	// of the same name (or contending for the last concurrency slot)
	// sees us already registered.
	branch := subsession.ComposeBranch(parentBranch, "bg."+rs.name)
	// Create the goroutine's cancellable context and register its
	// cancel on the handle BEFORE the handle becomes visible in
	// m.agents. Otherwise a Stop() arriving during the slow model build
	// below (which does network I/O) would find handle.cancel == nil,
	// mark the subagent Stopped, and return — while the goroutine still
	// launched and ran the full autonomous loop, burning budget under a
	// "stopped" status (#366). With cancel registered up front, such a
	// Stop() cancels goCtx, so when the goroutine launches autonomous.Run
	// exits immediately and the status stays Stopped.
	goCtx, cancel := context.WithCancel(contextWithoutCancel(ctx))
	goCtx = subsession.WithDepth(goCtx, subsession.CurrentDepth(ctx)+1)
	// Record the declaration, not the instance: what a nested spawn is
	// checked against is "which subagents am I inside of" (#732).
	goCtx = subsession.WithLineage(goCtx, rs.specName)
	goCtx = permissions.WithSubagentSource(goCtx, rs.name)
	handle := &Handle{
		Name:      rs.name,
		Branch:    branch,
		StartedAt: time.Now(),
		status:    StatusRunning,
		done:      make(chan struct{}),
		cancel:    cancel,
	}
	m.agents[rs.name] = handle
	m.mu.Unlock()

	// Build a fresh LLM per subagent — see docs/background-subagents-design.md
	// "LLM instance per subagent" for the rationale. For a catalog Spec this
	// goes to the provider's Model factory (which caches auth handles + HTTP
	// transport, so it's cheap); for a template it calls the declarative
	// builder's factory (same provider.Model underneath). Done after the
	// reservation so a Stop during its I/O cancels cleanly (#366).
	subModel, err := rs.buildModel(ctx)
	if err != nil {
		// Undo the reservation since the goroutine never launches, and
		// release the goroutine context we registered up front (#366).
		m.abortSpawn(rs.name, handle, cancel)
		return nil, fmt.Errorf("background: build subagent model: %w", err)
	}

	// Branch-wrap the parent's session.Service so every event the
	// subagent emits picks up the correct Branch label. The session
	// row itself is derived from the parent's so two concurrent
	// runners don't collide on ADK's optimistic-concurrency check.
	parentSvc := parent.SessionService()
	wrappedSvc := &subsession.BranchInjectingService{
		Inner:  parentSvc,
		Branch: branch,
	}
	// No invocation-unique component: a background agent is addressed
	// (and resumed/reported on) by its stable name, so its derived row
	// is intentionally deterministic (unlike the parallel-tool-call
	// subagent path in subagent.go, #364).
	subSessionID := subsession.DeriveSessionID(parent.SessionID(), "bg."+rs.name, "")

	name := rs.name
	goal := rs.goal
	sched := rs.scheduler
	budgets := rs.budgets
	priceModelID := rs.priceModelID
	baseTools := rs.tools
	toolsets := rs.toolsets
	instrOpts := rs.instrOpts
	maxDepth := rs.maxDepth
	// Resolved here rather than in the two callers so ModeAuto's rule
	// has exactly one implementation (#730).
	bounded := resolveMode(rs.mode, rs.scheduler) != ModeStanding

	// Build phase: the autonomous driver hands us a done-tool we have
	// to include alongside our subagent's tools + our own report
	// tools. The Agent we build inside `build` runs in its own
	// goroutine so the construction happens after the goroutine
	// starts (autonomous.Run calls build).
	build := func(extraTools []tool.Tool) (*agent.Agent, error) {
		// extraTools is the return_result tool (plus its aliases) the
		// autonomous driver injected; merge it with our subagent's
		// chosen tools and the always-on report_alert tool.
		//
		// report_completed used to be built here as its own tool. It
		// is now one of the driver's return aliases (#728) so calling
		// it actually returns, instead of acking and leaving the loop
		// to re-drive the model past its own answer.
		all := make([]tool.Tool, 0, len(baseTools)+len(extraTools)+1)
		all = append(all, baseTools...)
		all = append(all, extraTools...)
		all = append(all, newReportAlertTool(m, name))
		opts := make([]agent.Option, 0, len(instrOpts)+9)
		opts = append(opts,
			agent.WithAppName(parent.AppName()),
			agent.WithName(name),
			// name may be MODEL-CHOSEN (ad-hoc) — stamping it on the
			// invocation histogram would accrete one series per invented
			// name on a long-lived daemon. Metrics get the bounded
			// class-level identity instead.
			agent.WithMetricAgentName("background_subagent"),
			agent.WithMode(agent.ModeAutonomous),
			agent.WithStreaming(parent.Streaming()),
			agent.WithSession(parent.UserID(), subSessionID),
			agent.WithTools(all),
			agent.WithSessionService(wrappedSvc),
		)
		// Inherit the parent's resolved MeterProvider so a spawned
		// subagent's gen_ai.* instruments land in the SAME provider as
		// the parent — otherwise agent.New falls back to the process
		// global, and an embedder that passed a non-global provider to
		// the parent would see subagent turns/tools vanish from their
		// pipeline. In the daemon the parent's provider IS the global, so
		// this is a no-op there. Nil-guarded for hand-constructed parents.
		if mp := parent.MeterProvider(); mp != nil {
			opts = append(opts, agent.WithMeterProvider(mp))
		}
		opts = append(opts, instrOpts...)
		// AFTER instrOpts: WithExtraInstruction appends in call order,
		// and a spec's persona may itself be installed via
		// WithInstruction (which replaces layers 1-3). Appending last
		// keeps the return contract present under either arrangement.
		contract := subagentReturnContract
		if bounded {
			// A bounded delegation has no return tool to name, so it
			// gets the last-message form of the same contract — telling
			// it to call a tool that isn't registered is the #641 shape
			// (reached for mark_task_done, got tool-not-found, never
			// recovered) with a new name.
			contract = boundedReturnContract
		}
		opts = append(opts, agent.WithExtraInstruction(contract))
		if len(toolsets) > 0 {
			opts = append(opts, agent.WithToolsets(toolsets))
		}
		if maxDepth > 0 {
			opts = append(opts, agent.WithSubagentMaxDepth(maxDepth))
		}
		return agent.New(subModel, opts...)
	}

	go func() {
		defer close(handle.done)
		defer cancel()

		// A subagent's completion report is a deliverable handed to
		// another agent, not a status line for a human scanning a log.
		// The driver's default prose asks for "a one-sentence detail",
		// which is what produced the content-free "successfully diagnosed
		// the issue" reports the parent then had to re-derive (#641).
		//
		// A bounded delegation takes the other path (#730): it ends
		// when the model stops calling tools, so it registers no
		// return tool at all and there is nothing to describe.
		var opts []autonomous.Option
		if bounded {
			opts = append(opts, autonomous.WithStopOnNaturalEnd())
		} else {
			opts = append(opts, autonomous.WithReturnTool(autonomous.ReturnToolConfig{
				Aliases:     subagentReturnToolAliases,
				Description: subagentDoneToolDescription,
			}))
		}
		if budgets.MaxTurns > 0 {
			opts = append(opts, autonomous.WithMaxTurns(budgets.MaxTurns))
		}
		if budgets.MaxCost > 0 {
			opts = append(opts, autonomous.WithMaxCost(budgets.MaxCost))
		}
		if budgets.MaxWallclock > 0 {
			opts = append(opts, autonomous.WithMaxWallclock(budgets.MaxWallclock))
		}
		if budgets.PerTurnTimeout > 0 {
			opts = append(opts, autonomous.WithPerTurnTimeout(budgets.PerTurnTimeout))
		}
		if m.gate != nil {
			opts = append(opts, autonomous.WithPermissionsGate(m.gate))
		}
		if sched != nil {
			opts = append(opts, autonomous.WithScheduler(sched))
		}
		// Roll background subagent turns into the parent agent's usage
		// tracker so /usage + /stats reflect the actual session cost,
		// not just the parent conversation. Pricing is looked up per
		// the subagent's own model ID (may differ from parent when the
		// manager was constructed with a cheaper flash-tier model), so
		// the per-model attribution in /usage stays accurate.
		//
		// Pricing goes on whether or not the parent has a tracker
		// (#729). The driver computes the run's cost from it, and
		// budgets.MaxCost is checked against that number — so a
		// tracker-less parent used to hand its subagents a MaxCost the
		// driver could never evaluate, because every turn priced out
		// at exactly $0. A budget that silently doesn't apply is worse
		// than no budget: the caller believes the delegation is bounded.
		price := usage.PriceFor(priceModelID, nil)
		if parent.Tracker() != nil {
			opts = append(opts, autonomous.WithTracker(parent.Tracker(), price))
		} else {
			opts = append(opts, autonomous.WithPricing(price))
		}

		result, runErr := autonomous.Run(goCtx, build, goal, opts...)

		handle.mu.Lock()
		handle.result = &result
		handle.err = runErr
		// Status precedence: an explicit Stop already set Stopped;
		// otherwise classify by outcome.
		if handle.status == StatusRunning {
			switch {
			case runErr != nil:
				handle.status = StatusFailed
			case result.Reason == autonomous.StopReasonCompleted:
				handle.status = StatusCompleted
			case result.Reason == autonomous.StopReasonDeferred,
				result.Reason == autonomous.StopReasonWallclockExceeded,
				result.Reason == autonomous.StopReasonMaxTurns,
				result.Reason == autonomous.StopReasonMaxTokens,
				result.Reason == autonomous.StopReasonMaxCost:
				handle.status = StatusDeferred
			default:
				handle.status = StatusFailed
			}
		}
		finalStatus := handle.status
		handle.mu.Unlock()

		kind, text := terminalAlertText(finalStatus, result, runErr)
		if !m.shouldAlert(name, handle) {
			return
		}
		// A spawn_agent {wait: true} caller consumes this completion
		// inline as its tool result, so pushing the terminal alert too
		// would deliver the same outcome twice — once in the tool result
		// and again as a "[Background reports]" line on the parent's next
		// turn (#646). Skip it; the claim is marked consumed so a waiter
		// that times out in this exact window still delivers the result.
		if handle.takeSyncClaim() {
			return
		}
		m.pushAlert(Alert{
			From:      name,
			Text:      text,
			Kind:      kind,
			Timestamp: time.Now(),
		})
	}()

	return handle, nil
}

// terminalAlertText renders a finished subagent's outcome as the
// (kind, text) pair its terminal [Background reports] alert carries.
//
// This is the async twin of completionResult (pkg/agent/background/
// tools.go) and must stay in step with it: a spawn_agent {wait: true}
// whose wait times out is delivered HERE instead, so anything only the
// sync path renders is unreachable exactly when the subagent took long
// enough to be worth waiting for. That was #691 — the alert read
// DoneDetail alone, so a 6-minute subagent's findings reached the
// parent as the one-line status "successfully diagnosed the issue and
// provided the patch" (no patch included), and the parent re-derived
// the whole diagnosis over 91 turns and $1.31.
//
// The two renderings differ only in shape, not content: the tool result
// has two JSON fields, an alert has one text blob, so the same
// supplementary text is appended under the sync path's field name
// rather than beside it.
func terminalAlertText(status Status, result autonomous.RunResult, runErr error) (kind, text string) {
	kind, text = "completed", result.DoneDetail
	switch status {
	case StatusStopped:
		// An explicit parent Stop: the parent asked for this and the
		// run was cancelled mid-thought, so partial text is noise.
		return "stopped", "stopped by parent"
	case StatusDeferred:
		kind = "deferred"
		text = "stopped: " + string(result.Reason)
	case StatusFailed:
		kind = "failed"
		if runErr != nil {
			text = runErr.Error()
		} else {
			text = "stopped: " + string(result.Reason)
		}
	}
	// Whatever the outcome line says, the work itself still has to
	// reach the parent. A budget-capped or failed subagent never calls
	// return_result, so its last assistant text is the ONLY record of
	// what it found — which is why subagentReturnContract tells it so
	// in the instruction rather than only in that tool's description.
	if status != StatusCompleted && result.DoneDetail != "" && result.DoneDetail != text {
		text += "\n\n" + result.DoneDetail
	}
	if result.FinalText != "" && result.FinalText != text && !strings.Contains(text, result.FinalText) {
		if text == "" {
			text = result.FinalText
		} else {
			// Same field name the sync tool result uses, so the two
			// surfaces are one thing a model has to understand.
			text += "\n\nfinal_text: " + result.FinalText
		}
	}
	if text == "" {
		text = "(no detail provided)"
	}
	// Machine-readable outcome, last so it can't be mistaken for part
	// of the deliverable. The async twin of spawnAgentResult.StopReason
	// (#730): a parent reading an alert has to make the same
	// finished-vs-partial call as one reading a sync tool result.
	//
	// Rendered only when the stop was NOT a natural end. Every alert
	// lands in the parent's next prompt as one bullet, so a trailer on
	// the common case is pure noise — and it loses nothing: `kind`
	// already separates completed from failed/deferred/stopped, so the
	// only ambiguity left is *inside* completed, and those are exactly
	// the classes that still get annotated. No trailer under
	// kind=completed therefore means "natural", unambiguously.
	//
	// The sync path (spawnAgentResult.StopReason) always populates it:
	// a JSON field costs nothing and machine readers shouldn't infer.
	if class := stopClass(status, result.Reason, runErr); class != StopNatural {
		text += "\n\nstop_reason: " + string(class)
	}
	return kind, text
}

// StopClass is the machine-readable answer to the one question a
// delegating agent has to answer about a returned result: is this
// finished, or is it a partial (#730)?
//
// Removing the "continue" re-drive from a bounded delegation means a
// subagent that runs out of room hands back what it has. That is the
// right contract — the parent holds the goal and can re-ask with
// specifics, which a blind "continue" injected inside the subagent
// cannot — but only if the parent can tell the two apart. Prose in a
// text blob is not sufficient.
type StopClass string

const (
	// StopNatural: the subagent finished — it stopped asking for tools
	// (bounded) or signalled completion (standing). The output is the
	// deliverable.
	StopNatural StopClass = "natural"
	// StopMaxSteps: the turn cap fired. A partial; re-ask with what is
	// still missing, or raise MaxTurns.
	StopMaxSteps StopClass = "max_steps"
	// StopBudget: a cost, token, or wall-clock bound fired. A partial.
	StopBudget StopClass = "budget"
	// StopDeferred: the subagent scheduled its own next turn and will
	// resume. Not a partial to re-ask — it isn't done with the loop.
	StopDeferred StopClass = "deferred"
	// StopStopped: the parent (or operator) stopped it. Whatever text
	// exists was cut mid-thought.
	StopStopped StopClass = "stopped"
	// StopError: the run failed, was cancelled, or exhausted its retry
	// policy.
	StopError StopClass = "error"
)

// stopClass classifies a terminal run. Status is consulted first,
// because the launch goroutine has already resolved the cases where
// status carries information the reason does not: an explicit Stop and
// a context cancellation reach the driver as the same StopReason but
// mean different things to the parent, and a run that failed is a
// failure whatever reason it recorded on the way out.
func stopClass(status Status, reason autonomous.StopReason, runErr error) StopClass {
	switch status {
	case StatusStopped:
		return StopStopped
	case StatusCompleted:
		return StopNatural
	case StatusFailed:
		return StopError
	}
	if runErr != nil {
		return StopError
	}
	switch reason {
	case autonomous.StopReasonCompleted:
		return StopNatural
	case autonomous.StopReasonMaxTurns:
		return StopMaxSteps
	case autonomous.StopReasonMaxCost,
		autonomous.StopReasonMaxTokens,
		autonomous.StopReasonWallclockExceeded:
		return StopBudget
	case autonomous.StopReasonDeferred:
		return StopDeferred
	default:
		// StopReasonContextCancelled, StopReasonRetryAborted, and any
		// reason added later: the run did not reach an ending of its
		// own. Erring toward "error" keeps a parent from treating an
		// unrecognized outcome as a finished result.
		return StopError
	}
}

// shouldAlert reports whether handle's terminal alert should still
// be delivered: yes unless the name is now owned by a DIFFERENT
// handle (#488). The parent consumes alerts by name, so an old
// incarnation's "completed" arriving after a same-name re-spawn
// would read as the NEW subagent finishing. A name that is merely
// gone (terminal-evicted, never re-spawned) still alerts — only a
// different handle owning the name suppresses.
func (m *Manager) shouldAlert(name string, handle *Handle) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	current, occupied := m.agents[name]
	return !occupied || current == handle
}

// abortSpawn rolls back a reservation whose goroutine will never
// launch: cancels the pre-registered goroutine context, releases the
// identity-checked reservation (#488), and closes handle.done —
// without that close, a Manager.Close that snapshotted the handle in
// the window between the reservation and this rollback would block
// on <-h.done forever, because the goroutine that normally closes it
// never starts (#502).
func (m *Manager) abortSpawn(name string, handle *Handle, cancel context.CancelFunc) {
	cancel()
	m.unreserve(name, handle)
	// Leave the orphan self-consistent for any holder that Get()'d
	// it mid-resolution: a terminal status (preserving an explicit
	// Stop's StatusStopped from the #366 flow) rather than a
	// forever-"running" handle whose done channel is closed.
	handle.mu.Lock()
	if handle.status == StatusRunning {
		handle.status = StatusFailed
		if handle.err == nil {
			handle.err = errors.New("background: spawn aborted before launch")
		}
	}
	handle.mu.Unlock()
	close(handle.done)
}

// unreserve rolls back a Spawn reservation after a pre-launch
// failure (tool/scheduler/model resolution), deleting the map slot
// ONLY if it still holds this attempt's handle. The identity check
// matters (#488): resolution runs outside m.mu and can do network
// I/O, so in the gap a Stop can mark this handle terminal, a
// same-name re-Spawn can evict it and register a NEW handle — and an
// unconditional delete would then remove the new subagent's handle
// (it keeps running, unreachable by name, with the name freed for a
// duplicate).
func (m *Manager) unreserve(name string, handle *Handle) {
	m.mu.Lock()
	if m.agents[name] == handle {
		delete(m.agents, name)
	}
	m.mu.Unlock()
}

// validateSpawnName rejects names that can't seed a branch label: empty,
// whitespace-padded, or carrying separators that would confuse branch
// parsing. Shared by validateSpec, validatePredefinedSpec, and launch.
func validateSpawnName(name string) error {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return fmt.Errorf("background: subagent name is required")
	}
	if trimmed != name {
		return fmt.Errorf("background: subagent name must not have leading/trailing whitespace: %q", name)
	}
	if strings.ContainsAny(trimmed, ". /") {
		return fmt.Errorf("background: subagent name must not contain '.', '/' or spaces: %q", trimmed)
	}
	return nil
}

// validateSpec rejects invalid Spec values early. Names are required
// and must be reasonable (no whitespace; no separators that would
// confuse branch parsing); a catalog Spec also needs a SystemPrompt and
// Goal (the per-spawn form always carries both).
func validateSpec(spec Spec) error {
	if err := validateSpawnName(spec.Name); err != nil {
		return err
	}
	if strings.TrimSpace(spec.SystemPrompt) == "" {
		return fmt.Errorf("background: spec.SystemPrompt is required")
	}
	if strings.TrimSpace(spec.Goal) == "" {
		return fmt.Errorf("background: spec.Goal is required")
	}
	switch spec.Mode {
	case ModeAuto, ModeBounded, ModeStanding:
	default:
		return fmt.Errorf("background: unknown spec.Mode %q (want %q, %q, or empty)", spec.Mode, ModeBounded, ModeStanding)
	}
	return nil
}

// resolveMode applies ModeAuto's rule: a subagent that can ask to be
// re-run later is a standing worker; everything else is a bounded
// delegation. Explicit modes pass through.
func resolveMode(mode Mode, sched coretools.Scheduler) Mode {
	if mode != ModeAuto {
		return mode
	}
	if sched != nil {
		return ModeStanding
	}
	return ModeBounded
}

// mergeBudgets returns a budget that uses spec's non-zero values and
// falls back to defaults for any zero field.
func mergeBudgets(defaults, spec Budgets) Budgets {
	out := defaults
	if spec.MaxTurns > 0 {
		out.MaxTurns = spec.MaxTurns
	}
	if spec.MaxCost > 0 {
		out.MaxCost = spec.MaxCost
	}
	if spec.MaxWallclock > 0 {
		out.MaxWallclock = spec.MaxWallclock
	}
	if spec.PerTurnTimeout > 0 {
		out.PerTurnTimeout = spec.PerTurnTimeout
	}
	return out
}

// contextWithoutCancel returns a context that carries ctx's values
// but is NOT cancelled when ctx is. The goroutine keeps running even
// if the spawn tool's caller goes away (e.g., the parent's turn
// completes while the subagent is still working).
//
// Go 1.21 added context.WithoutCancel for this exact purpose.
func contextWithoutCancel(ctx context.Context) context.Context {
	return context.WithoutCancel(ctx)
}
