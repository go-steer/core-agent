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

	"google.golang.org/adk/tool"

	"github.com/go-steer/core-agent/v2/pkg/agent"
	"github.com/go-steer/core-agent/v2/pkg/agent/autonomous"
	"github.com/go-steer/core-agent/v2/pkg/agent/internal/subsession"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
	"github.com/go-steer/core-agent/v2/pkg/usage"
)

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
	// Validation + caps + parent presence are all checked under the
	// manager lock so a burst of concurrent Spawn calls can't all
	// pass the cap check before any registers a handle.
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
	if err := validateSpec(spec); err != nil {
		m.mu.Unlock()
		return nil, err
	}
	if depth := subsession.CurrentDepth(ctx); depth >= m.maxDepth {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w (depth=%d, max=%d)", ErrDepthExceeded, depth, m.maxDepth)
	}
	if existing, exists := m.agents[spec.Name]; exists {
		if existing.Status() == StatusRunning {
			m.mu.Unlock()
			return nil, fmt.Errorf("%w: %q", ErrSubagentExists, spec.Name)
		}
		// The previous holder of this name reached a terminal state
		// (completed / failed / stopped / deferred) — evict its handle
		// so the name is reusable. Without the eviction a name was
		// burned forever once its subagent finished, which broke
		// re-spawn-with-same-name workflows.
		delete(m.agents, spec.Name)
	}
	if m.maxConcurrent > 0 && m.runningCount() >= m.maxConcurrent {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w (running=%d, max=%d)", ErrTooManyConcurrent, m.runningCount(), m.maxConcurrent)
	}
	// Reserve the slot before we drop the lock so a concurrent Spawn
	// of the same name (or contending for the last concurrency slot)
	// sees us already registered.
	branch := subsession.ComposeBranch(parentBranch, "bg."+spec.Name)
	// Create the goroutine's cancellable context and register its
	// cancel on the handle BEFORE the handle becomes visible in
	// m.agents. Otherwise a Stop() arriving during the slow tool /
	// scheduler / model resolution below (which can do network I/O)
	// would find handle.cancel == nil, mark the subagent Stopped, and
	// return — while the goroutine still launched and ran the full
	// autonomous loop, burning budget under a "stopped" status (#366).
	// With cancel registered up front, such a Stop() cancels goCtx, so
	// when the goroutine launches autonomous.Run exits immediately and
	// the status stays Stopped.
	goCtx, cancel := context.WithCancel(contextWithoutCancel(ctx))
	goCtx = subsession.WithDepth(goCtx, subsession.CurrentDepth(ctx)+1)
	goCtx = permissions.WithSubagentSource(goCtx, spec.Name)
	handle := &Handle{
		Name:      spec.Name,
		Branch:    branch,
		StartedAt: time.Now(),
		status:    StatusRunning,
		done:      make(chan struct{}),
		cancel:    cancel,
	}
	m.agents[spec.Name] = handle
	m.mu.Unlock()

	// Resolve tools outside the lock — catalog is read-only after
	// construction, so safe.
	tools, err := m.resolveTools(append([]string{}, append(spec.Tools, spec.Extras...)...))
	if err != nil {
		// Undo the reservation since the goroutine never launches, and
		// release the goroutine context we registered up front (#366).
		m.abortSpawn(spec.Name, handle, cancel)
		return nil, err
	}

	// Resolve the per-spawn scheduler choice. A nil scheduler is a
	// valid outcome — it means "no between-turn pacing for this
	// subagent" and the schedule_next_turn tool simply isn't
	// registered.
	sched, err := m.resolveScheduler(spec.Scheduler)
	if err != nil {
		m.abortSpawn(spec.Name, handle, cancel)
		return nil, err
	}

	// Build a fresh LLM per subagent — see docs/background-subagents-design.md
	// "LLM instance per subagent" for the rationale. Each call goes to the
	// provider's Model factory, which caches auth handles and HTTP transport
	// internally, so this is cheap.
	subModel, err := m.provider.Model(ctx, m.modelID)
	if err != nil {
		m.abortSpawn(spec.Name, handle, cancel)
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
	subSessionID := subsession.DeriveSessionID(parent.SessionID(), "bg."+spec.Name, "")

	// Per-spawn budgets: spec overrides default, default fills the
	// rest. Zero values mean "no cap" for that dimension.
	budgets := mergeBudgets(m.defaultBudgets, spec.Budgets)

	// Build phase: the autonomous driver hands us a done-tool we have
	// to include alongside our subagent's tools + our own report
	// tools. The Agent we build inside `build` runs in its own
	// goroutine so the construction happens after the goroutine
	// starts (autonomous.Run calls build).
	subagentInstruction := spec.SystemPrompt
	replacePrompt := spec.ReplaceSystemPrompt
	subagentName := spec.Name
	subagentGoal := spec.Goal

	build := func(extraTools []tool.Tool) (*agent.Agent, error) {
		// extraTools is the report_done tool the autonomous driver
		// injected; merge it with our subagent's chosen tools and
		// the always-on report_alert / report_completed tools.
		all := make([]tool.Tool, 0, len(tools)+len(extraTools)+2)
		all = append(all, tools...)
		all = append(all, extraTools...)
		all = append(all,
			newReportAlertTool(m, subagentName),
			newReportCompletedTool(m, subagentName),
		)
		instrOpt := agent.WithExtraInstruction(subagentInstruction)
		if replacePrompt {
			// Pre-#459 escape hatch: bare prompt, no harness layers.
			instrOpt = agent.WithInstruction(subagentInstruction)
		}
		return agent.New(subModel,
			agent.WithAppName(parent.AppName()),
			agent.WithName(subagentName),
			agent.WithMode(agent.ModeAutonomous),
			instrOpt,
			agent.WithStreaming(parent.Streaming()),
			agent.WithSession(parent.UserID(), subSessionID),
			agent.WithTools(all),
			agent.WithSessionService(wrappedSvc),
		)
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
			opts = append(opts, autonomous.WithTracker(parent.Tracker(), usage.PriceFor(m.modelID, nil)))
		}

		result, runErr := autonomous.Run(goCtx, build, subagentGoal, opts...)

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
		if !m.shouldAlert(subagentName, handle) {
			return
		}
		m.pushAlert(Alert{
			From:      subagentName,
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

// validateSpec rejects invalid Spec values early. Names are required
// and must be reasonable (no whitespace; no separators that would
// confuse branch parsing).
func validateSpec(spec Spec) error {
	name := strings.TrimSpace(spec.Name)
	if name == "" {
		return fmt.Errorf("background: spec.Name is required")
	}
	if name != spec.Name {
		return fmt.Errorf("background: spec.Name must not have leading/trailing whitespace: %q", spec.Name)
	}
	if strings.ContainsAny(name, ". /") {
		return fmt.Errorf("background: spec.Name must not contain '.', '/' or spaces: %q", name)
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
