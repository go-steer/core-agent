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
	return m.launch(ctx, parentBranch, resolvedSpawn{
		name:         spec.Name,
		goal:         spec.Goal,
		instrOpts:    instrOpts,
		tools:        tools,
		buildModel:   func(c context.Context) (adkmodel.LLM, error) { return m.provider.Model(c, resolvedModelID) },
		priceModelID: resolvedModelID,
		budgets:      mergeBudgets(m.defaultBudgets, spec.Budgets),
		scheduler:    sched,
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

	// Build phase: the autonomous driver hands us a done-tool we have
	// to include alongside our subagent's tools + our own report
	// tools. The Agent we build inside `build` runs in its own
	// goroutine so the construction happens after the goroutine
	// starts (autonomous.Run calls build).
	build := func(extraTools []tool.Tool) (*agent.Agent, error) {
		// extraTools is the report_done tool the autonomous driver
		// injected; merge it with our subagent's chosen tools and
		// the always-on report_alert / report_completed tools.
		all := make([]tool.Tool, 0, len(baseTools)+len(extraTools)+2)
		all = append(all, baseTools...)
		all = append(all, extraTools...)
		all = append(all,
			newReportAlertTool(m, name),
			newReportCompletedTool(m, name),
		)
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

		opts := []autonomous.Option{}
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
		if parent.Tracker() != nil {
			opts = append(opts, autonomous.WithTracker(parent.Tracker(), usage.PriceFor(priceModelID, nil)))
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

		kind := "completed"
		text := result.DoneDetail
		switch finalStatus {
		case StatusCompleted:
			if text == "" {
				text = "(no detail provided)"
			}
		case StatusStopped:
			kind = "stopped"
			text = "stopped by parent"
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
		if !m.shouldAlert(name, handle) {
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
	return nil
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
