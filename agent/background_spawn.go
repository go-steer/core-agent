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
	"fmt"
	"strings"
	"time"

	"google.golang.org/adk/tool"

	"github.com/go-steer/core-agent/permissions"
)

// Spawn launches a new background subagent under spec. parentBranch
// is the branch the calling tool's context carries (typically empty
// for the top-level parent, "bg.<name>" when nested); the subagent's
// own branch becomes "<parentBranch>.bg.<spec.Name>" via composeBranch
// so the eventlog audit trail remains hierarchical.
//
// Returns the handle immediately; the subagent's goroutine runs
// RunAutonomous against spec.Goal until budgets fire, the model
// signals done via report_completed, the parent calls Stop, or the
// goroutine's context is cancelled.
//
// Returned errors are pre-flight: invalid spec, depth or concurrency
// cap exceeded, unknown tool name, or manager not yet attached to a
// parent. Once the goroutine is running, terminal errors land on the
// handle (h.Err()) and a corresponding Alert is pushed.
func (m *BackgroundAgentManager) Spawn(ctx context.Context, parentBranch string, spec BackgroundSpec) (*BackgroundHandle, error) {
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
	if depth := CurrentSubagentDepth(ctx); depth >= m.maxDepth {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w (depth=%d, max=%d)", ErrDepthExceeded, depth, m.maxDepth)
	}
	if _, exists := m.agents[spec.Name]; exists {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: %q", ErrSubagentExists, spec.Name)
	}
	if m.maxConcurrent > 0 && m.runningCount() >= m.maxConcurrent {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w (running=%d, max=%d)", ErrTooManyConcurrent, m.runningCount(), m.maxConcurrent)
	}
	// Reserve the slot before we drop the lock so a concurrent Spawn
	// of the same name (or contending for the last concurrency slot)
	// sees us already registered.
	branch := composeBranch(parentBranch, "bg."+spec.Name)
	handle := &BackgroundHandle{
		Name:      spec.Name,
		Branch:    branch,
		StartedAt: time.Now(),
		status:    StatusRunning,
		done:      make(chan struct{}),
	}
	m.agents[spec.Name] = handle
	m.mu.Unlock()

	// Resolve tools outside the lock — catalog is read-only after
	// construction, so safe.
	tools, err := m.resolveTools(append([]string{}, append(spec.Tools, spec.Extras...)...))
	if err != nil {
		// Undo the reservation since the goroutine never launches.
		m.mu.Lock()
		delete(m.agents, spec.Name)
		m.mu.Unlock()
		return nil, err
	}

	// Resolve the per-spawn scheduler choice. A nil scheduler is a
	// valid outcome — it means "no between-turn pacing for this
	// subagent" and the schedule_next_turn tool simply isn't
	// registered.
	sched, err := m.resolveScheduler(spec.Scheduler)
	if err != nil {
		m.mu.Lock()
		delete(m.agents, spec.Name)
		m.mu.Unlock()
		return nil, err
	}

	// Build a fresh LLM per subagent — see docs/background-subagents-design.md
	// "LLM instance per subagent" for the rationale. Each call goes to the
	// provider's Model factory, which caches auth handles and HTTP transport
	// internally, so this is cheap.
	subModel, err := m.provider.Model(ctx, m.modelID)
	if err != nil {
		m.mu.Lock()
		delete(m.agents, spec.Name)
		m.mu.Unlock()
		return nil, fmt.Errorf("agent: BackgroundAgentManager: build subagent model: %w", err)
	}

	// Branch-wrap the parent's session.Service so every event the
	// subagent emits picks up the correct Branch label. The session
	// row itself is derived from the parent's so two concurrent
	// runners don't collide on ADK's optimistic-concurrency check.
	parentSvc := parent.SessionService()
	wrappedSvc := &branchInjectingService{
		inner:  parentSvc,
		branch: branch,
	}
	subSessionID := deriveSubagentSessionID(parent.SessionID(), "bg."+spec.Name)

	// Per-spawn budgets: spec overrides default, default fills the
	// rest. Zero values mean "no cap" for that dimension.
	budgets := mergeBudgets(m.defaultBudgets, spec.Budgets)

	// Build phase: the autonomous driver hands us a done-tool we have
	// to include alongside our subagent's tools + our own report
	// tools. The Agent we build inside `build` runs in its own
	// goroutine so the construction happens after the goroutine
	// starts (RunAutonomous calls build).
	subagentInstruction := spec.SystemPrompt
	subagentName := spec.Name
	subagentGoal := spec.Goal

	// Each spawn gets its own bounded goroutine context derived from
	// the caller's ctx. Stop cancels via the saved CancelFunc.
	goCtx, cancel := context.WithCancel(contextWithoutCancel(ctx))
	goCtx = context.WithValue(goCtx, subagentDepthKey{}, CurrentSubagentDepth(ctx)+1)
	goCtx = permissions.WithSubagentSource(goCtx, subagentName)

	handle.mu.Lock()
	handle.cancel = cancel
	handle.mu.Unlock()

	build := func(extraTools []tool.Tool) (*Agent, error) {
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
		return New(subModel,
			WithAppName(parent.AppName()),
			WithName(subagentName),
			WithInstruction(subagentInstruction),
			WithStreaming(parent.streaming),
			WithSession(parent.UserID(), subSessionID),
			WithTools(all),
			WithSessionService(wrappedSvc),
		)
	}

	go func() {
		defer close(handle.done)
		defer cancel()

		opts := []AutonomousOption{}
		if budgets.MaxTurns > 0 {
			opts = append(opts, WithMaxTurns(budgets.MaxTurns))
		}
		if budgets.MaxCost > 0 {
			opts = append(opts, WithMaxCost(budgets.MaxCost))
		}
		if budgets.MaxWallclock > 0 {
			opts = append(opts, WithMaxWallclock(budgets.MaxWallclock))
		}
		if budgets.PerTurnTimeout > 0 {
			opts = append(opts, WithPerTurnTimeout(budgets.PerTurnTimeout))
		}
		if m.gate != nil {
			opts = append(opts, WithPermissionsGate(m.gate))
		}
		if sched != nil {
			opts = append(opts, WithScheduler(sched))
		}

		result, runErr := RunAutonomous(goCtx, build, subagentGoal, opts...)

		handle.mu.Lock()
		handle.result = &result
		handle.err = runErr
		// Status precedence: an explicit Stop already set Stopped;
		// otherwise classify by outcome.
		if handle.status == StatusRunning {
			switch {
			case runErr != nil:
				handle.status = StatusFailed
			case result.Reason == StopReasonCompleted:
				handle.status = StatusCompleted
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
		case StatusFailed:
			kind = "failed"
			if runErr != nil {
				text = runErr.Error()
			} else {
				text = "stopped: " + string(result.Reason)
			}
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

// validateSpec rejects invalid Spec values early. Names are required
// and must be reasonable (no whitespace; no separators that would
// confuse branch parsing).
func validateSpec(spec BackgroundSpec) error {
	name := strings.TrimSpace(spec.Name)
	if name == "" {
		return fmt.Errorf("agent: BackgroundAgentManager: spec.Name is required")
	}
	if name != spec.Name {
		return fmt.Errorf("agent: BackgroundAgentManager: spec.Name must not have leading/trailing whitespace: %q", spec.Name)
	}
	if strings.ContainsAny(name, ". /") {
		return fmt.Errorf("agent: BackgroundAgentManager: spec.Name must not contain '.', '/' or spaces: %q", name)
	}
	if strings.TrimSpace(spec.SystemPrompt) == "" {
		return fmt.Errorf("agent: BackgroundAgentManager: spec.SystemPrompt is required")
	}
	if strings.TrimSpace(spec.Goal) == "" {
		return fmt.Errorf("agent: BackgroundAgentManager: spec.Goal is required")
	}
	return nil
}

// mergeBudgets returns a budget that uses spec's non-zero values and
// falls back to defaults for any zero field.
func mergeBudgets(defaults, spec BackgroundBudgets) BackgroundBudgets {
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
